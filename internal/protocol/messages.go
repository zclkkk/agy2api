package protocol

import (
	"context"
	"encoding/json"
	"strings"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

type MessageRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	System      json.RawMessage `json:"system,omitempty"`
	MaxTokens   *int            `json:"max_tokens"`
	Stream      bool            `json:"stream,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	TopK        *int            `json:"top_k,omitempty"`
	Stop        []string        `json:"stop_sequences,omitempty"`
	Tools       []MessageTool   `json:"tools,omitempty"`
	ToolChoice  json.RawMessage `json:"tool_choice,omitempty"`
	Thinking    *struct {
		Type    string `json:"type"`
		Budget  *int   `json:"budget_tokens,omitempty"`
		Display string `json:"display,omitempty"`
	} `json:"thinking,omitempty"`
	OutputConfig *struct {
		Effort string `json:"effort,omitempty"`
		Format *struct {
			Type   string          `json:"type"`
			Schema json.RawMessage `json:"schema"`
		} `json:"format,omitempty"`
	} `json:"output_config,omitempty"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
}
type MessageTool struct {
	Type         string          `json:"type,omitempty"`
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema,omitempty"`
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
	Strict       bool            `json:"strict,omitempty"`
}

// Replay preserves the exact native signature placement, including signatures on
// non-thinking text. Visible text is checked before reattaching such signatures.
type Replay struct {
	Parts        []ag.Part `json:"parts,omitempty"`
	PreviousText string    `json:"previous_text,omitempty"`
	Signature    string    `json:"signature,omitempty"`
	SessionID    string    `json:"session_id,omitempty"`
}

func (d Decoder) Messages(ctx context.Context, data []byte, countOnly bool) (Input, error) {
	var r MessageRequest
	if err := strict(data, &r); err != nil {
		return Input{}, err
	}
	in := Input{Model: r.Model, Stream: r.Stream, Request: ag.Request{Generation: ag.Generation{MaxTokens: r.MaxTokens, Temperature: r.Temperature, TopP: r.TopP, TopK: r.TopK, Stop: r.Stop}}}
	if !countOnly && r.MaxTokens == nil {
		return in, invalid("max_tokens", "max_tokens is required")
	}
	if present(r.System) {
		parts, err := mediaParts(r.System, "system")
		if err != nil {
			return in, err
		}
		for _, p := range parts {
			if p.InlineData != nil || p.FileData != nil {
				return in, unsupported("non-text system content")
			}
			system(&in.Request, p.Text)
		}
	}
	for _, message := range r.Messages {
		role := message.Role
		if role == "assistant" {
			role = "model"
		} else if role != "user" {
			return in, invalid("messages.role", "role must be user or assistant")
		}
		bs, err := blocks(message.Content)
		if err != nil {
			return in, err
		}
		var parts []ag.Part
		for _, b := range bs {
			if present(b.CacheControl) {
				return in, unsupported("cache_control")
			}
			switch b.Type {
			case "thinking":
				if role != "model" {
					return in, invalid("thinking", "thinking blocks must be in assistant messages")
				}
				if strings.HasPrefix(b.Signature, "agy1.") {
					var replay Replay
					if err := d.State.Unseal(in.Model, b.Signature, &replay); err != nil {
						return in, invalid("signature", err.Error())
					}
					if in.Request.SessionID == "" {
						in.Request.SessionID = replay.SessionID
					}
					if replay.PreviousText != "" {
						if len(parts) == 0 || digest(raw(parts[len(parts)-1].Text)) != replay.PreviousText {
							return in, invalid("signature", "signed text was changed")
						}
						parts[len(parts)-1].Signature = replay.Signature
					} else {
						var text strings.Builder
						for _, p := range replay.Parts {
							text.WriteString(p.Text)
						}
						if text.String() != b.Thinking {
							return in, invalid("thinking", "signed thinking was changed")
						}
						parts = append(parts, replay.Parts...)
					}
				} else {
					if b.Signature == "" {
						return in, invalid("signature", "thinking replay requires its original signature")
					}
					parts = append(parts, ag.Part{Text: b.Thinking, Thought: true, Signature: b.Signature})
				}
			case "tool_use":
				if role != "model" || b.ID == "" || b.Name == "" {
					return in, invalid("tool_use", "tool_use requires an assistant role, id and name")
				}
				args, err := objectArguments(b.Input)
				if err != nil {
					return in, err
				}
				call := &ag.Call{ID: b.ID, Name: b.Name, Args: args}
				sig, err := d.RestoreCall(ctx, in.Model, call)
				if err != nil {
					return in, err
				}
				parts = append(parts, ag.Part{FunctionCall: call, Signature: sig})
			case "tool_result":
				if role != "user" {
					return in, invalid("tool_result", "tool results must be in user messages")
				}
				name, err := toolName(b.ToolUseID, in.Request.Contents)
				if err != nil {
					return in, err
				}
				p, err := toolResult(b.ToolUseID, name, b.Content, b.IsError)
				if err != nil {
					return in, err
				}
				parts = append(parts, p)
			default:
				p, err := mediaPart(b)
				if err != nil {
					return in, err
				}
				parts = append(parts, p)
			}
		}
		appendContent(&in.Request, role, parts)
	}
	for _, t := range r.Tools {
		if t.Strict {
			return in, unsupported("tools.strict")
		}
		if present(t.CacheControl) {
			return in, unsupported("tools.cache_control")
		}
		if strings.HasPrefix(t.Type, "web_search_") {
			in.Search = true
			in.Request.Tools = append(in.Request.Tools, ag.Tool{GoogleSearch: raw(map[string]any{})})
			continue
		}
		if t.Type != "" && t.Type != "custom" {
			return in, unsupported("tool type " + t.Type)
		}
		if t.Name == "" {
			return in, invalid("tools.name", "tool name is required")
		}
		params, err := objectArguments(t.InputSchema)
		if err != nil {
			return in, err
		}
		in.Request.Tools = append(in.Request.Tools, ag.Tool{Functions: []ag.Function{{Name: t.Name, Description: t.Description, Parameters: params}}})
	}
	if present(r.ToolChoice) {
		var choice struct {
			Type            string `json:"type"`
			Name            string `json:"name,omitempty"`
			DisableParallel bool   `json:"disable_parallel_tool_use,omitempty"`
		}
		if err := strict(r.ToolChoice, &choice); err != nil {
			return in, err
		}
		if choice.DisableParallel {
			return in, unsupported("disable_parallel_tool_use")
		}
		if err := setChoice(&in, choice.Type, choice.Name); err != nil {
			return in, err
		}
	}
	if r.Thinking != nil {
		t := r.Thinking
		if t.Display != "" && t.Display != "summarized" {
			return in, unsupported("thinking.display")
		}
		switch t.Type {
		case "enabled":
			if t.Budget == nil || *t.Budget < 1 {
				return in, invalid("thinking.budget_tokens", "a positive budget is required")
			}
			if r.MaxTokens != nil && *t.Budget >= *r.MaxTokens {
				return in, invalid("thinking.budget_tokens", "thinking budget must be less than max_tokens")
			}
			in.Request.Generation.Thinking = &ag.Thinking{Budget: t.Budget, Include: true}
		case "adaptive":
			n := -1
			in.Request.Generation.Thinking = &ag.Thinking{Budget: &n, Include: true}
		case "disabled":
			n := 0
			in.Request.Generation.Thinking = &ag.Thinking{Budget: &n, Include: false}
		default:
			return in, unsupported("thinking.type")
		}
	}
	if r.OutputConfig != nil {
		if r.OutputConfig.Effort != "" {
			if err := effort(&in, r.OutputConfig.Effort); err != nil {
				return in, err
			}
		}
		if f := r.OutputConfig.Format; f != nil {
			if f.Type != "json_schema" || !present(f.Schema) {
				return in, invalid("output_config.format", "expected json_schema with schema")
			}
			in.Request.Generation.MIMEType = "application/json"
			in.Request.Generation.Schema = f.Schema
		}
	}
	return in, validate(&in)
}

