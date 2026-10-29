package server

import (
	"encoding/json"
	"strings"
	"testing"
)

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
	if item["type"] != "compaction" || !strings.HasPrefix(item["encrypted_content"].(string), "agy2.") {
		t.Fatalf("item = %v", item)
	}
	// The next request carries the compaction item in place of the history.
	next := map[string]any{"model": "gemini-test", "input": []any{item, map[string]string{"role": "user", "content": "Continue please."}}}
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
}
