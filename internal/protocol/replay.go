package protocol

import (
	"strings"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

// Gemini attaches thought signatures to function calls, generated images and
// text, and rejects a turn whose first function call or image arrives without
// one. Clients do not carry signatures on tool calls, so the bridge keeps them
// in local state keyed by the call IDs it issues. Thinking signatures travel
// sealed inside reasoning items. Responses text signatures use a sealed carrier;
// Messages text signatures use a history-scoped cache to keep text last.

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
	Version      int       `json:"version,omitempty"`
	Model        string    `json:"model,omitempty"`
	Kind         string    `json:"kind,omitempty"`
	Parts        []ag.Part `json:"parts,omitempty"`
	PreviousText string    `json:"previous_text,omitempty"`
	Signature    string    `json:"signature,omitempty"`
}

func agyCallID(id string) bool {
	return strings.HasPrefix(id, "call_agy_") || strings.HasPrefix(id, "toolu_agy_")
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
