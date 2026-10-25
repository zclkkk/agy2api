package protocol

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
	"github.com/zclkkk/agy2api/internal/state"
)

func testStore(t *testing.T) *state.Store {
	t.Helper()
	s, err := state.Open(t.TempDir(), time.Hour, 1000, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestResponsesFullOutputReplay(t *testing.T) {
	s := testStore(t)
	d := Decoder{State: s}
	ctx := context.Background()
	in, err := d.Responses(ctx, []byte(`{"model":"gemini-test","input":"hello","store":false}`))
	if err != nil {
		t.Fatal(err)
	}
	r := NewRenderer("responses", in, s, nil)
	for _, chunk := range []ag.Chunk{
		{Parts: []ag.Part{{Thought: true, Text: "think "}}},
		{Parts: []ag.Part{{Thought: true, Text: "more", Signature: "thought-signature"}}},
		{Parts: []ag.Part{{Text: "answer", Signature: "text-signature"}}},
		{Parts: []ag.Part{{FunctionCall: &ag.Call{ID: "native-call", Name: "lookup", Args: raw(map[string]string{"title": "keep", "format": "unchanged"})}, Signature: "tool-signature"}}, Finish: "STOP"},
	} {
		if err := r.Add(ctx, chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	var response struct {
		Output []json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(r.Result, &response); err != nil {
		t.Fatal(err)
	}
	input := []json.RawMessage{raw(map[string]any{"role": "user", "content": "hello"})}
	input = append(input, response.Output...)
	var tool ResponseItem
	_ = json.Unmarshal(response.Output[len(response.Output)-1], &tool)
	input = append(input, raw(map[string]any{"type": "function_call_output", "call_id": tool.CallID, "output": "found"}))
	next, err := d.Responses(ctx, raw(map[string]any{"model": "gemini-test", "input": input, "store": false}))
	if err != nil {
		t.Fatal(err)
	}
	native, err := d.NativeRequest(ctx, next.Request, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(native.Contents) != 3 {
		t.Fatalf("contents=%s", raw(native.Contents))
	}
	parts := native.Contents[1].Parts
	if len(parts) != 3 || parts[0].Text != "think more" || parts[0].Signature != "thought-signature" || parts[1].Signature != "text-signature" || parts[2].Signature != "tool-signature" {
		t.Fatalf("lost replay data: %s", raw(parts))
	}
	if parts[2].FunctionCall.ID != "native-call" || native.Contents[2].Parts[0].FunctionResponse.ID != "native-call" {
		t.Fatal("native tool IDs were not restored")
	}
	if !strings.Contains(string(parts[2].FunctionCall.Args), `"title":"keep"`) {
		t.Fatal("tool data modified")
	}
	var stored StoredResponse
	if err := s.Get(ctx, "response", r.ID, &stored); err == nil {
		t.Fatal("store:false persisted a response")
	}
}

func TestMessagesFullOutputReplay(t *testing.T) {
	s := testStore(t)
	d := Decoder{State: s}
	ctx := context.Background()
	in, err := d.Messages(ctx, []byte(`{"model":"claude-test","max_tokens":2048,"messages":[{"role":"user","content":"hello"}]}`), false)
	if err != nil {
		t.Fatal(err)
	}
	r := NewRenderer("messages", in, s, nil)
	if err := r.Add(ctx, ag.Chunk{Parts: []ag.Part{{Text: "thinking", Thought: true}, {Signature: "opaque-thought"}, {Text: "calling"}, {FunctionCall: &ag.Call{ID: "native", Name: "lookup", Args: raw(map[string]int{"x": 1})}, Signature: "opaque-tool"}}, Finish: "STOP"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	var output struct {
		Content []Block `json:"content"`
		Stop    string  `json:"stop_reason"`
	}
	_ = json.Unmarshal(r.Result, &output)
	if output.Stop != "tool_use" {
		t.Fatal(output.Stop)
	}
	callID := output.Content[len(output.Content)-1].ID
	next, err := d.Messages(ctx, raw(map[string]any{"model": "claude-test", "max_tokens": 2048, "messages": []any{map[string]any{"role": "user", "content": "hello"}, map[string]any{"role": "assistant", "content": output.Content}, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": callID, "content": "done"}}}}}), false)
	if err != nil {
		t.Fatal(err)
	}
	native, err := d.NativeRequest(ctx, next.Request, true)
	if err != nil {
		t.Fatal(err)
	}
	if native.Contents[1].Parts[0].Signature != "opaque-thought" {
		t.Fatalf("lost signature: %s", raw(native))
	}
	if native.Contents[2].Parts[0].FunctionResponse.ID != "native" {
		t.Fatal("native ID lost")
	}
}

func TestUnsupportedInputIsRejectedWithoutDroppingAttachment(t *testing.T) {
	d := Decoder{State: testStore(t)}
	for _, request := range []string{
		`{"model":"gemini-test","input":"hello","background":true}`,
		`{"model":"gemini-test","input":[{"role":"user","content":[{"type":"input_text","text":"hello"},{"type":"input_image","file_id":"missing"}]}]}`,
		`{"model":"gemini-test","input":"hello","conversation":"conv_1"}`,
		`{"model":"gemini-test","input":[{"type":"item_reference","id":"msg_1"}]}`,
	} {
		if _, err := d.Responses(context.Background(), []byte(request)); err == nil {
			t.Fatalf("accepted unsupported input: %s", request)
		}
	}
}

func TestStoredResponsesDoNotInheritInstructionsOrIncludeOwnOutputAsInput(t *testing.T) {
	s := testStore(t)
	d := Decoder{State: s}
	ctx := context.Background()
	in, err := d.Responses(ctx, []byte(`{"model":"gemini-test","input":"first","instructions":"old instructions"}`))
	if err != nil {
		t.Fatal(err)
	}
	r := NewRenderer("responses", in, s, nil)
	if err := r.Add(ctx, ag.Chunk{Parts: []ag.Part{{Text: "answer"}}, Finish: "STOP"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	var stored StoredResponse
	if err := s.Get(ctx, "response", r.ID, &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.Items) != 1 {
		t.Fatalf("input_items includes own output: %s", raw(stored.Items))
	}
	next, err := d.Responses(ctx, raw(map[string]any{"model": "gemini-test", "input": "second", "previous_response_id": r.ID}))
	if err != nil {
		t.Fatal(err)
	}
	if next.Request.System != nil {
		t.Fatal("top-level instructions inherited")
	}
	if len(next.Request.Contents) != 3 || len(next.OriginalItems) != 3 {
		t.Fatalf("context lost: %s", raw(next))
	}
}

func TestGeneratedImageReplayAndUnicodeCitations(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	d := Decoder{State: s}
	in, err := d.Responses(ctx, []byte(`{"model":"gemini-image","input":"draw","store":false}`))
	if err != nil {
		t.Fatal(err)
	}
	r := NewRenderer("responses", in, s, nil)
	if err := r.Add(ctx, ag.Chunk{Parts: []ag.Part{{InlineData: &ag.Blob{MIMEType: "image/png", Data: "aGVsbG8="}, Signature: "image-signature"}}, Finish: "STOP"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	var output struct {
		Output []json.RawMessage `json:"output"`
	}
	_ = json.Unmarshal(r.Result, &output)
	items := []json.RawMessage{raw(map[string]string{"role": "user", "content": "draw"})}
	items = append(items, output.Output...)
	items = append(items, raw(map[string]string{"role": "user", "content": "edit"}))
	next, err := d.Responses(ctx, raw(map[string]any{"model": "gemini-image", "input": items}))
	if err != nil {
		t.Fatal(err)
	}
	if next.Request.Contents[1].Parts[0].Signature != "image-signature" {
		t.Fatal("image signature lost")
	}
	var grounding ag.Grounding
	if err := json.Unmarshal([]byte(`{"groundingChunks":[{"web":{"uri":"https://example.com/","title":"Source"}}],"groundingSupports":[{"segment":{"startIndex":0,"endIndex":6,"text":"你好"},"groundingChunkIndices":[0]}]}`), &grounding); err != nil {
		t.Fatal(err)
	}
	r = NewRenderer("responses", Input{Model: "gemini-test"}, s, nil)
	r.grounding = &grounding
	annotations := r.annotations("你好 world")
	if len(annotations) != 1 {
		t.Fatal(annotations)
	}
	if annotations[0].(map[string]any)["end_index"] != 2 {
		t.Fatal("byte offsets exposed as character indices")
	}
	citations, err := r.messageCitations("你好 world")
	if err != nil {
		t.Fatal(err)
	}
	if citations[0].(map[string]any)["cited_text"] != "你好" {
		t.Fatal(citations)
	}
}

func TestParallelResultsAreReorderedWithoutMutatingCallerHistory(t *testing.T) {
	d := Decoder{State: testStore(t)}
	request := ag.Request{Contents: []ag.Content{
		{Role: "user", Parts: []ag.Part{{Text: "start"}}},
		{Role: "model", Parts: []ag.Part{{FunctionCall: &ag.Call{ID: "a", Name: "first", Args: raw(map[string]any{})}}, {FunctionCall: &ag.Call{ID: "b", Name: "second", Args: raw(map[string]any{})}}}},
		{Role: "user", Parts: []ag.Part{{FunctionResponse: &ag.Result{ID: "b", Name: "second", Response: raw(map[string]string{"title": "untouched"})}}, {Text: "keep this text"}, {FunctionResponse: &ag.Result{ID: "a", Name: "first", Response: raw(map[string]string{"format": "untouched"})}}}},
	}}
	native, err := d.NativeRequest(context.Background(), request, true)
	if err != nil {
		t.Fatal(err)
	}
	parts := native.Contents[2].Parts
	if parts[0].FunctionResponse.ID != "a" || parts[2].FunctionResponse.ID != "b" || parts[1].Text != "keep this text" {
		t.Fatalf("bad ordering: %s", raw(parts))
	}
	if request.Contents[2].Parts[0].FunctionResponse.ID != "b" {
		t.Fatal("caller history was mutated")
	}
	if string(parts[2].FunctionResponse.Response) != `{"title":"untouched"}` {
		t.Fatal("result data changed")
	}
}

func TestClaudeResponsesEffortUsesBudget(t *testing.T) {
	d := Decoder{State: testStore(t)}
	in, err := d.Responses(context.Background(), []byte(`{"model":"claude-sonnet-4-6","input":"think","reasoning":{"effort":"medium","summary":"auto"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := in.Prepare(testCatalog); err != nil {
		t.Fatal(err)
	}
	g := in.Request.Generation
	if g.Thinking == nil || g.Thinking.Budget == nil || *g.Thinking.Budget != 8192 || g.Thinking.Level != "" || !g.Thinking.Include {
		t.Fatalf("incorrect Claude thinking config: %+v", g.Thinking)
	}
	if g.MaxTokens == nil || *g.MaxTokens != 64000 {
		t.Fatal("advertised output limit not applied")
	}
	// A budget that cannot fit is reduced, and below Claude's minimum thinking
	// is turned off, instead of failing the request.
	in, err = d.Responses(context.Background(), []byte(`{"model":"claude-sonnet-4-6","input":"think","max_output_tokens":1024,"reasoning":{"effort":"high"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := in.Prepare(testCatalog); err != nil {
		t.Fatal(err)
	}
	if b := in.Request.Generation.Thinking.Budget; b == nil || *b != 0 || len(in.Warnings) == 0 {
		t.Fatalf("expected thinking off with a warning: %+v %v", in.Request.Generation.Thinking, in.Warnings)
	}
	in, _ = d.Responses(context.Background(), []byte(`{"model":"claude-sonnet-4-6","input":"think","max_output_tokens":4000,"reasoning":{"effort":"high"}}`))
	if err := in.Prepare(testCatalog); err != nil {
		t.Fatal(err)
	}
	if b := in.Request.Generation.Thinking.Budget; b == nil || *b != 3999 {
		t.Fatalf("budget not reduced to fit: %+v", in.Request.Generation.Thinking)
	}
}
