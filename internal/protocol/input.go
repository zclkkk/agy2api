// Package protocol implements the Responses and Messages wire contracts on top
// of Antigravity's Gemini models.
package protocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
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

// ThinkingIntent records what the client asked for; Prepare turns it into the
// native thinking configuration once the model is known.
type ThinkingIntent struct {
	Mode   string // "", "budget", "adaptive", "disabled" or "effort"
	Budget int
	Effort string
}

// RequestIntent preserves client intent. Decoding performs no state or network
// access. Its content uses Gemini's part vocabulary, with opaque replay items
// kept separately until Planner has selected the actual upstream model.
type RequestIntent struct {
	Format        Format
	Model         string // model requested by the client
	Request       ag.Request
	Stream        bool
	Store         bool
	PreviousID    string
	OriginalItems []json.RawMessage
	Search        bool
	Choice        ToolChoice
	Metadata      json.RawMessage
	Options       map[string]json.RawMessage
	Thinking      ThinkingIntent
	HideThinking  bool                // Messages thinking.display=omitted
	Tools         map[string]ToolName // native function name -> client identity
	ClientSession string              // session ID found in the request body
	Compact       string              // "trigger" (compaction_trigger item) or "endpoint" (/v1/responses/compact)
	Warnings      []string
	Instructions  *ag.Content // this request's top-level instructions
	HistorySystem *ag.Content // system/developer messages that belong to history
	references    []replayReference
}

func (in *RequestIntent) warn(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	for _, w := range in.Warnings {
		if w == message {
			return
		}
	}
	in.Warnings = append(in.Warnings, message)
}

// Decoder only parses the public wire protocols; Planner owns replay and state.
type Decoder struct{}

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

// validateIntent checks fields available without resolving conversation history.
func validateIntent(in *RequestIntent) error {
	if strings.TrimSpace(in.Model) == "" {
		return invalid("model", "model is required")
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
