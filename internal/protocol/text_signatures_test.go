package protocol

import (
	"context"
	"net/http"
	"testing"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

// Gemini can sign the final text of a Messages reply. The signature is not
// shown to the client; it is recorded for the session and system instructions
// and restored when the same text is replayed.
func TestMessagesTextSignatureIsRestored(t *testing.T) {
	for name, withThinking := range map[string]bool{"text only": false, "thinking then text": true} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := testStore(t)
			p := testPipeline{State: s, headers: http.Header{"Session-Id": {"text-signature-test"}}}
			in := p.mustMessages(t, messages(user("hello")))
			var blockTypes []string
			r := NewRenderer(in, s, func(name string, event map[string]any) error {
				if name == "content_block_start" {
					blockTypes = append(blockTypes, event["content_block"].(map[string]any)["type"].(string))
				}
				return nil
			})
			chunks := []ag.Chunk{{Parts: []ag.Part{{Text: "Hello "}}}, {Parts: []ag.Part{{Text: "world", Signature: "text-signature"}}, Finish: "STOP"}}
			if withThinking {
				chunks = append([]ag.Chunk{{Parts: []ag.Part{{Thought: true, Text: "Plan", Signature: "thought-signature"}}}}, chunks...)
			}
			for _, chunk := range chunks {
				if err := r.Add(ctx, chunk); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.Finish(ctx); err != nil {
				t.Fatal(err)
			}
			reply := messageContent(t, r)
			wantBlocks := 1
			if withThinking {
				wantBlocks = 2
			}
			if len(reply) != wantBlocks || reply[len(reply)-1].Type != "text" || blockTypes[len(blockTypes)-1] != "text" {
				t.Fatalf("reply blocks %v, streamed %v", reply, blockTypes)
			}
			body := messages(user("hello"), assistant(reply), user("continue"))
			parts := p.mustMessages(t, body).Request.Contents[1].Parts
			last := parts[len(parts)-1]
			if last.Text != "Hello world" || last.Signature != "text-signature" {
				t.Fatalf("text signature was lost: %+v", last)
			}
			if withThinking && parts[0].Signature != "thought-signature" {
				t.Fatal("thinking signature changed")
			}
			// Another session or other system instructions get the text unsigned.
			other := testPipeline{State: s, headers: http.Header{"Session-Id": {"another-session"}}}
			if last := other.mustMessages(t, body).Request.Contents[1].Parts[len(parts)-1]; last.Signature != "" || last.Text != "Hello world" {
				t.Fatal("text signature crossed sessions or text was changed")
			}
			body["system"] = "Different instructions"
			if p.mustMessages(t, body).Request.Contents[1].Parts[len(parts)-1].Signature != "" {
				t.Fatal("text signature crossed system contexts")
			}
		})
	}
}

func TestTextSignatureCacheSeparatesRepeatedAnswers(t *testing.T) {
	s := testStore(t)
	p := testPipeline{State: s, headers: http.Header{"Session-Id": {"same-session"}}}
	history := []any{user("first")}
	for _, signature := range []string{"signature-one", "signature-two"} {
		r := turn(t, s, p.mustMessages(t, messages(history...)), ag.Part{Text: "OK", Signature: signature})
		history = append(history, assistant(messageContent(t, r)), user("next"))
	}
	in := p.mustMessages(t, messages(history...))
	if in.Request.Contents[1].Parts[0].Signature != "signature-one" || in.Request.Contents[3].Parts[0].Signature != "signature-two" {
		t.Fatal("equal text in different turns shared a signature")
	}
}

func TestMissingTextSignatureRecordsAreReported(t *testing.T) {
	s := testStore(t)
	in := func(session string) testPipeline {
		return testPipeline{State: s, headers: http.Header{"Session-Id": {session}}}
	}
	reply := func(session, text, signature string) []Block {
		t.Helper()
		return messageContent(t, turn(t, s, in(session).mustMessages(t, messages(user("hello"))), ag.Part{Text: text, Signature: signature}))
	}
	restore := func(session string, content []Block) PreparedRequest {
		t.Helper()
		return in(session).mustMessages(t, messages(user("hello"), assistant(content), user("next")))
	}
	// A reply that came back unsigned is recorded as such and is not a miss.
	if got := restore("a", reply("a", "unsigned answer", "")); len(got.Warnings) != 0 || got.Request.Contents[1].Parts[0].Signature != "" {
		t.Fatalf("unsigned reply: %v %+v", got.Warnings, got.Request.Contents[1].Parts[0])
	}
	// A signed reply under another session, or text this service never
	// produced, is a miss.
	signed := reply("a", "signed answer", "sig")
	for name, got := range map[string]PreparedRequest{
		"other session": restore("b", signed),
		"client text":   restore("a", []Block{{Type: "text", Text: "prefilled by the client"}}),
	} {
		if len(got.Warnings) != 1 || !warned(got.Warnings, "no text signature record for 1") {
			t.Errorf("%s: warnings %v", name, got.Warnings)
		}
	}
	if got := restore("a", signed); len(got.Warnings) != 0 || got.Request.Contents[1].Parts[0].Signature != "sig" {
		t.Fatalf("matching replay: %v %+v", got.Warnings, got.Request.Contents[1].Parts[0])
	}
}

// Only Messages text carries signatures to restore; Responses requests and
// token counts do not look them up.
func TestTextSignaturesAreOnlyRestoredForMessages(t *testing.T) {
	s := testStore(t)
	p := testPipeline{State: s}
	history := []any{user("hi"), assistant("earlier answer"), user("more")}
	if in := p.mustResponses(t, map[string]any{"model": "gemini-test", "input": history}); warned(in.Warnings, "text signature") {
		t.Fatalf("Responses looked up text signatures: %v", in.Warnings)
	}
	intent, err := (Decoder{}).Messages(raw(map[string]any{"model": "gemini-test", "messages": history}), true)
	if err != nil {
		t.Fatal(err)
	}
	counted, err := (Planner{State: s}).Prepare(context.Background(), intent, testCatalog, CountTokens, nil)
	if err != nil || warned(counted.Warnings, "text signature") {
		t.Fatalf("token count looked up text signatures: %v %v", err, counted.Warnings)
	}
	if in := p.mustMessages(t, messages(history...)); !warned(in.Warnings, "no text signature record for 1") {
		t.Fatalf("a Messages miss should be reported: %v", in.Warnings)
	}
}
