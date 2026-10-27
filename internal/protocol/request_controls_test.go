package protocol

import (
	"context"
	"strings"
	"testing"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

// Best-effort controls are accepted, and each reports the guarantee that does
// not hold without logging request content.
func TestBestEffortControlsWarn(t *testing.T) {
	for _, tc := range []struct {
		format Format
		body   string
		fields []string
	}{
		{Responses, `{
			"model":"gemini-test","store":false,"truncation":"auto",
			"input":[{"role":"user","content":[{"type":"input_text","text":"hello"},{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgo=","detail":"high"}]}],
			"context_management":[{"type":"compaction","compact_threshold":1000}],
			"top_logprobs":3,"include":["message.output_text.logprobs"],
			"reasoning":{"summary":"concise"},
			"text":{"verbosity":"high","format":{"type":"json_schema","name":"result","strict":true,"schema":{"type":"object"}}},
			"tools":[
				{"type":"function","name":"lookup","strict":true,"parameters":{"type":"object"}},
				{"type":"custom","name":"edit","format":{"type":"grammar","syntax":"regex","definition":"DO_NOT_LOG"}}
			]
		}`, []string{"truncation", "image detail", "context_management", "top_logprobs", "logprobs are not returned", "reasoning.summary", "text.verbosity", "text.format.strict", "tools.strict", "tools.format.grammar"}},
		{Messages, `{
			"model":"gemini-test","max_tokens":100,
			"messages":[{"role":"user","content":[
				{"type":"document","source":{"type":"text","media_type":"text/plain","data":"DO_NOT_LOG"},"citations":{"enabled":true}},
				{"type":"search_result","source":"https://example.com","title":"t","content":[{"type":"text","text":"DO_NOT_LOG"}],"citations":{"enabled":true}},
				{"type":"text","text":"hello"}]}],
			"tools":[{"name":"lookup","strict":true,"input_schema":{"type":"object"}}],
			"context_management":{"edits":[{"type":"clear_tool_uses_20250919"}]},
			"stop_sequences":["DO_NOT_LOG"],
			"output_config":{"format":{"type":"json_schema","schema":{"type":"object"}}}
		}`, []string{"tools.strict", "context_management", "stop_sequences", "output_config.format", "document citations.enabled", "search_result citations.enabled"}},
	} {
		t.Run(string(tc.format), func(t *testing.T) {
			in := prepareControls(t, tc.format, tc.body)
			warnings := strings.Join(in.Warnings, "\n")
			for _, field := range tc.fields {
				if !strings.Contains(warnings, field) {
					t.Fatalf("missing warning for %s: %s", field, warnings)
				}
			}
			if len(in.Warnings) != len(tc.fields) {
				t.Fatalf("unexpected warnings: %s", warnings)
			}
			if strings.Contains(warnings, "DO_NOT_LOG") {
				t.Fatal("request content leaked through warnings")
			}
			if len(in.Request.Tools) == 0 || in.Request.ToolConfig == nil {
				t.Fatal("warnings changed tool availability")
			}
		})
	}
}

// Controls the standard attaches no observable guarantee to are accepted
// quietly, as are explicit no-op values of best-effort controls.
func TestGuaranteedControlsStayQuiet(t *testing.T) {
	for _, tc := range []struct {
		format Format
		body   string
	}{
		{Responses, `{
			"model":"gemini-test","input":"hello","parallel_tool_calls":false,"truncation":"disabled",
			"context_management":[],"service_tier":"priority","prompt_cache_retention":"24h",
			"stream_options":{"include_obfuscation":true},"top_logprobs":0,"max_tool_calls":3,
			"user":"u","safety_identifier":"s","client_metadata":{"k":"v"},
			"include":["reasoning.encrypted_content","web_search_call.results","message.input_image.image_url"],
			"reasoning":{"effort":"low","summary":"none"},
			"tools":[{"type":"function","name":"lookup","strict":false,"parameters":{"type":"object"}}]
		}`},
		{Messages, `{
			"model":"gemini-test","max_tokens":100,"cache_control":{"type":"ephemeral"},
			"system":[{"type":"text","text":"rules","cache_control":{"type":"ephemeral"}}],
			"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral","ttl":"1h"}}]}],
			"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]},
			"service_tier":"standard_only","inference_geo":"us","container":{"id":"c"},
			"tool_choice":{"type":"auto","disable_parallel_tool_use":true},
			"tools":[{"name":"lookup","strict":false,"input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}]
		}`},
	} {
		t.Run(string(tc.format), func(t *testing.T) {
			if in := prepareControls(t, tc.format, tc.body); len(in.Warnings) != 0 {
				t.Fatalf("quiet controls warned: %v", in.Warnings)
			}
		})
	}
}

// A disabled parallel-call limit is reported only when a response breaks it.
func TestParallelCallLimitIsReportedWhenBroken(t *testing.T) {
	call := func(name string) ag.Part {
		return ag.Part{FunctionCall: &ag.Call{Name: name, Args: []byte(`{}`)}}
	}
	for _, tc := range []struct {
		format Format
		body   string
	}{
		{Responses, `{"model":"gemini-test","input":"hi","parallel_tool_calls":false,"tools":[{"type":"function","name":"a"},{"type":"function","name":"b"}]}`},
		{Messages, `{"model":"gemini-test","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"auto","disable_parallel_tool_use":true},"tools":[{"name":"a"},{"name":"b"}]}`},
	} {
		for calls, want := range map[int]bool{1: false, 2: true} {
			s := testStore(t)
			in := prepareControls(t, tc.format, tc.body)
			r := NewRenderer(in, s, nil)
			parts := []ag.Part{call("a"), call("b")}[:calls]
			if err := r.Add(context.Background(), ag.Chunk{Parts: parts, Finish: "STOP"}); err != nil {
				t.Fatal(err)
			}
			if err := r.Finish(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(strings.Join(r.Warnings, "\n"), "is not enforced"); got != want {
				t.Fatalf("%s with %d calls: warnings %v", tc.format, calls, r.Warnings)
			}
			result := string(r.Result)
			if tc.format == Responses && !strings.Contains(result, `"parallel_tool_calls":false`) {
				t.Fatalf("parallel_tool_calls not echoed: %s", result)
			}
			if tc.format == Messages && !strings.Contains(result, `"cache_creation_input_tokens":0`) {
				t.Fatalf("usage lacks cache_creation_input_tokens: %s", result)
			}
		}
	}
}

func prepareControls(t *testing.T, format Format, body string) PreparedRequest {
	t.Helper()
	p := testPipeline{State: testStore(t)}
	var in PreparedRequest
	var err error
	if format == Responses {
		in, err = p.Responses(context.Background(), []byte(body))
	} else {
		in, err = p.Messages(context.Background(), []byte(body), false)
	}
	if err != nil {
		t.Fatal(err)
	}
	return in
}
