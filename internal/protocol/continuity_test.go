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
		in := Input{Request: ag.Request{Contents: tc.contents}}
		in.EndWithUser()
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

func TestSessionFallbacks(t *testing.T) {
	inherited := Input{Request: ag.Request{SessionID: "-42", Contents: []ag.Content{{Role: "user", Parts: []ag.Part{{Text: "a"}}}}}}
	inherited.AssignSession(http.Header{})
	if inherited.Request.SessionID != "-42" {
		t.Fatal("a session restored from replay state must be kept")
	}
	inherited.AssignSession(http.Header{"Session-Id": {"codex"}})
	if inherited.Request.SessionID == "-42" {
		t.Fatal("the client's session must win")
	}
	if claudeCodeSession([]byte(`{"user_id":"user_ab_account_cd_session_ef12"}`)) != "ef12" {
		t.Fatal("legacy Claude Code user_id format")
	}
	a, b := Input{}, Input{}
	a.AssignSession(http.Header{})
	b.AssignSession(http.Header{})
	if a.Request.SessionID == b.Request.SessionID || !strings.HasPrefix(a.Request.SessionID, "-") {
		t.Fatal("requests without any hint get distinct random sessions")
	}
}
