package protocol

import (
	"context"
	"encoding/json"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

func (r *Renderer) recordBlock(ctx context.Context, b *outputBlock) error {
	// Stream text deltas form one signed native part, not separate historical turns.
	if b.Kind == "text" || b.Kind == "thinking" {
		b.Parts = []ag.Part{{Text: b.Text, Thought: b.Kind == "thinking", Signature: b.Signature}}
	}
	if b.Kind == "tool" {
		if err := r.state.Put(ctx, "tool", b.CallID, SignatureRecord{Model: r.model(), Name: b.Name, Tool: b.Public, Digest: digest(b.Args), Signature: b.Signature}); err != nil {
			return err
		}
		b.Parts = []ag.Part{{FunctionCall: &ag.Call{ID: b.CallID, Name: b.Name, Args: b.Args}, Signature: b.Signature}}
	}
	if r.textSignatures != nil {
		// Unsigned text is recorded too, so a later miss means lost state.
		if b.Kind == "text" {
			if err := r.state.Put(ctx, "text_signature", r.textSignatures.key(b.Text), b.Signature); err != nil {
				return err
			}
		}
		for _, part := range b.Parts {
			r.textSignatures.add(part)
		}
	}

	if b.Kind == "thinking" {
		sealed, err := (replayStore{r.state}).seal(r.model(), Replay{Parts: b.Parts})
		if err != nil {
			return err
		}
		b.Sealed = sealed
	}
	if b.Kind == "text" && r.input.Format == Responses && b.Signature != "" {
		sealed, err := (replayStore{r.state}).seal(r.model(), Replay{PreviousText: digest(raw(b.Text)), Signature: b.Signature})
		if err != nil {
			return err
		}
		b.Sealed = sealed
	}
	return nil
}
func (r *Renderer) recordResponse(ctx context.Context, contents []ag.Content) error {
	if r.input.Format != Responses || !r.input.Store {
		return nil
	}
	tools := map[string]ToolName{}
	for _, content := range contents {
		for _, part := range content.Parts {
			if call := part.FunctionCall; call != nil {
				tools[call.Name] = r.input.PublicTool(call.Name)
			}
		}
	}
	return r.state.Put(ctx, "response", r.id, StoredResponse{Version: recordVersion, Model: r.input.Model, HistorySystem: r.input.HistorySystem,
		Contents: contents, Tools: tools, Response: r.Result, Items: append([]json.RawMessage(nil), r.input.OriginalItems...)})
}
