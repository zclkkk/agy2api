package protocol

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

func TestMessagesEndsWithTextAndRestoresItsSignature(t *testing.T) {
	for _, withThinking := range []bool{false, true} {
		t.Run(map[bool]string{false: "text only", true: "thinking then text"}[withThinking], func(t *testing.T) {
			ctx := context.Background()
			s := testStore(t)
			d := testPipeline{State: s}
			headers := http.Header{"Session-Id": []string{"text-signature-test"}}
			d.headers = headers
			in, err := d.Messages(ctx, []byte(`{"model":"gemini-test","max_tokens":1024,"messages":[{"role":"user","content":"hello"}]}`), false)
			if err != nil {
				t.Fatal(err)
			}
			var blockTypes []string
			r := NewRenderer(in, s, func(name string, event map[string]any) error {
				if name == "content_block_start" {
					blockTypes = append(blockTypes, event["content_block"].(map[string]any)["type"].(string))
				}
				return nil
			})
			if withThinking {
				if err := r.Add(ctx, ag.Chunk{Parts: []ag.Part{{Thought: true, Text: "Plan", Signature: "thought-signature"}}}); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.Add(ctx, ag.Chunk{Parts: []ag.Part{{Text: "Hello "}}}); err != nil {
				t.Fatal(err)
			}
			if err := r.Add(ctx, ag.Chunk{Parts: []ag.Part{{Text: "world", Signature: "text-signature"}}, Finish: "STOP"}); err != nil {
				t.Fatal(err)
			}
			if err := r.Finish(ctx); err != nil {
				t.Fatal(err)
			}
			var reply struct {
				Content []Block `json:"content"`
			}
			if err := json.Unmarshal(r.Result, &reply); err != nil {
				t.Fatal(err)
			}
			if blockTypes[len(blockTypes)-1] != "text" || reply.Content[len(reply.Content)-1].Type != "text" {
				t.Fatal("a synthetic thinking block followed the visible answer")
			}
			if !withThinking && len(reply.Content) != 1 {
				t.Fatal("plain text acquired an extra thinking block")
			}
			body := raw(map[string]any{"model": "gemini-test", "max_tokens": 1024, "messages": []any{
				map[string]any{"role": "user", "content": "hello"}, map[string]any{"role": "assistant", "content": reply.Content}, map[string]any{"role": "user", "content": "continue"},
			}})
			next, err := d.Messages(ctx, body, false)
			if err != nil {
				t.Fatal(err)
			}
			parts := next.Request.Contents[1].Parts
			last := parts[len(parts)-1]
			if last.Text != "Hello world" || last.Signature != "text-signature" {
				t.Fatalf("text signature was lost: %+v", last)
			}
			if withThinking && parts[0].Signature != "thought-signature" {
				t.Fatal("thinking signature changed")
			}
			d.headers = http.Header{"Session-Id": []string{"another-session"}}
			changed, err := d.Messages(ctx, body, false)
			if err != nil {
				t.Fatal(err)
			}
			last = changed.Request.Contents[1].Parts[len(parts)-1]
			if last.Signature != "" || last.Text != "Hello world" {
				t.Fatal("text signature crossed sessions or text was changed")
			}
			d.headers = headers
			var changedBody map[string]json.RawMessage
			if err := json.Unmarshal(body, &changedBody); err != nil {
				t.Fatal(err)
			}
			changedBody["system"] = raw("Different instructions")
			changed, err = d.Messages(ctx, raw(changedBody), false)
			if err != nil {
				t.Fatal(err)
			}
			if changed.Request.Contents[1].Parts[len(parts)-1].Signature != "" {
				t.Fatal("text signature crossed system contexts")
			}
		})
	}
}

func TestTextSignatureCacheSeparatesRepeatedAnswers(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	d := testPipeline{State: s}
	headers := http.Header{"Session-Id": []string{"same-session"}}
	d.headers = headers
	history := []any{map[string]string{"role": "user", "content": "first"}}
	for _, signature := range []string{"signature-one", "signature-two"} {
		in, err := d.Messages(ctx, raw(map[string]any{"model": "gemini-test", "max_tokens": 1024, "messages": history}), false)
		if err != nil {
			t.Fatal(err)
		}
		r := NewRenderer(in, s, nil)
		if err := r.Add(ctx, ag.Chunk{Parts: []ag.Part{{Text: "OK", Signature: signature}}, Finish: "STOP"}); err != nil {
			t.Fatal(err)
		}
		if err := r.Finish(ctx); err != nil {
			t.Fatal(err)
		}
		var reply struct {
			Content []Block `json:"content"`
		}
		if err := json.Unmarshal(r.Result, &reply); err != nil {
			t.Fatal(err)
		}
		history = append(history, map[string]any{"role": "assistant", "content": reply.Content}, map[string]string{"role": "user", "content": "next"})
	}
	in, err := d.Messages(ctx, raw(map[string]any{"model": "gemini-test", "max_tokens": 1024, "messages": history}), false)
	if err != nil {
		t.Fatal(err)
	}
	if in.Request.Contents[1].Parts[0].Signature != "signature-one" || in.Request.Contents[3].Parts[0].Signature != "signature-two" {
		t.Fatal("equal text in different turns shared a signature")
	}

}

func TestMissingTextSignatureRecordsAreReported(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	d := testPipeline{State: s}
	reply := func(session, text, signature string) []Block {
		t.Helper()
		d.headers = http.Header{"Session-Id": []string{session}}
		in, err := d.Messages(ctx, []byte(`{"model":"gemini-test","max_tokens":1024,"messages":[{"role":"user","content":"hello"}]}`), false)
		if err != nil {
			t.Fatal(err)
		}
		r := NewRenderer(in, s, nil)
		if err := r.Add(ctx, ag.Chunk{Parts: []ag.Part{{Text: text, Signature: signature}}, Finish: "STOP"}); err != nil {
			t.Fatal(err)
		}
		if err := r.Finish(ctx); err != nil {
			t.Fatal(err)
		}
		var out struct {
			Content []Block `json:"content"`
		}
		if err := json.Unmarshal(r.Result, &out); err != nil {
			t.Fatal(err)
		}
		return out.Content
	}
	restore := func(session string, assistant []Block) PreparedRequest {
		t.Helper()
		d.headers = http.Header{"Session-Id": []string{session}}
		in, err := d.Messages(ctx, raw(map[string]any{"model": "gemini-test", "max_tokens": 1024, "messages": []any{
			map[string]string{"role": "user", "content": "hello"}, map[string]any{"role": "assistant", "content": assistant}, map[string]string{"role": "user", "content": "next"},
		}}), false)
		if err != nil {
			t.Fatal(err)
		}
		return in
	}
	// A reply that came back unsigned is recorded as such and is not a miss.
	if in := restore("a", reply("a", "unsigned answer", "")); len(in.Warnings) != 0 || in.Request.Contents[1].Parts[0].Signature != "" {
		t.Fatalf("unsigned reply: %v %+v", in.Warnings, in.Request.Contents[1].Parts[0])
	}
	// A signed reply replayed under another session, or text this bridge never produced, is a miss.
	signed := reply("a", "signed answer", "sig")
	for name, in := range map[string]PreparedRequest{
		"other session": restore("b", signed),
		"client text":   restore("a", []Block{{Type: "text", Text: "prefilled by the client"}}),
	} {
		if len(in.Warnings) != 1 || !strings.Contains(in.Warnings[0], "no text signature record for 1") {
			t.Errorf("%s: warnings %v", name, in.Warnings)
		}
	}
	if in := restore("a", signed); len(in.Warnings) != 0 || in.Request.Contents[1].Parts[0].Signature != "sig" {
		t.Fatalf("matching replay: %v %+v", in.Warnings, in.Request.Contents[1].Parts[0])
	}
}
