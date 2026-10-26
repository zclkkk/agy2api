package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

func TestReadableToolSymbols(t *testing.T) {
	for _, tc := range []struct {
		tool ToolName
		want string
	}{
		{ToolName{Name: "Bash"}, "Bash"},
		{ToolName{Name: "exec_command"}, "exec_command"},
		{ToolName{Name: "agy_read"}, "agy_read"},
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

func TestToolIdentityCheckedAcrossHistoryAndDeclarations(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	d := testPipeline{State: s}
	custom := ToolName{Name: "apply_patch", Custom: true}
	native := toolSymbol(custom)
	in, err := d.Responses(ctx, []byte(`{"model":"gemini-test","input":"patch","tools":[{"type":"custom","name":"apply_patch"},{"type":"function","name":"unused"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	r := NewRenderer(in, s, nil)
	if err := r.Add(ctx, ag.Chunk{Parts: []ag.Part{{FunctionCall: &ag.Call{Name: native, Args: raw(map[string]string{"input": "patch"})}, Signature: "fixture-signature"}}, Finish: "STOP"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	var output struct {
		Output []ResponseItem `json:"output"`
	}
	if err := json.Unmarshal(r.Result, &output); err != nil {
		t.Fatal(err)
	}
	call := output.Output[0]
	result := map[string]any{"type": "custom_tool_call_output", "call_id": call.CallID, "output": "done"}
	stored, err := LoadResponse(ctx, s, r.id)
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
		next, err := d.Responses(ctx, raw(body))
		if err != nil {
			t.Fatal(err)
		}
		part := next.Request.Contents[1].Parts[0]
		if part.Signature != "fixture-signature" || part.FunctionCall.Name != native || next.PublicTool(native) != custom {
			t.Fatal("tool identity or signature lost on replay")
		}
		// Historical tools must not become callable through tool_choice alone.
		body["tool_choice"] = map[string]string{"type": "custom", "name": "apply_patch"}
		if _, err := d.Responses(ctx, raw(body)); err == nil {
			t.Fatal("historical tool treated as a current declaration")
		}
		delete(body, "tool_choice")
		body["tools"] = []any{map[string]string{"type": "function", "name": native}}
		if _, err := d.Responses(ctx, raw(body)); err == nil {
			t.Fatal("historical custom tool aliased a current ordinary function")
		}
	}
	// Reusing a call ID with a different identity must not reuse the signature,
	// even when the native name and argument bytes match: the short hash is not
	// an identity check. The call is treated as edited history, not rejected.
	forged := ResponseItem{Type: "function_call", CallID: call.CallID, Name: native, Arguments: `{"input":"patch"}`}
	next, err := d.Responses(ctx, raw(map[string]any{"model": "gemini-test", "input": []any{map[string]string{"role": "user", "content": "patch"}, forged, result}}))
	if err != nil {
		t.Fatal(err)
	}
	if sig := next.Request.Contents[1].Parts[0].Signature; sig == "fixture-signature" {
		t.Fatal("replay record signature reused for a different tool identity")
	}
}
