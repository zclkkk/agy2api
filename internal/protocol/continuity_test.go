package protocol

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
	"github.com/zclkkk/agy2api/internal/state"
)

func TestUnrestorableCompactionFailsRequest(t *testing.T) {
	p := testPipeline{State: testStore(t)}
	for _, token := range []string{"", "foreign", "agy2.!!", "agy2.e30"} {
		_, err := p.Responses(context.Background(), raw(map[string]any{
			"model": "gemini-test",
			"input": []any{
				map[string]string{"type": "compaction", "encrypted_content": token},
				map[string]string{"role": "user", "content": "Continue using the earlier context."},
			},
		}))
		if err == nil || !strings.Contains(err.Error(), "compaction") {
			t.Fatalf("unrestorable compaction was discarded: %v", err)
		}
	}
}

func TestEndWithUser(t *testing.T) {
	user := ag.Content{Role: "user", Parts: []ag.Part{{Text: "hi"}}}
	for name, tc := range map[string]struct {
		contents []ag.Content
		want     []string // role:text of each content's last part
	}{
		"ends with user":   {[]ag.Content{user}, []string{"user:hi"}},
		"assistant prefix": {[]ag.Content{user, {Role: "model", Parts: []ag.Part{{Text: "Red,"}}}}, []string{"user:hi", "model:Red,", "user:Continue."}},
		"reasoning only":   {[]ag.Content{user, {Role: "model", Parts: []ag.Part{{Text: "hmm", Thought: true}, {Signature: "sig"}}}}, []string{"user:hi"}},
		"lone assistant":   {[]ag.Content{{Role: "model", Parts: []ag.Part{{Text: "Hello"}}}}, []string{"model:Hello", "user:Continue."}},
		"pending tool call": {[]ag.Content{user, {Role: "model", Parts: []ag.Part{{FunctionCall: &ag.Call{ID: "c", Name: "f"}}}}},
			[]string{"user:hi", "model:"}},
	} {
		in := RequestIntent{Request: ag.Request{Contents: tc.contents}}
		in.endWithUser()
		var got []string
		for _, c := range in.Request.Contents {
			got = append(got, c.Role+":"+c.Parts[len(c.Parts)-1].Text)
		}
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("%s: got %v want %v", name, got, tc.want)
		}
	}
}

func TestCompactionCapsule(t *testing.T) {
	dir := t.TempDir()
	s, err := state.Open(dir, time.Hour, 100, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if s != nil {
			s.Close()
		}
	}()
	replay := replayStore{s}
	token, err := replay.sealCompaction("the private summary", "gemini-test")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := replay.openCompaction(token); err != nil || got != "the private summary" {
		t.Fatalf("round trip = %q %v", got, err)
	}
	sealed, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, "agy2."))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("the private summary")) {
		t.Fatal("compaction contains a plaintext summary")
	}
	sealed[len(sealed)-1] ^= 1
	tampered := "agy2." + base64.RawURLEncoding.EncodeToString(sealed)
	for _, bad := range []string{"", "gAAAA-openai", "agy2.!!", "agy2.e30", tampered} {
		if _, err := replay.openCompaction(bad); err == nil {
			t.Fatalf("%q must not open", bad)
		}
	}
	foreign := replayStore{testStore(t)}
	if _, err := foreign.openCompaction(token); err == nil {
		t.Fatal("another instance decrypted the summary")
	}
	wrongScope, err := s.Seal("agy2api/replay/v2", compactionCapsule{Summary: "wrong scope"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replay.openCompaction(wrongScope); err == nil {
		t.Fatal("reasoning token accepted as compaction")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = state.Open(dir, time.Hour, 100, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	in, err := (testPipeline{State: s}).Responses(context.Background(), raw(map[string]any{
		"model": "gemini-other",
		"input": []any{map[string]string{"type": "compaction", "encrypted_content": token}, map[string]string{"role": "user", "content": "continue"}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(in.Request.Contents[0].Parts[0].Text, "the private summary") {
		t.Fatal("summary was not restored after restart")
	}
}

func TestSessionAssignment(t *testing.T) {
	conversation := func(texts ...string) *RequestIntent {
		in := &RequestIntent{}
		for i, text := range texts {
			role := "user"
			if i%2 == 1 {
				role = "model"
			}
			in.Request.Contents = append(in.Request.Contents, ag.Content{Role: role, Parts: []ag.Part{{Text: text}}})
		}
		return in
	}
	first, followUp := conversation("a"), conversation("a", "ok", "more")
	first.assignSession(http.Header{})
	followUp.assignSession(http.Header{})
	if first.Request.SessionID != followUp.Request.SessionID || !strings.HasPrefix(first.Request.SessionID, "-") {
		t.Fatal("a conversation and its follow-up must share a session")
	}
	other := conversation("b")
	other.assignSession(http.Header{})
	if other.Request.SessionID == first.Request.SessionID {
		t.Fatal("different conversations must not share a session")
	}
	client := conversation("b")
	client.assignSession(http.Header{"Session-Id": {"codex"}})
	if client.Request.SessionID == other.Request.SessionID {
		t.Fatal("the client's session must win")
	}
	if claudeCodeSession([]byte(`{"user_id":"user_ab_account_cd_session_ef12"}`)) != "ef12" {
		t.Fatal("composite Claude Code user_id format")
	}
	a, b := conversation(), conversation()
	a.assignSession(http.Header{})
	b.assignSession(http.Header{})
	if a.Request.SessionID == b.Request.SessionID {
		t.Fatal("requests without any hint get distinct random sessions")
	}
}
