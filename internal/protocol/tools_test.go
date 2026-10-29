package protocol

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

func namespaceTool(namespace, name string) map[string]any {
	return map[string]any{"type": "namespace", "name": namespace, "tools": []any{
		map[string]any{"type": "function", "name": name, "parameters": map[string]string{"type": "object"}},
	}}
}

func TestReadableToolSymbols(t *testing.T) {
	for _, tc := range []struct {
		tool ToolName
		want string
	}{
		{ToolName{Name: "Bash"}, "Bash"},
		{ToolName{Name: "exec_command"}, "exec_command"},
		{ToolName{Name: "agy_read"}, "agy_read"},
		{ToolName{Name: "mcp.srv:do-it"}, "mcp.srv:do-it"},
		{ToolName{Namespace: "multi_agent_v1", Name: "close_agent"}, "multi_agent_v1__close_agent_4b27d8"},
		{ToolName{Name: "apply_patch", Custom: true}, "apply_patch_616f84"},
		{ToolName{Namespace: "files", Name: "read"}, "files__read_b88df0"},
		{ToolName{Namespace: "files", Name: "files__read"}, "files__read_232918"},
		{ToolName{Namespace: "files__", Name: "files__read"}, "files__read_a502c4"},
		{ToolName{Namespace: "files", Name: "mcp__srv__do"}, "mcp__srv__do_f906f1"},
		{ToolName{Name: "my tool/x"}, "my_tool_x_10f232"},
		{ToolName{Name: "1tool"}, "_1tool_3b07b1"},
	} {
		if got := toolSymbol(tc.tool); got != tc.want {
			t.Errorf("toolSymbol(%+v) = %q, want %q", tc.tool, got, tc.want)
		}
	}
	if nativeName("a b") == nativeName("a_b") {
		t.Fatal("a normalized name collided with the legal name it resembles")
	}
	if got := nativeName(strings.Repeat("x", 128)); got != strings.Repeat("x", 128) {
		t.Fatal("legal 128-byte function name changed")
	}
	for _, name := range []string{strings.Repeat("x", 129), strings.Repeat("x", 130)} {
		got := nativeName(name)
		if len(got) != 128 || !strings.HasPrefix(got, strings.Repeat("x", 121)+"_") || !regexp.MustCompile(`_[0-9a-f]{6}$`).MatchString(got) {
			t.Fatalf("bad truncation or suffix: %q", got)
		}
	}
	if nativeName(strings.Repeat("x", 129)) == nativeName(strings.Repeat("x", 130)) {
		t.Fatal("truncated characters were lost from identity")
	}
}

func TestToolNameIndependentOfDeclarationMetadataAndOrder(t *testing.T) {
	decode := func(tools []any) map[ToolName]string {
		t.Helper()
		in, err := (Decoder{}).Responses(raw(map[string]any{"model": "gemini-test", "input": "hello", "tools": tools}))
		if err != nil {
			t.Fatal(err)
		}
		out := map[ToolName]string{}
		for native, identity := range in.Tools {
			out[identity] = native
		}
		return out
	}
	tool := map[string]any{"type": "custom", "name": "apply_patch", "description": "old"}
	first := decode([]any{tool})
	tool["description"] = "new description"
	tool["format"] = map[string]string{"type": "grammar", "syntax": "lark", "definition": "start: WORD"}
	other := map[string]any{"type": "function", "name": "Bash", "parameters": map[string]string{"type": "object"}}
	for _, declarations := range [][]any{{other, tool}, {tool, other}} {
		got := decode(declarations)
		identity := ToolName{Name: "apply_patch", Custom: true}
		if got[identity] != first[identity] {
			t.Fatal("tool name changed with metadata or neighboring declarations")
		}
	}
}

