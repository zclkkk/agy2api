package protocol

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
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
		Format *struct {
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
}

// Fields with no effect on what the model generates.
var quietResponseFields = map[string]bool{"prompt_cache_key": true, "prompt_cache_retention": true, "client_metadata": true,
	"user": true, "safety_identifier": true, "service_tier": true, "stream_options": true, "top_logprobs": true}

type ResponseTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Format      *struct {
		Type       string `json:"type"`
		Syntax     string `json:"syntax,omitempty"`
		Definition string `json:"definition,omitempty"`
	} `json:"format,omitempty"`
	Tools []ResponseTool `json:"tools,omitempty"`
}
type ResponseItem struct {
	Type         string          `json:"type,omitempty"`
	Role         string          `json:"role,omitempty"`
	Content      json.RawMessage `json:"content,omitempty"`
	ID           string          `json:"id,omitempty"`
	CallID       string          `json:"call_id,omitempty"`
	Name         string          `json:"name,omitempty"`
	Namespace    string          `json:"namespace,omitempty"`
	Arguments    string          `json:"arguments,omitempty"`
	Input        string          `json:"input,omitempty"`
	Output       json.RawMessage `json:"output,omitempty"`
	Encrypted    string          `json:"encrypted_content,omitempty"`
	Summary      json.RawMessage `json:"summary,omitempty"`
	Status       string          `json:"status,omitempty"`
	Result       string          `json:"result,omitempty"`
	OutputFormat string          `json:"output_format,omitempty"`
	Action       json.RawMessage `json:"action,omitempty"`
}
type StoredResponse struct {
	Model     string            `json:"model"`
	Contents  []ag.Content      `json:"contents"`
	Response  json.RawMessage   `json:"response"`
	Items     []json.RawMessage `json:"items"`
	SessionID string            `json:"session_id,omitempty"`
}

