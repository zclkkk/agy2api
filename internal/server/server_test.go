package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
	"github.com/zclkkk/agy2api/internal/config"
	"github.com/zclkkk/agy2api/internal/state"
)

type fixedToken struct{}

func (fixedToken) AccessToken(context.Context) (string, error) { return "test-token", nil }
func (fixedToken) ProjectID() string                           { return "test-project" }

func setup(t *testing.T, generate http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing provider auth")
		}
		switch r.URL.Path {
		case "/v1internal:fetchAvailableModels":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"models":{"gemini-test":{"displayName":"Gemini Test"},"claude-test":{"maxOutputTokens":64000}},"webSearchModelIds":["gemini-test"]}`)
		case "/v1internal:countTokens":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"totalTokens":17}`)
		case "/v1internal:streamGenerateContent":
			calls.Add(1)
			generate(w, r)
		default:
			t.Errorf("unexpected upstream path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.APIKey = "local-key"
	store, err := state.Open(cfg.DataDir, time.Hour, 1000)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	client := &ag.Client{HTTP: up.Client(), BaseURL: up.URL, UserAgent: "test", Auth: fixedToken{}}
	svc := New(cfg, client, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	down := httptest.NewServer(svc.Handler())
	t.Cleanup(down.Close)
	return down, calls
}

func post(t *testing.T, s *httptest.Server, path string, value any) (*http.Response, []byte) {
	t.Helper()
	data, _ := json.Marshal(value)
	req, _ := http.NewRequest(http.MethodPost, s.URL+path, bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer local-key")
	req.Header.Set("Content-Type", "application/json")
	res, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res, body
}
func frame(w http.ResponseWriter, value any) {
	data, _ := json.Marshal(value)
	fmt.Fprintf(w, "data: %s\n\n", data)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
func chunk(parts []ag.Part, finish string, usage *ag.Usage) any {
	return map[string]any{"response": ag.Response{Candidates: []ag.Candidate{{Content: ag.Content{Role: "model", Parts: parts}, Finish: finish}}, Usage: usage}}
}

func TestBothProtocolsStreamAndNonstream(t *testing.T) {
	s, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		var request ag.Envelope
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.Project != "test-project" || request.Request.Contents[0].Parts[0].Text != "hello" {
			t.Error("bad request translation")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		frame(w, chunk([]ag.Part{{Text: "hello "}}, "", &ag.Usage{Input: 10, Cached: 3}))
		frame(w, chunk([]ag.Part{{Text: "world"}}, "STOP", nil))
		frame(w, map[string]any{"response": map[string]any{"usageMetadata": map[string]int{"candidatesTokenCount": 4, "thoughtsTokenCount": 2}}})
	})
	for _, format := range []string{"responses", "messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", format, stream), func(t *testing.T) {
				request := map[string]any{"model": "gemini-test", "stream": stream}
				if format == "responses" {
					request["input"] = "hello"
				} else {
					request["messages"] = []any{map[string]string{"role": "user", "content": "hello"}}
					request["max_tokens"] = 1024
				}
				res, body := post(t, s, "/v1/"+format, request)
				if res.StatusCode != 200 {
					t.Fatalf("status=%d body=%s", res.StatusCode, body)
				}
				if stream {
					if !strings.Contains(res.Header.Get("Content-Type"), "text/event-stream") {
						t.Fatal(res.Header)
					}
					var events []map[string]any
					if err := ag.DecodeSSE(bytes.NewReader(body), func(b []byte) error {
						var e map[string]any
						err := json.Unmarshal(b, &e)
						events = append(events, e)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					terminal := "response.completed"
					if format == "messages" {
						terminal = "message_stop"
					}
					if events[len(events)-1]["type"] != terminal {
						t.Fatalf("last event=%v", events[len(events)-1])
					}
					if format == "responses" {
						for i, e := range events {
							if e["sequence_number"] != float64(i) {
								t.Fatal("sequence gap", e)
							}
						}
					}
					if strings.Count(string(body), "event: "+terminal+"\n") != 1 {
						t.Fatal("duplicate terminal event")
					}
				} else {
					var out map[string]any
					if err := json.Unmarshal(body, &out); err != nil {
						t.Fatal(err)
					}
					if !bytes.Contains(body, []byte("hello world")) {
						t.Fatal(string(body))
					}
					usage := out["usage"].(map[string]any)
					wantInput := float64(10)
					if format == "messages" {
						wantInput = 7
					}
					if usage["input_tokens"] != wantInput || usage["output_tokens"] != float64(6) {
						t.Fatalf("split usage lost: %v", usage)
					}
				}
			})
		}
	}
}

func TestMessagesToolRoundTrip(t *testing.T) {
	var generation atomic.Int32
	s, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		var request ag.Envelope
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if generation.Add(1) == 1 {
			frame(w, chunk([]ag.Part{{Thought: true, Text: "plan", Signature: "think-sig"}, {FunctionCall: &ag.Call{ID: "upstream-1", Name: "lookup", Args: json.RawMessage(`{"title":"do not rewrite"}`)}, Signature: "tool-sig"}}, "STOP", &ag.Usage{Input: 8, Output: 4}))
			return
		}
		parts := request.Request.Contents[1].Parts
		if len(parts) != 2 || parts[0].Signature != "think-sig" || parts[1].Signature != "tool-sig" || parts[1].FunctionCall.ID != "upstream-1" {
			t.Errorf("bad replay: %+v", parts)
		}
		result := request.Request.Contents[2].Parts[0].FunctionResponse
		if result == nil || result.ID != "upstream-1" || result.Name != "lookup" {
			t.Errorf("bad function result: %+v", result)
		}
		frame(w, chunk([]ag.Part{{Text: "finished"}}, "STOP", nil))
	})
	messages := []any{map[string]any{"role": "user", "content": "hello"}}
	request := map[string]any{"model": "claude-test", "max_tokens": 1024, "messages": messages, "tools": []any{map[string]any{"name": "lookup", "input_schema": map[string]any{"type": "object", "properties": map[string]any{"title": map[string]string{"type": "string"}}}}}}
	res, body := post(t, s, "/v1/messages", request)
	if res.StatusCode != 200 {
		t.Fatal(string(body))
	}
	var first struct {
		Content []json.RawMessage `json:"content"`
	}
	_ = json.Unmarshal(body, &first)
	var call struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(first.Content[1], &call)
	messages = append(messages, map[string]any{"role": "assistant", "content": first.Content}, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": call.ID, "content": "result"}}})
	request["messages"] = messages
	res, body = post(t, s, "/v1/messages", request)
	if res.StatusCode != 200 || !bytes.Contains(body, []byte("finished")) {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
}

func TestFailuresDoNotProduceSuccessfulTerminalEvents(t *testing.T) {
	for _, format := range []string{"responses", "messages"} {
		t.Run(format, func(t *testing.T) {
			s, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				frame(w, chunk([]ag.Part{{Text: "partial"}}, "", nil))
				fmt.Fprint(w, `{"error":{"code":503}}`)
			})
			request := map[string]any{"model": "gemini-test", "stream": true}
			if format == "responses" {
				request["input"] = "hello"
			} else {
				request["max_tokens"] = 1024
				request["messages"] = []any{map[string]string{"role": "user", "content": "hello"}}
			}
			_, body := post(t, s, "/v1/"+format, request)
			if bytes.Contains(body, []byte("response.completed")) || bytes.Contains(body, []byte("message_stop")) {
				t.Fatal(string(body))
			}
			if !bytes.Contains(body, []byte("response.failed")) && !bytes.Contains(body, []byte("event: error")) {
				t.Fatal(string(body))
			}
		})
	}
}

