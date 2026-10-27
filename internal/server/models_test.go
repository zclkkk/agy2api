package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

const selectorCatalog = `{
	"models":{
		"gemini-z":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI","displayName":"Gemini Z"},
		"gemini-a":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"},
		"gemini-canvas":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"},
		"gemini-unlisted":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"},
		"tab_preview":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"},
		"claude-test":{"apiProvider":"API_PROVIDER_ANTHROPIC_VERTEX"}
	},
	"agentModelSorts":[{"groups":[{"modelIds":["claude-test","gemini-z","missing","gemini-a","gemini-z"]}]}],
	"imageGenerationModelIds":["gemini-canvas"]
}`

func checkModelListing(t *testing.T, server *httptest.Server, want []string) {
	t.Helper()
	for _, messages := range []bool{false, true} {
		req, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/models", nil)
		if messages {
			req.Header.Set("Anthropic-Version", "2023-06-01")
		}
		res, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var listing struct {
			Object string `json:"object"`
			Data   []struct {
				ID, Object, Type string
			} `json:"data"`
			FirstID *string `json:"first_id"`
			LastID  *string `json:"last_id"`
			HasMore bool    `json:"has_more"`
		}
		err = json.NewDecoder(res.Body).Decode(&listing)
		res.Body.Close()
		if err != nil || res.StatusCode != http.StatusOK || listing.Data == nil {
			t.Fatalf("invalid model list: status=%d error=%v listing=%+v", res.StatusCode, err, listing)
		}
		var ids []string
		for _, model := range listing.Data {
			ids = append(ids, model.ID)
			if messages && model.Type != "model" || !messages && model.Object != "model" {
				t.Fatalf("wrong model format: %+v", model)
			}
		}
		if !slices.Equal(ids, want) {
			t.Fatalf("listed models = %v, want %v", ids, want)
		}
		if messages {
			if listing.HasMore {
				t.Fatal("unexpected model pagination")
			}
			if len(want) == 0 {
				if listing.FirstID != nil || listing.LastID != nil {
					t.Fatal("empty list has pagination IDs")
				}
			} else if listing.FirstID == nil || listing.LastID == nil || *listing.FirstID != want[0] || *listing.LastID != want[len(want)-1] {
				t.Fatal("pagination IDs do not follow selector order")
			}
		} else if listing.Object != "list" {
			t.Fatal("missing OpenAI list object")
		}
	}
}

func TestModelListingDoesNotRestrictExplicitRequests(t *testing.T) {
	_, s := newService(t, nil, func(w http.ResponseWriter) { io.WriteString(w, selectorCatalog) }, func(w http.ResponseWriter, r *http.Request) {
		var request ag.Envelope
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.Model != "gemini-unlisted" {
			t.Errorf("upstream model = %q", request.Model)
		}
		answer("ok", "STOP")(w, r)
	})
	checkModelListing(t, s, []string{"gemini-z", "gemini-a", "gemini-canvas"})
	res, err := s.Client().Get(s.URL + "/v1/models/gemini-unlisted")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("unlisted model lookup returned %d", res.StatusCode)
	}
	for path, request := range map[string]string{
		"/v1/responses": `{"model":"gemini-unlisted","input":"hello","store":false}`,
		"/v1/messages":  `{"model":"gemini-unlisted","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`,
	} {
		res, body := send(t, s, path, request, nil)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("unlisted model request returned %d: %s", res.StatusCode, body)
		}
	}
}

func TestModelListingFollowsCatalogRefresh(t *testing.T) {
	var upstream atomic.Value
	upstream.Store(selectorCatalog)
	svc, s := newService(t, nil, func(w http.ResponseWriter) { io.WriteString(w, upstream.Load().(string)) }, answer("ok", "STOP"))
	checkModelListing(t, s, []string{"gemini-z", "gemini-a", "gemini-canvas"})
	upstream.Store(`{"models":{"gemini-new":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"}},"agentModelSorts":[{"groups":[{"modelIds":["gemini-new"]}]}]}`)
	checkModelListing(t, s, []string{"gemini-z", "gemini-a", "gemini-canvas"})
	svc.catalogMu.Lock()
	svc.catalogExpiry = time.Time{}
	svc.catalogMu.Unlock()
	checkModelListing(t, s, []string{"gemini-new"})
	upstream.Store(`{"models":{"gemini-new":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"}}}`)
	svc.catalogMu.Lock()
	svc.catalogExpiry = time.Time{}
	svc.catalogMu.Unlock()
	checkModelListing(t, s, nil)
}
