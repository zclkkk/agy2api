package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
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
		"gemini-lite":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"},
		"gemini-unlisted":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"},
		"gemini-disabled":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI","disabled":true},
		"tab_preview":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"},
		"claude-test":{"apiProvider":"API_PROVIDER_ANTHROPIC_VERTEX"}
	},
	"agentModelSorts":[{"groups":[{"modelIds":["claude-test","gemini-z","missing","gemini-a","gemini-z","gemini-disabled"]}]}],
	"defaultAgentModelId":"gemini-z",
	"commitMessageModelIds":["gemini-lite"],
	"tieredModelIds":{"flash":["gemini-unlisted"]},
	"tabModelIds":["tab_preview"],
	"imageGenerationModelIds":["gemini-canvas"]
}`

// Both formats list the same chat models; image models only serve the OpenAI
// Images API.
var selectorListing = []string{"gemini-z", "gemini-a", "gemini-lite", "gemini-canvas"}

func checkModelListing(t *testing.T, server *httptest.Server, openAI []string) {
	t.Helper()
	var messages []string
	for _, id := range openAI {
		if id != "gemini-canvas" {
			messages = append(messages, id)
		}
	}
	checkFormatListing(t, server, false, openAI)
	checkFormatListing(t, server, true, messages)
}

func checkFormatListing(t *testing.T, server *httptest.Server, messages bool, want []string) {
	t.Helper()
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
	checkModelListing(t, s, selectorListing)
	for _, tc := range []struct {
		id       string
		messages bool
		want     int
	}{
		{"gemini-unlisted", false, http.StatusOK},
		{"gemini-unlisted", true, http.StatusOK},
		{"gemini-canvas", false, http.StatusOK},
		{"gemini-canvas", true, http.StatusNotFound},
		{"gemini-disabled", false, http.StatusNotFound},
		{"claude-test", false, http.StatusNotFound},
	} {
		req, _ := http.NewRequest(http.MethodGet, s.URL+"/v1/models/"+tc.id, nil)
		if tc.messages {
			req.Header.Set("Anthropic-Version", "2023-06-01")
		}
		res, err := s.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != tc.want {
			t.Fatalf("lookup of %s (messages=%v) returned %d", tc.id, tc.messages, res.StatusCode)
		}
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
	// Disabled entries fail upstream; they are rejected before any call.
	res, body := send(t, s, "/v1/responses", `{"model":"gemini-disabled","input":"hello","store":false}`, nil)
	if res.StatusCode != http.StatusBadRequest || !strings.Contains(body, "not available") {
		t.Fatalf("disabled model request returned %d: %s", res.StatusCode, body)
	}
}

func TestModelListingFollowsCatalogRefresh(t *testing.T) {
	var upstream atomic.Value
	upstream.Store(selectorCatalog)
	svc, s := newService(t, nil, func(w http.ResponseWriter) { io.WriteString(w, upstream.Load().(string)) }, answer("ok", "STOP"))
	checkModelListing(t, s, selectorListing)
	upstream.Store(`{"models":{"gemini-new":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"}},"agentModelSorts":[{"groups":[{"modelIds":["gemini-new"]}]}]}`)
	checkModelListing(t, s, selectorListing)
	svc.accounts[0].catalogMu.Lock()
	svc.accounts[0].catalogExpiry = time.Time{}
	svc.accounts[0].catalogMu.Unlock()
	checkModelListing(t, s, []string{"gemini-new"})
	upstream.Store(`{"models":{"gemini-new":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"}}}`)
	svc.accounts[0].catalogMu.Lock()
	svc.accounts[0].catalogExpiry = time.Time{}
	svc.accounts[0].catalogMu.Unlock()
	checkModelListing(t, s, nil)
}
