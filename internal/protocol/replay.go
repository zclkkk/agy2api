package protocol

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

// Gemini attaches thought signatures to function calls, generated images and
// text, and rejects a turn whose first function call or image arrives without
// one. Clients do not carry signatures on tool calls, so the bridge keeps them
// in local state keyed by the call IDs it issues; thinking and text signatures
// travel sealed inside the reasoning items returned to the client.

// SkipSignature is Antigravity's documented placeholder for a function call
// whose original thought signature is unavailable.
const SkipSignature = "skip_thought_signature_validator"

// SignatureRecord is the replay state of a function call this bridge issued.
type SignatureRecord struct {
	Model     string `json:"model"`
	Name      string `json:"name"`
	Digest    string `json:"digest"`
	Signature string `json:"signature"`
}

// ImageRecord is the replay state of a generated image.
type ImageRecord struct {
	Model     string
	MIMEType  string
	Digest    string
	Signature string
}

// Replay preserves the exact native signature placement, including signatures on
// non-thinking text. Visible text is checked before reattaching such signatures.
type Replay struct {
	Parts        []ag.Part `json:"parts,omitempty"`
	PreviousText string    `json:"previous_text,omitempty"`
	Signature    string    `json:"signature,omitempty"`
}

func agyCallID(id string) bool {
	return strings.HasPrefix(id, "call_agy_") || strings.HasPrefix(id, "toolu_agy_")
}

// restoreCall returns the original signature for a call this bridge issued.
// Missing, expired or mismatched state degrades to no signature; Finalize then
// supplies the placeholder Antigravity accepts.
func (d Decoder) restoreCall(ctx context.Context, in *Input, call *ag.Call) string {
	if !agyCallID(call.ID) {
		return ""
	}
	var r SignatureRecord
	if err := d.State.Get(ctx, "tool", call.ID, &r); err != nil {
		in.warn("tool replay state for %s is missing or expired; using a placeholder signature", call.ID)
		return ""
	}
	if r.Model != in.Model || r.Name != call.Name || r.Digest != digest(call.Args) {
		in.warn("tool call %s changed or came from another model; using a placeholder signature", call.ID)
		return ""
	}
	return r.Signature
}

// replay restores a sealed thinking carrier. A carrier for a signed text part
// signs the last part of prev in place and returns nil. Carriers that cannot be
// restored are dropped, which is how Anthropic treats prior-turn thinking too.
func (d Decoder) replay(in *Input, token, visible string, prev []ag.Part) []ag.Part {
	if !strings.HasPrefix(token, "agy1.") {
		in.warn("dropped a reasoning item not issued by agy2api")
		return nil
	}
	var r Replay
	if err := d.State.Unseal(in.Model, token, &r); err != nil {
		in.warn("dropped a reasoning item: %v", err)
		return nil
	}
	if r.PreviousText != "" {
		if len(prev) == 0 || digest(raw(prev[len(prev)-1].Text)) != r.PreviousText {
			in.warn("dropped a text signature because the signed text changed")
			return nil
		}
		prev[len(prev)-1].Signature = r.Signature
		return nil
	}
	if visible != "" {
		var text strings.Builder
		for _, p := range r.Parts {
			text.WriteString(p.Text)
		}
		if text.String() != visible {
			in.warn("dropped a thinking block because its text changed")
			return nil
		}
	}
	return r.Parts
}

// restoreImage rebuilds a generated image turn. Without replay metadata the
// image is still sent with its type detected; Finalize adds the placeholder
// signature.
func (d Decoder) restoreImage(ctx context.Context, in *Input, v ResponseItem) ag.Part {
	var record ImageRecord
	if err := d.State.Get(ctx, "image", v.ID, &record); err == nil && record.Model == in.Model && record.Digest == digest(raw(v.Result)) {
		return ag.Part{InlineData: &ag.Blob{MIMEType: record.MIMEType, Data: v.Result}, Signature: record.Signature}
	}
	in.warn("image replay metadata for %s is missing or changed", v.ID)
	head := v.Result[:min(len(v.Result), 1024)]
	head = head[:len(head)/4*4]
	sample, _ := base64.StdEncoding.DecodeString(head)
	return ag.Part{InlineData: &ag.Blob{MIMEType: http.DetectContentType(sample), Data: v.Result}}
}

// stripSignatures removes signatures and thoughts from history that will be
// sent to a different model; signatures are bound to the model that made them.
func stripSignatures(contents []ag.Content) []ag.Content {
	out := make([]ag.Content, 0, len(contents))
	for _, c := range contents {
		parts := make([]ag.Part, 0, len(c.Parts))
		for _, p := range c.Parts {
			if p.Thought {
				continue
			}
			p.Signature = ""
			parts = append(parts, p)
		}
		if len(parts) > 0 {
			out = append(out, ag.Content{Role: c.Role, Parts: parts})
		}
	}
	return out
}

// addPlaceholders signs the first unsigned function call and every unsigned
// generated image of each model turn with SkipSignature.
func addPlaceholders(contents []ag.Content) {
	for i := range contents {
		if contents[i].Role != "model" {
			continue
		}
		firstCall := true
		for j := range contents[i].Parts {
			p := &contents[i].Parts[j]
			switch {
			case p.FunctionCall != nil && firstCall:
				firstCall = false
				if p.Signature == "" {
					p.Signature = SkipSignature
				}
			case p.InlineData != nil && p.Signature == "":
				p.Signature = SkipSignature
			}
		}
	}
}
