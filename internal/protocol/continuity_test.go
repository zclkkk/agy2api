package protocol

import (
	"net/http"
	"strings"
	"testing"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

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
	token := sealCompaction("the summary", "gemini-test")
	if got, ok := openCompaction(token); !ok || got != "the summary" {
		t.Fatalf("round trip = %q %v", got, ok)
	}
	for _, bad := range []string{"", "gAAAA-openai", compactionPrefix + "!!", compactionPrefix + "e30"} {
		if _, ok := openCompaction(bad); ok {
			t.Fatalf("%q must not open", bad)
		}
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
