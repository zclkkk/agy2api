package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

var allowedSchemaKeys = map[string]bool{"type": true, "format": true, "title": true, "description": true, "nullable": true, "enum": true,
	"items": true, "properties": true, "required": true, "minItems": true, "maxItems": true, "minLength": true, "maxLength": true,
	"pattern": true, "minimum": true, "maximum": true, "minProperties": true, "maxProperties": true, "default": true, "example": true,
	"propertyOrdering": true, "anyOf": true, "additionalProperties": true}

// checkSchema enforces what upstream accepts: known keys, string enums, a
// single type, arrays with items, required keys that exist, and for Claude no
// unions at all.
func checkSchema(t *testing.T, path string, v any, claude bool) {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s: schema is %T", path, v)
	}
	for k, child := range m {
		if !allowedSchemaKeys[k] {
			t.Errorf("%s: key %q not accepted upstream", path, k)
		}
		switch k {
		case "type":
			if _, ok := child.(string); !ok {
				t.Errorf("%s: type must be a single string", path)
			}
		case "enum":
			for _, e := range child.([]any) {
				if _, ok := e.(string); !ok {
					t.Errorf("%s: non-string enum %v", path, e)
				}
			}
		case "properties":
			for name, p := range child.(map[string]any) {
				checkSchema(t, path+"."+name, p, claude)
			}
		case "items", "additionalProperties":
			if _, isBool := child.(bool); !isBool {
				checkSchema(t, path+"."+k, child, claude)
			}
		case "anyOf":
			if claude {
				t.Errorf("%s: Claude rejects anyOf", path)
			}
			for _, b := range child.([]any) {
				checkSchema(t, path+".anyOf", b, claude)
			}
		case "required":
			props, _ := m["properties"].(map[string]any)
			for _, r := range child.([]any) {
				if props[r.(string)] == nil {
					t.Errorf("%s: required %v is not a property", path, r)
				}
			}
		}
	}
	if m["type"] == "array" && m["items"] == nil {
		t.Errorf("%s: array without items", path)
	}
	if m["properties"] != nil && m["type"] != "object" {
		t.Errorf("%s: properties without object type", path)
	}
}

const mcpStyleSchema = `{
 "$schema":"https://json-schema.org/draft/2020-12/schema","$id":"x","$comment":"c","title":"Args",
 "type":"object",
 "properties":{
  "path":{"type":"string","format":"uri","examples":["a"],"deprecated":true,"x-internal":1},
  "mode":{"type":["string","null"],"enum":["r","w",null]},
  "level":{"type":"integer","enum":[1,2,3]},
  "flag":{"const":true},
  "kind":{"const":"fixed"},
  "count":{"type":"number","exclusiveMinimum":0,"exclusiveMaximum":10,"multipleOf":2},
  "tags":{"type":"array","uniqueItems":true},
  "pair":{"type":"array","prefixItems":[{"type":"string"}]},
  "target":{"$ref":"#/$defs/Target"},
  "node":{"$ref":"#/$defs/Node"},
  "choice":{"anyOf":[{"type":"string"},{"type":"object","properties":{"id":{"type":"string"}}},{"type":"null"}]},
  "either":{"oneOf":[{"type":"integer"},{"type":"string"}]},
  "merged":{"allOf":[{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]},{"properties":{"b":{"type":"string"}}}]},
  "anything":{},
  "yes":true,
  "map":{"type":"object","additionalProperties":{"type":"string","pattern":"^a"},"propertyNames":{"pattern":"^k"}},
  "cond":{"type":"object","properties":{"x":{"type":"string"}},"if":{"properties":{"x":{"const":"a"}}},"then":{"required":["x"]}}
 },
 "required":["path","missing"],
 "additionalProperties":false,
 "$defs":{
  "Target":{"type":"object","properties":{"host":{"type":"string"}},"required":["host"]},
  "Node":{"type":"object","properties":{"child":{"$ref":"#/$defs/Node"}}}
 }
}`

func TestSchemaCleaningProducesAcceptedSubset(t *testing.T) {
	for _, claude := range []bool{false, true} {
		out, err := cleanToolSchema(json.RawMessage(mcpStyleSchema), claude)
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		if err := json.Unmarshal(out, &v); err != nil {
			t.Fatal(err)
		}
		checkSchema(t, "root", v, claude)
		props := v["properties"].(map[string]any)
		get := func(name string) map[string]any { return props[name].(map[string]any) }
		if get("mode")["type"] != "string" || get("mode")["nullable"] != true {
			t.Errorf("type list not converted: %v", get("mode"))
		}
		if !strings.Contains(get("level")["description"].(string), "[1,2,3]") {
			t.Errorf("numeric enum not preserved as guidance: %v", get("level"))
		}
		if e := get("kind")["enum"].([]any); len(e) != 1 || e[0] != "fixed" {
			t.Errorf("string const not converted: %v", get("kind"))
		}
		if get("count")["minimum"] == nil || get("count")["maximum"] == nil {
			t.Errorf("exclusive bounds lost: %v", get("count"))
		}
		if get("target")["properties"].(map[string]any)["host"] == nil {
			t.Errorf("$ref not inlined: %v", get("target"))
		}
		if get("node")["type"] != "object" {
			t.Errorf("recursive $ref not cut: %v", get("node"))
		}
		merged := get("merged")["properties"].(map[string]any)
		if merged["a"] == nil || merged["b"] == nil {
			t.Errorf("allOf not merged: %v", get("merged"))
		}
		if req := v["required"].([]any); len(req) != 1 || req[0] != "path" {
			t.Errorf("required not filtered: %v", req)
		}
		if claude && get("choice")["type"] != "object" {
			t.Errorf("Claude union should keep the object branch: %v", get("choice"))
		}
		if !claude && len(get("choice")["anyOf"].([]any)) != 2 {
			t.Errorf("Gemini union should keep non-null branches: %v", get("choice"))
		}
	}
}

func TestSchemaRootIsAlwaysAnObject(t *testing.T) {
	for _, in := range []string{`{}`, `{"properties":{"a":{"type":"string"}}}`, `{"anyOf":[{"type":"object","properties":{"a":{"type":"string"}}}]}`, `{"type":"string"}`} {
		out, err := cleanToolSchema(json.RawMessage(in), true)
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		_ = json.Unmarshal(out, &v)
		if v["type"] != "object" || v["anyOf"] != nil {
			t.Errorf("%s -> %s", in, out)
		}
	}
}
