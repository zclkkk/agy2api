// Package protocol implements the Responses and Messages wire contracts.
package protocol

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
	"github.com/zclkkk/agy2api/internal/state"
)

type Error struct {
	Message string
	Param   string
}

func (e *Error) Error() string            { return e.Message }
func invalid(param, message string) error { return &Error{Message: message, Param: param} }
func unsupported(param string) error {
	return invalid(param, "unsupported parameter or content: "+param)
}

type Input struct {
	Model         string
	Request       ag.Request
	Stream        bool
	Store         bool
	PreviousID    string
	OriginalItems []json.RawMessage
	Search        bool
	Metadata      json.RawMessage
	Options       json.RawMessage
}

type Decoder struct{ State *state.Store }
type SignatureRecord struct {
	Model     string `json:"model"`
	Name      string `json:"name"`
	Digest    string `json:"digest"`
	Signature string `json:"signature"`
	NativeID  string `json:"native_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

type ImageRecord struct {
	Model     string
	MIMEType  string
	Digest    string
	Signature string
	SessionID string
}

func strict(data []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return invalid("", err.Error())
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return invalid("", "expected one JSON object")
	}
	return nil
}
func raw(v any) json.RawMessage      { b, _ := json.Marshal(v); return b }
func present(v json.RawMessage) bool { return len(v) > 0 && string(v) != "null" }
func canonical(v json.RawMessage) ([]byte, error) {
	var obj any
	d := json.NewDecoder(bytes.NewReader(v))
	d.UseNumber()
	if err := d.Decode(&obj); err != nil {
		return nil, err
	}
	return json.Marshal(obj)
}
func digest(v json.RawMessage) string {
	b, _ := canonical(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (d Decoder) RestoreCall(ctx context.Context, model string, call *ag.Call) (string, error) {
	if !strings.HasPrefix(call.ID, "call_agy_") && !strings.HasPrefix(call.ID, "toolu_agy_") {
		return "", nil
	}
	var r SignatureRecord
	if err := d.State.Get(ctx, "tool", call.ID, &r); err != nil {
		return "", invalid("tool_call", "tool replay state is missing or expired; start a new conversation")
	}
	if r.Model != model || r.Name != call.Name || r.Digest != digest(call.Args) {
		return "", invalid("tool_call", "tool call was modified or belongs to another model")
	}
	return r.Signature, nil
}

// NativeRequest restores provider call IDs on a copy, keeping public IDs in stored
// histories so clients can continue referencing their original call_id values.
func (d Decoder) NativeRequest(ctx context.Context, model string, request ag.Request) (ag.Request, error) {
	data, err := json.Marshal(request)
	if err != nil {
		return request, err
	}
	var native ag.Request
	if err := json.Unmarshal(data, &native); err != nil {
		return request, err
	}
	// Gemini associates parallel results with the preceding call order. Clients
	// may finish tools in a different order; preserve all result data while sorting
	// only the result slots, leaving ordinary user text in place.
	var pending []string
	for i := range native.Contents {
		content := &native.Contents[i]
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
					return request, invalid("tool_result", "duplicate result for a tool call")
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
			return request, invalid("tool_result", "tool result does not belong to the preceding assistant turn")
		}
		for j, position := range positions {
			content.Parts[position] = ordered[j]
		}
		pending = nil
	}
	ids := map[string]string{}
	for i := range native.Contents {
		for j := range native.Contents[i].Parts {
			p := &native.Contents[i].Parts[j]
			if p.FunctionCall != nil {
				call := p.FunctionCall
				if strings.HasPrefix(call.ID, "call_agy_") || strings.HasPrefix(call.ID, "toolu_agy_") {
					var record SignatureRecord
					if err := d.State.Get(ctx, "tool", call.ID, &record); err != nil {
						return request, invalid("tool_call", "tool replay state is missing or expired")
					}
					ids[call.ID] = record.NativeID
					if native.SessionID == "" {
						native.SessionID = record.SessionID
					}
					call.ID = record.NativeID
				}
			}
			if p.FunctionResponse != nil {
				if id, ok := ids[p.FunctionResponse.ID]; ok {
					p.FunctionResponse.ID = id
				}
			}
		}
	}
	return native, nil
}

func appendContent(req *ag.Request, role string, parts []ag.Part) {
	if len(parts) == 0 {
		return
	}
	n := len(req.Contents)
	if n > 0 && req.Contents[n-1].Role == role {
		req.Contents[n-1].Parts = append(req.Contents[n-1].Parts, parts...)
	} else {
		req.Contents = append(req.Contents, ag.Content{Role: role, Parts: parts})
	}
}

func system(req *ag.Request, text string) {
	if text == "" {
		return
	}
	if req.System == nil {
		req.System = &ag.Content{Parts: []ag.Part{}}
	}
	req.System.Parts = append(req.System.Parts, ag.Part{Text: text})
}

func validate(in *Input) error {
	if strings.TrimSpace(in.Model) == "" {
		return invalid("model", "model is required")
	}
	if len(in.Request.Contents) == 0 {
		return invalid("input", "at least one message is required")
	}
	g := in.Request.Generation
	if g.MaxTokens != nil && *g.MaxTokens < 1 {
		return invalid("max_tokens", "token limit must be positive")
	}
	if g.Temperature != nil && (*g.Temperature < 0 || *g.Temperature > 2) {
		return invalid("temperature", "temperature must be between 0 and 2")
	}
	if g.TopP != nil && (*g.TopP < 0 || *g.TopP > 1) {
		return invalid("top_p", "top_p must be between 0 and 1")
	}
	if in.Search {
		for _, t := range in.Request.Tools {
			if len(t.Functions) > 0 {
				return unsupported("combining web search and function tools")
			}
		}
	}
	return nil
}

func objectArguments(v json.RawMessage) (json.RawMessage, error) {
	if !present(v) {
		return json.RawMessage(`{}`), nil
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(v, &obj) != nil || obj == nil {
		return nil, invalid("arguments", "tool arguments must be a JSON object")
	}
	return v, nil
}

func toolName(id string, contents []ag.Content) (string, error) {
	for i := len(contents) - 1; i >= 0; i-- {
		for _, p := range contents[i].Parts {
			if p.FunctionCall != nil && p.FunctionCall.ID == id {
				return p.FunctionCall.Name, nil
			}
		}
	}
	return "", invalid("tool_result", fmt.Sprintf("no matching tool call for %q", id))
}

func toolResult(id, name string, content json.RawMessage, isError bool) (ag.Part, error) {
	var text string
	if json.Unmarshal(content, &text) == nil {
		return ag.Part{FunctionResponse: &ag.Result{ID: id, Name: name, Response: raw(map[string]any{"output": text, "is_error": isError})}}, nil
	}
	parts, err := mediaParts(content, "tool_result")
	if err != nil {
		return ag.Part{}, err
	}
	var texts []string
	var media []ag.Part
	for _, part := range parts {
		if part.InlineData != nil || part.FileData != nil {
			media = append(media, part)
		} else {
			texts = append(texts, part.Text)
		}
	}
	return ag.Part{FunctionResponse: &ag.Result{ID: id, Name: name, Response: raw(map[string]any{"output": strings.Join(texts, "\n"), "is_error": isError}), Parts: media}}, nil
}