func TestNamespacedAndPlainNamesRemainDistinct(t *testing.T) {
	in := testPipeline{State: testStore(t)}.mustResponses(t, `{"model":"gemini-test","input":"x","tools":[{"type":"function","name":"files__read","parameters":{"type":"object"}},{"type":"namespace","name":"files","tools":[{"type":"function","name":"read","parameters":{"type":"object"}}]}]}`)
	seen := map[string]bool{}
	for _, tool := range in.Request.Tools {
		for _, f := range tool.Functions {
			if seen[f.Name] {
				t.Fatalf("distinct tools collide at native name %q", f.Name)
			}
			seen[f.Name] = true
		}
	}
}

func TestToolCollisionsAreRejected(t *testing.T) {
	custom := ToolName{Name: "apply_patch", Custom: true}
	// These namespaces have the same first 24 hash bits for mcp__srv__do.
	// Both keep the MCP stem, so this is an actual short-hash collision.
	first := ToolName{Namespace: "ns188", Name: "mcp__srv__do"}
	second := ToolName{Namespace: "ns2523", Name: "mcp__srv__do"}
	if toolSymbol(first) != "mcp__srv__do_78b5d0" || toolSymbol(first) != toolSymbol(second) {
		t.Fatal("collision fixture changed")
	}
	for _, pair := range [][2]ToolName{{custom, {Name: toolSymbol(custom)}}, {first, second}} {
		for _, reverse := range []bool{false, true} {
			a, b := pair[0], pair[1]
			if reverse {
				a, b = b, a
			}
			in := RequestIntent{}
			if _, err := in.declare(a); err != nil {
				t.Fatal(err)
			}
			_, err := in.declare(b)
			var validation *Error
			if !errors.As(err, &validation) || validation.Param != "tools" {
				t.Fatalf("colliding declarations accepted: %v", err)
			}
			history := RequestIntent{}
			if _, err := history.registerHistoryTool(a); err != nil {
				t.Fatal(err)
			}
			if _, err := history.registerHistoryTool(b); err == nil {
				t.Fatal("colliding historical calls accepted")
			}
		}
	}
}

func TestToolIdentityRoundTripsThroughOutput(t *testing.T) {
	s := testStore(t)
	p := testPipeline{State: s}
	in := p.mustResponses(t, codexRequest)
	namespaced := toolSymbol(ToolName{Namespace: "multi_agent_v1", Name: "close_agent"})
	custom := toolSymbol(ToolName{Name: "apply_patch", Custom: true})
	var events []string
	r := NewRenderer(in, s, func(name string, _ map[string]any) error { events = append(events, name); return nil })
	ctx := context.Background()
	err := r.Add(ctx, ag.Chunk{Parts: []ag.Part{
		{FunctionCall: &ag.Call{ID: "n1", Name: namespaced, Args: raw(map[string]string{"target": "a"})}, Signature: "sig-1"},
		{FunctionCall: &ag.Call{ID: "n2", Name: custom, Args: raw(map[string]string{"input": "*** Begin Patch"})}},
	}, Finish: "STOP"})
	if err == nil {
		err = r.Finish(ctx)
	}
	if err != nil {
		t.Fatal(err)
	}
	out := responseItems(t, r)
	if len(out) != 2 || out[0].Type != "function_call" || out[0].Name != "close_agent" || out[0].Namespace != "multi_agent_v1" {
		t.Fatalf("namespaced call: %+v", out)
	}
	if out[1].Type != "custom_tool_call" || out[1].Name != "apply_patch" || out[1].Input != "*** Begin Patch" {
		t.Fatalf("custom call: %+v", out[1])
	}
	if !strings.Contains(strings.Join(events, ","), "response.custom_tool_call_input.done") {
		t.Fatalf("custom tool events missing: %v", events)
	}
	// The client replays both calls with their outputs.
	items := []any{map[string]string{"role": "user", "content": "hi"}, out[0], out[1],
		map[string]any{"type": "function_call_output", "call_id": out[0].CallID, "output": "closed"},
		map[string]any{"type": "custom_tool_call_output", "call_id": out[1].CallID, "output": "applied"}}
	next := p.mustResponses(t, map[string]any{"model": "gemini-test", "input": items, "store": false})
	calls := next.Request.Contents[1].Parts
	if calls[0].FunctionCall.Name != namespaced || calls[0].FunctionCall.ID != out[0].CallID || calls[0].Signature != "sig-1" {
		t.Fatalf("first call not restored: %s", raw(calls[0]))
	}
	if calls[1].FunctionCall.Name != custom || string(calls[1].FunctionCall.Args) != `{"input":"*** Begin Patch"}` {
		t.Fatalf("custom call not restored: %s", raw(calls[1]))
	}
	if len(next.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", next.Warnings)
	}
}

