package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
	"github.com/zclkkk/agy2api/internal/state"
)

var filesRead = ToolName{Namespace: "files", Name: "read"}

func namespaceTool(namespace, name string) map[string]any {
	return map[string]any{"type": "namespace", "name": namespace, "tools": []any{
		map[string]any{"type": "function", "name": name, "parameters": map[string]string{"type": "object"}},
	}}
}

// issueNamespacedCall has the bridge emit a files/read call with a signature
// and returns the call item a client receives.
func issueNamespacedCall(t *testing.T, s *state.Store) ResponseItem {
	t.Helper()
	ctx := context.Background()
	in, err := (testPipeline{State: s}).Responses(ctx, raw(map[string]any{"model": "gemini-test", "input": "read a.txt", "store": false, "tools": []any{namespaceTool("files", "read")}}))
	if err != nil {
		t.Fatal(err)
	}
	r := NewRenderer(in, s, nil)
	if err := r.Add(ctx, ag.Chunk{Parts: []ag.Part{{FunctionCall: &ag.Call{Name: toolSymbol(filesRead), Args: raw(map[string]string{"path": "a.txt"})}, Signature: "sig-read"}}, Finish: "STOP"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	var out struct {
		Output []ResponseItem `json:"output"`
	}
	if err := json.Unmarshal(r.Result, &out); err != nil {
		t.Fatal(err)
	}
	if call := out.Output[0]; call.Namespace != "files" || call.Name != "read" {
		t.Fatalf("unexpected call item: %+v", call)
	}
	return out.Output[0]
}

func replayCall(s *state.Store, model string, call ResponseItem, tools []any) (PreparedRequest, error) {
	body := map[string]any{"model": model, "store": false, "input": []any{
		map[string]string{"role": "user", "content": "read a.txt"}, call,
		map[string]string{"type": "function_call_output", "call_id": call.CallID, "output": "contents"},
	}}
	if tools != nil {
		body["tools"] = tools
	}
	return (testPipeline{State: s}).Responses(context.Background(), raw(body))
}

func warned(in PreparedRequest, text string) bool {
	for _, w := range in.Warnings {
		if strings.Contains(w, text) {
			return true
		}
	}
	return false
}

func TestHistoricalCallMissingNamespaceIsCompleted(t *testing.T) {
	s := testStore(t)
	call := issueNamespacedCall(t, s)
	native := toolSymbol(filesRead)
	call.Namespace = ""
	for name, tools := range map[string][]any{"declared": {namespaceTool("files", "read")}, "history only": nil} {
		in, err := replayCall(s, "gemini-test", call, tools)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		part := in.Request.Contents[1].Parts[0]
		if part.FunctionCall.Name != native || part.Signature != "sig-read" || in.PublicTool(native) != filesRead {
			t.Fatalf("%s: identity or signature not restored from the record: %+v", name, part)
		}
		if result := in.Request.Contents[2].Parts[0].FunctionResponse; result.Name != native {
			t.Fatalf("%s: result not paired with the completed call: %+v", name, result)
		}
		if !warned(in, "from replay state") {
			t.Fatalf("%s: warnings %v", name, in.Warnings)
		}
	}
}

func TestCachedOrdinaryToolIdentityPrecedesCurrentDeclarations(t *testing.T) {
	s := testStore(t)
	args := raw(map[string]string{"path": "a.txt"})
	identity := ToolName{Name: "read"}
	if err := s.Put(context.Background(), "tool", "call_agy_root", SignatureRecord{
		Model: "gemini-test", Name: "read", Tool: identity,
		Digest: digest(args), Signature: "sig-root",
	}); err != nil {
		t.Fatal(err)
	}
	call := ResponseItem{Type: "function_call", CallID: "call_agy_root", Name: "read", Arguments: string(args)}
	for _, model := range []string{"gemini-test", "gemini-other"} {
		in, err := replayCall(s, model, call, []any{namespaceTool("files", "read")})
		if err != nil {
			t.Fatal(err)
		}
		part := in.Request.Contents[1].Parts[0]
		if part.FunctionCall.Name != "read" || in.PublicTool("read") != identity {
			t.Fatal("a known ordinary tool was reinterpreted through current declarations")
		}
		wantSignature := "sig-root"
		if model != "gemini-test" {
			wantSignature = SkipSignature
		}
		if part.Signature != wantSignature || warned(in, "filled the missing namespace") {
			t.Fatalf("ordinary identity or signature handling changed: %v", in.Warnings)
		}
		if result := in.Request.Contents[2].Parts[0].FunctionResponse; result.Name != "read" {
			t.Fatal("ordinary tool result was renamed")
		}
	}
}

func TestEmptyAndOmittedNamespaceUseTheSameResolution(t *testing.T) {
	s := testStore(t)
	call := issueNamespacedCall(t, s)
	for _, explicitEmpty := range []bool{false, true} {
		item := map[string]any{"type": "function_call", "call_id": call.CallID, "name": call.Name, "arguments": call.Arguments}
		if explicitEmpty {
			item["namespace"] = ""
		}
		body := map[string]any{"model": "gemini-test", "store": false,
			"input": []any{map[string]string{"role": "user", "content": "read a.txt"}, item,
				map[string]string{"type": "function_call_output", "call_id": call.CallID, "output": "contents"}},
			// The record takes precedence even when a current ordinary tool has
			// the same local name as the historical namespace child.
			"tools": []any{map[string]any{"type": "function", "name": "read", "parameters": map[string]string{"type": "object"}}},
		}
		in, err := (testPipeline{State: s}).Responses(context.Background(), raw(body))
		if err != nil {
			t.Fatal(err)
		}
		part := in.Request.Contents[1].Parts[0]
		if part.FunctionCall.Name != toolSymbol(filesRead) || part.Signature != "sig-read" {
			t.Fatalf("explicit empty=%v: namespace was not resolved from state", explicitEmpty)
		}
	}
}

func TestHistoricalNamespaceFromDeclarationsWhenRecordIsGone(t *testing.T) {
	call := issueNamespacedCall(t, testStore(t))
	call.Namespace = ""
	fresh := testStore(t) // the replay record is missing or expired
	in, err := replayCall(fresh, "gemini-test", call, []any{namespaceTool("files", "read")})
	if err != nil {
		t.Fatal(err)
	}
	part := in.Request.Contents[1].Parts[0]
	if part.FunctionCall.Name != toolSymbol(filesRead) || part.Signature != SkipSignature || !warned(in, "from declared tools") {
		t.Fatalf("expected the declared identity with a placeholder signature: %+v %v", part, in.Warnings)
	}
	// With several candidates the namespace is not guessed.
	in, err = replayCall(fresh, "gemini-test", call, []any{namespaceTool("files", "read"), namespaceTool("docs", "read")})
	if err != nil {
		t.Fatal(err)
	}
	if name := in.Request.Contents[1].Parts[0].FunctionCall.Name; name != "read" || !warned(in, "ambiguous") {
		t.Fatalf("ambiguous namespace was guessed: %s %v", name, in.Warnings)
	}
	// With no usable record, an exact ordinary declaration wins over aliases.
	in, err = replayCall(fresh, "gemini-test", call, []any{
		namespaceTool("files", "read"),
		map[string]any{"type": "function", "name": "read", "parameters": map[string]string{"type": "object"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if part := in.Request.Contents[1].Parts[0]; part.FunctionCall.Name != "read" || part.Signature != SkipSignature || warned(in, "from declared tools") {
		t.Fatal("ordinary declaration lost to a namespaced alias")
	}
}

func TestEditedHistoricalCallDeclinesSignatureWithoutFailing(t *testing.T) {
	s := testStore(t)
	call := issueNamespacedCall(t, s)
	call.Namespace = ""
	tools := []any{namespaceTool("files", "read")}
	edited := call
	edited.Arguments = `{"path":"b.txt"}`
	renamed := call
	renamed.Name = "write"
	moved := call
	moved.Namespace = "docs"
	for name, item := range map[string]ResponseItem{"arguments": edited, "name": renamed, "namespace": moved} {
		in, err := replayCall(s, "gemini-test", item, tools)
		if err != nil {
			t.Fatalf("%s: edited history must not be rejected: %v", name, err)
		}
		if sig := in.Request.Contents[1].Parts[0].Signature; sig == "sig-read" {
			t.Fatalf("%s: signature reused for an edited call", name)
		}
		if name == "namespace" && in.Request.Contents[1].Parts[0].FunctionCall.Name != toolSymbol(ToolName{Namespace: "docs", Name: "read"}) {
			t.Fatal("explicit nonempty namespace was replaced")
		}
	}
	// The record never fills a field the client sent with a different value.
	in, _ := replayCall(s, "gemini-test", renamed, tools)
	if got := in.Request.Contents[1].Parts[0].FunctionCall.Name; got != "write" {
		t.Fatalf("client-sent name replaced: %s", got)
	}
}

func TestCompletedHistoricalIdentityStillCollides(t *testing.T) {
	s := testStore(t)
	call := issueNamespacedCall(t, s)
	call.Namespace = ""
	// A current ordinary function literally named like the completed call.
	_, err := replayCall(s, "gemini-test", call, []any{map[string]any{"type": "function", "name": toolSymbol(filesRead), "parameters": map[string]string{"type": "object"}}})
	var validation *Error
	if !errors.As(err, &validation) || validation.Param != "input" {
		t.Fatalf("collision after completing the identity was accepted: %v", err)
	}
}

func TestCompletedIdentityAfterModelChangeKeepsSignatureBound(t *testing.T) {
	s := testStore(t)
	call := issueNamespacedCall(t, s)
	call.Namespace = ""
	in, err := replayCall(s, "gemini-other", call, []any{namespaceTool("files", "read")})
	if err != nil {
		t.Fatal(err)
	}
	part := in.Request.Contents[1].Parts[0]
	if part.FunctionCall.Name != toolSymbol(filesRead) || part.Signature == "sig-read" {
		t.Fatalf("identity should be completed but the signature stays with its model: %+v", part)
	}
}
