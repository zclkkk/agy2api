package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
	"github.com/zclkkk/agy2api/internal/config"
	"github.com/zclkkk/agy2api/internal/state"
)

// request builds a minimal generation request for either API.
func request(format string, stream bool) map[string]any {
	body := map[string]any{"model": "gemini-test", "stream": stream}
	if format == "responses" {
		body["input"] = "hello"
	} else {
		body["messages"] = []any{map[string]string{"role": "user", "content": "hello"}}
		body["max_tokens"] = 1024
	}
	return body
}

func TestBothProtocolsStreamAndNonstream(t *testing.T) {
	_, s := newService(t, nil, staticModels, func(w http.ResponseWriter, r *http.Request) {
		var envelope ag.Envelope
		if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
			t.Error(err)
		}
		if envelope.Project != "test-project" || envelope.Request.Contents[0].Parts[0].Text != "hello" {
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
				res, body := send(t, s, "/v1/"+format, request(format, stream), nil)
				if res.StatusCode != 200 {
					t.Fatalf("status=%d body=%s", res.StatusCode, body)
				}
				if !stream {
					var out map[string]any
					if err := json.Unmarshal([]byte(body), &out); err != nil || !strings.Contains(body, "hello world") {
						t.Fatal(body)
					}
					usage := out["usage"].(map[string]any)
					// Messages reports cached input separately; Responses includes it.
					wantInput := float64(10)
					if format == "messages" {
						wantInput = 7
					}
					if usage["input_tokens"] != wantInput || usage["output_tokens"] != float64(6) {
						t.Fatalf("usage split across chunks lost: %v", usage)
					}
					return
				}
				if !strings.Contains(res.Header.Get("Content-Type"), "text/event-stream") {
					t.Fatal(res.Header)
				}
				var events []map[string]any
				if err := ag.DecodeSSE(strings.NewReader(body), func(b []byte) error {
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
				if events[len(events)-1]["type"] != terminal || strings.Count(body, "event: "+terminal+"\n") != 1 {
					t.Fatalf("terminal event: %v", events[len(events)-1])
				}
				if format == "responses" {
					for i, e := range events {
						if e["sequence_number"] != float64(i) {
							t.Fatal("sequence gap", e)
						}
					}
				}
			})
		}
	}
}

// An upstream failure is an error response, or a failure event once a stream
// has started; it never ends in a successful terminal event.
func TestFailuresDoNotProduceSuccessfulTerminalEvents(t *testing.T) {
	_, s := newService(t, nil, staticModels, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		frame(w, chunk([]ag.Part{{Text: "partial"}}, "", nil))
		fmt.Fprint(w, `{"error":{"code":503}}`)
	})
	for _, format := range []string{"responses", "messages"} {
		res, body := send(t, s, "/v1/"+format, request(format, false), nil)
		if res.StatusCode != http.StatusBadGateway || !strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
			t.Fatalf("%s: %d %s", format, res.StatusCode, body)
		}
		_, body = send(t, s, "/v1/"+format, request(format, true), nil)
		if strings.Contains(body, "response.completed") || strings.Contains(body, "message_stop") {
			t.Fatal(body)
		}
		if format == "messages" {
			if !strings.Contains(body, "event: error") {
				t.Fatal(body)
			}
			continue
		}
		var code string
		if err := ag.DecodeSSE(strings.NewReader(body), func(data []byte) error {
			var event struct {
				Type     string `json:"type"`
				Response struct {
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				} `json:"response"`
			}
			if err := json.Unmarshal(data, &event); err != nil {
				return err
			}
			if event.Type == "response.failed" {
				code = event.Response.Error.Code
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if code != "server_error" {
			t.Fatalf("failure error code = %q", code)
		}
	}
}

// Validation errors are JSON responses sent before any stream opens, and
// nothing reaches the upstream. Which inputs are unsupported is covered by the
// protocol tests; this checks that every endpoint rejects them up front.
func TestUnsupportedRequestsFailBeforeStreaming(t *testing.T) {
	_, s := newService(t, nil, func(w http.ResponseWriter) {
		io.WriteString(w, `{"models":{"gemini-test":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"},"gemini-canvas":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"}},"imageGenerationModelIds":["gemini-canvas"]}`)
	}, func(w http.ResponseWriter, r *http.Request) {
		t.Error("unsupported request reached upstream generation")
		http.Error(w, "unexpected generation", 500)
	})
	for _, group := range []struct {
		paths []string
		base  string
		cases []string
	}{
		{[]string{"/v1/responses", "/v1/responses/input_tokens", "/v1/responses/compact"}, `{"model":"gemini-test","input":"hello","stream":true}`,
			[]string{`"model":"gemini-canvas"`, `"tools":[{"type":"unsupported_tool"}]`}},
		{[]string{"/v1/messages", "/v1/messages/count_tokens"}, `{"model":"gemini-test","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"stream":true}`,
			[]string{`"model":"gemini-canvas"`, `"tools":[{"type":"unsupported_tool","name":"tool"}]`}},
	} {
		for _, path := range group.paths {
			for _, fields := range group.cases {
				var body, overrides map[string]any
				if err := json.Unmarshal([]byte(group.base), &body); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal([]byte("{"+fields+"}"), &overrides); err != nil {
					t.Fatal(err)
				}
				for key, value := range overrides {
					body[key] = value
				}
				res, out := send(t, s, path, body, nil)
				if res.StatusCode != http.StatusBadRequest || !strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
					t.Fatalf("%s %s: %d %s", path, fields, res.StatusCode, out)
				}
			}
		}
	}
}

