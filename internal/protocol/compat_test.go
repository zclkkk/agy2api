package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

var testCatalog = ag.Catalog{
	Models: map[string]json.RawMessage{
		"claude-sonnet-4-6": json.RawMessage(`{"modelProvider":"MODEL_PROVIDER_ANTHROPIC","maxOutputTokens":64000}`),
		"claude-test":       json.RawMessage(`{"modelProvider":"MODEL_PROVIDER_ANTHROPIC","maxOutputTokens":64000}`),
		"gemini-test":       json.RawMessage(`{"modelProvider":"MODEL_PROVIDER_GOOGLE","maxOutputTokens":65536,"minThinkingBudget":128}`),
		"gemini-search":     json.RawMessage(`{"modelProvider":"MODEL_PROVIDER_GOOGLE"}`),
	},
	SearchModels: []string{"gemini-search"},
}

// A trimmed copy of the request shape Claude Code 2.1 sends.
const claudeCodeRequest = `{
 "model":"claude-sonnet-4-6","max_tokens":32000,"stream":true,
 "system":[{"type":"text","text":"You are Claude Code."},{"type":"text","text":"Rules","cache_control":{"type":"ephemeral"}}],
 "messages":[
  {"role":"user","content":[{"type":"text","text":"find the tool","cache_control":{"type":"ephemeral"}}]},
  {"role":"system","content":[{"type":"text","text":"# Environment"}]},
  {"role":"assistant","content":[{"type":"tool_use","id":"toolu_client_1","name":"ToolSearch","input":{"query":"notebook"}}]},
  {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_client_1","content":[{"type":"tool_reference","tool_name":"NotebookEdit"}],"cache_control":{"type":"ephemeral"}}]}
 ],
 "tools":[
  {"name":"ToolSearch","description":"search tools","input_schema":{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}},
  {"name":"NotebookEdit","description":"edit","defer_loading":true,"input_schema":{"type":"object","properties":{"cell":{"type":"integer","exclusiveMinimum":0}}}},
  {"name":"Hidden","description":"never referenced","defer_loading":true,"input_schema":{"type":"object"}},
  {"name":"mcp__srv.example__do","description":"dotted MCP name","input_schema":{"type":"object","cache_control":{"type":"ephemeral"}}}
 ],
 "tool_choice":{"type":"auto","disable_parallel_tool_use":true},
 "metadata":{"user_id":"u"},
 "thinking":{"type":"adaptive","display":"omitted"},
 "context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]},
 "output_config":{"effort":"high"},
 "some_future_field":1
}`

