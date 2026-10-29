package protocol

import (
	"net/http"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

// Gemini thinking levels; some models reject "minimal", so it maps to "low".
var thinkingLevels = map[string]string{"minimal": "low", "low": "low", "medium": "medium", "high": "high", "xhigh": "high", "max": "high"}

func knownEffort(effort string) bool { _, ok := thinkingLevels[effort]; return ok }

// applyEffort records a reasoning effort unless an explicit budget or a
// disabled state was requested. Whether thoughts are shown is unchanged.
func applyEffort(in *RequestIntent, value string) {
	switch {
	case value == "none":
		in.Thinking.Mode = "disabled"
	case !knownEffort(value):
		in.warn("ignored reasoning effort %q", value)
	case in.Thinking.Mode != "budget" && in.Thinking.Mode != "disabled":
		in.Thinking.Mode, in.Thinking.Effort = "effort", value
	}
}

// plan checks the model, converts thinking intent and rewrites schemas into
// the subset Antigravity accepts.
func (in *RequestIntent) plan(catalog ag.Catalog) error {
	info, ok := catalog.Info(in.Model)
	if !ok {
		return invalid("model", "model is not available; agy2api only serves Gemini models available to this account")
	}
	if info.Image {
		return invalid("model", "image generation models are not supported by the Responses or Messages endpoints")
	}
	if err := in.selectTools(); err != nil {
		return err
	}
	in.applyThinking(info)
	for i := range in.Request.Tools {
		for j := range in.Request.Tools[i].Functions {
			f := &in.Request.Tools[i].Functions[j]
			cleaned, err := ag.CleanToolSchema(f.Parameters)
			if err != nil {
				return invalid("tools", "tool "+in.PublicTool(f.Name).Name+" has an invalid JSON schema")
			}
			f.Parameters = cleaned
		}
	}
	if g := &in.Request.Generation; present(g.Schema) {
		cleaned, err := ag.CleanSchema(g.Schema)
		if err != nil {
			return invalid("schema", "response schema is not valid JSON")
		}
		g.Schema = cleaned
	}
	return nil
}

func hasFunctions(tools []ag.Tool) bool {
	for _, t := range tools {
		if len(t.Functions) > 0 {
			return true
		}
	}
	return false
}

func (in *RequestIntent) applyThinking(info ag.ModelInfo) {
	g := &in.Request.Generation
	switch t := in.Thinking; t.Mode {
	case "disabled":
		// Gemini cannot turn thinking off: a zero budget is rejected by some
		// models and ignored by others. The minimum budget comes closest.
		budget := info.MinThinkingBudget
		g.Thinking = &ag.Thinking{Budget: &budget}
		in.warn("Gemini cannot disable thinking; using the minimum thinking budget")
	case "effort":
		g.Thinking = &ag.Thinking{Level: thinkingLevels[t.Effort], Include: t.Show}
	case "budget":
		budget := max(t.Budget, info.MinThinkingBudget)
		g.Thinking = &ag.Thinking{Budget: &budget, Include: t.Show}
	case "adaptive":
		dynamic := -1
		g.Thinking = &ag.Thinking{Budget: &dynamic, Include: t.Show}
	}
}

// finalize applies Antigravity's rules for the conversation itself: it ends
// with a user turn, tool results follow the order of their calls, unsigned
// calls carry the placeholder signature, and the session ID is set.
func (in *RequestIntent) finalize(headers http.Header) error {
	in.endWithUser()
	if err := orderResults(in.Request.Contents); err != nil {
		return err
	}
	addPlaceholders(in.Request.Contents)
	in.assignSession(headers)
	return nil
}

// continuation is the user turn appended after a trailing model turn.
// Antigravity rejects requests that end with a model turn, and treats an empty
// user turn as missing.
const continuation = "Continue."

// endWithUser makes the request end with a user turn. A trailing model turn
// that holds only reasoning has nothing to continue from and is dropped; any
// other trailing model turn gets a short user turn after it, so the model
// continues from there. A trailing tool call without its result is left for
// the upstream to reject.
func (in *RequestIntent) endWithUser() {
	c := in.Request.Contents
	for len(c) > 1 && c[len(c)-1].Role == "model" && reasoningOnly(c[len(c)-1].Parts) {
		c = c[:len(c)-1]
		in.warn("dropped trailing reasoning that had no reply after it")
	}
	in.Request.Contents = c
	if len(c) == 0 || c[len(c)-1].Role != "model" {
		return
	}
	for _, p := range c[len(c)-1].Parts {
		if p.FunctionCall != nil {
			return
		}
	}
	appendContent(&in.Request, "user", []ag.Part{{Text: continuation}})
	in.warn("appended a user turn: Antigravity rejects requests that end with an assistant turn")
}

func reasoningOnly(parts []ag.Part) bool {
	for _, p := range parts {
		if !p.Thought && (p.Text != "" || p.FunctionCall != nil || p.InlineData != nil || p.FileData != nil) {
			return false
		}
	}
	return true
}

// orderResults puts each turn's tool results in the order of the preceding
// calls, which is how Gemini pairs them. Clients may finish tools in any order;
// only the result slots are reordered, leaving other user parts in place.
func orderResults(contents []ag.Content) error {
	var pending []string
	for i := range contents {
		content := &contents[i]
		if content.Role == "model" {
			pending = nil
			for _, p := range content.Parts {
				if p.FunctionCall != nil {
					pending = append(pending, p.FunctionCall.ID)
				}
			}
			continue
		}
		byID := map[string]ag.Part{}
		var positions []int
		for j, p := range content.Parts {
			if p.FunctionResponse != nil {
				if _, exists := byID[p.FunctionResponse.ID]; exists {
					return invalid("tool_result", "duplicate result for a tool call")
				}
				byID[p.FunctionResponse.ID] = p
				positions = append(positions, j)
			}
		}
		var ordered []ag.Part
		for _, id := range pending {
			if p, ok := byID[id]; ok {
				ordered = append(ordered, p)
			}
		}
		if len(ordered) != len(positions) {
			return invalid("tool_result", "tool result does not belong to the preceding assistant turn")
		}
		for j, position := range positions {
			content.Parts[position] = ordered[j]
		}
		pending = nil
	}
	return nil
}
