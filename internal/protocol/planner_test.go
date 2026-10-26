package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

func TestDecodeDefersStateAccessAndPlannerPreservesIntent(t *testing.T) {
	ctx := context.Background()
	intent, err := (Decoder{}).Responses([]byte(`{"model":"gemini-test","input":[{"type":"function_call_output","call_id":"call_old","output":"done"}],"previous_response_id":"resp_missing"}`))
	if err != nil {
		t.Fatalf("pure decoding must not need stored history: %v", err)
	}
	s := testStore(t)
	if _, err := (Planner{State: s}).Prepare(ctx, intent, testCatalog, Generate, nil); err == nil {
		t.Fatal("missing history was not checked by the planner")
	}
	intent, err = (Decoder{}).Responses([]byte(`{"model":"gemini-test","input":"hello","tools":[{"type":"web_search"},{"type":"function","name":"lookup","parameters":{"type":"object"}}],"tool_choice":{"type":"web_search"}}`))
	if err != nil {
		t.Fatal(err)
	}
	before := string(raw(intent))
	planner := Planner{State: s}
	first, err := planner.Prepare(ctx, intent, testCatalog, Generate, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := planner.Prepare(ctx, intent, testCatalog, Generate, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw(intent)) != before || !reflect.DeepEqual(first, second) {
		t.Fatal("planning changed client intent or was not deterministic")
	}
	for key := range intent.Tools {
		delete(intent.Tools, key)
	}
	if len(first.Tools) != 1 {
		t.Fatal("prepared tool identities alias the decoded request")
	}
}

func TestLegacyHistoryMigrationAndReplayVersions(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	legacy := StoredResponse{Model: "gemini-test", Contents: []ag.Content{
		{Role: "user", Parts: []ag.Part{{Text: "first"}}},
		{Role: "model", Parts: []ag.Part{{Text: "old thought", Thought: true, Signature: "legacy-signature"}, {Text: "answer"}}},
	}, Items: []json.RawMessage{raw(map[string]string{"role": "developer", "content": "Always use JSON."}), raw(map[string]string{"role": "user", "content": "first"})},
		Response: raw(map[string]any{"output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]string{"type": "output_text", "text": "answer"}}}}})}
	if err := s.Put(ctx, "response", "legacy", legacy); err != nil {
		t.Fatal(err)
	}
	in, err := (testPipeline{State: s}).Responses(ctx, []byte(`{"model":"gemini-test","input":"next","previous_response_id":"legacy"}`))
	if err != nil {
		t.Fatal(err)
	}
	if in.Request.System == nil || in.Request.System.Parts[0].Text != "Always use JSON." || len(in.Request.Contents) != 3 {
		t.Fatal("legacy visible history or developer instructions were lost")
	}
	for _, c := range in.Request.Contents {
		for _, p := range c.Parts {
			if p.Thought || p.Signature != "" {
				t.Fatal("legacy signatures without provenance were trusted")
			}
		}
	}
	r := NewRenderer(in, s, nil)
	if err := r.Add(ctx, ag.Chunk{Parts: []ag.Part{{Text: "new"}}, Finish: "STOP"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	stored, err := (replayStore{s}).response(ctx, r.id)
	if err != nil || stored.Version != recordVersion || stored.UpstreamModel != "gemini-test" || stored.HistorySystem == nil {
		t.Fatalf("new response did not use versioned history: %v %+v", err, stored)
	}
	store := replayStore{s}
	for _, version := range []string{"v1", "v2"} {
		replay := Replay{Parts: []ag.Part{{Text: "plan", Thought: true, Signature: "sig"}}}
		var token string
		if version == "v1" {
			token, err = s.Seal("gemini-search", replay)
		} else {
			token, err = store.seal("gemini-search", replay)
		}
		if err != nil {
			t.Fatal(err)
		}
		got, err := store.open("gemini-search", token)
		if err != nil || len(got.Parts) != 1 || got.Parts[0].Signature != "sig" {
			t.Fatalf("%s replay lost: %v %+v", version, err, got)
		}
		if _, err := store.open("gemini-test", token); err == nil {
			t.Fatalf("%s crossed upstream models", version)
		}
		mid := len(token) / 2
		replacement := "A"
		if token[mid] == 'A' {
			replacement = "B"
		}
		if _, err := store.open("gemini-search", token[:mid]+replacement+token[mid+1:]); err == nil {
			t.Fatalf("%s accepted a modified carrier", version)
		}
	}
}

func TestReplayStorageFailureIsNotReportedAsCacheMiss(t *testing.T) {
	s := testStore(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := (testPipeline{State: s}).Messages(context.Background(), []byte(`{"model":"gemini-test","max_tokens":100,"messages":[{"role":"user","content":"start"},{"role":"assistant","content":[{"type":"tool_use","id":"toolu_agy_missing","name":"lookup","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_agy_missing","content":"done"}]}]}`), false)
	var validation *Error
	if err == nil || errors.As(err, &validation) || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("storage failure hidden as a cache miss: %v", err)
	}
}

func TestPlannerRejectsEmptyConversationAfterReplay(t *testing.T) {
	ctx := context.Background()
	planner := Planner{State: testStore(t)}
	for _, body := range []string{
		`{"model":"gemini-test","input":[{"role":"developer","content":"Use JSON."}]}`,
		`{"model":"gemini-test","input":[{"type":"compaction_trigger"}]}`,
		`{"model":"gemini-test","input":[{"type":"reasoning","encrypted_content":"foreign"}]}`,
		`{"model":"gemini-test","input":[{"type":"reasoning","encrypted_content":"foreign"},{"type":"compaction_trigger"}]}`,
	} {
		intent, err := (Decoder{}).Responses([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		for _, operation := range []Operation{Generate, CountTokens, CompactResponse} {
			_, err := planner.Prepare(ctx, intent, testCatalog, operation, nil)
			var validation *Error
			if !errors.As(err, &validation) || validation.Param != "input" {
				t.Fatalf("empty conversation accepted for operation %d: %v", operation, err)
			}
		}
	}
	intent, err := (Decoder{}).Messages([]byte(`{"model":"gemini-test","max_tokens":100,"messages":[]}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := planner.Prepare(ctx, intent, testCatalog, Generate, nil); err == nil {
		t.Fatal("empty Messages conversation accepted")
	}
}

func TestToolPolicyRejectsUnknownAndSeparatesEncodedNames(t *testing.T) {
	d := testPipeline{State: testStore(t)}
	for _, body := range []string{
		`{"model":"gemini-test","input":"x","tool_choice":{"type":"web_search"}}`,
		`{"model":"gemini-test","input":"x","tools":[{"type":"function","name":"read","parameters":{"type":"object"}}],"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"unknown"}]}}`,
		`{"model":"gemini-test","input":"x","tool_choice":"required"}`,
		`{"model":"gemini-test","input":"x","tools":[{"type":"function","name":"read"},{"type":"function","name":"read"}]}`,
	} {
		if _, err := d.Responses(context.Background(), []byte(body)); err == nil {
			t.Fatalf("invalid tool policy accepted: %s", body)
		}
	}
	encoded := toolSymbol(ToolName{Namespace: "files", Name: "read"})
	if nativeName(encoded) == encoded || nativeName(nativeName("bad name")) == nativeName("bad name") {
		t.Fatal("encoded and literal user tool names collide")
	}
	in, err := d.Responses(context.Background(), []byte(`{"model":"gemini-test","input":"x","tools":[{"type":"function","name":"run","parameters":{"type":"object"}},{"type":"custom","name":"run"}],"tool_choice":{"type":"custom","name":"run"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Tools) != 2 || len(in.Request.Tools) != 1 || !in.PublicTool(in.Request.Tools[0].Functions[0].Name).Custom {
		t.Fatal("tool type was lost from identity or explicit selection")
	}
}