func TestToolIdentityCheckedAcrossHistoryAndDeclarations(t *testing.T) {
	s := testStore(t)
	p := testPipeline{State: s}
	custom := ToolName{Name: "apply_patch", Custom: true}
	native := toolSymbol(custom)
	in := p.mustResponses(t, `{"model":"gemini-test","input":"patch","tools":[{"type":"custom","name":"apply_patch"},{"type":"function","name":"unused"}]}`)
	r := turn(t, s, in, ag.Part{FunctionCall: &ag.Call{Name: native, Args: raw(map[string]string{"input": "patch"})}, Signature: "fixture-signature"})
	call := responseItems(t, r)[0]
	result := map[string]any{"type": "custom_tool_call_output", "call_id": call.CallID, "output": "done"}
	stored, err := LoadResponse(context.Background(), s, r.id)
	if err != nil || len(stored.Tools) != 1 || stored.Tools[native] != custom {
		t.Fatalf("stored identities must describe actual history, not unused declarations: %v", err)
	}
	for _, chained := range []bool{false, true} {
		body := map[string]any{"model": "gemini-test", "input": []any{map[string]string{"role": "user", "content": "patch"}, call, result}}
		if chained {
			body["previous_response_id"] = r.id
			body["input"] = []any{result}
		}
		// Correct replay works without re-declaring the historical tool.
		next := p.mustResponses(t, body)
		part := next.Request.Contents[1].Parts[0]
		if part.Signature != "fixture-signature" || part.FunctionCall.Name != native || next.PublicTool(native) != custom {
			t.Fatal("tool identity or signature lost on replay")
		}
		// Historical tools must not become callable through tool_choice alone.
		body["tool_choice"] = map[string]string{"type": "custom", "name": "apply_patch"}
		if _, err := p.Responses(body); err == nil {
			t.Fatal("historical tool treated as a current declaration")
		}
		delete(body, "tool_choice")
		body["tools"] = []any{map[string]string{"type": "function", "name": native}}
		if _, err := p.Responses(body); err == nil {
			t.Fatal("historical custom tool aliased a current ordinary function")
		}
	}
	// Reusing a call ID with a different identity must not reuse the signature,
	// even when the native name and argument bytes match: the short hash is not
	// an identity check.
	forged := ResponseItem{Type: "function_call", CallID: call.CallID, Name: native, Arguments: `{"input":"patch"}`}
	next := p.mustResponses(t, map[string]any{"model": "gemini-test", "input": []any{map[string]string{"role": "user", "content": "patch"}, forged, result}})
	if sig := next.Request.Contents[1].Parts[0].Signature; sig == "fixture-signature" {
		t.Fatal("replay record signature reused for a different tool identity")
	}
}