func setChoice(in *Input, choice, name string) error {
	c := ag.CallingConfig{}
	switch choice {
	case "auto":
		c.Mode = "AUTO"
	case "any", "required":
		c.Mode = "ANY"
	case "none":
		c.Mode = "NONE"
	case "tool", "function":
		if name == "" {
			return invalid("tool_choice", "tool name is required")
		}
		found := false
		for _, t := range in.Request.Tools {
			for _, f := range t.Functions {
				if f.Name == name {
					found = true
				}
			}
		}
		if !found {
			return invalid("tool_choice", "selected tool is not declared")
		}
		c.Mode = "ANY"
		c.AllowedNames = []string{name}
	default:
		return unsupported("tool_choice")
	}
	if in.Search && choice != "auto" && choice != "any" && choice != "required" {
		return unsupported("web search tool_choice")
	}
	in.Request.ToolConfig = &ag.ToolConfig{FunctionCallingConfig: c}
	return nil
}

func effort(in *Input, value string) error {
	if value == "none" {
		n := 0
		in.Request.Generation.Thinking = &ag.Thinking{Budget: &n, Include: false}
		return nil
	}
	switch value {
	case "low", "medium", "high":
	default:
		return unsupported("reasoning effort " + value)
	}
	// Antigravity's Claude bridge consumes a numeric budget. Sending only a
	// Gemini thinkingLevel succeeds but leaves Claude thinking disabled.
	if strings.Contains(strings.ToLower(in.Model), "claude") {
		budget := map[string]int{"low": 1024, "medium": 8192, "high": 24576}[value]
		if limit := in.Request.Generation.MaxTokens; limit != nil && budget >= *limit {
			return invalid("reasoning.effort", "the selected Claude reasoning budget must be less than the output token limit")
		}
		in.Request.Generation.Thinking = &ag.Thinking{Budget: &budget, Include: true}
		return nil
	}
	in.Request.Generation.Thinking = &ag.Thinking{Level: value, Include: true}
	return nil
}
