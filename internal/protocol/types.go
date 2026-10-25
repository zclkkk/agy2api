// Package protocol implements the Responses and Messages wire contracts.
package protocol

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
	"github.com/zclkkk/agy2api/internal/state"
)

// SkipSignature is Antigravity's documented placeholder for a function call
// whose original thought signature is unavailable.
const SkipSignature = "skip_thought_signature_validator"

type Error struct {
	Message string
	Param   string
}

func (e *Error) Error() string            { return e.Message }
func invalid(param, message string) error { return &Error{Message: message, Param: param} }
func unsupported(param string) error {
	return invalid(param, "unsupported parameter or content: "+param)
}

// ThinkingIntent records what the client asked for; Prepare turns it into the
// provider's native thinking configuration once the model is known.
type ThinkingIntent struct {
	Mode   string // "", "budget", "adaptive", "disabled" or "effort"
	Budget int
	Effort string
}

// ToolName is the client-facing identity of a declared function.
type ToolName struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	Custom    bool   `json:"custom,omitempty"`
}

type Input struct {
	Model         string // model requested by the client
	Upstream      string // model sent to Antigravity when it differs from Model
	Kind          string // Antigravity request type, set by Prepare
	Request       ag.Request
	Stream        bool
	Store         bool
	PreviousID    string
	OriginalItems []json.RawMessage
	Search        bool
	Metadata      json.RawMessage
	Options       json.RawMessage
	Thinking      ThinkingIntent
	HideThinking  bool                // Messages thinking.display=omitted
	Claude        bool                // upstream model is served by Anthropic; set by Prepare
	Tools         map[string]ToolName // native function name -> client identity
	Warnings      []string
}

func (in *Input) UpstreamModel() string {
	if in.Upstream != "" {
		return in.Upstream
	}
	return in.Model
}

func (in *Input) warn(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	for _, w := range in.Warnings {
		if w == message {
			return
		}
	}
	in.Warnings = append(in.Warnings, message)
}

// declare registers a client tool and returns the name sent upstream.
func (in *Input) declare(t ToolName) string {
	native := nativeName(qualify(t.Namespace, t.Name))
	if in.Tools == nil {
		in.Tools = map[string]ToolName{}
	}
	in.Tools[native] = t
	return native
}

// PublicTool maps an upstream function name back to the client's identity.
func (in *Input) PublicTool(native string) ToolName {
	if t, ok := in.Tools[native]; ok {
		return t
	}
	return ToolName{Name: native}
}

// qualify flattens a Responses namespace into one function name, following the
// convention Codex uses for MCP tools.
func qualify(namespace, name string) string {
	if namespace == "" || strings.HasPrefix(name, "mcp__") || name == namespace || strings.HasPrefix(name, namespace+"__") {
		return name
	}
	if strings.HasSuffix(namespace, "__") {
		return namespace + name
	}
	return namespace + "__" + name
}

