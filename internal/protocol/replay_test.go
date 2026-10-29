package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

func TestResponsesOutputReplaysWithItsSignatures(t *testing.T) {
	s := testStore(t)
	p := testPipeline{State: s}
	ctx := context.Background()
	in := p.mustResponses(t, `{"model":"gemini-test","input":"hello","store":false}`)
	r := NewRenderer(in, s, nil)
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
	output := responseOutput(t, r)
	var tool ResponseItem
	_ = json.Unmarshal(output[len(output)-1], &tool)
	input := append([]json.RawMessage{raw(map[string]any{"role": "user", "content": "hello"})}, output...)
	input = append(input, raw(map[string]any{"type": "function_call_output", "call_id": tool.CallID, "output": "found"}))
	native := p.mustResponses(t, map[string]any{"model": "gemini-test", "input": input, "store": false}).Request
	if len(native.Contents) != 3 {
		t.Fatalf("contents=%s", raw(native.Contents))
	}
	parts := native.Contents[1].Parts
	if len(parts) != 3 || parts[0].Text != "think more" || parts[0].Signature != "thought-signature" || parts[1].Signature != "text-signature" || parts[2].Signature != "tool-signature" {
		t.Fatalf("lost replay data: %s", raw(parts))
	}
	// Antigravity pairs calls and results by the IDs sent, so the issued ID is used upstream.
	if parts[2].FunctionCall.ID != tool.CallID || native.Contents[2].Parts[0].FunctionResponse.ID != tool.CallID {
		t.Fatal("call and result IDs do not match")
	}
	if !strings.Contains(string(parts[2].FunctionCall.Args), `"title":"keep"`) {
		t.Fatal("tool data modified")
	}
	if _, err := LoadResponse(ctx, s, r.id); err == nil {
		t.Fatal("store:false persisted a response")
	}
}

func TestMessagesOutputReplaysWithItsSignatures(t *testing.T) {
	s := testStore(t)
	p := testPipeline{State: s}
	in := p.mustMessages(t, messages(user("hello")))
	r := turn(t, s, in, ag.Part{Text: "thinking", Thought: true}, ag.Part{Signature: "opaque-thought"}, ag.Part{Text: "calling"},
		ag.Part{FunctionCall: &ag.Call{ID: "native", Name: "lookup", Args: raw(map[string]int{"x": 1})}, Signature: "opaque-tool"})
	if !strings.Contains(string(r.Result), `"stop_reason":"tool_use"`) {
		t.Fatal(string(r.Result))
	}
	content := messageContent(t, r)
	callID := content[len(content)-1].ID
	native := p.mustMessages(t, messages(user("hello"), assistant(content), user([]any{map[string]any{"type": "tool_result", "tool_use_id": callID, "content": "done"}}))).Request
	if native.Contents[1].Parts[0].Signature != "opaque-thought" {
		t.Fatalf("lost signature: %s", raw(native))
	}
	if native.Contents[2].Parts[0].FunctionResponse.ID != callID {
		t.Fatal("result no longer matches its call")
	}
}