func (d Decoder) Responses(ctx context.Context, data []byte) (Input, error) {
	var r ResponsesRequest
	ignored, err := decodeRequest(data, &r, quietResponseFields)
	if err != nil {
		return Input{}, err
	}
	in := Input{Model: r.Model, Stream: r.Stream, Store: r.Store == nil || *r.Store, PreviousID: r.PreviousID, Metadata: r.Metadata, Options: data, Request: ag.Request{Generation: ag.Generation{MaxTokens: r.MaxTokens, Temperature: r.Temperature, TopP: r.TopP}}}
	if len(ignored) > 0 {
		in.warn("ignored request fields: %s", strings.Join(ignored, ", "))
	}
	switch {
	case r.Background:
		return in, unsupported("background")
	case present(r.Conversation):
		return in, unsupported("conversation")
	case present(r.Prompt):
		return in, unsupported("prompt")
	}
	if r.Parallel != nil && !*r.Parallel {
		in.warn("parallel_tool_calls=false is best effort")
	}
	for _, v := range r.Include {
		if v != "reasoning.encrypted_content" {
			in.warn("ignored include %s", v)
		}
	}
	if r.PreviousID != "" {
		var previous StoredResponse
		if err := d.State.Get(ctx, "response", r.PreviousID, &previous); err != nil {
			return in, invalid("previous_response_id", "previous response is missing, expired, or was not stored")
		}
		in.Request.Contents = previous.Contents
		if previous.Model != in.Model {
			in.Request.Contents = stripSignatures(previous.Contents)
			in.warn("model changed within a stored response chain; prior reasoning was dropped")
		}
		in.Request.SessionID = previous.SessionID
		in.OriginalItems = previous.Items
		var priorOutput struct {
			Output []json.RawMessage `json:"output"`
		}
		if err := json.Unmarshal(previous.Response, &priorOutput); err != nil {
			return in, err
		}
		in.OriginalItems = append(in.OriginalItems, priorOutput.Output...)
	}
	system(&in.Request, r.Instructions)
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
	in.OriginalItems = append(in.OriginalItems, items...)
	for _, item := range items {
		var v ResponseItem
		if err := json.Unmarshal(item, &v); err != nil {
			return in, invalid("input", "invalid input item: "+err.Error())
		}
		switch v.Type {
		case "", "message":
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
					system(&in.Request, p.Text)
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
			call := &ag.Call{ID: v.CallID, Name: nativeName(qualify(v.Namespace, v.Name)), Args: args}
			appendContent(&in.Request, "model", []ag.Part{{FunctionCall: call, Signature: d.restoreCall(ctx, &in, call)}})
		case "function_call_output", "custom_tool_call_output":
			name, err := toolName(v.CallID, in.Request.Contents)
			if err != nil {
				return in, err
			}
			p, err := toolResult(v.CallID, name, v.Output, false)
			if err != nil {
				return in, err
			}
			appendContent(&in.Request, "user", []ag.Part{p})
		case "reasoning":
			var prev []ag.Part
			if n := len(in.Request.Contents); n > 0 {
				prev = in.Request.Contents[n-1].Parts
			}
			appendContent(&in.Request, "model", d.replay(&in, v.Encrypted, "", prev))
		case "image_generation_call":
			appendContent(&in.Request, "model", []ag.Part{d.restoreImage(ctx, &in, v)})
		case "web_search_call":
			// A server-side tool trace. Its answer and citations are carried by
			// adjacent message items; it is not a client-executed function call.
		default:
			return in, unsupported("input item " + v.Type)
		}
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
		if r.Reasoning.Effort != "" {
			applyEffort(&in, r.Reasoning.Effort)
		}
		if r.Reasoning.Summary != "" && r.Reasoning.Summary != "none" && in.Thinking.Mode == "" {
			in.Thinking = ThinkingIntent{Mode: "adaptive"}
		}
	}
	if r.Text != nil && r.Text.Format != nil {
		f := r.Text.Format
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
	return in, validate(&in)
}

// restoreImage rebuilds a generated image turn. Without replay metadata the
// image is still sent with its type detected; NativeRequest adds the Gemini
// placeholder signature.
func (d Decoder) restoreImage(ctx context.Context, in *Input, v ResponseItem) ag.Part {
	var record ImageRecord
	if err := d.State.Get(ctx, "image", v.ID, &record); err == nil && record.Model == in.Model && record.Digest == digest(raw(v.Result)) {
		if in.Request.SessionID == "" {
			in.Request.SessionID = record.SessionID
		}
		return ag.Part{InlineData: &ag.Blob{MIMEType: record.MIMEType, Data: v.Result}, Signature: record.Signature}
	}
	in.warn("image replay metadata for %s is missing or changed", v.ID)
	head := v.Result[:min(len(v.Result), 1024)]
	head = head[:len(head)/4*4]
	sample, _ := base64.StdEncoding.DecodeString(head)
	return ag.Part{InlineData: &ag.Blob{MIMEType: http.DetectContentType(sample), Data: v.Result}}
}

// declareResponseTools flattens namespaces and maps custom (freeform) tools to
// functions with a single string argument.
func declareResponseTools(in *Input, tools []ResponseTool, namespace string) error {
	for _, t := range tools {
		switch t.Type {
		case "function":
			if t.Name == "" {
				return invalid("tools.name", "tool name is required")
			}
			params, err := objectArguments(t.Parameters)
			if err != nil {
				return err
			}
			in.Request.Tools = append(in.Request.Tools, ag.Tool{Functions: []ag.Function{{Name: in.declare(ToolName{Name: t.Name, Namespace: namespace}), Description: t.Description, Parameters: params}}})
		case "custom":
			if t.Name == "" {
				return invalid("tools.name", "tool name is required")
			}
			desc := "Raw text input for the tool."
			if t.Format != nil && t.Format.Type == "grammar" && t.Format.Definition != "" {
				desc = "Raw text input that must follow this " + t.Format.Syntax + " grammar:\n" + t.Format.Definition
			}
			params := raw(map[string]any{"type": "object", "properties": map[string]any{"input": map[string]any{"type": "string", "description": desc}}, "required": []string{"input"}})
			in.Request.Tools = append(in.Request.Tools, ag.Tool{Functions: []ag.Function{{Name: in.declare(ToolName{Name: t.Name, Namespace: namespace, Custom: true}), Description: t.Description, Parameters: params}}})
		case "namespace":
			if namespace != "" || t.Name == "" {
				return invalid("tools", "namespaces must be named and cannot be nested")
			}
			if err := declareResponseTools(in, t.Tools, t.Name); err != nil {
				return err
			}
		case "web_search", "web_search_preview", "web_search_2025_08_26", "web_search_preview_2025_03_11":
			in.Search = true
		default:
			return unsupported("tool type " + t.Type)
		}
	}
	return nil
}

func responseChoice(in *Input, value json.RawMessage) error {
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
		return setChoice(in, t.Type, qualify(t.Namespace, t.Name))
	case "allowed_tools":
		if t.Mode != "required" {
			return setChoice(in, "auto", "")
		}
		var names []string
		for _, tool := range t.Tools {
			if tool.Name != "" {
				names = append(names, nativeName(qualify(tool.Namespace, tool.Name)))
			}
		}
		in.Request.ToolConfig = &ag.ToolConfig{FunctionCallingConfig: ag.CallingConfig{Mode: "ANY", AllowedNames: names}}
		return nil
	}
	return setChoice(in, t.Type, t.Name)
}
