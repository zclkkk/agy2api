package server

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

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

func TestTrailingAssistantTurnIsFollowedByAUserTurn(t *testing.T) {
	rec := &recorder{}
	_, s := newService(t, nil, staticModels, rec.handle)
	for path, body := range map[string]string{
		"/v1/messages":  `{"model":"gemini-test","max_tokens":100,"messages":[{"role":"user","content":"List colors."},{"role":"assistant","content":"Red,"}]}`,
		"/v1/responses": `{"model":"claude-test","input":[{"role":"user","content":"List colors."},{"role":"assistant","content":"Red,"}]}`,
	} {
		res, out := send(t, s, path, body, nil)
		if res.StatusCode != 200 {
			t.Fatalf("%s: %d %s", path, res.StatusCode, out)
		}
		req := rec.last(t)
		if role, text := lastText(req); role != "user" || text != "Continue." {
			t.Fatalf("%s: request ends with %s %q", path, role, text)
		}
		if req.Contents[len(req.Contents)-2].Parts[0].Text != "Red," {
			t.Fatalf("%s: assistant turn was not kept", path)
		}
	}
	// A trailing tool call without its result is not papered over.
	send(t, s, "/v1/messages", `{"model":"gemini-test","max_tokens":100,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"f","input":{}}]}],"tools":[{"name":"f","input_schema":{"type":"object"}}]}`, nil)
	if role, _ := lastText(rec.last(t)); role != "model" {
		t.Fatal("a trailing tool call must stay last")
	}
}

var nativeSessionID = regexp.MustCompile(`^-\d{1,19}$`)

func TestSessionIDsFollowTheClientSession(t *testing.T) {
	rec := &recorder{}
	_, s := newService(t, nil, staticModels, rec.handle)
	session := func(path, body string, headers map[string]string) string {
		t.Helper()
		if res, out := send(t, s, path, body, headers); res.StatusCode != 200 {
			t.Fatalf("%d %s", res.StatusCode, out)
		}
		id := rec.last(t).SessionID
		if !nativeSessionID.MatchString(id) {
			t.Fatalf("session ID %q is not in the native format", id)
		}
		return id
	}
	responses := func(text string) string {
		return `{"model":"gemini-test","input":"` + text + `"}`
	}
	codex := map[string]string{"Session-Id": "01a173fc-cb86-72f0-a250-ce0969dcc4b5"}
	if a, b := session("/v1/responses", responses("one"), codex), session("/v1/responses", responses("two"), codex); a != b {
		t.Fatal("one Codex session must map to one upstream session")
	}
	key := `{"model":"gemini-test","prompt_cache_key":"thread-7","input":"`
	if a, b := session("/v1/responses", key+`one"}`, nil), session("/v1/responses", key+`two"}`, nil); a != b {
		t.Fatal("prompt_cache_key must identify the session")
	}
	claude := func(text string) string {
		return `{"model":"gemini-test","max_tokens":100,"metadata":{"user_id":"{\"device_id\":\"d\",\"session_id\":\"98a88910\"}"},"messages":[{"role":"user","content":"` + text + `"}]}`
	}
	viaMetadata := session("/v1/messages", claude("one"), nil)
	if viaMetadata != session("/v1/messages", claude("two"), nil) {
		t.Fatal("Claude Code metadata must identify the session")
	}
	if viaMetadata != session("/v1/messages", claude("three"), map[string]string{"X-Claude-Code-Session-Id": "98a88910"}) {
		t.Fatal("the Claude Code header and metadata must agree")
	}
	// Without a client session, a conversation is keyed by its first message.
	first := session("/v1/responses", responses("same start"), nil)
	if first != session("/v1/responses", `{"model":"gemini-test","input":[{"role":"user","content":"same start"},{"role":"assistant","content":"ok"},{"role":"user","content":"more"}]}`, nil) {
		t.Fatal("follow-ups of one conversation must share a session")
	}
	if first == session("/v1/responses", responses("another start"), nil) {
		t.Fatal("different conversations must not share a session")
	}
}

type compactionEvent struct {
	Type     string          `json:"type"`
	Item     json.RawMessage `json:"item"`
	Response struct {
		Output []map[string]any `json:"output"`
		Usage  map[string]any   `json:"usage"`
	} `json:"response"`
}

