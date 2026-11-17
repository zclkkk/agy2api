package protocol

import (
	"encoding/json"
	"strings"
	"testing"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

// Some models return thought text a request excluded. The client does not
// receive it, and a signature it carries still replays.
func TestUnrequestedThoughtsAreDropped(t *testing.T) {
	s := testStore(t)
	p := testPipeline{State: s}
	parts := []ag.Part{{Thought: true, Text: "secret "}, {Thought: true, Text: "plan", Signature: "thought-signature"}, {Text: "answer"}}
	r := turn(t, s, p.mustResponses(t, `{"model":"gemini-test","input":"hello","store":false}`), parts...)
	output := responseOutput(t, r)
	if strings.Contains(string(raw(output)), "secret") || strings.Contains(string(raw(output)), "plan") {
		t.Fatalf("unrequested thoughts returned: %s", raw(output))
	}
	input := append([]json.RawMessage{raw(map[string]any{"role": "user", "content": "hello"})}, output...)
	native := p.mustResponses(t, map[string]any{"model": "gemini-test", "input": input, "store": false}).Request
	if got := native.Contents[1].Parts; len(got) != 2 || !got[0].Thought || got[0].Text != "" || got[0].Signature != "thought-signature" || got[1].Text != "answer" {
		t.Fatalf("replay = %s", raw(got))
	}
	m := turn(t, s, p.mustMessages(t, messages(user("hello"))), parts...)
	for _, b := range messageContent(t, m) {
		if b.Thinking != "" || b.Type == "text" && b.Text != "answer" {
			t.Fatalf("unrequested thoughts returned: %+v", b)
		}
	}
}

// Each request carries a thinking configuration: the client's effort or
// budget, or its API's default, fitted to the levels the model offers.
func TestThinkingConfig(t *testing.T) {
	p := testPipeline{State: testStore(t)}
	responses := func(model, reasoning string) PreparedRequest {
		t.Helper()
		body := `{"model":"` + model + `","input":"x"`
		if reasoning != "" {
			body += `,"reasoning":` + reasoning
		}
		return p.mustResponses(t, body+`}`)
	}
	messages := func(model, fields string) PreparedRequest {
		t.Helper()
		return p.mustMessages(t, `{"model":"`+model+`","max_tokens":100,`+fields+`"messages":[{"role":"user","content":"x"}]}`)
	}
	level := func(n int) ag.Thinking { return ag.Thinking{Level: n} }
	budget := func(n int) ag.Thinking { return ag.Thinking{Budget: &n} }
	shown := func(th ag.Thinking) ag.Thinking { th.Include = true; return th }
	for _, tc := range []struct {
		name     string
		got      PreparedRequest
		want     ag.Thinking
		upstream string
		warning  string
	}{
		// Responses defaults to medium; a summary shows the thoughts.
		{"responses default", responses("gemini-test", ""), level(2), "gemini-test", ""},
		{"summary only", responses("gemini-test", `{"summary":"auto"}`), shown(level(2)), "gemini-test", ""},
		{"summary none", responses("gemini-test", `{"effort":"low","summary":"none"}`), level(1), "gemini-test", ""},
		{"effort above the model's levels", responses("gemini-test", `{"effort":"xhigh"}`), level(3), "gemini-test", ""},
		{"effort below the model's levels", responses("gemini-test", `{"effort":"minimal"}`), level(1), "gemini-test", ""},
		{"unknown effort", responses("gemini-test", `{"effort":"turbo"}`), level(2), "gemini-test", `ignored reasoning effort "turbo"`},
		{"effort none", responses("gemini-test", `{"effort":"none","summary":"auto"}`), budget(128), "gemini-test", "cannot disable thinking"},
		// Messages leaves thinking off unless asked; thinking defaults to high.
		{"messages default", messages("gemini-test", ""), budget(128), "gemini-test", ""},
		{"messages effort without thinking", messages("gemini-test", `"output_config":{"effort":"low"},`), level(1), "gemini-test", ""},
		{"adaptive", messages("gemini-test", `"thinking":{"type":"adaptive"},`), shown(level(3)), "gemini-test", ""},
		{"enabled without a budget", messages("gemini-test", `"thinking":{"type":"enabled"},`), shown(level(3)), "gemini-test", ""},
		{"budget below the minimum", messages("gemini-test", `"thinking":{"type":"enabled","budget_tokens":50},`), shown(budget(128)), "gemini-test", ""},
		{"disabled outranks effort", messages("gemini-test", `"thinking":{"type":"disabled"},"output_config":{"effort":"high"},`), budget(128), "gemini-test", "cannot disable thinking"},
		// Variants serve their own levels.
		{"variant default", responses("gemini-levels", ""), level(1), "gemini-levels-low", ""},
		{"variant effort", responses("gemini-levels", `{"effort":"xhigh"}`), level(100), "gemini-levels-xhigh", ""},
		{"variant max", messages("gemini-levels", `"thinking":{"type":"adaptive"},"output_config":{"effort":"max"},`), shown(level(100)), "gemini-levels-xhigh", ""},
		{"variant adaptive", messages("gemini-levels", `"thinking":{"type":"adaptive"},`), shown(level(3)), "gemini-levels-high", ""},
		{"variant budget follows the effort", messages("gemini-levels", `"thinking":{"type":"enabled","budget_tokens":4096},"output_config":{"effort":"low"},`), shown(budget(4096)), "gemini-levels-low", ""},
		{"variant effort none", responses("gemini-levels", `{"effort":"none"}`), budget(32), "gemini-levels-low", "cannot disable thinking"},
		// A model the catalog configures by budget keeps that budget.
		{"budget model default", responses("gemini-budget", `{"effort":"high","summary":"auto"}`), shown(budget(1024)), "gemini-budget", ""},
		{"budget model client budget", messages("gemini-budget", `"thinking":{"type":"enabled","budget_tokens":2048},`), shown(budget(2048)), "gemini-budget", ""},
		{"budget model off", messages("gemini-budget", ""), budget(128), "gemini-budget", ""},
	} {
		th := tc.got.Request.Generation.Thinking
		if th == nil || th.Level != tc.want.Level || th.Include != tc.want.Include || (th.Budget == nil) != (tc.want.Budget == nil) || (th.Budget != nil && *th.Budget != *tc.want.Budget) {
			t.Errorf("%s: thinking %+v, want %+v", tc.name, th, tc.want)
		}
		if tc.got.Upstream != tc.upstream {
			t.Errorf("%s: upstream %q, want %q", tc.name, tc.got.Upstream, tc.upstream)
		}
		if tc.warning == "" && len(tc.got.Warnings) > 0 || tc.warning != "" && !warned(tc.got.Warnings, tc.warning) {
			t.Errorf("%s: warnings %v, want %q", tc.name, tc.got.Warnings, tc.warning)
		}
	}
	// A model without thinking gets no thinking configuration.
	if in := responses("gemini-plain", `{"effort":"high"}`); in.Request.Generation.Thinking != nil || in.Upstream != "gemini-plain" {
		t.Errorf("model without thinking: %+v upstream %q", in.Request.Generation.Thinking, in.Upstream)
	}
	// Variants are offered through the shared ID only.
	if _, err := p.Responses(`{"model":"gemini-levels-high","input":"x"}`); err == nil || !strings.Contains(err.Error(), "model is not available") {
		t.Errorf("variant ID accepted: %v", err)
	}
}