func TestAuthenticationUnsupportedRequestsAndTokenCount(t *testing.T) {
	s, calls := setup(t, func(w http.ResponseWriter, r *http.Request) { t.Error("generation should not run") })
	res, err := s.Client().Get(s.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatal(res.StatusCode)
	}
	res, body := post(t, s, "/v1/responses", map[string]any{"model": "gemini-test", "input": "hello", "unexpected": true})
	if res.StatusCode != 400 {
		t.Fatal(string(body))
	}
	res, body = post(t, s, "/v1/responses", map[string]any{"model": "unknown", "input": "hello"})
	if res.StatusCode != 400 {
		t.Fatal(string(body))
	}
	res, body = post(t, s, "/v1/messages/count_tokens", map[string]any{"model": "gemini-test", "messages": []any{map[string]string{"role": "user", "content": "hello"}}})
	if res.StatusCode != 200 || !bytes.Contains(body, []byte(`"input_tokens":17`)) {
		t.Fatal(string(body))
	}
	res, body = post(t, s, "/v1/chat/completions", map[string]any{})
	if res.StatusCode != 404 {
		t.Fatal(string(body))
	}
	if calls.Load() != 0 {
		t.Fatal("unexpected generation")
	}
}

func TestCancellationReachesUpstream(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	s, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		frame(w, chunk([]ag.Part{{Text: "started"}}, "", nil))
		close(started)
		<-r.Context().Done()
		close(stopped)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, s.URL+"/v1/responses", strings.NewReader(`{"model":"gemini-test","input":"hello","stream":true}`))
	req.Header.Set("Authorization", "Bearer local-key")
	res, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	<-started
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not reach upstream")
	}
}

func TestAttachmentPrivateAddressesRejected(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "100.64.0.1", "::1", "fc00::1"} {
		if publicIP(net.ParseIP(host)) {
			t.Errorf("accepted %s", host)
		}
	}
}

func TestClaudeReasoningUsesAdvertisedOutputLimit(t *testing.T) {
	s, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		var envelope ag.Envelope
		if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
			t.Error(err)
		}
		g := envelope.Request.Generation
		if g.MaxTokens == nil || *g.MaxTokens != 64000 || g.Thinking == nil || g.Thinking.Budget == nil || *g.Thinking.Budget != 8192 {
			t.Errorf("incorrect generation settings: %+v", g)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		frame(w, chunk([]ag.Part{{Text: "answer"}}, "STOP", nil))
	})
	res, body := post(t, s, "/v1/responses", map[string]any{"model": "claude-test", "input": "think", "reasoning": map[string]string{"effort": "medium"}})
	if res.StatusCode != 200 {
		t.Fatal(string(body))
	}
}
