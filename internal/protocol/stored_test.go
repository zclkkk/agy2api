package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

func TestStoredChainKeepsHistorySystemButNotInstructions(t *testing.T) {
	s := testStore(t)
	p := testPipeline{State: s}
	ctx := context.Background()
	in := p.mustResponses(t, `{"model":"gemini-test","instructions":"old instructions","input":[{"role":"developer","content":"Always use JSON."},{"role":"user","content":"first"}]}`)
	r := turn(t, s, in, ag.Part{Text: `{"answer":"first"}`})
	stored, err := LoadResponse(ctx, s, r.id)
	if err != nil || stored.Version != recordVersion || stored.Model != "gemini-test" || stored.HistorySystem == nil {
		t.Fatalf("response not stored in the current format: %v", err)
	}
	if len(stored.Items) != 2 {
		t.Fatalf("input_items must hold only the input: %s", raw(stored.Items))
	}
	next := p.mustResponses(t, map[string]any{"model": "gemini-test", "input": "next", "previous_response_id": r.id})
	// The developer message is part of the history; top-level instructions
	// belong to the request that sent them.
	if next.Request.System == nil || len(next.Request.System.Parts) != 1 || next.Request.System.Parts[0].Text != "Always use JSON." {
		t.Fatalf("system = %s", raw(next.Request.System))
	}
	if len(next.Request.Contents) != 3 || len(next.OriginalItems) != 4 {
		t.Fatalf("context lost: %s", raw(next.Request.Contents))
	}
}

func TestStoredChainCanContinueWithOnlyInstructionsOrCompaction(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	r := turn(t, s, testPipeline{State: s}.mustResponses(t, `{"model":"gemini-test","input":"first"}`), ag.Part{Text: "answer"})
	for _, tc := range []struct {
		name      string
		item      map[string]string
		operation Operation
		last      string
	}{
		{"instructions", map[string]string{"role": "developer", "content": "Use JSON from now on."}, Generate, continuation},
		{"count instructions", map[string]string{"role": "developer", "content": "Use JSON from now on."}, CountTokens, continuation},
		{"compaction trigger", map[string]string{"type": "compaction_trigger"}, Generate, compactionPrompt},
		{"compaction endpoint", map[string]string{"role": "developer", "content": "Use JSON from now on."}, CompactResponse, compactionPrompt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			intent, err := (Decoder{}).Responses(raw(map[string]any{"model": "gemini-test", "previous_response_id": r.id, "input": []any{tc.item}}))
			if err != nil {
				t.Fatalf("decoder rejected input before loading its history: %v", err)
			}
			next, err := (Planner{State: s}).Prepare(ctx, intent, testCatalog, tc.operation, nil)
			if err != nil {
				t.Fatal(err)
			}
			contents := next.Request.Contents
			if len(contents) != 3 || contents[0].Parts[0].Text != "first" || contents[1].Parts[0].Text != "answer" || contents[2].Parts[0].Text != tc.last {
				t.Fatal("continuation did not retain and use the stored conversation")
			}
			if tc.item["role"] == "developer" && (next.Request.System == nil || next.Request.System.Parts[0].Text != tc.item["content"]) {
				t.Fatal("continuation lost the new developer instruction")
			}
		})
	}
}

func TestStoredResponseRequiresCurrentFormat(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	r := turn(t, s, testPipeline{State: s}.mustResponses(t, `{"model":"gemini-test","input":"first"}`), ag.Part{Text: "answer"})
	stored, err := LoadResponse(ctx, s, r.id)
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []int{0, recordVersion - 1, recordVersion + 1} {
		stored.Version = version
		if err := s.Put(ctx, "response", "unsupported", stored); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadResponse(ctx, s, "unsupported"); err == nil {
			t.Fatalf("unsupported record version %d accepted", version)
		}
	}
}

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
	ctx := context.Background()
	s := testStore(t)
	p := testPipeline{State: s}
	r := turn(t, s, p.mustResponses(t, `{"model":"gemini-test","input":"first"}`), ag.Part{Text: "answer"})
	stored, err := LoadResponse(ctx, s, r.id)
	if err != nil {
		t.Fatal(err)
	}
	next := p.mustResponses(t, map[string]any{"model": "gemini-test", "input": "second", "previous_response_id": r.id})
	if string(stored.Items[0]) != string(next.OriginalItems[0]) {
		t.Fatal("stored input identity changed on continuation")
	}
	if len(next.OriginalItems) != 3 || string(next.OriginalItems[1]) != string(responseOutput(t, r)[0]) {
		t.Fatal("prior output identity changed")
	}
}

// Input item IDs must be unique across the request and the history it
// continues, for every operation.
func TestResponseInputItemIDsRejectDuplicates(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	r := turn(t, s, testPipeline{State: s}.mustResponses(t, `{"model":"gemini-test","input":[{"id":"msg_client","role":"user","content":"first"}]}`), ag.Part{Text: "answer"})
	output := responseItems(t, r)
	if len(output) != 1 {
		t.Fatalf("output = %s", r.Result)
	}
	planner := Planner{State: s}
	for name, body := range map[string]any{
		"within the input": `{"model":"gemini-test","store":false,"input":[{"id":"msg_duplicate","role":"user","content":"first"},{"id":"msg_duplicate","role":"user","content":"second"},{"id":"msg_last","role":"user","content":"last"}]}`,
		"stored input":     map[string]any{"model": "gemini-test", "previous_response_id": r.id, "input": []any{map[string]string{"id": "msg_client", "role": "user", "content": "next"}}},
		"stored output":    map[string]any{"model": "gemini-test", "previous_response_id": r.id, "input": []any{map[string]string{"id": output[0].ID, "role": "user", "content": "next"}}},
	} {
		intent, err := (Decoder{}).Responses(toJSON(body))
		if err != nil {
			t.Fatal(err)
		}
		for _, operation := range []Operation{Generate, CountTokens, CompactResponse} {
			_, err := planner.Prepare(ctx, intent, testCatalog, operation, nil)
			var validation *Error
			if !errors.As(err, &validation) || validation.Param != "input" || !strings.Contains(validation.Message, "duplicate") {
				t.Fatalf("%s, operation %d: duplicate IDs accepted: %v", name, operation, err)
			}
		}
	}
}