// A call's signature is replayed only for the call it was issued for: the same
// model, full tool identity, name and arguments. A changed call keeps the
// identity the client sent and gets a placeholder signature; it is not rejected.
func TestChangedCallsDoNotReuseSignatures(t *testing.T) {
	s := testStore(t)
	p := testPipeline{State: s}
	tools := []any{namespaceTool("files", "read")}
	in := p.mustResponses(t, map[string]any{"model": "gemini-test", "input": "read a.txt", "store": false, "tools": tools})
	native := toolSymbol(ToolName{Namespace: "files", Name: "read"})
	call := responseItems(t, turn(t, s, in, ag.Part{FunctionCall: &ag.Call{Name: native, Args: raw(map[string]string{"path": "a.txt"})}, Signature: "sig-read"}))[0]
	replay := func(model string, c ResponseItem) PreparedRequest {
		t.Helper()
		return p.mustResponses(t, map[string]any{"model": model, "store": false, "tools": tools, "input": []any{
			map[string]string{"role": "user", "content": "read a.txt"}, c,
			map[string]string{"type": "function_call_output", "call_id": c.CallID, "output": "contents"},
		}})
	}
	if got := replay("gemini-test", call).Request.Contents[1].Parts[0].Signature; got != "sig-read" {
		t.Fatalf("unchanged call lost its signature: %q", got)
	}
	edited, renamed, moved, bare := call, call, call, call
	edited.Arguments = `{"path":"b.txt"}`
	renamed.Name = "write"
	moved.Namespace = "docs"
	bare.Namespace = "" // e.g. a client that drops the namespace field
	for name, tc := range map[string]struct {
		model string
		call  ResponseItem
	}{
		"arguments": {"gemini-test", edited}, "name": {"gemini-test", renamed}, "namespace": {"gemini-test", moved},
		"missing namespace": {"gemini-test", bare}, "model": {"gemini-other", call},
	} {
		in := replay(tc.model, tc.call)
		part := in.Request.Contents[1].Parts[0]
		if part.Signature != SkipSignature || !warned(in.Warnings, "placeholder signature") {
			t.Errorf("%s: signature %q, warnings %v", name, part.Signature, in.Warnings)
		}
		if got := in.PublicTool(part.FunctionCall.Name); got != (ToolName{Namespace: tc.call.Namespace, Name: tc.call.Name}) {
			t.Errorf("%s: the client's tool identity became %+v", name, got)
		}
		if result := in.Request.Contents[2].Parts[0].FunctionResponse; result.Name != part.FunctionCall.Name {
			t.Errorf("%s: result not paired with its call", name)
		}
	}
}

func TestToolCallingModes(t *testing.T) {
	p := testPipeline{State: testStore(t)}
	mode := func(body string) string {
		t.Helper()
		in := p.mustResponses(t, body)
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
}

func TestAllowedToolsAutoDeclaresOnlyTheSubset(t *testing.T) {
	in := testPipeline{State: testStore(t)}.mustResponses(t, `{"model":"gemini-test","input":"Choose a tool","tools":[{"type":"function","name":"read_file","parameters":{"type":"object"}},{"type":"function","name":"delete_file","parameters":{"type":"object"}}],"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"read_file"}]}}`)
	var declared []string
	for _, tool := range in.Request.Tools {
		for _, f := range tool.Functions {
			declared = append(declared, f.Name)
		}
	}
	if len(declared) != 1 || declared[0] != "read_file" {
		t.Fatalf("declared %v", declared)
	}
	if in.Request.ToolConfig == nil || in.Request.ToolConfig.FunctionCallingConfig.Mode != "VALIDATED" {
		t.Fatal("an auto subset keeps tool calls optional")
	}
}

func TestToolPolicyRejectsUnknownAndSeparatesToolTypes(t *testing.T) {
	p := testPipeline{State: testStore(t)}
	for _, body := range []string{
		`{"model":"gemini-test","input":"x","tool_choice":{"type":"web_search"}}`,
		`{"model":"gemini-test","input":"x","tools":[{"type":"function","name":"read","parameters":{"type":"object"}}],"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"unknown"}]}}`,
		`{"model":"gemini-test","input":"x","tool_choice":"required"}`,
		`{"model":"gemini-test","input":"x","tools":[{"type":"function","name":"read"},{"type":"function","name":"read"}]}`,
	} {
		if _, err := p.Responses(body); err == nil {
			t.Fatalf("invalid tool policy accepted: %s", body)
		}
	}
	in := p.mustResponses(t, `{"model":"gemini-test","input":"x","tools":[{"type":"function","name":"run","parameters":{"type":"object"}},{"type":"custom","name":"run"}],"tool_choice":{"type":"custom","name":"run"}}`)
	if len(in.Tools) != 2 || len(in.Request.Tools) != 1 || !in.PublicTool(in.Request.Tools[0].Functions[0].Name).Custom {
		t.Fatal("tool type was lost from identity or explicit selection")
	}
}
