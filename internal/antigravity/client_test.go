package antigravity

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type testToken struct{}

func (testToken) AccessToken(context.Context) (string, error) { return "test-token", nil }
func (testToken) ProjectID() string                           { return "test-project" }

func TestDecodeSSEFragmentationAndLimits(t *testing.T) {
	text := strings.Repeat("x", 100000)
	input := ": heartbeat\r\nevent: chunk\r\ndata: {\"text\":\r\ndata: \"" + text + "\"}\r\n\r\ndata: [DONE]"
	var frames []string
	if err := DecodeSSE(strings.NewReader(input), func(b []byte) error { frames = append(frames, string(b)); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 || frames[1] != "[DONE]" || !strings.Contains(frames[0], text) {
		t.Fatalf("unexpected frames: %d", len(frames))
	}
}

func TestGenerateDoesNotTreatTruncationOrTrailingErrorAsSuccess(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		wantErr    bool
	}{
		{"complete", "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}]}}\n\n", false},
		{"truncated", "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]}}]}}\n\ndata: [DONE]\n\n", true},
		{"trailing error", "data: {\"response\":{\"candidates\":[{\"finishReason\":\"STOP\"}]}}\n\n{\"error\":{\"code\":503}}", true},
		{"empty", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer test-token" {
					t.Error("missing auth")
				}
				if r.URL.Path != "/v1internal:streamGenerateContent" || r.URL.Query().Get("alt") != "sse" {
					t.Error(r.URL)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, tc.body)
			}))
			defer up.Close()
			client := Client{HTTP: up.Client(), BaseURL: up.URL, UserAgent: func() string { return "test" }, Auth: testToken{}}
			err := client.Generate(context.Background(), "gemini-test", Request{}, KindAgent, func(Chunk) error { return nil })
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestUpstreamErrorDoesNotLeakBody(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
		io.WriteString(w, "secret@example.com refresh-token-private")
	}))
	defer up.Close()
	c := Client{HTTP: up.Client(), BaseURL: up.URL, Auth: testToken{}}
	_, err := c.Models(context.Background())
	e, ok := err.(*HTTPError)
	if !ok || e.Status != 429 || e.RetryAfter != "60" {
		t.Fatal(err)
	}
	if strings.Contains(e.Error(), "secret") || strings.Contains(e.Error(), "private") {
		t.Fatal("body leaked")
	}
}

func TestUnknownNativeFieldsAreReportedNotFatal(t *testing.T) {
	var part Part
	if err := json.Unmarshal([]byte(`{"text":"hi","videoMetadata":{"fps":1},"thought_signature":"sig"}`), &part); err != nil {
		t.Fatal(err)
	}
	if part.Text != "hi" || part.Signature != "sig" || len(part.Unknown) != 1 || part.Unknown[0] != "videoMetadata" || part.Empty() {
		t.Fatalf("%+v", part)
	}
	part = Part{}
	if err := json.Unmarshal([]byte(`{"newMediaType":{"data":"opaque"}}`), &part); err != nil {
		t.Fatal(err)
	}
	if !part.Empty() || len(part.Unknown) != 1 {
		t.Fatalf("unknown-only part should be empty and reported: %+v", part)
	}
}

type rotatingToken struct {
	current     string
	invalidated []string
}

func (r *rotatingToken) AccessToken(context.Context) (string, error) { return r.current, nil }
func (r *rotatingToken) ProjectID() string                           { return "test-project" }
func (r *rotatingToken) Invalidate(token string) {
	r.invalidated = append(r.invalidated, token)
	r.current = "fresh-token"
}

func TestRejectedTokenIsRefreshedOnce(t *testing.T) {
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer fresh-token" {
			w.WriteHeader(401)
			return
		}
		io.WriteString(w, `{"models":{"m":{}}}`)
	}))
	defer up.Close()
	auth := &rotatingToken{current: "revoked-token"}
	c := Client{HTTP: up.Client(), BaseURL: up.URL, Auth: auth}
	if _, err := c.Models(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(auth.invalidated) != 1 || auth.invalidated[0] != "revoked-token" {
		t.Fatalf("calls=%d invalidated=%v", calls, auth.invalidated)
	}
}

func TestCatalogInfoUsesProviderAndModelLists(t *testing.T) {
	catalog := Catalog{
		Models:       map[string]json.RawMessage{"claude-x": json.RawMessage(`{"modelProvider":"MODEL_PROVIDER_ANTHROPIC","maxOutputTokens":64000}`), "painter": json.RawMessage(`{}`), "finder": json.RawMessage(`{"modelProvider":"MODEL_PROVIDER_GOOGLE"}`)},
		SearchModels: []string{"finder"},
		ImageModels:  []string{"painter"},
	}
	if info, ok := catalog.Info("claude-x"); !ok || !info.Claude() || info.MaxOutputTokens != 64000 || info.Image {
		t.Fatalf("%+v", info)
	}
	if info, _ := catalog.Info("painter"); !info.Image || info.Search {
		t.Fatalf("%+v", info)
	}
	if info, _ := catalog.Info("finder"); !info.Search || info.Claude() {
		t.Fatalf("%+v", info)
	}
	if _, ok := catalog.Info("missing"); ok {
		t.Fatal("unknown model reported as available")
	}
}

func TestModelCatalogExcludesInternalProvider(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"models":{"chat_internal":{"apiProvider":"API_PROVIDER_INTERNAL"},"gemini-test":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"},"future-model":{}}}`)
	}))
	defer up.Close()
	c := Client{HTTP: up.Client(), BaseURL: up.URL, Auth: testToken{}}
	catalog, err := c.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Models) != 2 {
		t.Fatal(catalog.Models)
	}
	if _, ok := catalog.Models["chat_internal"]; ok {
		t.Fatal("internal model was exposed as generative")
	}
}

func TestSafeUpstreamErrorReason(t *testing.T) {
	message := upstreamErrorMessage(503, []byte(`{"error":{"message":"secret@example.com sensitive-project","status":"UNAVAILABLE","details":[{"reason":"MODEL_CAPACITY_EXHAUSTED"},{"reason":"secret@example.com"}]}}`))
	if !strings.Contains(message, "MODEL_CAPACITY_EXHAUSTED") || strings.Contains(message, "secret") || strings.Contains(message, "project") {
		t.Fatal(message)
	}
}

func TestGoogleRetryInfoBecomesRetryAfter(t *testing.T) {
	body := []byte(`{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"1.25s"}]}}`)
	if got := upstreamRetryAfter("", body); got != "2" {
		t.Fatal(got)
	}
	if got := upstreamRetryAfter("60", body); got != "60" {
		t.Fatal(got)
	}
	if got := upstreamRetryAfter("", []byte(`{"error":{"details":[{"@type":"other","retryDelay":"100s"}]}}`)); got != "" {
		t.Fatal(got)
	}
}
