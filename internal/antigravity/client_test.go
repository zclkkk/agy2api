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
			client := Client{HTTP: up.Client(), BaseURL: up.URL, UserAgent: "test", Auth: testToken{}}
			err := client.Generate(context.Background(), "gemini-test", Request{}, false, func(Chunk) error { return nil })
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

func TestUnknownNativeContentIsNotSilentlyDiscarded(t *testing.T) {
	var part Part
	if err := json.Unmarshal([]byte(`{"newMediaType":{"data":"opaque"}}`), &part); err == nil {
		t.Fatal("unknown upstream content accepted")
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
