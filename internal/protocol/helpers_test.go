package protocol

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
	"github.com/zclkkk/agy2api/internal/state"
)

// testCatalog offers two models, so replay tests can switch between them.
var testCatalog = ag.Catalog{Models: map[string]json.RawMessage{
	"gemini-test":  json.RawMessage(`{"minThinkingBudget":128}`),
	"gemini-other": json.RawMessage(`{}`),
}}

func testStore(t *testing.T) *state.Store {
	t.Helper()
	s, err := state.Open(t.TempDir(), time.Hour, 1000, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// testPipeline decodes and plans requests as the HTTP handlers do.
type testPipeline struct {
	State   *state.Store
	headers http.Header
}

func (p testPipeline) prepare(in RequestIntent, err error, operation Operation) (PreparedRequest, error) {
	if err != nil {
		return PreparedRequest{}, err
	}
	return (Planner{State: p.State}).Prepare(context.Background(), in, testCatalog, operation, p.headers)
}

func (p testPipeline) Responses(body any) (PreparedRequest, error) {
	in, err := (Decoder{}).Responses(toJSON(body))
	return p.prepare(in, err, Generate)
}

func (p testPipeline) Messages(body any) (PreparedRequest, error) {
	in, err := (Decoder{}).Messages(toJSON(body), false)
	return p.prepare(in, err, Generate)
}

// mustResponses and mustMessages prepare requests that must be valid.
func (p testPipeline) mustResponses(t *testing.T, body any) PreparedRequest {
	t.Helper()
	in, err := p.Responses(body)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

func (p testPipeline) mustMessages(t *testing.T, body any) PreparedRequest {
	t.Helper()
	in, err := p.Messages(body)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

// toJSON accepts a request body as JSON text or as a value to marshal.
func toJSON(body any) []byte {
	switch b := body.(type) {
	case string:
		return []byte(b)
	case []byte:
		return b
	}
	return raw(body)
}

// turn renders one complete upstream reply to a prepared request.
func turn(t *testing.T, s *state.Store, in PreparedRequest, parts ...ag.Part) *Renderer {
	t.Helper()
	r := NewRenderer(in, s, nil)
	if err := r.Add(context.Background(), ag.Chunk{Parts: parts, Finish: "STOP"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(context.Background()); err != nil {
		t.Fatal(err)
	}
	return r
}

// responseOutput and messageContent decode what a client receives.
func responseOutput(t *testing.T, r *Renderer) []json.RawMessage {
	t.Helper()
	var out struct {
		Output []json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(r.Result, &out); err != nil {
		t.Fatal(err)
	}
	return out.Output
}

func responseItems(t *testing.T, r *Renderer) []ResponseItem {
	t.Helper()
	var items []ResponseItem
	for _, data := range responseOutput(t, r) {
		var item ResponseItem
		if err := json.Unmarshal(data, &item); err != nil {
			t.Fatal(err)
		}
		items = append(items, item)
	}
	return items
}

func messageContent(t *testing.T, r *Renderer) []Block {
	t.Helper()
	var out struct {
		Content []Block `json:"content"`
	}
	if err := json.Unmarshal(r.Result, &out); err != nil {
		t.Fatal(err)
	}
	return out.Content
}

// user and assistant build Messages turns.
func user(content any) map[string]any { return map[string]any{"role": "user", "content": content} }
func assistant(content any) map[string]any {
	return map[string]any{"role": "assistant", "content": content}
}

func messages(turns ...any) map[string]any {
	return map[string]any{"model": "gemini-test", "max_tokens": 1024, "messages": turns}
}

func warned(warnings []string, text string) bool {
	return strings.Contains(strings.Join(warnings, "\n"), text)
}
