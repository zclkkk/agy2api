package protocol

import (
	"strings"
	"unicode/utf8"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

const billingMarkerPrefix = "x-anthropic-billing-header:"

// SanitizeClientMarkers removes known client preamble lines from the normalized
// system instruction. Original request data, conversation history, tools, and
// signed parts are never edited. Counts identify rules without logging text.
func (in *Input) SanitizeClientMarkers() {
	if in.Request.System == nil {
		return
	}
	cleaner := clientMarkerCleaner{}
	parts := make([]ag.Part, 0, len(in.Request.System.Parts))
	for _, part := range in.Request.System.Parts {
		if part.Text == "" || part.Thought || part.Signature != "" || part.InlineData != nil || part.FileData != nil || part.FunctionCall != nil || part.FunctionResponse != nil {
			parts = append(parts, part)
			continue
		}
		text, changed := cleaner.clean(part.Text)
		if changed {
			part.Text = text
			if strings.TrimSpace(text) == "" {
				continue
			}
		}
		parts = append(parts, part)
	}
	if cleaner.billing == 0 && cleaner.identity == 0 {
		return
	}
	if len(parts) == 0 {
		in.Request.System = nil
	} else {
		instruction := *in.Request.System
		instruction.Parts = parts
		in.Request.System = &instruction
	}
	in.warn("sanitize_client_markers: billing_header=%d claude_identity=%d", cleaner.billing, cleaner.identity)
}

type clientMarkerCleaner struct {
	billing, identity int
	fence             byte
	fenceLength       int
}

// clean keeps surviving lines byte-for-byte, including their line endings.
// Fenced code examples are not client preambles; fence state spans system parts.
func (c *clientMarkerCleaner) clean(text string) (string, bool) {
	var out strings.Builder
	out.Grow(len(text))
	changed := false
	for rest := text; rest != ""; {
		end := strings.IndexAny(rest, "\r\n\u0085\u2028\u2029")
		if end < 0 {
			end = len(rest)
		}
		next := end
		if next < len(rest) {
			_, width := utf8.DecodeRuneInString(rest[next:])
			next += width
			if rest[end] == '\r' && next < len(rest) && rest[next] == '\n' {
				next++
			}
		}
		line := strings.TrimSpace(rest[:end])
		remove := false
		switch {
		case c.fenced(line):
		case strings.HasPrefix(line, billingMarkerPrefix):
			c.billing++
			remove = true
		case clientIdentityLine(line):
			c.identity++
			remove = true
		}
		if remove {
			changed = true
		} else {
			out.WriteString(rest[:next])
		}
		rest = rest[next:]
	}
	if !changed {
		return text, false
	}
	return out.String(), true
}

func clientIdentityLine(line string) bool {
	switch line {
	case "You are Claude Code, Anthropic's official CLI for Claude.",
		"You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK.",
		"You are a Claude agent, built on Anthropic's Claude Agent SDK.":
		return true
	}
	return false
}

func (c *clientMarkerCleaner) fenced(line string) bool {
	inside := c.fence != 0
	if len(line) < 3 || (line[0] != '`' && line[0] != '~') {
		return inside
	}
	n := 1
	for n < len(line) && line[n] == line[0] {
		n++
	}
	if inside {
		if line[0] == c.fence && n >= c.fenceLength && strings.TrimSpace(line[n:]) == "" {
			c.fence, c.fenceLength = 0, 0
		}
		return true
	}
	if n >= 3 {
		c.fence, c.fenceLength = line[0], n
		return true
	}
	return false
}
