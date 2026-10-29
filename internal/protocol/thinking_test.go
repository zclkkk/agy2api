package protocol

import (
	"testing"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

func TestGeminiThinkingConfig(t *testing.T) {
	p := testPipeline{State: testStore(t)}
	thinking := func(body string) (*ag.Thinking, []string) {
		t.Helper()
		in := p.mustResponses(t, body)
		return in.Request.Generation.Thinking, in.Warnings
	}
	// Reasoning text is returned only when a summary is requested.
	if th, _ := thinking(`{"model":"gemini-test","input":"x","reasoning":{"effort":"xhigh"}}`); th.Level != "high" || th.Budget != nil || th.Include {
		t.Fatalf("effort without summary: %+v", th)
	}
	if th, _ := thinking(`{"model":"gemini-test","input":"x","reasoning":{"effort":"xhigh","summary":"auto"}}`); th.Level != "high" || !th.Include {
		t.Fatalf("effort with summary: %+v", th)
	}
	if th, warnings := thinking(`{"model":"gemini-test","input":"x","reasoning":{"effort":"low","summary":"none"}}`); th.Level != "low" || th.Include || len(warnings) != 0 {
		t.Fatalf("summary none: %+v %v", th, warnings)
	}
	// A forced tool choice keeps thinking on.
	if th, _ := thinking(`{"model":"gemini-test","input":"x","tool_choice":"required","reasoning":{"effort":"minimal"},"tools":[{"type":"function","name":"f"}]}`); th.Level != "low" {
		t.Fatalf("forced tool choice: %+v", th)
	}
	if th, _ := thinking(`{"model":"gemini-test","input":"x","reasoning":{"summary":"auto"}}`); th.Budget == nil || *th.Budget != -1 {
		t.Fatalf("adaptive: %+v", th)
	}
	// Gemini cannot turn thinking off; the minimum budget is used and thoughts are hidden.
	if th, warnings := thinking(`{"model":"gemini-test","input":"x","reasoning":{"effort":"none"}}`); th.Budget == nil || *th.Budget != 128 || th.Include || !warned(warnings, "cannot disable thinking") {
		t.Fatalf("disabled: %+v %v", th, warnings)
	}
	in := p.mustMessages(t, `{"model":"gemini-test","max_tokens":100,"thinking":{"type":"enabled","budget_tokens":50},"messages":[{"role":"user","content":"x"}]}`)
	if b := in.Request.Generation.Thinking.Budget; b == nil || *b != 128 || !in.Request.Generation.Thinking.Include {
		t.Fatalf("budget below the model minimum: %+v", in.Request.Generation.Thinking)
	}
	// Messages shows thinking only when thinking is enabled, not for effort alone.
	in = p.mustMessages(t, `{"model":"gemini-test","max_tokens":100,"output_config":{"effort":"low"},"messages":[{"role":"user","content":"x"}]}`)
	if in.Request.Generation.Thinking.Level != "low" || in.Request.Generation.Thinking.Include {
		t.Fatalf("effort without thinking: %+v", in.Request.Generation.Thinking)
	}
}
