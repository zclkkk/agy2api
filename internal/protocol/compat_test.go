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
		"gemini-test":   json.RawMessage(`{"minThinkingBudget":128}`),
		"gemini-search": json.RawMessage(`{}`),
	},
	SearchModels: []string{"gemini-search"},
}

// A trimmed copy of the request shape Claude Code 2.1 sends.
const claudeCodeRequest = `{
 "model":"gemini-test","max_tokens":32000,"stream":true,
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
  {"name":"mcp__srv.example__do","description":"dotted MCP name","input_schema":{"type":"object","cache_control":{"type":"ephemeral"}}},
  {"name":"bad name/x","description":"needs mapping","input_schema":{"type":"object"}}
 ],
 "tool_choice":{"type":"auto","disable_parallel_tool_use":true},
 "metadata":{"user_id":"u"},
 "thinking":{"type":"adaptive","display":"omitted"},
 "context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]},
 "output_config":{"effort":"high"},
 "some_future_field":1
}`

func TestClaudeCodeRequestShapeIsAccepted(t *testing.T) {
	d := testPipeline{State: testStore(t)}
	in, err := d.Messages(context.Background(), []byte(claudeCodeRequest), false)
	if err != nil {
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
			if invalidNameChars.MatchString(f.Name) {
				t.Fatalf("invalid native tool name %q", f.Name)
			}
		}
	}
	if !declared["ToolSearch"] || !declared["NotebookEdit"] || declared["Hidden"] || !declared["mcp__srv.example__do"] || !declared["bad name/x"] {
		t.Fatalf("deferred tool handling wrong: %v", declared)
	}
	if in.Tools["mcp__srv.example__do"].Name == "" {
		t.Fatal("a name Gemini accepts must be sent unchanged")
	}
	if th := in.Request.Generation.Thinking; th == nil || th.Level != "high" {
		t.Fatalf("effort not applied: %+v", th)
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
	d := testPipeline{State: testStore(t)}
	in, err := d.Responses(context.Background(), []byte(codexRequest))
	if err != nil {
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
	foundNamespace := false
	for _, got := range names {
		if got.Name == "close_agent" && got.Namespace == "multi_agent_v1" {
			foundNamespace = true
		}
	}
	if !foundNamespace {
		t.Fatalf("namespace identity lost: %v", names)
	}
	foundCustom := false
	for _, got := range names {
		if got.Name == "apply_patch" && got.Custom {
			foundCustom = true
		}
	}
	if !foundCustom || names["exec_command"].Name != "exec_command" {
		t.Fatalf("custom tool not mapped: %v", names)
	}
	if th := in.Request.Generation.Thinking; th == nil || th.Level != "high" {
		t.Fatalf("xhigh effort not mapped: %+v", th)
	}
}

func TestToolIdentityRoundTripsThroughOutput(t *testing.T) {
	s := testStore(t)
	d := testPipeline{State: s}
	ctx := context.Background()
	in, err := d.Responses(ctx, []byte(codexRequest))
	if err != nil {
		t.Fatal(err)
	}
	var namespaced, custom string
	for name, public := range in.Tools {
		if public.Name == "close_agent" && public.Namespace == "multi_agent_v1" {
			namespaced = name
		}
		if public.Custom && public.Name == "apply_patch" {
			custom = name
		}
	}
	if namespaced == "" || custom == "" {
		t.Fatal("namespace declaration missing")
	}
	var events []string
	r := NewRenderer(in, s, func(name string, _ map[string]any) error { events = append(events, name); return nil })
	err = r.Add(ctx, ag.Chunk{Parts: []ag.Part{
		{FunctionCall: &ag.Call{ID: "n1", Name: namespaced, Args: raw(map[string]string{"target": "a"})}, Signature: "sig-1"},
		{FunctionCall: &ag.Call{ID: "n2", Name: custom, Args: raw(map[string]string{"input": "*** Begin Patch"})}},
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
	calls := next.Request.Contents[1].Parts
	if calls[0].FunctionCall.Name != namespaced || calls[0].FunctionCall.ID != out.Output[0].CallID || calls[0].Signature != "sig-1" {
		t.Fatalf("first call not restored: %s", raw(calls[0]))
	}
	if calls[1].FunctionCall.Name != custom || string(calls[1].FunctionCall.Args) != `{"input":"*** Begin Patch"}` {
		t.Fatalf("custom call not restored: %s", raw(calls[1]))
	}
	if len(next.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", next.Warnings)
	}
}

func TestMissingReplayStateDegrades(t *testing.T) {
	s := testStore(t)
	d := testPipeline{State: s}
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
	native := in.Request
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
	d := testPipeline{State: s}
	ctx := context.Background()
	in, _ := d.Responses(ctx, []byte(`{"model":"gemini-other","input":"first"}`))
	r := NewRenderer(in, s, nil)
	if err := r.Add(ctx, ag.Chunk{Parts: []ag.Part{{Text: "plan", Thought: true, Signature: "other-sig"}, {Text: "answer"}}, Finish: "STOP"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	next, err := d.Responses(ctx, raw(map[string]any{"model": "gemini-test", "input": "second", "previous_response_id": r.id}))
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
	d := testPipeline{State: s}
	ctx := context.Background()
	in, err := d.Messages(ctx, []byte(`{"model":"gemini-test","max_tokens":2048,"thinking":{"type":"adaptive","display":"omitted"},"messages":[{"role":"user","content":"hi"}]}`), false)
	if err != nil {
		t.Fatal(err)
	}
	var deltas []string
	r := NewRenderer(in, s, func(name string, v map[string]any) error {
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
	next, err := d.Messages(ctx, raw(map[string]any{"model": "gemini-test", "max_tokens": 2048, "messages": []any{
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
		r := NewRenderer(PreparedRequest{Format: Messages, Model: "gemini-test", Upstream: "gemini-test"}, s, nil)
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
	d := testPipeline{State: testStore(t)}
	in, err := d.Messages(context.Background(), []byte(`{"model":"gemini-test","max_tokens":100,"messages":[{"role":"user","content":"news"}],"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":5}]}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if in.UpstreamModel() != "gemini-search" || in.Kind != ag.KindSearch || in.Request.Tools[0].GoogleSearch == nil {
		t.Fatalf("search not routed: %+v", in)
	}
	for public, want := range map[string]string{"Bash": "Bash", "mcp.srv:do-it": "mcp.srv:do-it", "a b": "agy_fn_", "1tool": "agy_fn_", strings.Repeat("x", 130): "agy_fn_"} {
		got := nativeName(public)
		if !strings.HasPrefix(got, want) || len(got) > 128 || invalidNameChars.MatchString(got) || (want == public) != (got == public) {
			t.Errorf("nativeName(%q)=%q", public, got)
		}
	}
	if nativeName("a b") == nativeName("a_b") {
		t.Error("distinct names collided")
	}
}

func TestToolCallingModes(t *testing.T) {
	d := testPipeline{State: testStore(t)}
	mode := func(body string) string {
		t.Helper()
		in, err := d.Responses(context.Background(), []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if in.Request.ToolConfig == nil {
			return ""
		}
		return in.Request.ToolConfig.FunctionCallingConfig.Mode
	}
	tools := `"tools":[{"type":"function","name":"f","parameters":{"type":"object"}}]`
	for choice, want := range map[string]string{"": "VALIDATED", `"tool_choice":"auto",`: "VALIDATED", `"tool_choice":"required",`: "ANY", `"tool_choice":"none",`: "NONE"} {
		if got := mode(`{"model":"gemini-test","input":"x",` + choice + tools + `}`); got != want {
			t.Errorf("tool_choice %q: mode %q, want %q", choice, got, want)
		}
	}
	if got := mode(`{"model":"gemini-test","input":"x"}`); got != "" {
		t.Errorf("no tools: mode %q", got)
	}
	if got := mode(`{"model":"gemini-test","input":"x","tools":[{"type":"web_search"}]}`); got != "" {
		t.Errorf("search only: mode %q", got)
	}
}

func TestIgnoredSearchOptionsAreReported(t *testing.T) {
	d := Decoder{}
	in, err := d.Messages([]byte(`{"model":"gemini-test","max_tokens":100,"messages":[{"role":"user","content":"x"}],
		"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":3,"allowed_domains":["python.org"],"user_location":{"type":"approximate","country":"US"}}]}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Warnings) != 1 || !strings.Contains(in.Warnings[0], "allowed_domains, user_location") {
		t.Fatalf("warnings: %v", in.Warnings)
	}
	in, err = d.Responses([]byte(`{"model":"gemini-test","input":"x","tools":[{"type":"web_search","filters":{"allowed_domains":["python.org"]}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Warnings) != 1 || !strings.Contains(in.Warnings[0], "filters") {
		t.Fatalf("warnings: %v", in.Warnings)
	}
	in, _ = d.Responses([]byte(`{"model":"gemini-test","input":"x","tools":[{"type":"web_search"}]}`))
	if len(in.Warnings) != 0 {
		t.Fatalf("plain search should not warn: %v", in.Warnings)
	}
}

func TestForcedSearchChoiceIsAccepted(t *testing.T) {
	d := testPipeline{State: testStore(t)}
	in, err := d.Messages(context.Background(), []byte(`{"model":"gemini-test","max_tokens":100,"messages":[{"role":"user","content":"news"}],
		"tools":[{"type":"web_search_20250305","name":"web_search"}],"tool_choice":{"type":"tool","name":"web_search"}}`), false)
	if err != nil || !in.Search || in.Request.ToolConfig != nil {
		t.Fatalf("Messages forced search: %v %+v", err, in.Request.ToolConfig)
	}
	if _, err := d.Messages(context.Background(), []byte(`{"model":"gemini-test","max_tokens":100,"messages":[{"role":"user","content":"news"}],
		"tools":[{"type":"web_search_20250305","name":"web_search"}],"tool_choice":{"type":"tool","name":"missing"}}`), false); err == nil {
		t.Fatal("choosing an undeclared tool must still fail")
	}
	in, err = d.Responses(context.Background(), []byte(`{"model":"gemini-test","input":"news","tools":[{"type":"web_search"}],"tool_choice":{"type":"web_search"}}`))
	if err != nil || !in.Search || strings.Contains(strings.Join(in.Warnings, ";"), "ignored tool_choice") {
		t.Fatalf("Responses forced search: %v %v", err, in.Warnings)
	}
}

func TestMessagesSearchReturnsResultBlocks(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	var grounding ag.Grounding
	if err := json.Unmarshal([]byte(`{"webSearchQueries":["latest python"],"groundingChunks":[{"web":{"uri":"https://www.python.org/downloads/","title":"python.org"}},{"web":{"uri":"https://www.python.org/downloads/","title":"python.org"}},{"web":{"uri":"https://docs.python.org/3/","title":"docs.python.org"}}],
		"groundingSupports":[{"segment":{"startIndex":0,"endIndex":6,"text":"Python"},"groundingChunkIndices":[0]}]}`), &grounding); err != nil {
		t.Fatal(err)
	}
	var events []string
	var starts []string
	r := NewRenderer(PreparedRequest{Format: Messages, Model: "gemini-test", Upstream: "gemini-test", Search: true}, s, func(name string, v map[string]any) error {
		events = append(events, name)
		if block, ok := v["content_block"].(map[string]any); ok {
			starts = append(starts, block["type"].(string))
		}
		return nil
	})
	if err := r.Add(ctx, ag.Chunk{Parts: []ag.Part{{Text: "Python 3.15 is current."}}, Finish: "STOP", Grounding: &grounding}); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	if strings.Join(starts, ",") != "text,server_tool_use,web_search_tool_result" || events[len(events)-1] != "message_stop" {
		t.Fatalf("blocks %v events %v", starts, events)
	}
	var out struct {
		Content []struct {
			Type      string          `json:"type"`
			ID        string          `json:"id"`
			ToolUseID string          `json:"tool_use_id"`
			Input     json.RawMessage `json:"input"`
			Content   []struct {
				Type, URL, Title, EncryptedContent string
			} `json:"content"`
		} `json:"content"`
		Stop string `json:"stop_reason"`
	}
	if err := json.Unmarshal(r.Result, &out); err != nil {
		t.Fatal(err)
	}
	use, result := out.Content[1], out.Content[2]
	if use.Type != "server_tool_use" || string(use.Input) != `{"query":"latest python"}` || result.ToolUseID != use.ID || out.Stop != "end_turn" {
		t.Fatalf("search blocks: %s", r.Result)
	}
	if len(result.Content) != 2 || result.Content[0].URL != "https://www.python.org/downloads/" || result.Content[1].Title != "docs.python.org" {
		t.Fatalf("results should be the distinct cited pages: %+v", result.Content)
	}
	// Without cited pages there is nothing to report.
	r = NewRenderer(PreparedRequest{Format: Messages, Model: "gemini-test", Upstream: "gemini-test", Search: true}, s, nil)
	if err := r.Add(ctx, ag.Chunk{Parts: []ag.Part{{Text: "no sources"}}, Finish: "STOP", Grounding: &ag.Grounding{Queries: []string{"q"}}}); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(r.Result), "server_tool_use") {
		t.Fatalf("empty search reported blocks: %s", r.Result)
	}
}

func TestChosenSearchDropsFunctionTools(t *testing.T) {
	d := testPipeline{State: testStore(t)}
	prepared := func(name, body string) PreparedRequest {
		t.Helper()
		var in PreparedRequest
		var err error
		if name == "messages" {
			in, err = d.Messages(context.Background(), []byte(body), false)
		} else {
			in, err = d.Responses(context.Background(), []byte(body))
		}
		if err != nil {
			t.Fatal(err)
		}
		return in
	}
	for name, body := range map[string]string{
		"messages":  `{"model":"gemini-test","max_tokens":100,"messages":[{"role":"user","content":"news"}],"tools":[{"type":"web_search_20250305","name":"web_search"},{"name":"lookup","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"web_search"}}`,
		"responses": `{"model":"gemini-test","input":"news","tools":[{"type":"web_search"},{"type":"function","name":"lookup","parameters":{"type":"object"}}],"tool_choice":{"type":"web_search"}}`,
	} {
		in := prepared(name, body)
		if !in.Search || in.Kind != ag.KindSearch || in.Request.ToolConfig != nil || len(in.Request.Tools) != 1 || in.Request.Tools[0].GoogleSearch == nil {
			t.Errorf("%s: chosen search must replace the functions: %+v", name, in.Request.Tools)
		}
		if len(in.Warnings) == 0 || !strings.Contains(in.Warnings[0], "function tools omitted") {
			t.Errorf("%s: warnings %v", name, in.Warnings)
		}
	}
	// Without an explicit choice the functions win, as Codex expects.
	in := prepared("responses", `{"model":"gemini-test","input":"news","tools":[{"type":"web_search"},{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`)
	if in.Search || !hasFunctions(in.Request.Tools) {
		t.Fatalf("auto choice must keep the functions: %+v", in.Request.Tools)
	}
}