func TestClaudeCodeRequestShapeIsAccepted(t *testing.T) {
	d := Decoder{State: testStore(t)}
	in, err := d.Messages(context.Background(), []byte(claudeCodeRequest), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := in.Prepare(testCatalog); err != nil {
		t.Fatal(err)
	}
	if !in.HideThinking || in.Request.System == nil || len(in.Request.System.Parts) != 3 {
		t.Fatalf("system or thinking display lost: %+v", in)
	}
	declared := map[string]bool{}
	for _, tool := range in.Request.Tools {
		for _, f := range tool.Functions {
			declared[in.PublicTool(f.Name).Name] = true
			if strings.Contains(string(f.Parameters), "$schema") || strings.Contains(string(f.Parameters), "exclusiveMinimum") {
				t.Fatalf("schema not cleaned: %s", f.Parameters)
			}
			if strings.ContainsAny(f.Name, ".:") {
				t.Fatalf("invalid native tool name %q", f.Name)
			}
		}
	}
	if !declared["ToolSearch"] || !declared["NotebookEdit"] || declared["Hidden"] || !declared["mcp__srv.example__do"] {
		t.Fatalf("deferred tool handling wrong: %v", declared)
	}
	if b := in.Request.Generation.Thinking.Budget; b == nil || *b != 24576 {
		t.Fatalf("effort not applied: %+v", in.Request.Generation.Thinking)
	}
	if len(in.Warnings) != 1 || !strings.Contains(in.Warnings[0], "some_future_field") {
		t.Fatalf("only the unknown field should be reported: %v", in.Warnings)
	}
}

// A trimmed copy of the request shape Codex CLI 0.164 sends.
const codexRequest = `{
 "model":"gemini-test","stream":true,"store":false,"parallel_tool_calls":false,"tool_choice":"auto",
 "include":["reasoning.encrypted_content"],"prompt_cache_key":"k","client_metadata":{"session_id":"s"},
 "reasoning":{"effort":"xhigh","summary":"auto"},
 "input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"rules"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],
 "tools":[
  {"type":"function","name":"exec_command","strict":false,"parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"],"additionalProperties":false}},
  {"type":"namespace","name":"multi_agent_v1","description":"agents","tools":[{"type":"function","name":"close_agent","strict":false,"parameters":{"type":"object","properties":{"target":{"type":"string"}}}}]},
  {"type":"custom","name":"apply_patch","description":"patch","format":{"type":"grammar","syntax":"lark","definition":"start: line+"}},
  {"type":"web_search","external_web_access":false}
 ]
}`

func TestCodexRequestShapeIsAccepted(t *testing.T) {
	d := Decoder{State: testStore(t)}
	in, err := d.Responses(context.Background(), []byte(codexRequest))
	if err != nil {
		t.Fatal(err)
	}
	if err := in.Prepare(testCatalog); err != nil {
		t.Fatal(err)
	}
	if in.Search || in.Kind != ag.KindAgent {
		t.Fatal("web search should be dropped next to function tools")
	}
	names := map[string]ToolName{}
	for _, tool := range in.Request.Tools {
		if tool.GoogleSearch != nil {
			t.Fatal("search tool sent with functions")
		}
		for _, f := range tool.Functions {
			names[f.Name] = in.PublicTool(f.Name)
		}
	}
	if got := names["multi_agent_v1__close_agent"]; got.Name != "close_agent" || got.Namespace != "multi_agent_v1" {
		t.Fatalf("namespace not flattened: %v", names)
	}
	if !names["apply_patch"].Custom || names["exec_command"].Name != "exec_command" {
		t.Fatalf("custom tool not mapped: %v", names)
	}
	if th := in.Request.Generation.Thinking; th == nil || th.Level != "high" {
		t.Fatalf("xhigh effort not mapped: %+v", th)
	}
}

func TestToolIdentityRoundTripsThroughOutput(t *testing.T) {
	s := testStore(t)
	d := Decoder{State: s}
	ctx := context.Background()
	in, err := d.Responses(ctx, []byte(codexRequest))
	if err != nil {
		t.Fatal(err)
	}
	if err := in.Prepare(testCatalog); err != nil {
		t.Fatal(err)
	}
	var events []string
	r := NewRenderer("responses", in, s, func(name string, _ map[string]any) error { events = append(events, name); return nil })
	err = r.Add(ctx, ag.Chunk{Parts: []ag.Part{
		{FunctionCall: &ag.Call{ID: "n1", Name: "multi_agent_v1__close_agent", Args: raw(map[string]string{"target": "a"})}, Signature: "sig-1"},
		{FunctionCall: &ag.Call{ID: "n2", Name: "apply_patch", Args: raw(map[string]string{"input": "*** Begin Patch"})}},
	}, Finish: "STOP"})
	if err == nil {
		err = r.Finish(ctx)
	}
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Output []ResponseItem `json:"output"`
	}
	_ = json.Unmarshal(r.Result, &out)
	if len(out.Output) != 2 || out.Output[0].Type != "function_call" || out.Output[0].Name != "close_agent" || out.Output[0].Namespace != "multi_agent_v1" {
		t.Fatalf("namespaced call: %+v", out.Output)
	}
	if out.Output[1].Type != "custom_tool_call" || out.Output[1].Name != "apply_patch" || out.Output[1].Input != "*** Begin Patch" {
		t.Fatalf("custom call: %+v", out.Output[1])
	}
	if !strings.Contains(strings.Join(events, ","), "response.custom_tool_call_input.done") {
		t.Fatalf("custom tool events missing: %v", events)
	}
	// The client replays both calls with their outputs.
	var items []any
	_ = json.Unmarshal([]byte(`[{"role":"user","content":"hi"}]`), &items)
	for _, item := range out.Output {
		items = append(items, item)
	}
	items = append(items,
		map[string]any{"type": "function_call_output", "call_id": out.Output[0].CallID, "output": "closed"},
		map[string]any{"type": "custom_tool_call_output", "call_id": out.Output[1].CallID, "output": "applied"})
	next, err := d.Responses(ctx, raw(map[string]any{"model": "gemini-test", "input": items, "store": false}))
	if err != nil {
		t.Fatal(err)
	}
	native, err := d.NativeRequest(ctx, next.Request, true)
	if err != nil {
		t.Fatal(err)
	}
	calls := native.Contents[1].Parts
	if calls[0].FunctionCall.Name != "multi_agent_v1__close_agent" || calls[0].FunctionCall.ID != "n1" || calls[0].Signature != "sig-1" {
		t.Fatalf("first call not restored: %s", raw(calls[0]))
	}
	if calls[1].FunctionCall.Name != "apply_patch" || string(calls[1].FunctionCall.Args) != `{"input":"*** Begin Patch"}` {
		t.Fatalf("custom call not restored: %s", raw(calls[1]))
	}
	if len(next.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", next.Warnings)
	}
}

