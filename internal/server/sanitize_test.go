package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
	"github.com/zclkkk/agy2api/internal/config"
)

func TestClientMarkerCleanupIsSharedByAllRequestPaths(t *testing.T) {
	const identity = "You are a Claude agent, built on Anthropic's Claude Agent SDK."
	const system = "x-anthropic-billing-header: cc_version=test; cch=DO_NOT_LOG_MARKER;\r\nKeep business rule A.\r\n" + identity + "\r\nKeep business rule B."
	const cleaned = "Keep business rule A.\r\nKeep business rule B."
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("enabled=%v", enabled), func(t *testing.T) {
			var mu sync.Mutex
			var seen []ag.Request
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1internal:fetchAvailableModels" {
					staticModels(w)
					return
				}
				var envelope ag.Envelope
				if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
					t.Error(err)
				}
				mu.Lock()
				seen = append(seen, envelope.Request)
				mu.Unlock()
				if r.URL.Path == "/v1internal:countTokens" {
					writeJSON(w, 200, map[string]int{"totalTokens": 7})
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				frame(w, chunk([]ag.Part{{Text: "A valid summary or reply."}}, "STOP", &ag.Usage{Input: 7, Output: 5}))
			}))
			defer up.Close()
			cfg := config.Default()
			cfg.SanitizeClientMarkers = enabled
			var logs bytes.Buffer
			svc := newServer(t, cfg, slog.New(slog.NewJSONHandler(&logs, nil)), testAccount(t, "test", testKey, up))
			s := startServer(t, svc, testKey)
			cases := []struct {
				path            string
				stream, trigger bool
			}{
				{"/v1/messages", false, false}, {"/v1/messages", true, false},
				{"/v1/responses", false, false}, {"/v1/responses", true, false},
				{"/v1/messages/count_tokens", false, false}, {"/v1/responses/input_tokens", false, false},
				{"/v1/responses/compact", false, false},
				{"/v1/responses", false, true}, {"/v1/responses", true, true},
			}
			for _, tc := range cases {
				body := map[string]any{"model": "gemini-test", "stream": tc.stream}
				if strings.HasPrefix(tc.path, "/v1/messages") {
					body["max_tokens"], body["system"] = 1024, system
					body["messages"] = []any{map[string]string{"role": "user", "content": identity}}
				} else {
					body["instructions"] = system
					items := []any{map[string]string{"role": "user", "content": identity}}
					if tc.trigger {
						items = append(items, map[string]string{"type": "compaction_trigger"})
					}
					body["input"] = items
				}
				res, output := send(t, s, tc.path, body, nil)
				if res.StatusCode != 200 {
					t.Fatalf("%s stream=%v: %d %s", tc.path, tc.stream, res.StatusCode, output)
				}
				mu.Lock()
				request := seen[len(seen)-1]
				mu.Unlock()
				want := system
				if enabled {
					want = cleaned
				}
				if request.System == nil || len(request.System.Parts) != 1 || request.System.Parts[0].Text != want {
					t.Fatalf("%s stream=%v: unexpected system: %+v", tc.path, tc.stream, request.System)
				}
				if request.Contents[0].Parts[0].Text != identity {
					t.Fatal("the same text in the user message was changed")
				}
			}
			if enabled {
				if strings.Count(logs.String(), "sanitize_client_markers: billing_header=1 claude_identity=1") != len(cases) {
					t.Fatalf("cleanup not logged once per request: %s", logs.String())
				}
			} else if strings.Contains(logs.String(), "sanitize_client_markers") {
				t.Fatal("disabled cleanup emitted a cleanup warning")
			}
			if strings.Contains(logs.String(), "DO_NOT_LOG_MARKER") || strings.Contains(logs.String(), identity) || strings.Contains(logs.String(), "Keep business rule") {
				t.Fatal("request text leaked into cleanup logs")
			}
		})
	}
}

func TestCleanupCoversNormalizedDeveloperAndSystemMessages(t *testing.T) {
	const marker = "You are Claude Code, Anthropic's official CLI for Claude."
	rec := &recorder{}
	_, s := newService(t, nil, staticModels, rec.handle)
	for _, path := range []string{"/v1/messages", "/v1/responses"} {
		body := map[string]any{"model": "gemini-test"}
		messages := []any{map[string]string{"role": "system", "content": marker + "\nKeep system rule."}, map[string]string{"role": "user", "content": "hello"}}
		if path == "/v1/messages" {
			body["messages"], body["max_tokens"] = messages, 100
		} else {
			messages[0] = map[string]string{"role": "developer", "content": marker + "\nKeep system rule."}
			body["input"] = messages
		}
		res, out := send(t, s, path, body, nil)
		if res.StatusCode != 200 {
			t.Fatalf("%d %s", res.StatusCode, out)
		}
		if got := rec.last(t).System.Parts[0].Text; got != "Keep system rule." {
			t.Fatal(got)
		}
	}
}
