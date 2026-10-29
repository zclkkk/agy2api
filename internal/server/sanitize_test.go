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
	"sync/atomic"
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
			for _, tc := range []struct {
				path            string
				stream, trigger bool
			}{
				{"/v1/messages", false, false}, {"/v1/messages", true, false},
				{"/v1/responses", false, false}, {"/v1/responses", true, false},
				{"/v1/messages/count_tokens", false, false}, {"/v1/responses/input_tokens", false, false},
				{"/v1/responses/compact", false, false},
				{"/v1/responses", false, true}, {"/v1/responses", true, true},
			} {
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
				res, output := send(t, s, tc.path, mustJSON(body), nil)
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
				if strings.Count(logs.String(), "sanitize_client_markers: billing_header=1 claude_identity=1") != 9 {
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
		res, out := send(t, s, path, mustJSON(body), nil)
		if res.StatusCode != 200 {
			t.Fatalf("%d %s", res.StatusCode, out)
		}
		if got := rec.last(t).System.Parts[0].Text; got != "Keep system rule." {
			t.Fatal(got)
		}
	}
}

func TestMessagesTextSignatureIsRestoredWithoutTrailingThinking(t *testing.T) {
	var calls atomic.Int32
	_, s := newService(t, nil, staticModels, func(w http.ResponseWriter, r *http.Request) {
		var envelope ag.Envelope
		if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			frame(w, chunk([]ag.Part{{Thought: true, Text: "Plan", Signature: "thought-signature"}, {Text: "FIRST_ANSWER", Signature: "text-signature"}}, "STOP", nil))
			return
		}
		parts := envelope.Request.Contents[1].Parts
		if len(parts) != 2 || parts[0].Signature != "thought-signature" || parts[1].Text != "FIRST_ANSWER" || parts[1].Signature != "text-signature" {
			t.Errorf("native replay lost its signatures: %+v", parts)
		}
		frame(w, chunk([]ag.Part{{Text: "SECOND_ANSWER"}}, "STOP", nil))
	})
	headers := map[string]string{"Session-Id": "same-client-session"}
	res, body := send(t, s, "/v1/messages", `{"model":"gemini-test","max_tokens":1024,"messages":[{"role":"user","content":"first"}]}`, headers)
	if res.StatusCode != 200 {
		t.Fatal(body)
	}
	var first struct {
		Content []json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal([]byte(body), &first); err != nil {
		t.Fatal(err)
	}
	var last struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(first.Content[len(first.Content)-1], &last); err != nil {
		t.Fatal(err)
	}
	if last.Type != "text" || last.Text != "FIRST_ANSWER" {
		t.Fatalf("last block is not the final answer: %+v", last)
	}
	request := map[string]any{"model": "gemini-test", "max_tokens": 1024, "messages": []any{
		map[string]string{"role": "user", "content": "first"}, map[string]any{"role": "assistant", "content": first.Content}, map[string]string{"role": "user", "content": "second"},
	}}
	res, body = send(t, s, "/v1/messages", mustJSON(request), headers)
	if res.StatusCode != 200 || !strings.Contains(body, "SECOND_ANSWER") {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
}

func TestTextSignaturesAreOnlyRestoredForMessages(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1internal:fetchAvailableModels":
			staticModels(w)
			return
		case "/v1internal:countTokens":
			writeJSON(w, 200, map[string]int{"totalTokens": 7})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		frame(w, chunk([]ag.Part{{Text: "ok"}}, "STOP", nil))
	}))
	defer up.Close()
	var logs bytes.Buffer
	svc := newServer(t, config.Default(), slog.New(slog.NewJSONHandler(&logs, nil)), testAccount(t, "test", testKey, up))
	s := startServer(t, svc, testKey)
	history := `[{"role":"user","content":"hi"},{"role":"assistant","content":"earlier answer"},{"role":"user","content":"more"}]`
	if res, body := send(t, s, "/v1/responses", `{"model":"gemini-test","input":`+history+`}`, nil); res.StatusCode != 200 {
		t.Fatal(body)
	}
	if res, body := send(t, s, "/v1/messages/count_tokens", `{"model":"gemini-test","messages":`+history+`}`, nil); res.StatusCode != 200 {
		t.Fatal(body)
	}
	if strings.Contains(logs.String(), "text signature") {
		t.Fatalf("Responses and token counts must not look up text signatures: %s", logs.String())
	}
	if res, body := send(t, s, "/v1/messages", `{"model":"gemini-test","max_tokens":100,"messages":`+history+`}`, nil); res.StatusCode != 200 {
		t.Fatal(body)
	}
	if !strings.Contains(logs.String(), "no text signature record for 1") {
		t.Fatalf("a Messages miss should be logged: %s", logs.String())
	}
}
