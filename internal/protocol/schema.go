package protocol

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
)

// Antigravity validates tool and response schemas against a protobuf Schema
// message rather than JSON Schema, and its Claude bridge re-validates the
// result as JSON Schema without unions. These keys pass through unchanged;
// refs, unions, const, exclusive bounds and type lists are translated, and
// everything else is dropped.
var schemaScalars = []string{"format", "title", "description", "minItems", "maxItems", "minLength", "maxLength",
	"pattern", "minimum", "maximum", "minProperties", "maxProperties", "default", "example", "propertyOrdering"}

const maxSchemaDepth = 32

type schemaCleaner struct {
	defs   map[string]any
	active map[string]bool
	claude bool
}

// cleanToolSchema returns function parameters in the accepted subset. The root
// is always an object, as both Gemini and Claude require.
func cleanToolSchema(raw json.RawMessage, claude bool) (json.RawMessage, error) {
	out, err := cleanSchemaValue(raw, claude)
	if err != nil {
		return nil, err
	}
	out = flatten(out)
	if out["type"] != "object" {
		props, _ := out["properties"].(map[string]any)
		out = map[string]any{"type": "object", "properties": props, "description": out["description"]}
		if props == nil {
			out["properties"] = map[string]any{}
		}
		if out["description"] == nil {
			delete(out, "description")
		}
	}
	return json.Marshal(out)
}

// cleanSchema returns a response schema in the accepted subset.
func cleanSchema(raw json.RawMessage, claude bool) (json.RawMessage, error) {
	out, err := cleanSchemaValue(raw, claude)
	if err != nil {
		return nil, err
	}
	return json.Marshal(out)
}

func cleanSchemaValue(raw json.RawMessage, claude bool) (map[string]any, error) {
	var root any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&root); err != nil {
		return nil, err
	}
	c := schemaCleaner{defs: map[string]any{}, active: map[string]bool{}, claude: claude}
	if m, ok := root.(map[string]any); ok {
		for _, key := range []string{"$defs", "definitions"} {
			if defs, ok := m[key].(map[string]any); ok {
				for name, v := range defs {
					c.defs["#/"+key+"/"+name] = v
				}
			}
		}
	}
	return c.clean(root, 0), nil
}

func (c *schemaCleaner) clean(node any, depth int) map[string]any {
	in, ok := node.(map[string]any)
	if !ok || depth > maxSchemaDepth {
		return map[string]any{}
	}
	if ref, ok := in["$ref"].(string); ok {
		rest := without(in, "$ref")
		target, found := c.defs[ref].(map[string]any)
		if !found || c.active[ref] {
			// Unresolvable or recursive: keep what the reference site says.
			out := c.clean(rest, depth+1)
			if len(out) == 0 {
				out["type"] = "object"
			}
			return out
		}
		c.active[ref] = true
		merged := copyMap(target)
		for k, v := range rest {
			merged[k] = v
		}
		out := c.clean(merged, depth+1)
		delete(c.active, ref)
		return out
	}
	if all, ok := in["allOf"].([]any); ok {
		merged := without(in, "allOf")
		for _, item := range all {
			m, _ := item.(map[string]any)
			if ref, ok := m["$ref"].(string); ok && !c.active[ref] {
				if target, found := c.defs[ref].(map[string]any); found {
					m = target
				}
			}
			mergeSchema(merged, m)
		}
		return c.clean(merged, depth+1)
	}

	out := map[string]any{}
	var hints []string
	nullable := in["nullable"] == true

	typ, _ := in["type"].(string)
	var branches []any
	if list, ok := in["type"].([]any); ok {
		var kinds []string
		for _, v := range list {
			if s, ok := v.(string); ok {
				if s == "null" {
					nullable = true
				} else {
					kinds = append(kinds, s)
				}
			}
		}
		if len(kinds) == 1 {
			typ = kinds[0]
		}
		if len(kinds) > 1 {
			for _, k := range kinds {
				branches = append(branches, map[string]any{"type": k})
			}
		}
	}
	for _, key := range []string{"anyOf", "oneOf"} {
		if list, ok := in[key].([]any); ok {
			branches = append(branches, list...)
		}
	}
	var union []map[string]any
	for _, b := range branches {
		cb := c.clean(b, depth+1)
		if cb["type"] == "null" && len(cb) == 1 {
			nullable = true
			continue
		}
		union = append(union, cb)
	}

	var enum []any
	if v, ok := in["const"]; ok {
		enum = []any{v}
	}
	if list, ok := in["enum"].([]any); ok {
		enum = list
	}
	if enum != nil {
		var values []any
		mixed := false
		for _, v := range enum {
			switch v.(type) {
			case nil:
				nullable = true
			case string:
				values = append(values, v)
			default:
				mixed = true
			}
		}
		// Upstream enums are string-only; keep other values as guidance.
		if !mixed && len(values) > 0 {
			out["enum"] = values
			if typ == "" {
				typ = "string"
			}
		} else if len(enum) > 0 {
			b, _ := json.Marshal(enum)
			hints = append(hints, "allowed values: "+string(b))
		}
	}

	for _, key := range schemaScalars {
		if v, ok := in[key]; ok {
			out[key] = v
		}
	}
	if examples, ok := in["examples"].([]any); ok && len(examples) > 0 && out["example"] == nil {
		out["example"] = examples[0]
	}
	for key, bound := range map[string]string{"exclusiveMinimum": "minimum", "exclusiveMaximum": "maximum"} {
		if v, ok := in[key].(json.Number); ok && out[bound] == nil {
			out[bound] = v
		}
	}

	if props, ok := in["properties"].(map[string]any); ok {
		cleaned := make(map[string]any, len(props))
		for name, v := range props {
			cleaned[name] = c.clean(v, depth+1)
		}
		out["properties"] = cleaned
		if typ == "" {
			typ = "object"
		}
	}
	if req, ok := in["required"].([]any); ok {
		props, _ := out["properties"].(map[string]any)
		var keep []any
		for _, r := range req {
			if s, ok := r.(string); ok && props[s] != nil {
				keep = append(keep, s)
			}
		}
		if len(keep) > 0 {
			out["required"] = keep
		}
	}
	switch items := in["items"].(type) {
	case map[string]any:
		out["items"] = c.clean(items, depth+1)
	case []any:
		if len(items) > 0 {
			out["items"] = c.clean(items[0], depth+1)
		}
	}
	if prefix, ok := in["prefixItems"].([]any); ok && len(prefix) > 0 && out["items"] == nil {
		out["items"] = c.clean(prefix[0], depth+1)
	}
	if typ == "" && out["items"] != nil {
		typ = "array"
	}
	if typ == "array" && out["items"] == nil {
		out["items"] = map[string]any{}
	}
	switch ap := in["additionalProperties"].(type) {
	case bool:
		out["additionalProperties"] = ap
	case map[string]any:
		out["additionalProperties"] = c.clean(ap, depth+1)
	}
	if typ != "" {
		out["type"] = typ
	}

	switch {
	case len(union) == 1:
		mergeMissing(out, union[0])
	case len(union) > 1:
		out["anyOf"] = toAny(union)
		if c.claude {
			out = flatten(out)
		}
	}
	if nullable {
		out["nullable"] = true
	}
	if len(hints) > 0 {
		desc, _ := out["description"].(string)
		out["description"] = strings.TrimSpace(desc + " (" + strings.Join(hints, "; ") + ")")
	}
	return out
}

