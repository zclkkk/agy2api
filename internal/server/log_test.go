package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

// Each request writes one completion line that carries its outcome: what was
// called, the usage, and for a failure the client's error with its upstream
// or internal cause.
func TestRequestLogLine(t *testing.T) {
	catalog := `{"models":{
		"gemini-v-low":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI","supportsThinking":true,"thinkingLevel":1},
		"gemini-v-high":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI","supportsThinking":true,"thinkingLevel":3}
	}}`
	var mode atomic.Value
	mode.Store("ok")
	svc, s := newService(t, nil, func(w http.ResponseWriter) { io.WriteString(w, catalog) }, func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load() {
		case "limited":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED","details":[
				{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED"},
				{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"1.5s"}]}}`)
		case "broken":
			// The stream ends without a finish reason.
			w.Header().Set("Content-Type", "text/event-stream")
			frame(w, chunk([]ag.Part{{Text: "partial"}}, "", nil))
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			frame(w, chunk([]ag.Part{{Text: "ok"}}, "STOP", &ag.Usage{Input: 11, Output: 3, Thoughts: 5, Cached: 2}))
		}
	})
	var logs bytes.Buffer
	svc.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	line := func(name string) map[string]any {
		t.Helper()
		text := strings.TrimSpace(logs.String())
		logs.Reset()
		if strings.Count(text, "\n") != 0 {
			t.Fatalf("%s: want one log line, got:\n%s", name, text)
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(text), &entry); err != nil || entry["msg"] != "request completed" {
			t.Fatalf("%s: %v %s", name, err, text)
		}
		return entry
	}
	expect := func(name string, entry map[string]any, want map[string]any, absent ...string) {
		t.Helper()
		for key, value := range want {
			got, _ := json.Marshal(entry[key])
			wanted, _ := json.Marshal(value)
			if string(got) != string(wanted) {
				t.Errorf("%s: %s = %s, want %s (line %v)", name, key, got, wanted, entry)
			}
		}
		for _, key := range absent {
			if _, ok := entry[key]; ok {
				t.Errorf("%s: unexpected %s in %v", name, key, entry)
			}
		}
	}
	agent := map[string]string{"User-Agent": "harness/1.0"}
	request := `{"model":"gemini-v","input":"hi","store":false,"reasoning":{"effort":"high"}}`

	send(t, s, "/v1/responses", request, agent)
	expect("success", line("success"), map[string]any{
		"level": "INFO", "status": 200, "account": "test", "model": "gemini-v", "upstream_model": "gemini-v-high",
		"thinking_level": 3, "user_agent": "harness/1.0",
		"usage": map[string]int{"input": 11, "output": 3, "thoughts": 5, "cached": 2},
	}, "error", "cause", "upstream_status", "stream_failed")

	mode.Store("limited")
	if res, body := send(t, s, "/v1/responses", request, agent); res.StatusCode != http.StatusTooManyRequests || res.Header.Get("Retry-After") != "2" {
		t.Fatalf("limited: %d %s", res.StatusCode, body)
	}
	expect("upstream error", line("upstream error"), map[string]any{
		"level": "WARN", "status": 429, "upstream_status": 429, "upstream_reason": "RESOURCE_EXHAUSTED: RATE_LIMIT_EXCEEDED",
		"upstream_detail": "Resource has been exhausted (e.g. check quota).", "retry_after": "2",
		"error": "Antigravity returned HTTP 429 (RESOURCE_EXHAUSTED: RATE_LIMIT_EXCEEDED)",
	}, "cause", "usage")

	mode.Store("broken")
	stream := `{"model":"gemini-v","input":"hi","store":false,"stream":true}`
	if res, _ := send(t, s, "/v1/responses", stream, agent); res.StatusCode != http.StatusOK {
		t.Fatalf("broken stream returned %d", res.StatusCode)
	}
	broken := line("broken stream")
	expect("broken stream", broken, map[string]any{
		"level": "WARN", "status": 200, "stream_failed": true, "error": "generation failed; inspect the local service log",
	}, "upstream_status")
	if cause, _ := broken["cause"].(string); !strings.Contains(cause, "finish reason") {
		t.Errorf("broken stream cause = %q", cause)
	}

	// The client's own mistakes are not warnings.
	send(t, s, "/v1/responses", `{"model":"gemini-v-high","input":"hi"}`, agent)
	expect("invalid request", line("invalid request"), map[string]any{
		"level": "INFO", "status": 400, "model": "gemini-v-high",
		"error": "model is not available; agy2api only serves Gemini models available to this account",
	}, "cause", "upstream_model")
	res, err := http.Post(s.URL+"/v1/responses", "application/json", strings.NewReader(request))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	expect("unauthenticated", line("unauthenticated"), map[string]any{
		"level": "INFO", "status": 401, "error": "invalid API key",
	}, "account")
}
