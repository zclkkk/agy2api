package protocol

import (
	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

// Claude through Antigravity takes a numeric budget; sending only a Gemini
// thinkingLevel succeeds but leaves Claude thinking disabled.
var claudeBudgets = map[string]int{"minimal": 1024, "low": 1024, "medium": 8192, "high": 24576, "xhigh": 32768, "max": 32768}

// Gemini levels; some models reject "minimal", so it maps to "low".
var geminiLevels = map[string]string{"minimal": "low", "low": "low", "medium": "medium", "high": "high", "xhigh": "high", "max": "high"}

const claudeMinBudget = 1024

func knownEffort(effort string) bool { _, ok := geminiLevels[effort]; return ok }

// Prepare applies model-specific request shaping: it checks the model, routes
// web search, picks the request type, converts thinking intent and rewrites
// schemas into the subset Antigravity accepts.
func (in *Input) Prepare(catalog ag.Catalog) error {
	info, ok := catalog.Info(in.Model)
	if !ok {
		return invalid("model", "model is not available to this account")
	}
	in.Kind = ag.KindAgent
	if info.Image {
		in.Kind = ag.KindImage
	}
	if in.Search {
		switch {
		case hasFunctions(in.Request.Tools):
			in.Search = false
			in.warn("web search ignored: Antigravity cannot combine it with function tools")
		case !info.Search && len(catalog.SearchModels) == 0:
			return invalid("tools", "web search is not available to this account")
		default:
			if !info.Search {
				in.Upstream = catalog.SearchModels[0]
				info, _ = catalog.Info(in.Upstream)
				in.warn("web search routed to %s", in.Upstream)
			}
			in.Kind = ag.KindSearch
			in.Request.ToolConfig = nil
			in.Request.Tools = append(in.Request.Tools, ag.Tool{GoogleSearch: raw(map[string]any{})})
		}
	}
	in.Claude = info.Claude()
	if err := in.applyThinking(info); err != nil {
		return err
	}
	for i := range in.Request.Tools {
		for j := range in.Request.Tools[i].Functions {
			f := &in.Request.Tools[i].Functions[j]
			cleaned, err := cleanToolSchema(f.Parameters, info.Claude())
			if err != nil {
				return invalid("tools", "tool "+in.PublicTool(f.Name).Name+" has an invalid JSON schema")
			}
			f.Parameters = cleaned
		}
	}
	if g := &in.Request.Generation; present(g.Schema) {
		cleaned, err := cleanSchema(g.Schema, info.Claude())
		if err != nil {
			return invalid("schema", "response schema is not valid JSON")
		}
		g.Schema = cleaned
	}
	return nil
}

// continuation is the user turn appended after a trailing model turn.
// Antigravity rejects requests that end with a model turn for every model,
// and treats an empty user turn as missing.
const continuation = "Continue."

// EndWithUser makes the request end with a user turn. A trailing model turn
// that holds only reasoning has nothing to continue from and is dropped; any
// other trailing model turn gets a short user turn after it, so the model
// continues from there. A trailing tool call without its result is left for
// the upstream to reject.
func (in *Input) EndWithUser() {
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

func hasFunctions(tools []ag.Tool) bool {
	for _, t := range tools {
		if len(t.Functions) > 0 {
			return true
		}
	}
	return false
}

func (in *Input) applyThinking(info ag.ModelInfo) error {
	g := &in.Request.Generation
	t := in.Thinking
	switch t.Mode {
	case "":
		return nil
	case "disabled":
		zero := 0
		g.Thinking = &ag.Thinking{Budget: &zero}
		return nil
	}
	if info.Claude() {
		// Claude rejects thinking while tool_choice forces a tool call; OpenAI
		// clients commonly combine the two.
		if c := in.Request.ToolConfig; c != nil && c.FunctionCallingConfig.Mode == "ANY" {
			zero := 0
			g.Thinking = &ag.Thinking{Budget: &zero}
			in.warn("thinking disabled: Claude cannot think while tool_choice forces a tool call")
			return nil
		}
		budget := -1
		switch t.Mode {
		case "budget":
			budget = t.Budget
		case "effort":
			budget = claudeBudgets[t.Effort]
		}
		if g.MaxTokens == nil {
			if info.MaxOutputTokens <= 0 {
				return invalid("max_output_tokens", "specify max_output_tokens when this Claude model's output limit is not advertised")
			}
			limit := info.MaxOutputTokens
			g.MaxTokens = &limit
		}
		if budget >= *g.MaxTokens {
			budget = *g.MaxTokens - 1
			in.warn("thinking budget reduced to %d to fit the output limit", budget)
		}
		if budget >= 0 && budget < claudeMinBudget {
			zero := 0
			g.Thinking = &ag.Thinking{Budget: &zero}
			in.warn("thinking disabled: the output limit leaves less than Claude's %d-token minimum budget", claudeMinBudget)
			return nil
		}
		g.Thinking = &ag.Thinking{Budget: &budget, Include: true}
		return nil
	}
	switch t.Mode {
	case "effort":
		g.Thinking = &ag.Thinking{Level: geminiLevels[t.Effort], Include: true}
	case "budget":
		budget := max(t.Budget, info.MinThinkingBudget)
		g.Thinking = &ag.Thinking{Budget: &budget, Include: true}
	case "adaptive":
		dynamic := -1
		g.Thinking = &ag.Thinking{Budget: &dynamic, Include: true}
	}
	return nil
}
