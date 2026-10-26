package protocol

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
	"github.com/zclkkk/agy2api/internal/state"
)

// Messages has no signature field on text blocks. A trailing synthetic thinking
// block makes Claude Code's final result empty, so text signatures use bounded
// local state instead. Keys bind the model, session, system, preceding native history,
// and exact text; equal text in a different turn does not share a signature.
type textSignatureIndex struct{ prefix hash.Hash }

func newTextSignatureIndex(model, session string, system *ag.Content) *textSignatureIndex {
	h := sha256.New()
	_, _ = h.Write([]byte("agy2api-text-signature-v1\x00" + model + "\x00" + session + "\x00"))
	_, _ = h.Write([]byte(digest(raw(system))))
	return &textSignatureIndex{prefix: h}
}

func (x *textSignatureIndex) begin(role string) { _, _ = x.prefix.Write([]byte(digest(raw(role)))) }
func (x *textSignatureIndex) add(part ag.Part)  { _, _ = x.prefix.Write([]byte(digest(raw(part)))) }
func (x *textSignatureIndex) key(text string) string {
	return digest(raw([2]string{hex.EncodeToString(x.prefix.Sum(nil)), text}))
}

// restoreTextSignatures runs on Messages generation requests after routing and
// session assignment. Existing client-carried signatures are preserved. Every
// reply text is recorded, signed or not, so a missing record means the history
// before it changed or the state expired; such text is sent unsigned and the
// count is reported.
func (d replayStore) restoreTextSignatures(ctx context.Context, in *requestPlan) error {
	index := newTextSignatureIndex(in.Model, in.Request.SessionID, in.Request.System)
	missing := 0
	for i := range in.Request.Contents {
		content := &in.Request.Contents[i]
		index.begin(content.Role)
		for j := range content.Parts {
			part := &content.Parts[j]
			if content.Role == "model" && part.Text != "" && !part.Thought && part.Signature == "" && part.FunctionCall == nil && part.FunctionResponse == nil && part.InlineData == nil && part.FileData == nil {
				var signature string
				err := d.State.Get(ctx, "text_signature", index.key(part.Text), &signature)
				switch {
				case err == nil:
					part.Signature = signature
				case errors.Is(err, state.ErrNotFound):
					missing++
				default:
					return err
				}
			}
			index.add(*part)
		}
	}
	if missing > 0 {
		in.warn("no text signature record for %d earlier assistant text part(s); sent unsigned", missing)
	}
	return nil
}
