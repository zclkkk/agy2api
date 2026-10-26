package protocol

import (
	"context"
	"testing"
)

func TestAllowedToolsAutoPreservesSubset(t *testing.T) {
	d := testPipeline{State: testStore(t)}
	in, err := d.Responses(context.Background(), []byte(`{"model":"gemini-test","input":"Choose a tool","tools":[{"type":"function","name":"read_file","parameters":{"type":"object"}},{"type":"function","name":"delete_file","parameters":{"type":"object"}}],"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"read_file"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	var declared []string
	for _, tool := range in.Request.Tools {
		for _, f := range tool.Functions {
			declared = append(declared, f.Name)
		}
	}
	if len(declared) != 1 || declared[0] != "read_file" {
		t.Fatalf("allowed subset changed: declared=%v", declared)
	}
	if in.Request.ToolConfig == nil || in.Request.ToolConfig.FunctionCallingConfig.Mode != "VALIDATED" {
		t.Fatal("auto subset must retain validated optional tool calling")
	}
}

func TestNamespaceNamesRemainDistinct(t *testing.T) {
	d := testPipeline{State: testStore(t)}
	in, err := d.Responses(context.Background(), []byte(`{"model":"gemini-test","input":"x","tools":[{"type":"function","name":"files__read","parameters":{"type":"object"}},{"type":"namespace","name":"files","tools":[{"type":"function","name":"read","parameters":{"type":"object"}}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, tool := range in.Request.Tools {
		for _, f := range tool.Functions {
			if seen[f.Name] {
				t.Fatalf("distinct client identities collide at native name %q; mapping=%+v", f.Name, in.Tools)
			}
			seen[f.Name] = true
		}
	}
}