func TestMissingReplayStateDegrades(t *testing.T) {
	s := testStore(t)
	d := Decoder{State: s}
	ctx := context.Background()
	foreign, _ := s.Seal("other-model", Replay{Parts: []ag.Part{{Text: "x", Thought: true, Signature: "s"}}})
	own, _ := s.Seal("gemini-test", Replay{Parts: []ag.Part{{Text: "real thought", Thought: true, Signature: "s"}}})
	body := raw(map[string]any{"model": "gemini-test", "max_tokens": 100, "messages": []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "thinking", "thinking": "from elsewhere", "signature": "EqQBCkYIBRgCKkB"},
			map[string]any{"type": "thinking", "thinking": "x", "signature": foreign},
			map[string]any{"type": "thinking", "thinking": "edited thought", "signature": own},
			map[string]any{"type": "redacted_thinking", "data": "opaque"},
			map[string]any{"type": "tool_use", "id": "toolu_agy_expired", "name": "lookup", "input": map[string]any{}},
		}},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_agy_expired", "content": "ok"}}},
	}})
	in, err := d.Messages(ctx, body, false)
	if err != nil {
		t.Fatal(err)
	}
	native, err := d.NativeRequest(ctx, in.Request, true)
	if err != nil {
		t.Fatal(err)
	}
	model := native.Contents[1].Parts
	if len(model) != 1 || model[0].FunctionCall == nil {
		t.Fatalf("unrestorable thinking should be dropped: %s", raw(model))
	}
	if model[0].FunctionCall.ID != "toolu_agy_expired" || model[0].Signature != SkipSignature {
		t.Fatalf("missing tool state should use the placeholder: %s", raw(model[0]))
	}
	if native.Contents[2].Parts[0].FunctionResponse.ID != "toolu_agy_expired" {
		t.Fatal("result no longer matches its call")
	}
	if len(in.Warnings) < 4 {
		t.Fatalf("degradations should be reported: %v", in.Warnings)
	}
}

