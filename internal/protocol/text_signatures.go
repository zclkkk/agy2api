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

// RestoreTextSignatures runs after routing and session assignment. Existing
// client-carried signatures are preserved; a cache miss leaves text unchanged.
func (d Decoder) RestoreTextSignatures(ctx context.Context, in *Input) error {
	index := newTextSignatureIndex(in.UpstreamModel(), in.Request.SessionID, in.Request.System)
	for i := range in.Request.Contents {
		content := &in.Request.Contents[i]
		index.begin(content.Role)
		for j := range content.Parts {
			part := &content.Parts[j]
			if content.Role == "model" && part.Text != "" && !part.Thought && part.Signature == "" && part.FunctionCall == nil && part.FunctionResponse == nil && part.InlineData == nil && part.FileData == nil {
				var signature string
				err := d.State.Get(ctx, "text_signature", index.key(part.Text), &signature)
				if err == nil {
					part.Signature = signature
				} else if !errors.Is(err, state.ErrNotFound) {
					return err
				}
			}
			index.add(*part)
		}
	}
	return nil
}
