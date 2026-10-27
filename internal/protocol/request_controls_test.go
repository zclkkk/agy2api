package protocol

import (
	"context"
	"strings"
	"testing"
)

func TestUnimplementedControlsWarnWithoutRejectingRequests(t *testing.T) {
	for _, tc := range []struct {
		format Format
		body   string
		fields []string
	}{
		{Responses, `{
			"model":"gemini-test","input":"hello","store":false,
			"parallel_tool_calls":false,"truncation":"auto",
			"context_management":[{"type":"compaction","compact_threshold":1000}],
			"prompt_cache_retention":"24h","service_tier":"priority",
			"stream_options":{"include_obfuscation":true},"top_logprobs":3,
			"reasoning":{"summary":"concise"},
			"text":{"verbosity":"high","format":{"type":"json_schema","name":"result","strict":true,"schema":{"type":"object"}}},
			"tools":[
				{"type":"function","name":"lookup","strict":true,"parameters":{"type":"object"}},
				{"type":"custom","name":"edit","format":{"type":"grammar","syntax":"regex","definition":"DO_NOT_LOG"}}
			]
		}`, []string{"parallel_tool_calls", "truncation", "context_management", "prompt_cache_retention", "service_tier", "stream_options", "top_logprobs", "reasoning.summary", "text.verbosity", "text.format.strict", "tools.strict", "tools.format.grammar"}},
		{Messages, `{
			"model":"gemini-test","max_tokens":100,
			"messages":[{"role":"user","content":[{"type":"document","source":{"type":"text","media_type":"text/plain","data":"DO_NOT_LOG"},"citations":{"enabled":true},"cache_control":{"type":"ephemeral"}},{"type":"text","text":"hello"}]}],
			"tools":[{"name":"lookup","strict":true,"input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}],
			"tool_choice":{"type":"auto","disable_parallel_tool_use":true},
			"context_management":{"edits":[{"type":"clear_tool_uses_20250919"}]},
			"service_tier":"standard_only","container":{"id":"DO_NOT_LOG"},"inference_geo":"us",
			"output_config":{"format":{"type":"json_schema","schema":{"type":"object"}}}
		}`, []string{"tools.strict", "disable_parallel_tool_use", "context_management", "service_tier", "container", "inference_geo", "output_config.format", "cache_control", "citations.enabled"}},
	} {
		t.Run(string(tc.format), func(t *testing.T) {
			p := testPipeline{State: testStore(t)}
			var in PreparedRequest
			var err error
			if tc.format == Responses {
				in, err = p.Responses(context.Background(), []byte(tc.body))
			} else {
				in, err = p.Messages(context.Background(), []byte(tc.body), false)
			}
			if err != nil {
				t.Fatal(err)
			}
			warnings := strings.Join(in.Warnings, "\n")
			for _, field := range tc.fields {
				if !strings.Contains(warnings, field) {
					t.Fatalf("missing warning for %s: %s", field, warnings)
				}
			}
			if strings.Contains(warnings, "DO_NOT_LOG") {
				t.Fatal("request content leaked through warnings")
			}
			if tc.format == Messages && strings.Count(warnings, "cache_control") != 1 {
				t.Fatal("repeated controls were not deduplicated")
			}
			if len(in.Request.Tools) == 0 || in.Request.ToolConfig == nil {
				t.Fatal("warnings changed tool availability")
			}
		})
	}
}

func TestNoopControlsStayQuiet(t *testing.T) {
	p := testPipeline{State: testStore(t)}
	in, err := p.Responses(context.Background(), []byte(`{
		"model":"gemini-test","input":"hello","parallel_tool_calls":true,"truncation":"disabled",
		"context_management":[],"service_tier":"auto","prompt_cache_retention":null,
		"stream_options":{"include_obfuscation":false},"top_logprobs":0,
		"tools":[{"type":"function","name":"lookup","strict":false,"parameters":{"type":"object"}}]
	}`))
	if err != nil || len(in.Warnings) != 0 {
		t.Fatalf("no-op Responses controls: %v %v", err, in.Warnings)
	}
	in, err = p.Messages(context.Background(), []byte(`{
		"model":"gemini-test","max_tokens":100,"messages":[{"role":"user","content":"hello"}],
		"context_management":{"edits":[]},"service_tier":"auto","inference_geo":"global","container":null,
		"tool_choice":{"type":"auto","disable_parallel_tool_use":false},
		"tools":[{"name":"lookup","strict":false,"input_schema":{"type":"object"}}]
	}`), false)
	if err != nil || len(in.Warnings) != 0 {
		t.Fatalf("no-op Messages controls: %v %v", err, in.Warnings)
	}
}
