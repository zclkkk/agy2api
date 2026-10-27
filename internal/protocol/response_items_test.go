package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

func TestResponseInputItemsAreNormalized(t *testing.T) {
	in, err := (Decoder{}).Responses([]byte(`{"model":"gemini-test","input":[
		{"role":"user","content":"first"},
		{"role":"assistant","id":"msg_client","phase":"commentary","content":"answer"},
		{"type":"function_call","call_id":"call1","name":"lookup","arguments":"{}"},
		{"type":"function_call_output","call_id":"call1","output":"result"},
		{"role":"user","content":[{"type":"input_text","text":"see this"},{"type":"input_image","image_url":"data:image/png;base64,YWJj"}]}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(in.OriginalItems) != 5 {
		t.Fatalf("items = %s", raw(in.OriginalItems))
	}
	ids := map[string]bool{}
	for i, data := range in.OriginalItems {
		var item ResponseItem
		if err := json.Unmarshal(data, &item); err != nil {
			t.Fatal(err)
		}
		if item.ID == "" || ids[item.ID] || item.Type == "" || item.Status != "completed" {
			t.Fatalf("non-canonical item: %s", data)
		}
		ids[item.ID] = true
		if item.Type == "message" {
			var content []map[string]any
			if err := json.Unmarshal(item.Content, &content); err != nil {
				t.Fatal(err)
			}
			want := "input_text"
			if item.Role == "assistant" {
				want = "output_text"
			}
			if content[0]["type"] != want {
				t.Fatalf("message content: %s", item.Content)
			}
			if i == 4 && content[1]["image_url"] != "data:image/png;base64,YWJj" {
				t.Fatal("image input changed")
			}
		}
	}
	var assistant map[string]any
	_ = json.Unmarshal(in.OriginalItems[1], &assistant)
	if assistant["id"] != "msg_client" || assistant["phase"] != "commentary" {
		t.Fatal("client metadata lost")
	}
}

func TestResponseInputItemIDsSurviveContinuation(t *testing.T) {
	s := testStore(t)
	p := testPipeline{State: s}
	ctx := context.Background()
	in, err := p.Responses(ctx, []byte(`{"model":"gemini-test","input":"first"}`))
	if err != nil {
		t.Fatal(err)
	}
	r := NewRenderer(in, s, nil)
	if err := r.Add(ctx, ag.Chunk{Parts: []ag.Part{{Text: "answer"}}, Finish: "STOP"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	stored, err := LoadResponse(ctx, s, r.id)
	if err != nil {
		t.Fatal(err)
	}
	next, err := p.Responses(ctx, raw(map[string]any{"model": "gemini-test", "input": "second", "previous_response_id": r.id}))
	if err != nil {
		t.Fatal(err)
	}
	if string(stored.Items[0]) != string(next.OriginalItems[0]) {
		t.Fatal("stored input identity changed on continuation")
	}
	var response struct {
		Output []json.RawMessage `json:"output"`
	}
	_ = json.Unmarshal(r.Result, &response)
	if len(next.OriginalItems) != 3 || string(next.OriginalItems[1]) != string(response.Output[0]) {
		t.Fatal("prior output identity changed")
	}
}

func TestResponseInputItemIDsRejectDuplicates(t *testing.T) {
	intent, err := (Decoder{}).Responses([]byte(`{"model":"gemini-test","store":false,"input":[
		{"id":"msg_duplicate","role":"user","content":"first"},
		{"id":"msg_duplicate","role":"user","content":"second"},
		{"id":"msg_last","role":"user","content":"last"}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	planner := Planner{State: testStore(t)}
	for name, operation := range map[string]Operation{"generate": Generate, "count": CountTokens, "compact": CompactResponse} {
		t.Run(name, func(t *testing.T) {
			_, err := planner.Prepare(context.Background(), intent, testCatalog, operation, nil)
			var validation *Error
			if !errors.As(err, &validation) || validation.Param != "input" || !strings.Contains(validation.Message, "duplicate") {
				t.Fatalf("duplicate input item IDs were not rejected: %v", err)
			}
		})
	}
}

func TestResponseInputItemIDsRejectHistoryCollisions(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	in, err := (testPipeline{State: s}).Responses(ctx, []byte(`{"model":"gemini-test","input":[{"id":"msg_client","role":"user","content":"first"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	r := NewRenderer(in, s, nil)
	if err := r.Add(ctx, ag.Chunk{Parts: []ag.Part{{Text: "answer"}}, Finish: "STOP"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	var response struct {
		Output []ResponseItem `json:"output"`
	}
	if err := json.Unmarshal(r.Result, &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Output) != 1 {
		t.Fatalf("output = %s", r.Result)
	}
	planner := Planner{State: s}
	for name, id := range map[string]string{"stored input": "msg_client", "stored output": response.Output[0].ID} {
		t.Run(name, func(t *testing.T) {
			intent, err := (Decoder{}).Responses(raw(map[string]any{
				"model": "gemini-test", "previous_response_id": r.id,
				"input": []any{map[string]string{"id": id, "role": "user", "content": "next"}},
			}))
			if err != nil {
				t.Fatal(err)
			}
			for name, operation := range map[string]Operation{"generate": Generate, "count": CountTokens, "compact": CompactResponse} {
				t.Run(name, func(t *testing.T) {
					_, err := planner.Prepare(ctx, intent, testCatalog, operation, nil)
					var validation *Error
					if !errors.As(err, &validation) || validation.Param != "input" || !strings.Contains(validation.Message, "duplicate") {
						t.Fatalf("historical input item ID collision was not rejected: %v", err)
					}
				})
			}
		})
	}
}
