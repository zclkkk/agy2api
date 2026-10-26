package server

import (
	"encoding/json"
	"io"
	"log/slog"
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

const testModels = `{"models":{"gemini-test":{"modelProvider":"MODEL_PROVIDER_GOOGLE"},"claude-test":{"modelProvider":"MODEL_PROVIDER_ANTHROPIC","maxOutputTokens":64000}}}`

// newService starts a service against a fake upstream whose catalog and
// generation handlers are supplied by the test.
func newService(t *testing.T, configure func(*config.Config), models func(http.ResponseWriter), generate http.HandlerFunc) (*Server, *httptest.Server) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1internal:fetchAvailableModels":
			models(w)
		case "/v1internal:countTokens":
			io.WriteString(w, `{"totalTokens":5}`)
		default:
			generate(w, r)
		}
	}))
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	if configure != nil {
		configure(&cfg)
	}
	store, err := state.Open(cfg.DataDir, time.Hour, 1000, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	svc := New(cfg, &ag.Client{HTTP: up.Client(), BaseURL: up.URL, UserAgent: func() string { return "test" }, Auth: fixedToken{}}, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	down := httptest.NewServer(svc.Handler())
	t.Cleanup(down.Close)
	return svc, down
}

func staticModels(w http.ResponseWriter) { io.WriteString(w, testModels) }

func answer(text, finish string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		frame(w, chunk([]ag.Part{{Text: text}}, finish, nil))
	}
}

func send(t *testing.T, s *httptest.Server, path, body string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, s.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		if k == "Host" {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}
	res, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	return res, string(data)
}

const helloMessages = `{"model":"gemini-test","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`

func TestWithoutAPIKeyOnlyLocalClientsAreServed(t *testing.T) {
	_, s := newService(t, nil, staticModels, answer("ok", "STOP"))
	for name, tc := range map[string]struct {
		headers map[string]string
		want    int
	}{
		"local":              {nil, 200},
		"localhost host":     {map[string]string{"Host": "localhost:8080"}, 200},
		"local origin":       {map[string]string{"Origin": "http://localhost:3000"}, 200},
		"cross-site browser": {map[string]string{"Origin": "https://evil.example"}, 403},
		"dns rebinding":      {map[string]string{"Host": "evil.example:8080"}, 403},
	} {
		res, body := send(t, s, "/v1/messages", helloMessages, tc.headers)
		if res.StatusCode != tc.want {
			t.Errorf("%s: status=%d body=%s", name, res.StatusCode, body)
		}
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
	svc.catalogMu.Lock()
	svc.catalogExpiry = time.Time{}
	svc.catalogMu.Unlock()
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
	_, body := send(t, s, "/v1/messages", `{"model":"gemini-test","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if !strings.Contains(body, "event: ping") || !strings.HasSuffix(strings.TrimSpace(body), `data: {"type":"message_stop"}`) {
		t.Fatalf("no heartbeat or broken stream: %s", body)
	}
	_, body = send(t, s, "/v1/responses", `{"model":"gemini-test","stream":true,"input":"hi"}`, nil)
	if !strings.Contains(body, ": keep-alive") || !strings.Contains(body, "response.completed") {
		t.Fatalf("no heartbeat for Responses: %s", body)
	}
}

func TestMalformedFunctionCallIsReportedAsRetryable(t *testing.T) {
	_, s := newService(t, nil, staticModels, answer("partial", "MALFORMED_FUNCTION_CALL"))
	res, body := send(t, s, "/v1/messages", helloMessages, nil)
	if res.StatusCode != 502 || !strings.Contains(body, "MALFORMED_FUNCTION_CALL") {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
}

func TestClaudeCodeShapedRequestReachesUpstreamClean(t *testing.T) {
	var envelope ag.Envelope
	_, s := newService(t, nil, staticModels, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&envelope)
		declared := envelope.Request.Tools[0].Functions[0].Name
		w.Header().Set("Content-Type", "text/event-stream")
		frame(w, chunk([]ag.Part{{Text: "plan", Thought: true, Signature: "sig"}, {FunctionCall: &ag.Call{Name: declared, Args: json.RawMessage(`{"cmd":"ls"}`)}, Signature: "call-sig"}}, "STOP", nil))
	})
	body := `{"model":"claude-test","max_tokens":32000,"stream":false,
	 "system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],
	 "messages":[{"role":"user","content":[{"type":"text","text":"run ls","cache_control":{"type":"ephemeral"}}]}],
	 "tools":[{"name":"mcp__srv.example__run","input_schema":{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"cmd":{"type":["string","null"]}},"additionalProperties":false}}],
	 "thinking":{"type":"adaptive","display":"omitted"},"output_config":{"effort":"high"},
	 "context_management":{"edits":[]},"metadata":{"user_id":"u"}}`
	res, out := send(t, s, "/v1/messages", body, nil)
	if res.StatusCode != 200 {
		t.Fatal(out)
	}
	tool := envelope.Request.Tools[0].Functions[0]
	if strings.Contains(string(tool.Parameters), "$schema") || tool.Name == "mcp__srv.example__run" {
		t.Fatalf("tool not normalized: %+v %s", tool, tool.Parameters)
	}
	if b := envelope.Request.Generation.Thinking.Budget; b == nil || *b != 24576 {
		t.Fatalf("effort not applied: %+v", envelope.Request.Generation.Thinking)
	}
	if !strings.Contains(out, `"name":"mcp__srv.example__run"`) || strings.Contains(out, `"thinking":"plan"`) {
		t.Fatalf("response did not restore the tool name or exposed hidden thinking: %s", out)
	}
}
