package protocol

import (
	"context"
	"errors"
	"strings"
	"testing"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

func TestFinishReasonsAndUnknownParts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	run := func(chunk ag.Chunk) (*Renderer, error) {
		r := NewRenderer(PreparedRequest{Format: Messages, Model: "gemini-test"}, s, nil)
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
	if err != nil || !strings.Contains(string(r.Result), `"stop_reason":"end_turn"`) || !warned(r.Warnings, "finish reason SOMETHING_NEW") {
		t.Fatalf("unknown finish should be a stop with a warning: %v %s %v", err, r.Result, r.Warnings)
	}
	r, err = run(ag.Chunk{Parts: []ag.Part{{Unknown: []string{"newThing"}}, {Text: "kept", Unknown: []string{"meta"}}}, Finish: "STOP"})
	if err != nil || !strings.Contains(string(r.Result), "kept") || !warned(r.Warnings, "fields: newThing") || !warned(r.Warnings, "fields: meta") {
		t.Fatalf("%v %s %v", err, r.Result, r.Warnings)
	}
	r, err = run(ag.Chunk{Finish: "STOP"})
	if err != nil || !strings.Contains(string(r.Result), `"content":[]`) {
		t.Fatalf("empty completion should succeed: %v %s", err, r.Result)
	}
}