func TestModelChangeInStoredChainDropsSignatures(t *testing.T) {
	s := testStore(t)
	d := Decoder{State: s}
	ctx := context.Background()
	in, _ := d.Responses(ctx, []byte(`{"model":"claude-test","input":"first"}`))
	r := NewRenderer("responses", in, s, nil)
	if err := r.Add(ctx, ag.Chunk{Parts: []ag.Part{{Text: "plan", Thought: true, Signature: "claude-sig"}, {Text: "answer"}}, Finish: "STOP"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	next, err := d.Responses(ctx, raw(map[string]any{"model": "gemini-test", "input": "second", "previous_response_id": r.ID}))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range next.Request.Contents {
		for _, p := range c.Parts {
			if p.Thought || p.Signature != "" {
				t.Fatalf("signature carried across models: %s", raw(next.Request.Contents))
			}
		}
	}
	if len(next.Request.Contents) != 3 || len(next.Warnings) == 0 {
		t.Fatalf("context lost or change not reported: %s %v", raw(next.Request.Contents), next.Warnings)
	}
}

func TestHiddenThinkingStillReplays(t *testing.T) {
	s := testStore(t)
	d := Decoder{State: s}
	ctx := context.Background()
	in, err := d.Messages(ctx, []byte(`{"model":"claude-test","max_tokens":2048,"thinking":{"type":"adaptive","display":"omitted"},"messages":[{"role":"user","content":"hi"}]}`), false)
	if err != nil {
		t.Fatal(err)
	}
	var deltas []string
	r := NewRenderer("messages", in, s, func(name string, v map[string]any) error {
		if delta, ok := v["delta"].(map[string]any); ok {
			kind, _ := delta["type"].(string)
			deltas = append(deltas, kind)
		}
		return nil
	})
	if err := r.Add(ctx, ag.Chunk{Parts: []ag.Part{{Text: "secret plan", Thought: true, Signature: "sig"}, {Text: "answer"}}, Finish: "STOP"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(deltas, ","), "thinking_delta") || strings.Contains(string(r.Result), "secret plan") {
		t.Fatalf("thinking text exposed: %v %s", deltas, r.Result)
	}
	var out struct {
		Content []Block `json:"content"`
	}
	_ = json.Unmarshal(r.Result, &out)
	next, err := d.Messages(ctx, raw(map[string]any{"model": "claude-test", "max_tokens": 2048, "messages": []any{
		map[string]any{"role": "user", "content": "hi"}, map[string]any{"role": "assistant", "content": out.Content}, map[string]any{"role": "user", "content": "more"}}}), false)
	if err != nil {
		t.Fatal(err)
	}
	if p := next.Request.Contents[1].Parts[0]; p.Text != "secret plan" || p.Signature != "sig" {
		t.Fatalf("hidden thinking not replayed: %s", raw(next.Request.Contents[1]))
	}
}

func TestFinishReasonsAndUnknownParts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	run := func(chunk ag.Chunk) (*Renderer, error) {
		r := NewRenderer("messages", Input{Model: "gemini-test"}, s, nil)
		if err := r.Add(ctx, chunk); err != nil {
			return r, err
		}
		return r, r.Finish(ctx)
	}
	r, err := run(ag.Chunk{Parts: []ag.Part{{Text: "x"}}, Finish: "MALFORMED_FUNCTION_CALL"})
	var upstream *ag.HTTPError
	if !errors.As(err, &upstream) || upstream.Status != 502 || !strings.Contains(upstream.Message, "MALFORMED_FUNCTION_CALL") {
		t.Fatalf("malformed call should be a retryable error: %v", err)
	}
	r, err = run(ag.Chunk{Parts: []ag.Part{{Text: "x"}}, Finish: "IMAGE_SAFETY"})
	if err != nil || !strings.Contains(string(r.Result), `"stop_reason":"refusal"`) {
		t.Fatalf("%v %s", err, r.Result)
	}
	r, err = run(ag.Chunk{Parts: []ag.Part{{Text: "x"}}, Finish: "SOMETHING_NEW"})
	if err != nil || !strings.Contains(string(r.Result), `"stop_reason":"end_turn"`) || len(r.Warnings) != 1 {
		t.Fatalf("unknown finish should be a stop with a warning: %v %s %v", err, r.Result, r.Warnings)
	}
	r, err = run(ag.Chunk{Parts: []ag.Part{{Unknown: []string{"newThing"}}, {Text: "kept", Unknown: []string{"meta"}}}, Finish: "STOP"})
	if err != nil || !strings.Contains(string(r.Result), "kept") || len(r.Warnings) != 2 {
		t.Fatalf("%v %s %v", err, r.Result, r.Warnings)
	}
	r, err = run(ag.Chunk{Finish: "STOP"})
	if err != nil || !strings.Contains(string(r.Result), `"content":[]`) {
		t.Fatalf("empty completion should succeed: %v %s", err, r.Result)
	}
}

func TestSearchRoutingAndToolNames(t *testing.T) {
	d := Decoder{State: testStore(t)}
	in, err := d.Messages(context.Background(), []byte(`{"model":"claude-test","max_tokens":100,"messages":[{"role":"user","content":"news"}],"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":5}]}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := in.Prepare(testCatalog); err != nil {
		t.Fatal(err)
	}
	if in.UpstreamModel() != "gemini-search" || in.Kind != ag.KindSearch || in.Request.Tools[0].GoogleSearch == nil {
		t.Fatalf("search not routed: %+v", in)
	}
	for public, want := range map[string]string{"Bash": "Bash", "a.b": "a_b_", "1tool": "_1tool_", strings.Repeat("x", 130): strings.Repeat("x", 119) + "_"} {
		got := nativeName(public)
		if !strings.HasPrefix(got, want) || len(got) > 128 || invalidNameChars.MatchString(got) {
			t.Errorf("nativeName(%q)=%q", public, got)
		}
	}
	if nativeName("a.b") == nativeName("a_b") {
		t.Error("distinct names collided")
	}
}

func TestClaudeForcedToolChoiceDisablesThinking(t *testing.T) {
	d := Decoder{State: testStore(t)}
	in, err := d.Responses(context.Background(), []byte(`{"model":"claude-test","input":"x","tool_choice":"required","reasoning":{"effort":"low"},"tools":[{"type":"function","name":"f","parameters":{"type":"object"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := in.Prepare(testCatalog); err != nil {
		t.Fatal(err)
	}
	if b := in.Request.Generation.Thinking.Budget; b == nil || *b != 0 || len(in.Warnings) != 1 {
		t.Fatalf("%+v %v", in.Request.Generation.Thinking, in.Warnings)
	}
	in, _ = d.Responses(context.Background(), []byte(`{"model":"gemini-test","input":"x","tool_choice":"required","reasoning":{"effort":"low"},"tools":[{"type":"function","name":"f","parameters":{"type":"object"}}]}`))
	_ = in.Prepare(testCatalog)
	if in.Request.Generation.Thinking.Level != "low" {
		t.Fatal("Gemini thinking should be unaffected")
	}
}

func TestClaudeGetsNoSignaturePlaceholder(t *testing.T) {
	d := Decoder{State: testStore(t)}
	ctx := context.Background()
	in, err := d.Messages(ctx, []byte(`{"model":"claude-test","max_tokens":100,"messages":[{"role":"user","content":"x"},{"role":"assistant","content":[{"type":"tool_use","id":"toolu_agy_gone","name":"f","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_agy_gone","content":"ok"}]}]}`), false)
	if err != nil {
		t.Fatal(err)
	}
	native, err := d.NativeRequest(ctx, in.Request, false)
	if err != nil {
		t.Fatal(err)
	}
	if sig := native.Contents[1].Parts[0].Signature; sig != "" {
		t.Fatalf("Claude call got signature %q", sig)
	}
}