// flatten replaces a union with its most structured branch, describing the
// others, for consumers that reject anyOf.
func flatten(s map[string]any) map[string]any {
	list, ok := s["anyOf"].([]any)
	if !ok || len(list) == 0 {
		return s
	}
	delete(s, "anyOf")
	best := 0
	rank := func(v any) int {
		m, _ := v.(map[string]any)
		switch m["type"] {
		case "object":
			return 3
		case "array":
			return 2
		case "string":
			return 1
		}
		return 0
	}
	var kinds []string
	for i, b := range list {
		if rank(b) > rank(list[best]) {
			best = i
		}
		m, _ := b.(map[string]any)
		kind, _ := m["type"].(string)
		if kind == "" {
			kind = "any"
		}
		kinds = append(kinds, kind)
	}
	branch, _ := list[best].(map[string]any)
	mergeMissing(s, branch)
	desc, _ := s["description"].(string)
	s["description"] = strings.TrimSpace(desc + " (accepts one of: " + strings.Join(kinds, " | ") + ")")
	return s
}

func mergeSchema(dst, src map[string]any) {
	for k, v := range src {
		switch k {
		case "properties":
			props, _ := dst["properties"].(map[string]any)
			if props == nil {
				props = map[string]any{}
			}
			if m, ok := v.(map[string]any); ok {
				for name, p := range m {
					if _, exists := props[name]; !exists {
						props[name] = p
					}
				}
			}
			dst["properties"] = props
		case "required":
			seen := map[string]bool{}
			var merged []any
			for _, list := range []any{dst["required"], v} {
				items, _ := list.([]any)
				for _, r := range items {
					if s, ok := r.(string); ok && !seen[s] {
						seen[s] = true
						merged = append(merged, s)
					}
				}
			}
			dst["required"] = merged
		default:
			if _, exists := dst[k]; !exists {
				dst[k] = v
			}
		}
	}
}

func mergeMissing(dst, src map[string]any) {
	keys := make([]string, 0, len(src))
	for k := range src {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, exists := dst[k]; !exists {
			dst[k] = src[k]
		}
	}
}

func without(m map[string]any, key string) map[string]any {
	out := copyMap(m)
	delete(out, key)
	return out
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func toAny(list []map[string]any) []any {
	out := make([]any, len(list))
	for i, v := range list {
		out[i] = v
	}
	return out
}
