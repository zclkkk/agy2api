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
		r := NewRenderer(PreparedRequest{requestFields{Format: Messages, Model: "gemini-test"}}, s, nil)
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

// Responses and Messages produce text, reasoning and tool calls; media output
// fails the turn before any of its chunk is emitted.
func TestMediaOutputIsAnError(t *testing.T) {
	for _, part := range []ag.Part{{InlineData: &ag.Blob{MIMEType: "image/png", Data: "aGVsbG8="}}, {FileData: &ag.File{URI: "https://example.com/a.png"}}} {
		var events []string
		r := NewRenderer(PreparedRequest{requestFields{Format: Responses, Model: "gemini-test", Stream: true}}, testStore(t), func(name string, _ map[string]any) error {
			events = append(events, name)
			return nil
		})
		err := r.Add(context.Background(), ag.Chunk{Parts: []ag.Part{{Text: "caption"}, part}, Finish: "STOP"})
		if err == nil || !strings.Contains(err.Error(), "media output") || len(events) != 0 {
			t.Fatalf("media output: %v, events %v", err, events)
		}
	}
}
