package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

// ToolName is the client-facing identity of a declared function.
type ToolName struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	Custom    bool   `json:"custom,omitempty"`
}

// ToolChoice retains the requested mode and subset until all tools are known.
// A nil Allowed slice means unrestricted; an empty non-nil slice allows none.
type ToolChoice struct {
	Mode    string
	Allowed []ToolName
}

func (in *RequestIntent) declare(t ToolName) (string, error) {
	native := toolSymbol(t)
	if in.Tools == nil {
		in.Tools = map[string]ToolName{}
	}
	if _, exists := in.Tools[native]; exists {
		return "", invalid("tools", "duplicate or colliding tool identity")
	}
	in.Tools[native] = t
	return native, nil
}

var invalidNameChars = regexp.MustCompile(`[^a-zA-Z0-9_.:-]`)

func nativeName(public string) string { return toolSymbol(ToolName{Name: public}) }

// qualify avoids repeating prefixes already present in Codex/MCP tool names.
// The complete namespace still participates in the identity hash.
func qualify(namespace, name string) string {
	if namespace == "" || strings.HasPrefix(name, "mcp__") || name == namespace {
		return name
	}
	prefix := namespace
	if !strings.HasSuffix(prefix, "__") {
		prefix += "__"
	}
	if strings.HasPrefix(name, prefix) {
		return name
	}
	return prefix + name
}

// toolSymbol preserves legal ordinary function names. Encoded identities keep
// a readable stem and a six-hex suffix; callers must check for collisions.
func toolSymbol(t ToolName) string {
	readable := qualify(t.Namespace, t.Name)
	name := invalidNameChars.ReplaceAllString(readable, "_")
	validStart := len(name) > 0 && (name[0] == '_' || name[0] >= 'a' && name[0] <= 'z' || name[0] >= 'A' && name[0] <= 'Z')
	if t.Namespace == "" && !t.Custom && validStart && name == t.Name && len(name) <= 128 {
		return name
	}
	if !validStart {
		name = "_" + name
	}
	kind := "function"
	if t.Custom {
		kind = "custom"
	}
	// Hash the unmodified identity, independent of descriptions, schemas, other
	// declarations and truncation. Seven bytes are reserved for "_" + six hex.
	sum := sha256.Sum256(raw([3]string{t.Namespace, t.Name, kind}))
	return name[:min(len(name), 121)] + "_" + hex.EncodeToString(sum[:3])
}

// historyCall locates a call in the client's input and the identity the client
// gave it. Clients may omit fields such as the namespace, so the planner
// completes the identity before naming the call.
type historyCall struct {
	Content, Part int
	Tool          ToolName
}

func (in *RequestIntent) appendHistoryCall(id string, t ToolName, args json.RawMessage) {
	appendContent(&in.Request, "model", []ag.Part{{FunctionCall: &ag.Call{ID: id, Name: toolSymbol(t), Args: args}}})
	i := len(in.Request.Contents) - 1
	in.historyCalls = append(in.historyCalls, historyCall{Content: i, Part: len(in.Request.Contents[i].Parts) - 1, Tool: t})
}

// registerHistoryTool adds an identity seen in history. A historical tool is
// not callable, but it must not alias another identity in the same context.
func (in *RequestIntent) registerHistoryTool(t ToolName) (string, error) {
	native := toolSymbol(t)
	if previous, ok := in.Tools[native]; ok && previous != t {
		return "", invalid("input", "historical tool identity collides with a declared tool")
	}
	if previous, ok := in.historyTools[native]; ok && previous != t {
		return "", invalid("input", "colliding tool identities in history")
	}
	if in.historyTools == nil {
		in.historyTools = map[string]ToolName{}
	}
	in.historyTools[native] = t
	return native, nil
}

func (in *RequestIntent) selectTools() error {
	choice := in.Choice
	mode := choice.Mode
	if mode == "" {
		mode = "auto"
	}
	if choice.Allowed != nil {
		allowed := map[string]bool{}
		for _, tool := range choice.Allowed {
			name := toolSymbol(tool)
			declared, ok := in.Tools[name]
			if !ok || declared != tool {
				return invalid("tool_choice", "selected tool is not declared")
			}
			allowed[name] = true
		}
		var filtered []ag.Tool
		for _, tool := range in.Request.Tools {
			var functions []ag.Function
			for _, f := range tool.Functions {
				if allowed[f.Name] {
					functions = append(functions, f)
				}
			}
			if len(functions) > 0 {
				filtered = append(filtered, ag.Tool{Functions: functions})
			}
		}
		in.Request.Tools = filtered
	}
	in.Request.ToolConfig = nil
	if mode == "required" && !hasFunctions(in.Request.Tools) {
		return invalid("tool_choice", "required tool choice has no callable tools")
	}
	if hasFunctions(in.Request.Tools) || mode == "none" {
		nativeMode := "VALIDATED"
		if mode == "required" {
			nativeMode = "ANY"
		}
		if mode == "none" {
			nativeMode = "NONE"
		}
		config := ag.CallingConfig{Mode: nativeMode}
		if mode == "required" && choice.Allowed != nil {
			for _, tool := range in.Request.Tools {
				for _, f := range tool.Functions {
					config.AllowedNames = append(config.AllowedNames, f.Name)
				}
			}
		}
		in.Request.ToolConfig = &ag.ToolConfig{FunctionCallingConfig: config}
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

// setChoice maps a tool choice. Parallel-call limits are not enforced by this adapter.
func setChoice(in *RequestIntent, choice, name string) error {
	switch choice {
	case "auto", "none":
		in.Choice = ToolChoice{Mode: choice}
	case "any", "required":
		in.Choice = ToolChoice{Mode: "required"}
	case "tool", "function", "custom":
		if name == "" {
			return invalid("tool_choice", "tool name is required")
		}
		in.Choice = ToolChoice{Mode: "required", Allowed: []ToolName{{Name: name, Custom: choice == "custom"}}}
	default:
		return invalid("tool_choice", "unsupported tool choice type: "+choice)
	}
	return nil
}
