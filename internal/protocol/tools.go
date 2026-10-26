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

var invalidNameChars = regexp.MustCompile(`[^a-zA-Z0-9_.:-]`)

// nativeName returns a function name Gemini accepts: 1-128 characters from
// [a-zA-Z0-9_.:-], starting with a letter or underscore. Names that need
// changes get a short hash so distinct client names stay distinct.
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

// setChoice maps a tool choice. Parallel-call limits have no Antigravity
// equivalent and are accepted as best effort.
func setChoice(in *Input, choice, name string) error {
	c := ag.CallingConfig{}
	switch choice {
	case "auto":
		c.Mode = "AUTO"
	case "any", "required":
		c.Mode = "ANY"
	case "none":
		c.Mode = "NONE"
	case "tool", "function", "custom":
		if name == "" {
			return invalid("tool_choice", "tool name is required")
		}
		native := nativeName(name)
		found := false
		for _, t := range in.Request.Tools {
			for _, f := range t.Functions {
				if f.Name == native {
					found = true
				}
			}
		}
		if !found {
			return invalid("tool_choice", "selected tool is not declared")
		}
		c.Mode = "ANY"
		c.AllowedNames = []string{native}
	default:
		in.warn("ignored tool_choice type %q", choice)
		return nil
	}
	in.Request.ToolConfig = &ag.ToolConfig{FunctionCallingConfig: c}
	return nil
}