var invalidNameChars = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// nativeName returns a function name both Gemini and Claude accept: 1-128
// characters from [a-zA-Z0-9_-], starting with a letter or underscore. Names
// that need changes get a short hash so distinct client names stay distinct.
func nativeName(public string) string {
	name := invalidNameChars.ReplaceAllString(public, "_")
	if name == "" || !(name[0] == '_' || ('a' <= name[0] && name[0] <= 'z') || ('A' <= name[0] && name[0] <= 'Z')) {
		name = "_" + name
	}
	if name == public && len(name) <= 128 {
		return name
	}
	sum := sha256.Sum256([]byte(public))
	if len(name) > 119 {
		name = name[:119]
	}
	return name + "_" + hex.EncodeToString(sum[:4])
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

// Replay preserves the exact native signature placement, including signatures on
// non-thinking text. Visible text is checked before reattaching such signatures.
type Replay struct {
	Parts        []ag.Part `json:"parts,omitempty"`
	PreviousText string    `json:"previous_text,omitempty"`
	Signature    string    `json:"signature,omitempty"`
	SessionID    string    `json:"session_id,omitempty"`
}

// decodeRequest decodes a top-level request object. Unknown fields are ignored
// so new client fields do not break requests; their names are reported unless
// they are known to have no effect on generation.
func decodeRequest(data []byte, dst any, quiet map[string]bool) ([]string, error) {
	if err := json.Unmarshal(data, dst); err != nil {
		return nil, invalid("", "invalid request body: "+err.Error())
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, invalid("", "request body must be a JSON object")
	}
	known := map[string]bool{}
	t := reflect.TypeOf(dst).Elem()
	for i := 0; i < t.NumField(); i++ {
		if name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ","); name != "" {
			known[strings.ToLower(name)] = true
		}
	}
	var ignored []string
	for key := range fields {
		if !known[strings.ToLower(key)] && !quiet[key] {
			ignored = append(ignored, key)
		}
	}
	sort.Strings(ignored)
	return ignored, nil
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

func agyCallID(id string) bool {
	return strings.HasPrefix(id, "call_agy_") || strings.HasPrefix(id, "toolu_agy_")
}

// restoreCall returns the original signature for a call this bridge issued.
// Missing, expired or mismatched state degrades to no signature; for Gemini,
// NativeRequest then supplies the placeholder Antigravity accepts.
func (d Decoder) restoreCall(ctx context.Context, in *Input, call *ag.Call) string {
	if !agyCallID(call.ID) {
		return ""
	}
	var r SignatureRecord
	if err := d.State.Get(ctx, "tool", call.ID, &r); err != nil {
		in.warn("tool replay state for %s is missing or expired; using a placeholder signature", call.ID)
		return ""
	}
	if r.Model != in.Model || r.Name != call.Name || r.Digest != digest(call.Args) {
		in.warn("tool call %s changed or came from another model; using a placeholder signature", call.ID)
		return ""
	}
	return r.Signature
}

// replay restores a sealed thinking carrier. A carrier for a signed text part
// signs the last part of prev in place and returns nil. Carriers that cannot be
// restored are dropped, which is how Anthropic treats prior-turn thinking too.
func (d Decoder) replay(in *Input, token, visible string, prev []ag.Part) []ag.Part {
	if !strings.HasPrefix(token, "agy1.") {
		in.warn("dropped a reasoning item not issued by agy2api")
		return nil
	}
	var r Replay
	if err := d.State.Unseal(in.Model, token, &r); err != nil {
		in.warn("dropped a reasoning item: %v", err)
		return nil
	}
	if in.Request.SessionID == "" {
		in.Request.SessionID = r.SessionID
	}
	if r.PreviousText != "" {
		if len(prev) == 0 || digest(raw(prev[len(prev)-1].Text)) != r.PreviousText {
			in.warn("dropped a text signature because the signed text changed")
			return nil
		}
		prev[len(prev)-1].Signature = r.Signature
		return nil
	}
	if visible != "" {
		var text strings.Builder
		for _, p := range r.Parts {
			text.WriteString(p.Text)
		}
		if text.String() != visible {
			in.warn("dropped a thinking block because its text changed")
			return nil
		}
	}
	return r.Parts
}

// stripSignatures removes provider signatures and thoughts from history that
// will be sent to a different model.
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

// NativeRequest restores provider call IDs on a copy, keeping public IDs in stored
// histories so clients can continue referencing their original call_id values.
// placeholder adds SkipSignature to unsigned calls, which Gemini requires.
func (d Decoder) NativeRequest(ctx context.Context, request ag.Request, placeholder bool) (ag.Request, error) {
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
		content := &native.Contents[i]
		for j := range content.Parts {
			p := &content.Parts[j]
			if call := p.FunctionCall; call != nil && agyCallID(call.ID) {
				var record SignatureRecord
				// Without a record the public ID is a valid correlation ID upstream.
				if err := d.State.Get(ctx, "tool", call.ID, &record); err == nil {
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
		if content.Role != "model" || !placeholder {
			continue
		}
		// Gemini rejects a step whose first function call, or a generated image,
		// has no signature. Claude must not get one: its bridge would treat it
		// as the signature of the turn's thinking block.
		firstCall := true
		for j := range content.Parts {
			p := &content.Parts[j]
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
	if json.Unmarshal(content, &text) == nil || !present(content) {
		return ag.Part{FunctionResponse: &ag.Result{ID: id, Name: name, Response: raw(map[string]any{"output": text, "is_error": isError})}}, nil
	}
	parts, err := mediaParts(content)
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