func TestTokenCountAndUnknownRoutes(t *testing.T) {
	_, s := newService(t, nil, staticModels, func(w http.ResponseWriter, r *http.Request) { t.Error("generation should not run") })
	for path, body := range map[string]string{
		"/v1/messages/count_tokens":  helloMessages,
		"/v1/responses/input_tokens": `{"model":"gemini-test","input":"hello"}`,
	} {
		if res, out := send(t, s, path, body, nil); res.StatusCode != 200 || !strings.Contains(out, `"input_tokens":17`) {
			t.Fatalf("%s: %d %s", path, res.StatusCode, out)
		}
	}
	if res, out := send(t, s, "/v1/chat/completions", `{}`, nil); res.StatusCode != 404 {
		t.Fatalf("%d %s", res.StatusCode, out)
	}
}

func TestCancellationReachesUpstream(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	_, s := newService(t, nil, staticModels, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		frame(w, chunk([]ag.Part{{Text: "started"}}, "", nil))
		close(started)
		<-r.Context().Done()
		close(stopped)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, s.URL+"/v1/responses", strings.NewReader(`{"model":"gemini-test","input":"hello","stream":true}`))
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

func TestBurstsWaitForASlot(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	_, s := newService(t, func(c *config.Config) { c.MaxConcurrentRequests = 1 }, staticModels, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			<-release
		}
		answer("ok", "STOP")(w, r)
	})
	var wg sync.WaitGroup
	statuses := make([]int, 2)
	for i := range statuses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, _ := send(t, s, "/v1/messages", helloMessages, nil)
			statuses[i] = res.StatusCode
		}()
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	if statuses[0] != 200 || statuses[1] != 200 {
		t.Fatalf("queued request was rejected: %v", statuses)
	}
}

func TestCatalogFailureServesCachedCatalog(t *testing.T) {
	var fail atomic.Bool
	svc, s := newService(t, nil, func(w http.ResponseWriter) {
		if fail.Load() {
			w.WriteHeader(503)
			return
		}
		staticModels(w)
	}, answer("ok", "STOP"))
	if res, body := send(t, s, "/v1/messages", helloMessages, nil); res.StatusCode != 200 {
		t.Fatal(body)
	}
	fail.Store(true)
	a := svc.accounts[0]
	a.catalogMu.Lock()
	a.catalogExpiry = time.Time{}
	a.catalogMu.Unlock()
	if res, body := send(t, s, "/v1/messages", helloMessages, nil); res.StatusCode != 200 {
		t.Fatalf("cached catalog not used: %s", body)
	}
}

func TestIdleStreamsReceiveHeartbeats(t *testing.T) {
	svc, s := newService(t, nil, staticModels, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		frame(w, chunk([]ag.Part{{Text: "thinking about it"}}, "", nil))
		time.Sleep(300 * time.Millisecond)
		frame(w, chunk([]ag.Part{{Text: " done"}}, "STOP", nil))
	})
	svc.heartbeat = 60 * time.Millisecond
	_, body := send(t, s, "/v1/messages", request("messages", true), nil)
	if !strings.Contains(body, "event: ping") || !strings.HasSuffix(strings.TrimSpace(body), `data: {"type":"message_stop"}`) {
		t.Fatalf("no heartbeat or broken stream: %s", body)
	}
	_, body = send(t, s, "/v1/responses", request("responses", true), nil)
	if !strings.Contains(body, ": keep-alive") || !strings.Contains(body, "response.completed") {
		t.Fatalf("no heartbeat for Responses: %s", body)
	}
}

// A stored response is JSON; a client asking for an event stream is rejected
// rather than handed JSON it would fail to parse.
func TestStreamingRetrievalIsRejected(t *testing.T) {
	store, err := state.Open(t.TempDir(), time.Hour, 10, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	handler := newServer(t, config.Default(), nil, &Account{ID: "test", Key: testKey, State: store}).Handler()
	for query, want := range map[string]int{"?stream=true": 400, "?starting_after=3": 400, "?stream=false": 404} {
		response := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://localhost/v1/responses/missing"+query, nil)
		req.Header.Set("Authorization", "Bearer "+testKey)
		handler.ServeHTTP(response, req)
		if response.Code != want {
			t.Fatalf("%s: status %d, want %d", query, response.Code, want)
		}
	}
}

func TestAttachmentPrivateAddressesRejected(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "100.64.0.1", "::1", "fc00::1"} {
		if publicIP(net.ParseIP(host)) {
			t.Errorf("accepted %s", host)
		}
	}
}
