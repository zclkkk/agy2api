package protocol

import (
	"context"
	"encoding/json"
	"testing"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

func TestStoredChainDropsReasoningWhenModelChanges(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	d := testPipeline{State: s}
	in, err := d.Responses(ctx, []byte(`{"model":"gemini-test","input":"first"}`))
	if err != nil {
		t.Fatal(err)
	}
	r := NewRenderer(in, s, nil)
	if err := r.Add(ctx, ag.Chunk{Parts: []ag.Part{{Text: "first-model thought", Thought: true, Signature: "first-model-signature"}, {Text: "answer"}}, Finish: "STOP"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	// A new model must not receive reasoning signed by the previous model.
	next, err := d.Responses(ctx, raw(map[string]any{"model": "gemini-other", "input": "continue", "previous_response_id": r.id}))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range next.Request.Contents {
		for _, p := range c.Parts {
			if p.Thought || p.Signature == "first-model-signature" {
				t.Fatalf("signed thought from %s is replayed to %s", in.Model, next.Model)
			}
		}
	}
}

func TestStoredChainPreservesSystemMessages(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	d := testPipeline{State: s}
	in, err := d.Responses(ctx, []byte(`{"model":"gemini-test","input":[{"role":"developer","content":"Always use JSON."},{"role":"user","content":"first"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	r := NewRenderer(in, s, nil)
	if err := r.Add(ctx, ag.Chunk{Parts: []ag.Part{{Text: `{"answer":"first"}`}}, Finish: "STOP"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	next, err := d.Responses(ctx, raw(map[string]any{"model": "gemini-test", "input": "next", "previous_response_id": r.id}))
	if err != nil {
		t.Fatal(err)
	}
	if next.Request.System == nil || len(next.Request.System.Parts) == 0 {
		t.Fatal("developer message stored in input history is not included in the next native request")
	}
}

func TestStoredChainCanContinueWithOnlyInstructionsOrCompaction(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	in, err := (testPipeline{State: s}).Responses(ctx, []byte(`{"model":"gemini-test","input":"first"}`))
	if err != nil {
		t.Fatal(err)
	}
	r := NewRenderer(in, s, nil)
	if err := r.Add(ctx, ag.Chunk{Parts: []ag.Part{{Text: "answer"}}, Finish: "STOP"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		item      map[string]string
		operation Operation
		last      string
	}{
		{"instructions", map[string]string{"role": "developer", "content": "Use JSON from now on."}, Generate, continuation},
		{"count instructions", map[string]string{"role": "developer", "content": "Use JSON from now on."}, CountTokens, continuation},
		{"compaction trigger", map[string]string{"type": "compaction_trigger"}, Generate, compactionPrompt},
		{"compaction endpoint", map[string]string{"role": "developer", "content": "Use JSON from now on."}, CompactResponse, compactionPrompt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			intent, err := (Decoder{}).Responses(raw(map[string]any{"model": "gemini-test", "previous_response_id": r.id, "input": []any{tc.item}}))
			if err != nil {
				t.Fatalf("decoder rejected input before loading its history: %v", err)
			}
			next, err := (Planner{State: s}).Prepare(ctx, intent, testCatalog, tc.operation, nil)
			if err != nil {
				t.Fatal(err)
			}
			contents := next.Request.Contents
			if len(contents) != 3 || contents[0].Parts[0].Text != "first" || contents[1].Parts[0].Text != "answer" || contents[2].Parts[0].Text != tc.last {
				t.Fatal("continuation did not retain and use the stored conversation")
			}
			if tc.item["role"] == "developer" && (next.Request.System == nil || next.Request.System.Parts[0].Text != tc.item["content"]) {
				t.Fatal("continuation lost the new developer instruction")
			}
		})
	}
}

func TestMessagesReplayKeepsThinkingAndTextSignatures(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	d := testPipeline{State: s}
	decode := func(messages []any) PreparedRequest {
		t.Helper()
		in, err := d.Messages(ctx, raw(map[string]any{"model": "gemini-test", "max_tokens": 100, "messages": messages}), false)
		if err != nil {
			t.Fatal(err)
		}
		return in
	}
	first := []any{map[string]string{"role": "user", "content": "start"}}
	in := decode(first)
	r := NewRenderer(in, s, nil)
	if err := r.Add(ctx, ag.Chunk{Parts: []ag.Part{{Text: "plan", Thought: true, Signature: "thought-signature"}, {Text: "answer", Signature: "text-signature"}}, Finish: "STOP"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	var response struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(r.Result, &response); err != nil {
		t.Fatal(err)
	}
	next := decode(append(first, map[string]any{"role": "assistant", "content": response.Content}, map[string]string{"role": "user", "content": "more"}))
	parts := next.Request.Contents[1].Parts
	if len(parts) != 2 || parts[0].Signature != "thought-signature" || parts[1].Signature != "text-signature" {
		t.Fatalf("replay lost thinking or text signatures: parts=%d warnings=%v", len(parts), next.Warnings)
	}
}
