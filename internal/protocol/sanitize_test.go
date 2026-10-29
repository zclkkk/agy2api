package protocol

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

func TestSanitizeClientMarkersPreservesBusinessInstructions(t *testing.T) {
	const identity = "You are a Claude agent, built on Anthropic's Claude Agent SDK."
	const billing = "x-anthropic-billing-header: cc_version=2.1.310; cch=PRIVATE_VALUE;"
	for _, tc := range []struct {
		name, input, want string
		billing, identity int
	}{
		{"mixed lines", "Keep rule one.\n" + billing + "\n" + identity + "\n  Keep rule two.  \n", "Keep rule one.\n  Keep rule two.  \n", 1, 1},
		{"CRLF and indentation", "\t" + billing + "\r\n" + identity + " \r\nKeep\r\n\r\n", "Keep\r\n\r\n", 1, 1},
		{"CR separators", billing + "\rKeep\r" + identity, "Keep\r", 1, 1},
		{"Unicode separators", billing + "\u2028Keep\u2029" + identity, "Keep\u2029", 1, 1},
		{"no final newline", "Keep\n" + identity, "Keep\n", 0, 1},
		{"only markers", billing + "\n" + identity + "\n", "", 1, 1},
		{"blank remainder", "\n" + billing + "\n\t\n", "", 1, 0},
		{"CLI identity", "You are Claude Code, Anthropic's official CLI for Claude.\nKeep", "Keep", 0, 1},
		{"SDK CLI identity", "You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK.\nKeep", "Keep", 0, 1},
		{"ordinary names", "Explain Claude, Anthropic, API, and proxy.\nKeep all identifiers.", "Explain Claude, Anthropic, API, and proxy.\nKeep all identifiers.", 0, 0},
		{"embedded prefix", "Document x-anthropic-billing-header: and its meaning.", "Document x-anthropic-billing-header: and its meaning.", 0, 0},
		{"extended identity", identity + " Keep this extra instruction.", identity + " Keep this extra instruction.", 0, 0},
		{"quoted identity", "> " + identity + "\n\"" + identity + "\"", "> " + identity + "\n\"" + identity + "\"", 0, 0},
		{"fenced example", billing + "\n```text\n" + identity + "\n" + billing + "\n```\nKeep\n", "```text\n" + identity + "\n" + billing + "\n```\nKeep\n", 1, 0},
		{"tilde fence", "~~~\n" + identity + "\n~~~\n" + identity, "~~~\n" + identity + "\n~~~\n", 0, 1},
		{"long fence", "````\n```\n" + identity + "\n````\n" + identity, "````\n```\n" + identity + "\n````\n", 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := &ag.Content{Role: "user", Parts: []ag.Part{{Text: tc.input}}}
			in := RequestIntent{requestFields: requestFields{Request: ag.Request{System: original}}}
			in.SanitizeClientMarkers()
			if tc.want == "" {
				if in.Request.System != nil {
					t.Fatalf("expected no system instruction, got %+v", in.Request.System)
				}
			} else if in.Request.System == nil || len(in.Request.System.Parts) != 1 || in.Request.System.Parts[0].Text != tc.want || in.Request.System.Role != "user" {
				t.Fatalf("system=%+v, want text %q", in.Request.System, tc.want)
			}
			if original.Parts[0].Text != tc.input {
				t.Fatal("the source system instruction was mutated")
			}
			if tc.billing+tc.identity == 0 {
				if len(in.Warnings) != 0 || in.Request.System != original {
					t.Fatal("a no-op request was changed")
				}
			} else {
				want := RequestIntent{}
				want.warn("sanitize_client_markers: billing_header=%d claude_identity=%d", tc.billing, tc.identity)
				if !reflect.DeepEqual(in.Warnings, want.Warnings) {
					t.Fatalf("warnings=%v", in.Warnings)
				}
			}
			if strings.Contains(strings.Join(in.Warnings, " "), "PRIVATE_VALUE") || strings.Contains(strings.Join(in.Warnings, " "), identity) {
				t.Fatal("removed text was logged")
			}
			before := string(raw(in))
			in.SanitizeClientMarkers()
			if string(raw(in)) != before {
				t.Fatal("sanitization is not idempotent")
			}
		})
	}
}

