package protocol

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

var regressionCatalog = ag.Catalog{Models: map[string]json.RawMessage{"gemini-test": json.RawMessage(`{}`), "gemini-search": json.RawMessage(`{}`)}, SearchModels: []string{"gemini-search"}}

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

func TestReroutedSearchReasoningSurvivesSecondTurn(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	d := testPipeline{State: s}
	in, err := d.Responses(ctx, []byte(`{"model":"gemini-test","input":"look up a topic","store":false,"tools":[{"type":"web_search"}],"reasoning":{"effort":"medium"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if in.UpstreamModel() != "gemini-search" {
		t.Fatal("fixture must reroute")
	}
	renderer := NewRenderer(in, s, nil)
	if err := renderer.Add(ctx, ag.Chunk{Parts: []ag.Part{{Text: "signed reasoning", Thought: true, Signature: "provider-signature"}, {Text: "answer"}}, Finish: "STOP"}); err != nil {
		t.Fatal(err)
	}
	if err := renderer.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	var first struct {
		Output []json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(renderer.Result, &first); err != nil {
		t.Fatal(err)
	}
	items := []json.RawMessage{raw(map[string]string{"role": "user", "content": "look up a topic"})}
	items = append(items, first.Output...)
	items = append(items, raw(map[string]string{"role": "user", "content": "continue searching"}))
	next, err := d.Responses(ctx, raw(map[string]any{"model": "gemini-test", "input": items, "tools": []any{map[string]string{"type": "web_search"}}, "store": false}))
	if err != nil {
		t.Fatal(err)
	}
	thoughts := 0
	for _, c := range next.Request.Contents {
		for _, p := range c.Parts {
			if p.Thought {
				thoughts++
			}
		}
	}
	if thoughts == 0 {
		t.Fatalf("same actual model lost its reasoning: upstream=%s warnings=%s", next.UpstreamModel(), strings.Join(next.Warnings, "; "))
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
