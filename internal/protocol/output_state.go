package protocol

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

func (r *Renderer) recordBlock(ctx context.Context, b *outputBlock) error {
	// Stream text deltas form one signed native part, not separate historical turns.
	if b.Kind == "text" || b.Kind == "thinking" {
		b.Parts = []ag.Part{{Text: b.Text, Thought: b.Kind == "thinking", Signature: b.Signature}}
	}
	if b.Kind == "tool" {
		if err := r.state.Put(ctx, "tool", b.CallID, SignatureRecord{Model: r.model(), Name: b.Name, Digest: digest(b.Args), Signature: b.Signature}); err != nil {
			return err
		}
		b.Parts = []ag.Part{{FunctionCall: &ag.Call{ID: b.CallID, Name: b.Name, Args: b.Args}, Signature: b.Signature}}
	}
	if b.Kind == "image" {
		if b.Blob == nil || !strings.HasPrefix(b.Blob.MIMEType, "image/") {
			return errors.New("upstream returned a non-image media output")
		}
		if _, err := base64.StdEncoding.DecodeString(b.Blob.Data); err != nil {
			return errors.New("upstream returned invalid image data")
		}
		if err := r.state.Put(ctx, "image", b.ID, ImageRecord{Model: r.model(), MIMEType: b.Blob.MIMEType, Digest: digest(raw(b.Blob.Data)), Signature: b.Signature}); err != nil {
			return err
		}
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
	if b.Kind == "text" {
		if r.input.Format == Messages {
			citations, err := r.messageCitations(b.Text)
			if err != nil {
				return err
			}
			b.Citations = citations
		} else {
			b.Annotations = r.annotations(b.Text)
			if b.Signature != "" {
				sealed, err := (replayStore{r.state}).seal(r.model(), Replay{PreviousText: digest(raw(b.Text)), Signature: b.Signature})
				if err != nil {
					return err
				}
				b.Sealed = sealed
			}
		}
	}
	return nil
}
func (r *Renderer) recordResponse(ctx context.Context, contents []ag.Content) error {
	if r.input.Format != Responses || !r.input.Store {
		return nil
	}
	return r.state.Put(ctx, "response", r.id, StoredResponse{Version: recordVersion, Model: r.input.Model, UpstreamModel: r.model(), HistorySystem: r.input.HistorySystem,
		Contents: contents, Response: r.Result, Items: append([]json.RawMessage(nil), r.input.OriginalItems...)})
}