func TestSanitizeClientMarkersAcrossSystemBlocks(t *testing.T) {
	const identity = "You are a Claude agent, built on Anthropic's Claude Agent SDK."
	in := RequestIntent{requestFields: requestFields{Request: ag.Request{System: &ag.Content{Parts: []ag.Part{
		{Text: "x-anthropic-billing-header: cc_version=test;"},
		{Text: "Example follows:\n```text\n"},
		{Text: identity + "\n"},
		{Text: "```\n" + identity + "\nBusiness instructions"},
	}}}}}
	in.SanitizeClientMarkers()
	want := []ag.Part{{Text: "Example follows:\n```text\n"}, {Text: identity + "\n"}, {Text: "```\nBusiness instructions"}}
	if in.Request.System == nil || !reflect.DeepEqual(in.Request.System.Parts, want) {
		t.Fatalf("system=%+v", in.Request.System)
	}
	if len(in.Warnings) != 1 || in.Warnings[0] != "sanitize_client_markers: billing_header=1 claude_identity=1" {
		t.Fatal(in.Warnings)
	}
}

func TestSanitizeClientMarkersLeavesHistoryToolsAndSignedDataAlone(t *testing.T) {
	const marker = "You are a Claude agent, built on Anthropic's Claude Agent SDK."
	in := RequestIntent{requestFields: requestFields{
		Model: "gemini-test", Options: map[string]json.RawMessage{"instructions": raw(marker)}, Metadata: raw(map[string]string{"session_id": "session-test"}),
		OriginalItems: []json.RawMessage{raw(map[string]string{"role": "user", "content": marker})},
		Request: ag.Request{
			System: &ag.Content{Parts: []ag.Part{{Text: marker}, {Text: "Keep this system instruction."}}},
			Contents: []ag.Content{
				{Role: "user", Parts: []ag.Part{{Text: marker}}},
				{Role: "model", Parts: []ag.Part{{Text: marker, Thought: true, Signature: "signed-thinking"}, {FunctionCall: &ag.Call{ID: "call-1", Name: "echo", Args: raw(map[string]string{"text": marker})}, Signature: "signed-call"}}},
				{Role: "user", Parts: []ag.Part{{FunctionResponse: &ag.Result{ID: "call-1", Name: "echo", Response: raw(map[string]string{"result": marker})}}}},
			},
			Tools:     []ag.Tool{{Functions: []ag.Function{{Name: "echo", Description: marker, Parameters: raw(map[string]any{"type": "object", "description": marker})}}}},
			SessionID: "stable-session",
		},
	}}
	before := string(raw(in))
	systemBefore := in.Request.System
	in.SanitizeClientMarkers()
	if len(in.Request.System.Parts) != 1 || in.Request.System.Parts[0].Text != "Keep this system instruction." {
		t.Fatal(in.Request.System)
	}
	in.Request.System, in.Warnings = systemBefore, nil
	if string(raw(in)) != before {
		t.Fatal("sanitization changed data outside the system instruction")
	}
	for _, part := range []ag.Part{{Text: marker, Thought: true, Signature: "opaque"}, {Text: marker, Signature: "opaque"}} {
		in := RequestIntent{requestFields: requestFields{Request: ag.Request{System: &ag.Content{Parts: []ag.Part{part}}}}}
		in.SanitizeClientMarkers()
		if !reflect.DeepEqual(in.Request.System.Parts[0], part) || len(in.Warnings) != 0 {
			t.Fatal("signed system data was edited")
		}
	}
	var empty RequestIntent
	empty.SanitizeClientMarkers()
	if empty.Request.System != nil || len(empty.Warnings) != 0 {
		t.Fatal("missing system instruction was changed")
	}
}
