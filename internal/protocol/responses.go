package protocol

import (
	"context"
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
		Format *struct {
			Type        string          `json:"type"`
			Name        string          `json:"name,omitempty"`
			Schema      json.RawMessage `json:"schema,omitempty"`
			Strict      bool            `json:"strict,omitempty"`
			Description string          `json:"description,omitempty"`
		} `json:"format,omitempty"`
	} `json:"text,omitempty"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
	Include    []string        `json:"include,omitempty"`
	Background bool            `json:"background,omitempty"`
	Truncation string          `json:"truncation,omitempty"`
}
type ResponseTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      bool            `json:"strict,omitempty"`
}
type ResponseItem struct {
	Type         string          `json:"type,omitempty"`
	Role         string          `json:"role,omitempty"`
	Content      json.RawMessage `json:"content,omitempty"`
	ID           string          `json:"id,omitempty"`
	CallID       string          `json:"call_id,omitempty"`
	Name         string          `json:"name,omitempty"`
	Arguments    string          `json:"arguments,omitempty"`
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
	if err := strict(data, &r); err != nil {
		return Input{}, err
	}
	in := Input{Model: r.Model, Stream: r.Stream, Store: r.Store == nil || *r.Store, PreviousID: r.PreviousID, Metadata: r.Metadata, Options: data, Request: ag.Request{Generation: ag.Generation{MaxTokens: r.MaxTokens, Temperature: r.Temperature, TopP: r.TopP}}}
	if r.Background {
		return in, unsupported("background")
	}
	if r.Truncation != "" && r.Truncation != "disabled" {
		return in, unsupported("truncation")
	}
	if r.Parallel != nil && !*r.Parallel {
		return in, unsupported("parallel_tool_calls=false")
	}
	for _, v := range r.Include {
		if v != "reasoning.encrypted_content" {
			return in, unsupported("include " + v)
		}
	}
	if r.PreviousID != "" {
		var previous StoredResponse
		if err := d.State.Get(ctx, "response", r.PreviousID, &previous); err != nil {
			return in, invalid("previous_response_id", "previous response is missing, expired, or was not stored")
		}
		if previous.Model != in.Model {
			return in, invalid("model", "cannot change models within a stored response chain")
		}
		in.Request.Contents = previous.Contents
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
		if err := strict(item, &v); err != nil {
			return in, err
		}
		switch v.Type {
		case "", "message":
			parts, err := mediaParts(v.Content, "input.content")
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
		case "function_call":
			if v.CallID == "" || v.Name == "" {
				return in, invalid("function_call", "call_id and name are required")
			}
			args, err := objectArguments(json.RawMessage(v.Arguments))
			if err != nil {
				return in, err
			}
			call := &ag.Call{ID: v.CallID, Name: v.Name, Args: args}
			sig, err := d.RestoreCall(ctx, in.Model, call)
			if err != nil {
				return in, err
			}
			appendContent(&in.Request, "model", []ag.Part{{FunctionCall: call, Signature: sig}})
		case "function_call_output":
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
			if v.Encrypted == "" {
				return in, invalid("encrypted_content", "reasoning replay requires encrypted_content")
			}
			var replay Replay
			if err := d.State.Unseal(in.Model, v.Encrypted, &replay); err != nil {
				return in, invalid("encrypted_content", err.Error())
			}
			if in.Request.SessionID == "" {
				in.Request.SessionID = replay.SessionID
			}
			if replay.PreviousText != "" {
				n := len(in.Request.Contents)
				if n == 0 {
					return in, invalid("reasoning", "missing signed text")
				}
				parts := in.Request.Contents[n-1].Parts
				if len(parts) == 0 || digest(raw(parts[len(parts)-1].Text)) != replay.PreviousText {
					return in, invalid("reasoning", "signed text was changed")
				}
				in.Request.Contents[n-1].Parts[len(parts)-1].Signature = replay.Signature
			} else {
				appendContent(&in.Request, "model", replay.Parts)
			}
		case "image_generation_call":
			var record ImageRecord
			if err := d.State.Get(ctx, "image", v.ID, &record); err != nil {
				return in, invalid("image_generation_call", "image replay metadata is missing or expired")
			}
			if record.Model != in.Model || record.Digest != digest(raw(v.Result)) {
				return in, invalid("image_generation_call", "generated image was changed or belongs to another model")
			}
			appendContent(&in.Request, "model", []ag.Part{{InlineData: &ag.Blob{MIMEType: record.MIMEType, Data: v.Result}, Signature: record.Signature}})
			if in.Request.SessionID == "" {
				in.Request.SessionID = record.SessionID
			}
		case "web_search_call":
			// This is a server-side tool trace. Its answer and citations are carried
			// by adjacent message items; it is not a client-executed function call.
			if !strings.HasPrefix(v.ID, "ws_agy_") {
				return in, unsupported("foreign web_search_call")
			}
		default:
			return in, unsupported("input item " + v.Type)
		}
	}
	for _, t := range r.Tools {
		switch t.Type {
		case "function":
			if t.Strict {
				return in, unsupported("tools.strict")
			}
			if t.Name == "" {
				return in, invalid("tools.name", "tool name is required")
			}
			params, err := objectArguments(t.Parameters)
			if err != nil {
				return in, err
			}
			in.Request.Tools = append(in.Request.Tools, ag.Tool{Functions: []ag.Function{{Name: t.Name, Description: t.Description, Parameters: params}}})
		case "web_search", "web_search_preview":
			in.Search = true
			in.Request.Tools = append(in.Request.Tools, ag.Tool{GoogleSearch: raw(map[string]any{})})
		default:
			return in, unsupported("tool type " + t.Type)
		}
	}
	if present(r.ToolChoice) {
		var choice string
		if json.Unmarshal(r.ToolChoice, &choice) == nil {
			if err := setChoice(&in, choice, ""); err != nil {
				return in, err
			}
		} else {
			var t struct {
				Type string `json:"type"`
				Name string `json:"name"`
			}
			if err := strict(r.ToolChoice, &t); err != nil {
				return in, err
			}
			if err := setChoice(&in, t.Type, t.Name); err != nil {
				return in, err
			}
		}
	}
	if r.Reasoning != nil {
		if r.Reasoning.Effort != "" {
			if err := effort(&in, r.Reasoning.Effort); err != nil {
				return in, err
			}
		}
		if r.Reasoning.Summary != "" {
			switch r.Reasoning.Summary {
			case "auto", "concise", "detailed":
				if in.Request.Generation.Thinking == nil {
					n := -1
					in.Request.Generation.Thinking = &ag.Thinking{Budget: &n, Include: true}
				}
			default:
				return in, unsupported("reasoning.summary")
			}
		}
	}
	if r.Text != nil && r.Text.Format != nil {
		f := r.Text.Format
		if f.Strict {
			return in, unsupported("text.format.strict")
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
	return in, validate(&in)
}