func events(t *testing.T, stream string) []compactionEvent {
	t.Helper()
	var out []compactionEvent
	for _, line := range strings.Split(stream, "\n") {
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			var e compactionEvent
			if err := json.Unmarshal([]byte(data), &e); err != nil {
				t.Fatal(err)
			}
			out = append(out, e)
		}
	}
	return out
}

func TestCompactionTriggerReturnsACompactionItem(t *testing.T) {
	rec := &recorder{text: "SUMMARY: the user wants a parser; tests pass."}
	_, s := newService(t, nil, staticModels, rec.handle)
	body := `{"model":"gemini-test","stream":true,"store":false,"tool_choice":"auto","tools":[{"type":"function","name":"exec","parameters":{"type":"object"}}],
		"input":[{"role":"user","content":"Write a parser."},{"type":"function_call","call_id":"call_1","name":"exec","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"done"},{"type":"compaction_trigger"}]}`
	res, stream := send(t, s, "/v1/responses", body, nil)
	if res.StatusCode != 200 {
		t.Fatalf("%d %s", res.StatusCode, stream)
	}
	req := rec.last(t)
	if role, text := lastText(req); role != "user" || !strings.Contains(text, "being compacted") {
		t.Fatalf("summary prompt missing: %s %q", role, text)
	}
	if req.ToolConfig == nil || req.ToolConfig.FunctionCallingConfig.Mode != "NONE" || len(req.Tools) == 0 {
		t.Fatal("tools must stay declared with calling disabled")
	}
	var types []string
	var item map[string]any
	for _, e := range events(t, stream) {
		types = append(types, e.Type)
		if e.Type == "response.completed" {
			if len(e.Response.Output) != 1 {
				t.Fatalf("output = %v", e.Response.Output)
			}
			item = e.Response.Output[0]
		}
	}
	if strings.Join(types, ",") != "response.created,response.in_progress,response.output_item.added,response.output_item.done,response.completed" {
		t.Fatalf("events = %v", types)
	}
	if item["type"] != "compaction" || !strings.HasPrefix(item["encrypted_content"].(string), "agy2api.compaction.v1.") {
		t.Fatalf("item = %v", item)
	}
	// The next request carries the compaction item in place of the history.
	next := `{"model":"gemini-test","input":[` + mustJSON(item) + `,{"role":"user","content":"Continue please."}]}`
	if res, out := send(t, s, "/v1/responses", next, nil); res.StatusCode != 200 {
		t.Fatalf("%d %s", res.StatusCode, out)
	}
	req = rec.last(t)
	first := req.Contents[0]
	if len(req.Contents) != 1 || first.Role != "user" || !strings.Contains(first.Parts[0].Text, "SUMMARY: the user wants a parser") || first.Parts[1].Text != "Continue please." {
		t.Fatalf("expanded history = %+v", req.Contents)
	}
}

func TestCompactEndpoint(t *testing.T) {
	rec := &recorder{text: "Short summary."}
	_, s := newService(t, nil, staticModels, rec.handle)
	res, out := send(t, s, "/v1/responses/compact", `{"model":"gemini-test","instructions":"Be terse.","input":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"}]}`, nil)
	if res.StatusCode != 200 {
		t.Fatalf("%d %s", res.StatusCode, out)
	}
	var compacted struct {
		Object string           `json:"object"`
		Output []map[string]any `json:"output"`
		Usage  map[string]any   `json:"usage"`
	}
	if err := json.Unmarshal([]byte(out), &compacted); err != nil {
		t.Fatal(err)
	}
	if compacted.Object != "response.compaction" || len(compacted.Output) != 1 || compacted.Output[0]["type"] != "compaction" || compacted.Usage["input_tokens"] != float64(40) {
		t.Fatalf("response = %s", out)
	}
	if _, text := lastText(rec.last(t)); !strings.Contains(text, "being compacted") {
		t.Fatal("summary prompt missing")
	}
	// A compaction item from another provider cannot be expanded.
	res, out = send(t, s, "/v1/responses", `{"model":"gemini-test","input":[{"type":"compaction","encrypted_content":"gAAAA-openai"},{"role":"user","content":"next"}]}`, nil)
	if res.StatusCode != 200 || len(rec.last(t).Contents) != 1 {
		t.Fatalf("foreign compaction: %d %s", res.StatusCode, out)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
