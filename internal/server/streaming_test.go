package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

// held is an upstream that accepts at once and sends its output only when
// released, like a model thinking without visible output.
func held(release chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		case <-time.After(10 * time.Second):
		}
		frame(w, chunk([]ag.Part{{Text: "hello"}}, "STOP", &ag.Usage{Input: 10, Output: 1}))
	}
}

// stream opens a streaming request and returns its lines as they arrive.
func stream(t *testing.T, url, path string, body string) (*http.Response, <-chan string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, url+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	lines := make(chan string)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(res.Body)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	return res, lines
}

// until reads lines up to and including the first that starts with prefix,
// failing if one takes longer than a held upstream would wait.
func until(t *testing.T, lines <-chan string, prefix string) []string {
	t.Helper()
	var seen []string
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("stream ended before %q: %q", prefix, seen)
			}
			seen = append(seen, line)
			if strings.HasPrefix(line, prefix) {
				return seen
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("no %q within 2s: %q", prefix, seen)
		}
	}
}

// A stream begins when the upstream accepts, not with the first output:
// Responses sends response.created at once, and Messages, whose message_start
// reports the input usage, keeps the stream alive with comments until then.
func TestStreamsBeginWhenTheUpstreamAccepts(t *testing.T) {
	t.Run("responses", func(t *testing.T) {
		release := make(chan struct{})
		_, s := newService(t, nil, staticModels, held(release))
		res, r := stream(t, s.URL, "/v1/responses", `{"model":"gemini-test","input":"hi","stream":true}`)
		if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
			t.Fatalf("%d %v", res.StatusCode, res.Header)
		}
		until(t, r, "event: response.created")
		until(t, r, "event: response.in_progress")
		close(release)
		until(t, r, "event: response.completed")
	})
	t.Run("messages", func(t *testing.T) {
		release := make(chan struct{})
		svc, s := newService(t, nil, staticModels, held(release))
		svc.heartbeat = 60 * time.Millisecond
		res, r := stream(t, s.URL, "/v1/messages", `{"model":"gemini-test","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		if res.StatusCode != 200 {
			t.Fatal(res.StatusCode)
		}
		if before := until(t, r, ": keep-alive"); len(before) != 1 {
			t.Fatalf("frames before the first heartbeat: %q", before)
		}
		close(release)
		lines := until(t, r, "data: ")
		if lines[len(lines)-2] != "event: message_start" || !strings.Contains(lines[len(lines)-1], `"input_tokens":10`) {
			t.Fatalf("message_start without the input usage, or an event before it: %q", lines)
		}
		until(t, r, "event: message_stop")
	})
}

// Errors the upstream reports by status still reach the client as that
// status; once it has accepted, a failure ends the stream with an error event.
func TestStreamErrorsBeforeAndAfterAcceptance(t *testing.T) {
	_, s := newService(t, nil, staticModels, func(w http.ResponseWriter, r *http.Request) {
		if body, _ := io.ReadAll(r.Body); strings.Contains(string(body), "busy") {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(429)
			fmt.Fprint(w, `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"busy"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `{"error":{"code":503}}`)
	})
	for _, tc := range []struct{ path, body, failure string }{
		{"/v1/responses", `{"model":"gemini-test","stream":true,"input":"%s"}`, "event: response.failed"},
		{"/v1/messages", `{"model":"gemini-test","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"%s"}]}`, "event: error"},
	} {
		res, _ := stream(t, s.URL, tc.path, fmt.Sprintf(tc.body, "busy"))
		if res.StatusCode != 429 || res.Header.Get("Retry-After") != "30" || !strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
			t.Fatalf("%s: status error became %d %v", tc.path, res.StatusCode, res.Header)
		}
		res, r := stream(t, s.URL, tc.path, fmt.Sprintf(tc.body, "hi"))
		if res.StatusCode != 200 {
			t.Fatalf("%s: accepted request answered %d", tc.path, res.StatusCode)
		}
		until(t, r, tc.failure)
	}
}