// Messages thinking hidden with display=omitted reaches the client without
// its text, yet the next turn replays the original thinking.
func TestHiddenThinkingStillReplays(t *testing.T) {
	s := testStore(t)
	p := testPipeline{State: s}
	body := messages(user("hi"))
	body["thinking"] = map[string]string{"type": "adaptive", "display": "omitted"}
	in := p.mustMessages(t, body)
	var deltas []string
	r := NewRenderer(in, s, func(name string, v map[string]any) error {
		if delta, ok := v["delta"].(map[string]any); ok {
			kind, _ := delta["type"].(string)
			deltas = append(deltas, kind)
		}
		return nil
	})
	ctx := context.Background()
	if err := r.Add(ctx, ag.Chunk{Parts: []ag.Part{{Text: "secret plan", Thought: true, Signature: "sig"}, {Text: "answer"}}, Finish: "STOP"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(deltas, ","), "thinking_delta") || strings.Contains(string(r.Result), "secret plan") {
		t.Fatalf("thinking text exposed: %v %s", deltas, r.Result)
	}
	next := p.mustMessages(t, messages(user("hi"), assistant(messageContent(t, r)), user("more")))
	if part := next.Request.Contents[1].Parts[0]; part.Text != "secret plan" || part.Signature != "sig" {
		t.Fatalf("hidden thinking not replayed: %s", raw(next.Request.Contents[1]))
	}
}

// History that cannot be verified is dropped or sent with a placeholder, and
// each degradation is reported; the request itself still runs.
func TestUnverifiableReplayStateDegrades(t *testing.T) {
	s := testStore(t)
	p := testPipeline{State: s}
	foreign, _ := (replayStore{s}).seal("gemini-other", Replay{Parts: []ag.Part{{Text: "x", Thought: true, Signature: "s"}}})
	own, _ := (replayStore{s}).seal("gemini-test", Replay{Parts: []ag.Part{{Text: "real thought", Thought: true, Signature: "s"}}})
	in := p.mustMessages(t, messages(user("hi"),
		assistant([]any{
			// Not sealed by this instance, e.g. after the data directory was replaced.
			map[string]any{"type": "thinking", "thinking": "lost key", "signature": "agy2.EqQBCkYIBRgCKkB"},
			map[string]any{"type": "thinking", "thinking": "x", "signature": foreign},
			map[string]any{"type": "thinking", "thinking": "edited thought", "signature": own},
			map[string]any{"type": "tool_use", "id": "toolu_agy_expired", "name": "lookup", "input": map[string]any{}},
		}),
		user([]any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_agy_expired", "content": "ok"}})))
	model := in.Request.Contents[1].Parts
	if len(model) != 1 || model[0].FunctionCall == nil {
		t.Fatalf("unrestorable thinking should be dropped: %s", raw(model))
	}
	if model[0].FunctionCall.ID != "toolu_agy_expired" || model[0].Signature != SkipSignature {
		t.Fatalf("missing tool state should use the placeholder: %s", raw(model[0]))
	}
	if in.Request.Contents[2].Parts[0].FunctionResponse.ID != "toolu_agy_expired" {
		t.Fatal("result no longer matches its call")
	}
	if strings.Count(strings.Join(in.Warnings, "\n"), "dropped a reasoning item") != 2 ||
		!warned(in.Warnings, "belongs to a different upstream model") ||
		!warned(in.Warnings, "its text changed") || !warned(in.Warnings, "placeholder signature") {
		t.Fatalf("degradations not reported: %v", in.Warnings)
	}
}

func TestModelChangeInStoredChainDropsSignatures(t *testing.T) {
	s := testStore(t)
	p := testPipeline{State: s}
	in := p.mustResponses(t, `{"model":"gemini-other","input":"first"}`)
	r := turn(t, s, in, ag.Part{Text: "plan", Thought: true, Signature: "other-sig"}, ag.Part{Text: "answer"})
	next := p.mustResponses(t, map[string]any{"model": "gemini-test", "input": "second", "previous_response_id": r.id})
	for _, c := range next.Request.Contents {
		for _, part := range c.Parts {
			if part.Thought || part.Signature != "" {
				t.Fatalf("signature carried across models: %s", raw(next.Request.Contents))
			}
		}
	}
	if len(next.Request.Contents) != 3 || !warned(next.Warnings, "model changed") {
		t.Fatalf("context lost or change not reported: %s %v", raw(next.Request.Contents), next.Warnings)
	}
}

func TestReplayTokensAreSealedToModelAndInstance(t *testing.T) {
	s := testStore(t)
	store := replayStore{s}
	for _, replay := range []Replay{
		{Parts: []ag.Part{{Text: "plan", Thought: true, Signature: "sig"}}},
		{PreviousText: digest(raw("answer")), Signature: "sig"},
	} {
		token, err := store.seal("gemini-other", replay)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := store.open("gemini-other", token); err != nil || got.Version != recordVersion || got.Model != "gemini-other" {
			t.Fatalf("valid replay token rejected: %v", err)
		}
		if _, err := store.open("gemini-test", token); err == nil {
			t.Fatal("replay crossed upstream models")
		}
		if _, err := (replayStore{testStore(t)}).open("gemini-other", token); err == nil {
			t.Fatal("replay crossed instances")
		}
		mid := len(token) / 2
		replacement := "A"
		if token[mid] == 'A' {
			replacement = "B"
		}
		if _, err := store.open("gemini-other", token[:mid]+replacement+token[mid+1:]); err == nil {
			t.Fatal("modified replay accepted")
		}
	}
	for _, version := range []int{0, recordVersion + 1} {
		token, err := s.Seal("agy2api/replay/v2", Replay{Version: version, Model: "gemini-other"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.open("gemini-other", token); err == nil {
			t.Fatalf("replay record version %d accepted", version)
		}
	}
	token, err := s.Seal("unrelated-purpose", Replay{Version: recordVersion, Model: "gemini-other"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.open("gemini-other", token); err == nil {
		t.Fatal("replay from another authenticated scope accepted")
	}
}

func TestReplayStorageFailureIsNotReportedAsCacheMiss(t *testing.T) {
	s := testStore(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := (testPipeline{State: s}).Messages(`{"model":"gemini-test","max_tokens":100,"messages":[{"role":"user","content":"start"},{"role":"assistant","content":[{"type":"tool_use","id":"toolu_agy_missing","name":"lookup","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_agy_missing","content":"done"}]}]}`)
	var validation *Error
	if err == nil || errors.As(err, &validation) || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("storage failure hidden as a cache miss: %v", err)
	}
}

func TestParallelResultsFollowCallOrder(t *testing.T) {
	contents := []ag.Content{
		{Role: "user", Parts: []ag.Part{{Text: "start"}}},
		{Role: "model", Parts: []ag.Part{{FunctionCall: &ag.Call{ID: "a", Name: "first", Args: raw(map[string]any{})}}, {FunctionCall: &ag.Call{ID: "b", Name: "second", Args: raw(map[string]any{})}}}},
		{Role: "user", Parts: []ag.Part{{FunctionResponse: &ag.Result{ID: "b", Name: "second", Response: raw(map[string]string{"title": "untouched"})}}, {Text: "keep this text"}, {FunctionResponse: &ag.Result{ID: "a", Name: "first", Response: raw(map[string]string{"format": "untouched"})}}}},
	}
	if err := orderResults(contents); err != nil {
		t.Fatal(err)
	}
	parts := contents[2].Parts
	if parts[0].FunctionResponse.ID != "a" || parts[2].FunctionResponse.ID != "b" || parts[1].Text != "keep this text" {
		t.Fatalf("bad ordering: %s", raw(parts))
	}
	if string(parts[2].FunctionResponse.Response) != `{"title":"untouched"}` {
		t.Fatal("result data changed")
	}
	contents[2].Parts = append(contents[2].Parts, contents[2].Parts[0])
	if orderResults(contents) == nil {
		t.Fatal("a duplicate result must be rejected")
	}
}
