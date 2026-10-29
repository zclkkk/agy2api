package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
	"github.com/zclkkk/agy2api/internal/config"
	"github.com/zclkkk/agy2api/internal/state"
)

const testKey = "test-key"

const testModels = `{"models":{"gemini-test":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"},"claude-test":{"apiProvider":"API_PROVIDER_ANTHROPIC_VERTEX"}}}`

func staticModels(w http.ResponseWriter) { io.WriteString(w, testModels) }

type fixedToken struct{}

func (fixedToken) AccessToken(context.Context) (string, error) { return "test-token", nil }
func (fixedToken) ProjectID() string                           { return "test-project" }

// newService serves one account against a fake upstream whose catalog and
// generation handlers are supplied by the test. Its client sends the key.
func newService(t *testing.T, configure func(*config.Config), models func(http.ResponseWriter), generate http.HandlerFunc) (*Server, *httptest.Server) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("%s: missing upstream credentials", r.URL.Path)
		}
		switch r.URL.Path {
		case "/v1internal:fetchAvailableModels":
			models(w)
		case "/v1internal:countTokens":
			io.WriteString(w, `{"totalTokens":17}`)
		default:
			generate(w, r)
		}
	}))
	t.Cleanup(up.Close)
	cfg := config.Default()
	if configure != nil {
		configure(&cfg)
	}
	svc := newServer(t, cfg, nil, testAccount(t, "test", testKey, up))
	return svc, startServer(t, svc, testKey)
}

// testAccount is an account with its own state whose upstream is up. A nil
// up gives an account that must not reach the upstream.
func testAccount(t *testing.T, id, key string, up *httptest.Server) *Account {
	t.Helper()
	store, err := state.Open(t.TempDir(), time.Hour, 1000, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	a := &Account{ID: id, Key: key, State: store}
	if up != nil {
		a.Upstream = &ag.Client{HTTP: up.Client(), BaseURL: up.URL, UserAgent: func() string { return "test" }, Auth: fixedToken{}}
	}
	return a
}

func newServer(t *testing.T, cfg config.Config, logger *slog.Logger, accounts ...*Account) *Server {
	t.Helper()
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	svc, err := New(cfg, accounts, logger)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// startServer serves svc. Its client sends key unless a request carries one.
func startServer(t *testing.T, svc *Server, key string) *httptest.Server {
	t.Helper()
	down := httptest.NewServer(svc.Handler())
	t.Cleanup(down.Close)
	client := down.Client()
	client.Transport = keyTransport{client.Transport, key}
	return down
}

type keyTransport struct {
	base http.RoundTripper
	key  string
}

func (k keyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Header.Get("Authorization") == "" && r.Header.Get("X-Api-Key") == "" {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+k.key)
	}
	return k.base.RoundTrip(r)
}

// send posts a body given as JSON text or as a value to marshal.
func send(t *testing.T, s *httptest.Server, path string, body any, headers map[string]string) (*http.Response, string) {
	t.Helper()
	data, ok := body.(string)
	if !ok {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		data = string(encoded)
	}
	req, _ := http.NewRequest(http.MethodPost, s.URL+path, strings.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res, string(out)
}

const helloMessages = `{"model":"gemini-test","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`

// answer replies to every generation with text.
func answer(text, finish string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		frame(w, chunk([]ag.Part{{Text: text}}, finish, nil))
	}
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

// recorder answers every generation with text and keeps the native requests.
type recorder struct {
	mu       sync.Mutex
	requests []ag.Envelope
	text     string
}

func (rec *recorder) handle(w http.ResponseWriter, r *http.Request) {
	var envelope ag.Envelope
	data, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(data, &envelope)
	rec.mu.Lock()
	rec.requests = append(rec.requests, envelope)
	text := rec.text
	rec.mu.Unlock()
	if text == "" {
		text = "ok"
	}
	w.Header().Set("Content-Type", "text/event-stream")
	frame(w, chunk([]ag.Part{{Text: "thinking about it", Thought: true}, {Text: text}}, "STOP", &ag.Usage{Input: 40, Output: 7}))
}

func (rec *recorder) last(t *testing.T) ag.Request {
	t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.requests) == 0 {
		t.Fatal("no upstream request")
	}
	return rec.requests[len(rec.requests)-1].Request
}

func lastText(req ag.Request) (string, string) {
	c := req.Contents[len(req.Contents)-1]
	return c.Role, c.Parts[len(c.Parts)-1].Text
}
