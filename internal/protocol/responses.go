package protocol

import (
	"encoding/json"
	"strings"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

type ResponsesRequest struct {
	Model        string          `json:"model"`
	Input        json.RawMessage `json:"input"`
	Instructions string          `json:"instructions,omitempty"`
	Stream       bool            `json:"stream,omitempty"`
	Store        *bool           `json:"store,omitempty"`
	PreviousID   string          `json:"previous_response_id,omitempty"`
	MaxTokens    *int            `json:"max_output_tokens,omitempty"`
	Temperature  *float64        `json:"temperature,omitempty"`
	TopP         *float64        `json:"top_p,omitempty"`
	Tools        []ResponseTool  `json:"tools,omitempty"`
	ToolChoice   json.RawMessage `json:"tool_choice,omitempty"`
	Parallel     *bool           `json:"parallel_tool_calls,omitempty"`
	Reasoning    *struct {
		Effort  string `json:"effort,omitempty"`
		Summary string `json:"summary,omitempty"`
	} `json:"reasoning,omitempty"`
	Text *struct {
		Verbosity string `json:"verbosity,omitempty"`
		Format    *struct {
			Type        string          `json:"type"`
			Name        string          `json:"name,omitempty"`
			Schema      json.RawMessage `json:"schema,omitempty"`
			Strict      bool            `json:"strict,omitempty"`
			Description string          `json:"description,omitempty"`
		} `json:"format,omitempty"`
	} `json:"text,omitempty"`
	Metadata     json.RawMessage `json:"metadata,omitempty"`
	Include      []string        `json:"include,omitempty"`
	Background   bool            `json:"background,omitempty"`
	Truncation   string          `json:"truncation,omitempty"`
	Conversation json.RawMessage `json:"conversation,omitempty"`
	Prompt       json.RawMessage `json:"prompt,omitempty"`
	// Codex sets prompt_cache_key to its session ID.
	PromptCacheKey    string          `json:"prompt_cache_key,omitempty"`
	ContextManagement json.RawMessage `json:"context_management,omitempty"`
	TopLogprobs       json.RawMessage `json:"top_logprobs,omitempty"`
}

// Accepted without effect, which the standard allows: client identifiers,
// cache retention and service tiers carry no output guarantee, obfuscation is
// optional, and max_tool_calls only limits built-in tools, which are rejected.
var quietResponseFields = map[string]bool{"client_metadata": true, "user": true, "safety_identifier": true,
	"prompt_cache_retention": true, "service_tier": true, "stream_options": true, "max_tool_calls": true}

// Include values that only concern hosted tools or input echoes this adapter
// never returns. reasoning.encrypted_content is always included.
var quietIncludes = map[string]bool{"reasoning.encrypted_content": true, "web_search_call.results": true, "web_search_call.action.sources": true,
	"file_search_call.results": true, "code_interpreter_call.outputs": true, "computer_call_output.output.image_url": true, "message.input_image.image_url": true}

type ResponseTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      bool            `json:"strict,omitempty"`
	Format      *struct {
		Type       string `json:"type"`
		Syntax     string `json:"syntax,omitempty"`
		Definition string `json:"definition,omitempty"`
	} `json:"format,omitempty"`
	Tools []ResponseTool `json:"tools,omitempty"`
}
type ResponseItem struct {
	Type      string          `json:"type,omitempty"`
	Role      string          `json:"role,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	ID        string          `json:"id,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Namespace string          `json:"namespace,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	Input     string          `json:"input,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
	Encrypted string          `json:"encrypted_content,omitempty"`
	Summary   json.RawMessage `json:"summary,omitempty"`
	Status    string          `json:"status,omitempty"`
}

func (d Decoder) Responses(data []byte) (RequestIntent, error) {
	var r ResponsesRequest
	ignored, err := decodeRequest(data, &r, quietResponseFields)
	if err != nil {
		return RequestIntent{}, err
	}
	in := RequestIntent{Format: Responses, Model: r.Model, Stream: r.Stream, Store: r.Store == nil || *r.Store, PreviousID: r.PreviousID, Metadata: r.Metadata, Options: responseOptions(data), ClientSession: r.PromptCacheKey, Request: ag.Request{Generation: ag.Generation{MaxTokens: r.MaxTokens, Temperature: r.Temperature, TopP: r.TopP}}}
	if len(ignored) > 0 {
		in.warn("ignored request fields: %s", strings.Join(ignored, ", "))
	}
	in.warnIgnoredOption("context_management", r.ContextManagement, "[]", "{}")
	in.warnIgnoredOption("top_logprobs", r.TopLogprobs, "0")
	switch {
	case r.Background:
		return in, unsupported("background")
	case present(r.Conversation):
		return in, unsupported("conversation")
	case present(r.Prompt):
		return in, unsupported("prompt")
	}
	// Reported by the renderer only if several calls are actually returned.
	in.SingleCall = r.Parallel != nil && !*r.Parallel
	if r.Truncation != "" && r.Truncation != "disabled" {
		in.warn("truncation is ignored; automatic input truncation is not implemented")
	}
	for _, v := range r.Include {
		if v == "message.output_text.logprobs" {
			in.warn("include message.output_text.logprobs is ignored; logprobs are not returned")
		} else if !quietIncludes[v] {
			in.warn("ignored include %s", v)
		}
	}
	system(&in.Request, r.Instructions)
	in.Instructions = joinSystem(in.Request.System)
	var text string
	var items []json.RawMessage
	if !present(r.Input) {
		return in, invalid("input", "input is required")
	}
	if json.Unmarshal(r.Input, &text) == nil {
		items = []json.RawMessage{raw(ResponseItem{Type: "message", Role: "user", Content: raw(text)})}
	} else if err := json.Unmarshal(r.Input, &items); err != nil {
		return in, invalid("input", "input must be a string or an array of items")
	}
	if len(items) == 0 {
		return in, invalid("input", "input must not be empty")
	}
	for _, item := range items {
		var v ResponseItem
		if err := json.Unmarshal(item, &v); err != nil {
			return in, invalid("input", "invalid input item: "+err.Error())
		}
		switch v.Type {
		case "", "message":
			in.warnContentOptions(v.Content)
			parts, err := mediaParts(v.Content)
			if err != nil {
				return in, err
			}
			switch v.Role {
			case "developer", "system":
				for _, p := range parts {
					if p.InlineData != nil || p.FileData != nil {
						return in, unsupported("non-text system message")
					}
					in.historySystem(p.Text)
				}
			case "user":
				appendContent(&in.Request, "user", parts)
			case "assistant":
				appendContent(&in.Request, "model", parts)
			default:
				return in, invalid("role", "unsupported message role")
			}
		case "function_call", "custom_tool_call":
			if v.CallID == "" || v.Name == "" {
				return in, invalid(v.Type, "call_id and name are required")
			}
			args := json.RawMessage(v.Arguments)
			if v.Type == "custom_tool_call" {
				args = raw(map[string]string{"input": v.Input})
			}
			args, err := objectArguments(args)
			if err != nil {
				return in, err
			}
			in.appendHistoryCall(v.CallID, ToolName{Name: v.Name, Namespace: v.Namespace, Custom: v.Type == "custom_tool_call"}, args)
		case "function_call_output", "custom_tool_call_output":
			in.warnContentOptions(v.Output)
			p, err := toolResult(v.CallID, "", v.Output, false)
			if err != nil {
				return in, err
			}
			appendContent(&in.Request, "user", []ag.Part{p})
		case "reasoning":
			in.deferReplay(v.Encrypted, "")
		case "compaction":
			in.deferCompaction(v.Encrypted)
		case "compaction_trigger":
			// Codex asks for the history to be compacted (remote_compaction v2).
			in.Compact = "trigger"
			continue // A control item is not part of the public input history.
		default:
			return in, unsupported("input item " + v.Type)
		}
		normalized, err := responseInputItem(item, v)
		if err != nil {
			return in, err
		}
		in.OriginalItems = append(in.OriginalItems, normalized)
	}
	if err := declareResponseTools(&in, r.Tools, ""); err != nil {
		return in, err
	}
	if present(r.ToolChoice) {
		if err := responseChoice(&in, r.ToolChoice); err != nil {
			return in, err
		}
	}
	if r.Reasoning != nil {
		summary := r.Reasoning.Summary
		if summary != "" && summary != "auto" && summary != "none" {
			in.warn("reasoning.summary level is not enforced; upstream reasoning summaries are returned as available")
		}
		if r.Reasoning.Effort != "" {
			applyEffort(&in, r.Reasoning.Effort)
		}
		// Reasoning text is returned only when a summary is requested.
		in.Thinking.Show = summary != "" && summary != "none"
		if in.Thinking.Show && in.Thinking.Mode == "" {
			in.Thinking.Mode = "adaptive"
		}
	}
	if r.Text != nil && r.Text.Verbosity != "" {
		in.warn("text.verbosity is ignored; verbosity control is not implemented")
	}
	if r.Text != nil && r.Text.Format != nil {
		f := r.Text.Format
		if f.Strict {
			in.warn("text.format.strict is not enforced; JSON Schema is translated to the upstream subset")
		}
		switch f.Type {
		case "text":
		case "json_object":
			in.Request.Generation.MIMEType = "application/json"
		case "json_schema":
			if !present(f.Schema) {
				return in, invalid("text.format.schema", "schema is required")
			}
			in.Request.Generation.MIMEType = "application/json"
			in.Request.Generation.Schema = f.Schema
		default:
			return in, unsupported("text.format.type")
		}
	}
	return in, validateIntent(&in)
}

// declareResponseTools flattens namespaces and maps custom (freeform) tools to
// functions with a single string argument.
func declareResponseTools(in *RequestIntent, tools []ResponseTool, namespace string) error {
	for _, t := range tools {
		switch t.Type {
		case "function":
			if t.Strict {
				in.warn("tools.strict is not enforced; tool schemas are translated to the upstream subset")
			}
			if t.Name == "" {
				return invalid("tools.name", "tool name is required")
			}
			params, err := objectArguments(t.Parameters)
			if err != nil {
				return err
			}
			name, err := in.declare(ToolName{Name: t.Name, Namespace: namespace})
			if err != nil {
				return err
			}
			in.Request.Tools = append(in.Request.Tools, ag.Tool{Functions: []ag.Function{{Name: name, Description: t.Description, Parameters: params}}})
		case "custom":
			if t.Name == "" {
				return invalid("tools.name", "tool name is required")
			}
			desc := "Raw text input for the tool."
			if t.Format != nil && t.Format.Type == "grammar" {
				in.warn("tools.format.grammar is guidance only; grammar validation is not implemented")
			}
			if t.Format != nil && t.Format.Type == "grammar" && t.Format.Definition != "" {
				desc = "Raw text input that must follow this " + t.Format.Syntax + " grammar:\n" + t.Format.Definition
			}
			params := raw(map[string]any{"type": "object", "properties": map[string]any{"input": map[string]any{"type": "string", "description": desc}}, "required": []string{"input"}})
			name, err := in.declare(ToolName{Name: t.Name, Namespace: namespace, Custom: true})
			if err != nil {
				return err
			}
			in.Request.Tools = append(in.Request.Tools, ag.Tool{Functions: []ag.Function{{Name: name, Description: t.Description, Parameters: params}}})
		case "namespace":
			if namespace != "" || t.Name == "" {
				return invalid("tools", "namespaces must be named and cannot be nested")
			}
			if err := declareResponseTools(in, t.Tools, t.Name); err != nil {
				return err
			}
		default:
			return unsupported("tool type " + t.Type)
		}
	}
	return nil
}

func responseChoice(in *RequestIntent, value json.RawMessage) error {
	var choice string
	if json.Unmarshal(value, &choice) == nil {
		return setChoice(in, choice, "")
	}
	var t struct {
		Type      string `json:"type"`
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		Mode      string `json:"mode"`
		Tools     []struct {
			Type      string `json:"type"`
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(value, &t); err != nil {
		return invalid("tool_choice", "tool_choice must be a string or an object")
	}
	switch t.Type {
	case "function", "custom":
		in.Choice = ToolChoice{Mode: "required", Allowed: []ToolName{{Name: t.Name, Namespace: t.Namespace, Custom: t.Type == "custom"}}}
		return nil
	case "allowed_tools":
		if t.Mode != "auto" && t.Mode != "required" {
			return invalid("tool_choice", "allowed_tools mode must be auto or required")
		}
		in.Choice = ToolChoice{Mode: t.Mode, Allowed: []ToolName{}}
		for _, tool := range t.Tools {
			switch tool.Type {
			case "function", "custom":
				in.Choice.Allowed = append(in.Choice.Allowed, ToolName{Name: tool.Name, Namespace: tool.Namespace, Custom: tool.Type == "custom"})
			default:
				return unsupported("allowed tool type " + tool.Type)
			}
		}
		return nil
	}
	return setChoice(in, t.Type, t.Name)
}

// responseOptions retains only fields echoed in Responses output metadata.
func responseOptions(data []byte) map[string]json.RawMessage {
	var input map[string]json.RawMessage
	_ = json.Unmarshal(data, &input)
	result := map[string]json.RawMessage{}
	for _, key := range []string{"instructions", "max_output_tokens", "temperature", "top_p", "tool_choice", "tools", "parallel_tool_calls", "reasoning", "text"} {
		if value, ok := input[key]; ok {
			result[key] = value
		}
	}
	return result
}
