package protocol

import (
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
	Metadata          json.RawMessage `json:"metadata,omitempty"`
	MCPServers        json.RawMessage `json:"mcp_servers,omitempty"`
	ContextManagement json.RawMessage `json:"context_management,omitempty"`
}

// Accepted without effect, which the standard allows: caching and service
// routing are never guaranteed, and a container only serves code execution,
// whose tools are rejected.
var quietMessageFields = map[string]bool{"cache_control": true, "service_tier": true, "inference_geo": true, "container": true}

type MessageTool struct {
	Type         string          `json:"type,omitempty"`
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema,omitempty"`
	DeferLoading bool            `json:"defer_loading,omitempty"`
	Strict       bool            `json:"strict,omitempty"`
}

func (d Decoder) Messages(data []byte, countOnly bool) (RequestIntent, error) {
	var r MessageRequest
	ignored, err := decodeRequest(data, &r, quietMessageFields)
	if err != nil {
		return RequestIntent{}, err
	}
	in := RequestIntent{Format: Messages, Model: r.Model, Stream: r.Stream, Request: ag.Request{Generation: ag.Generation{MaxTokens: r.MaxTokens, Temperature: r.Temperature, TopP: r.TopP, TopK: r.TopK, Stop: r.Stop}}}
	if len(ignored) > 0 {
		in.warn("ignored request fields: %s", strings.Join(ignored, ", "))
	}
	if present(r.ContextManagement) && !contextEditsNoop(r.ContextManagement) {
		in.warn("context_management is ignored; context editing is not implemented")
	}
	if len(r.Stop) > 0 {
		in.warn("stop_sequences truncate output, but the matched sequence is not reported")
	}
	in.ClientSession = claudeCodeSession(r.Metadata)
	if !countOnly && r.MaxTokens == nil {
		return in, invalid("max_tokens", "max_tokens is required")
	}
	if present(r.MCPServers) && string(r.MCPServers) != "[]" {
		return in, unsupported("mcp_servers")
	}
	if present(r.System) {
		in.warnContentOptions(r.System)
		parts, err := mediaParts(r.System)
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
	in.Instructions = joinSystem(in.Request.System)
	referenced := map[string]bool{}
	for _, message := range r.Messages {
		in.warnContentOptions(message.Content)
		role := message.Role
		switch role {
		case "assistant":
			role = "model"
		case "user":
		case "system":
			// Mid-conversation system messages join the system instruction,
			// as Responses developer messages do.
			parts, err := mediaParts(message.Content)
			if err != nil {
				return in, err
			}
			for _, p := range parts {
				if p.InlineData != nil || p.FileData != nil {
					return in, unsupported("non-text system content")
				}
				in.historySystem(p.Text)
			}
			continue
		default:
			return in, invalid("messages.role", "role must be user, assistant or system")
		}
		bs, err := blocks(message.Content)
		if err != nil {
			return in, err
		}
		for _, b := range bs {
			switch {
			case b.Type == "thinking":
				if role != "model" {
					return in, invalid("thinking", "thinking blocks must be in assistant messages")
				}
				in.deferReplay(b.Signature, b.Thinking)
			case b.Type == "tool_use":
				if role != "model" || b.ID == "" || b.Name == "" {
					return in, invalid("tool_use", "tool_use requires an assistant role, id and name")
				}
				args, err := objectArguments(b.Input)
				if err != nil {
					return in, err
				}
				in.appendHistoryCall(b.ID, ToolName{Name: b.Name}, args)
			case b.Type == "tool_result":
				if role != "user" {
					return in, invalid("tool_result", "tool results must be in user messages")
				}
				p, err := toolResult(b.ToolUseID, "", b.Content, b.IsError)
				if err != nil {
					return in, err
				}
				collectToolReferences(b.Content, referenced)
				appendContent(&in.Request, role, []ag.Part{p})
			default:
				parts, err := blockParts(b)
				if err != nil {
					return in, err
				}
				appendContent(&in.Request, role, parts)
			}
		}
	}
	for _, t := range r.Tools {
		if t.Strict {
			in.warn("tools.strict is not enforced; tool schemas are translated to the upstream subset")
		}
		if t.Type != "" && t.Type != "custom" {
			return in, unsupported("tool type " + t.Type)
		}
		if t.Name == "" {
			return in, invalid("tools.name", "tool name is required")
		}
		// Deferred tools become callable once a tool search result references
		// them, matching Anthropic's tool search behavior.
		if t.DeferLoading && !referenced[t.Name] {
			continue
		}
		params, err := objectArguments(t.InputSchema)
		if err != nil {
			return in, err
		}
		name, err := in.declare(ToolName{Name: t.Name})
		if err != nil {
			return in, err
		}
		in.Request.Tools = append(in.Request.Tools, ag.Tool{Functions: []ag.Function{{Name: name, Description: t.Description, Parameters: params}}})
	}
	if present(r.ToolChoice) {
		var choice struct {
			Type            string `json:"type"`
			Name            string `json:"name,omitempty"`
			DisableParallel bool   `json:"disable_parallel_tool_use,omitempty"`
		}
		if err := json.Unmarshal(r.ToolChoice, &choice); err != nil {
			return in, invalid("tool_choice", "tool_choice must be an object")
		}
		// Reported by the renderer only if several calls are actually returned.
		in.SingleCall = choice.DisableParallel
		if err := setChoice(&in, choice.Type, choice.Name); err != nil {
			return in, err
		}
	}
	if t := r.Thinking; t != nil {
		if t.Display == "omitted" {
			in.HideThinking = true
		}
		switch t.Type {
		case "enabled":
			if t.Budget != nil && *t.Budget > 0 {
				in.Thinking = ThinkingIntent{Mode: "budget", Budget: *t.Budget, Show: true}
			} else {
				in.Thinking = ThinkingIntent{Mode: "adaptive", Show: true}
			}
		case "adaptive":
			in.Thinking = ThinkingIntent{Mode: "adaptive", Show: true}
		case "disabled":
			in.Thinking = ThinkingIntent{Mode: "disabled"}
		default:
			in.warn("ignored thinking.type %q", t.Type)
		}
	}
	if c := r.OutputConfig; c != nil {
		if c.Effort != "" {
			applyEffort(&in, c.Effort)
		}
		if f := c.Format; f != nil {
			if f.Type != "json_schema" || !present(f.Schema) {
				return in, invalid("output_config.format", "expected json_schema with schema")
			}
			in.warn("output_config.format uses the upstream JSON Schema subset; full schema validation is not implemented")
			in.Request.Generation.MIMEType = "application/json"
			in.Request.Generation.Schema = f.Schema
		}
	}
	return in, validateIntent(&in)
}

// contextEditsNoop reports whether context_management requests no edits, or
// only clear_thinking edits that keep all thinking, as Claude Code sends.
func contextEditsNoop(value json.RawMessage) bool {
	var c struct {
		Edits []struct {
			Type string          `json:"type"`
			Keep json.RawMessage `json:"keep"`
		} `json:"edits"`
	}
	if json.Unmarshal(value, &c) != nil {
		return false
	}
	for _, edit := range c.Edits {
		var keep string
		if !strings.HasPrefix(edit.Type, "clear_thinking_") || json.Unmarshal(edit.Keep, &keep) != nil || keep != "all" {
			return false
		}
	}
	return true
}

func collectToolReferences(content json.RawMessage, names map[string]bool) {
	var items []Block
	if json.Unmarshal(content, &items) != nil {
		return
	}
	for _, b := range items {
		if b.Type == "tool_reference" {
			if b.ToolName != "" {
				names[b.ToolName] = true
			} else if b.Name != "" {
				names[b.Name] = true
			}
		}
	}
}
