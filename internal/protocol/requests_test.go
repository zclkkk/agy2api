package protocol

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

// A trimmed copy of the request shape Claude Code 2.1 sends.
const claudeCodeRequest = `{
 "model":"gemini-test","max_tokens":32000,"stream":true,
 "system":[{"type":"text","text":"You are Claude Code."},{"type":"text","text":"Rules","cache_control":{"type":"ephemeral"}}],
 "messages":[
  {"role":"user","content":[{"type":"text","text":"find the tool","cache_control":{"type":"ephemeral"}}]},
  {"role":"system","content":[{"type":"text","text":"# Environment"}]},
  {"role":"assistant","content":[{"type":"tool_use","id":"toolu_client_1","name":"ToolSearch","input":{"query":"notebook"}}]},
  {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_client_1","content":[{"type":"tool_reference","tool_name":"NotebookEdit"}],"cache_control":{"type":"ephemeral"}}]}
 ],
 "tools":[
  {"name":"ToolSearch","description":"search tools","input_schema":{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}},
  {"name":"NotebookEdit","description":"edit","defer_loading":true,"input_schema":{"type":"object","properties":{"cell":{"type":"integer","exclusiveMinimum":0}}}},
  {"name":"Hidden","description":"never referenced","defer_loading":true,"input_schema":{"type":"object"}},
  {"name":"mcp__srv.example__do","description":"dotted MCP name","input_schema":{"type":"object","cache_control":{"type":"ephemeral"}}},
  {"name":"bad name/x","description":"needs mapping","input_schema":{"type":"object"}}
 ],
 "tool_choice":{"type":"auto","disable_parallel_tool_use":true},
 "metadata":{"user_id":"u"},
 "thinking":{"type":"adaptive","display":"omitted"},
 "context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]},
 "output_config":{"effort":"high"},
 "some_future_field":1
}`

func TestClaudeCodeRequestShapeIsAccepted(t *testing.T) {
	in := testPipeline{State: testStore(t)}.mustMessages(t, claudeCodeRequest)
	if !in.HideThinking || in.Request.System == nil || len(in.Request.System.Parts) != 3 {
		t.Fatalf("system or thinking display lost: %+v", in)
	}
	declared := map[string]bool{}
	for _, tool := range in.Request.Tools {
		for _, f := range tool.Functions {
			declared[in.PublicTool(f.Name).Name] = true
			if strings.Contains(string(f.Parameters), "$schema") || strings.Contains(string(f.Parameters), "exclusiveMinimum") {
				t.Fatalf("schema not cleaned: %s", f.Parameters)
			}
			if invalidNameChars.MatchString(f.Name) {
				t.Fatalf("invalid native tool name %q", f.Name)
			}
		}
	}
	// Deferred tools are declared once a tool_reference names them.
	if !declared["ToolSearch"] || !declared["NotebookEdit"] || declared["Hidden"] || !declared["mcp__srv.example__do"] || !declared["bad name/x"] {
		t.Fatalf("deferred tool handling wrong: %v", declared)
	}
	if in.Tools["mcp__srv.example__do"].Name == "" {
		t.Fatal("a name Gemini accepts must be sent unchanged")
	}
	if th := in.Request.Generation.Thinking; th == nil || th.Level != "high" || !th.Include {
		t.Fatalf("effort not applied to adaptive thinking: %+v", th)
	}
	// Caching hints and the keep-all thinking edit are no-ops; the parallel-call
	// limit is reported only if a response breaks it. Only the unknown field warns.
	if len(in.Warnings) != 1 || !strings.Contains(in.Warnings[0], "some_future_field") || !in.SingleCall {
		t.Fatalf("warnings = %v, single call = %v", in.Warnings, in.SingleCall)
	}
}

// A trimmed Codex CLI 0.164 request with hosted web search disabled.
const codexRequest = `{
 "model":"gemini-test","stream":true,"store":false,"parallel_tool_calls":false,"tool_choice":"auto",
 "include":["reasoning.encrypted_content"],"prompt_cache_key":"k","client_metadata":{"session_id":"s"},
 "reasoning":{"effort":"xhigh","summary":"auto"},
 "input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"rules"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],
 "tools":[
  {"type":"function","name":"exec_command","strict":false,"parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"],"additionalProperties":false}},
  {"type":"namespace","name":"multi_agent_v1","description":"agents","tools":[{"type":"function","name":"close_agent","strict":false,"parameters":{"type":"object","properties":{"target":{"type":"string"}}}}]},
  {"type":"custom","name":"apply_patch","description":"patch","format":{"type":"grammar","syntax":"lark","definition":"start: line+"}}
 ]
}`

func TestCodexRequestShapeIsAccepted(t *testing.T) {
	in := testPipeline{State: testStore(t)}.mustResponses(t, codexRequest)
	identities := map[ToolName]bool{}
	for _, tool := range in.Request.Tools {
		for _, f := range tool.Functions {
			identities[in.PublicTool(f.Name)] = true
		}
	}
	for _, want := range []ToolName{{Name: "exec_command"}, {Namespace: "multi_agent_v1", Name: "close_agent"}, {Name: "apply_patch", Custom: true}} {
		if !identities[want] {
			t.Fatalf("tool %+v not declared: %v", want, identities)
		}
	}
	if in.Tools["exec_command"].Name != "exec_command" {
		t.Fatal("a legal function name must be sent unchanged")
	}
	if th := in.Request.Generation.Thinking; th == nil || th.Level != "high" {
		t.Fatalf("xhigh effort not mapped: %+v", th)
	}
}

// Platform features, and history only another provider produces, are rejected
// rather than silently dropped.
func TestUnsupportedInputIsRejected(t *testing.T) {
	unsupported := func(err error) bool {
		var e *Error
		return errors.As(err, &e) && strings.HasPrefix(e.Message, "unsupported")
	}
	p := testPipeline{State: testStore(t)}
	for _, body := range []string{
		`{"model":"gemini-test","input":"hello","background":true}`,
		`{"model":"gemini-test","input":"hello","conversation":"conv_1"}`,
		`{"model":"gemini-test","input":[{"type":"item_reference","id":"msg_1"}]}`,
		`{"model":"gemini-test","input":[{"role":"user","content":[{"type":"input_text","text":"hello"},{"type":"input_image","file_id":"missing"}]}]}`,
		`{"model":"gemini-test","input":[{"role":"assistant","content":[{"type":"refusal","refusal":"no"}]},{"role":"user","content":"next"}]}`,
	} {
		var validation *Error
		if _, err := p.Responses(body); !errors.As(err, &validation) {
			t.Errorf("accepted: %s (%v)", body, err)
		}
	}
	for _, block := range []string{
		`{"type":"server_tool_use","id":"srvtoolu_1","name":"web_fetch","input":{}}`,
		`{"type":"web_fetch_tool_result","tool_use_id":"srvtoolu_1","content":{}}`,
		`{"type":"redacted_thinking","data":"opaque"}`,
	} {
		body := `{"model":"gemini-test","max_tokens":10,"messages":[{"role":"user","content":"q"},{"role":"assistant","content":[` + block + `]},{"role":"user","content":"more"}]}`
		if _, err := p.Messages(body); !unsupported(err) {
			t.Errorf("%s: %v", block, err)
		}
	}
}

func TestEmptyConversationIsRejected(t *testing.T) {
	planner := Planner{State: testStore(t)}
	for _, body := range []string{
		`{"model":"gemini-test","input":[{"role":"developer","content":"Use JSON."}]}`,
		`{"model":"gemini-test","input":[{"type":"compaction_trigger"}]}`,
		`{"model":"gemini-test","input":[{"type":"reasoning","encrypted_content":"foreign"}]}`,
		`{"model":"gemini-test","input":[{"type":"reasoning","encrypted_content":"foreign"},{"type":"compaction_trigger"}]}`,
	} {
		intent, err := (Decoder{}).Responses([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		for _, operation := range []Operation{Generate, CountTokens, CompactResponse} {
			_, err := planner.Prepare(context.Background(), intent, testCatalog, operation, nil)
			var validation *Error
			if !errors.As(err, &validation) || validation.Param != "input" {
				t.Fatalf("empty conversation accepted for operation %d: %v", operation, err)
			}
		}
	}
	if _, err := (testPipeline{State: testStore(t)}).Messages(messages()); err == nil {
		t.Fatal("empty Messages conversation accepted")
	}
}

func TestDecodingIsPureAndPlanningDeterministic(t *testing.T) {
	ctx := context.Background()
	intent, err := (Decoder{}).Responses([]byte(`{"model":"gemini-test","input":[{"type":"function_call_output","call_id":"call_old","output":"done"}],"previous_response_id":"resp_missing"}`))
	if err != nil {
		t.Fatalf("decoding must not need stored history: %v", err)
	}
	s := testStore(t)
	if _, err := (Planner{State: s}).Prepare(ctx, intent, testCatalog, Generate, nil); err == nil {
		t.Fatal("missing history was not checked by the planner")
	}
	intent, err = (Decoder{}).Responses([]byte(`{"model":"gemini-test","input":"hello","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"tool_choice":{"type":"function","name":"lookup"}}`))
	if err != nil {
		t.Fatal(err)
	}
	before := string(raw(intent))
	planner := Planner{State: s}
	first, err := planner.Prepare(ctx, intent, testCatalog, Generate, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := planner.Prepare(ctx, intent, testCatalog, Generate, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw(intent)) != before || !reflect.DeepEqual(first, second) {
		t.Fatal("planning changed client intent or was not deterministic")
	}
	for key := range intent.Tools {
		delete(intent.Tools, key)
	}
	if len(first.Tools) != 1 {
		t.Fatal("prepared tool identities alias the decoded request")
	}
}

func TestModelsOutsideTheCatalogAreRejected(t *testing.T) {
	p := testPipeline{State: testStore(t)}
	if _, err := p.Responses(`{"model":"gemini-unknown","input":"hi"}`); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("unknown model accepted: %v", err)
	}
}

func TestRequestsEndWithAUserTurn(t *testing.T) {
	u := ag.Content{Role: "user", Parts: []ag.Part{{Text: "hi"}}}
	for name, tc := range map[string]struct {
		contents []ag.Content
		want     []string // role:text of each content's last part
	}{
		"ends with user":   {[]ag.Content{u}, []string{"user:hi"}},
		"assistant prefix": {[]ag.Content{u, {Role: "model", Parts: []ag.Part{{Text: "Red,"}}}}, []string{"user:hi", "model:Red,", "user:Continue."}},
		"reasoning only":   {[]ag.Content{u, {Role: "model", Parts: []ag.Part{{Text: "hmm", Thought: true}, {Signature: "sig"}}}}, []string{"user:hi"}},
		"lone assistant":   {[]ag.Content{{Role: "model", Parts: []ag.Part{{Text: "Hello"}}}}, []string{"model:Hello", "user:Continue."}},
		"pending tool call": {[]ag.Content{u, {Role: "model", Parts: []ag.Part{{FunctionCall: &ag.Call{ID: "c", Name: "f"}}}}},
			[]string{"user:hi", "model:"}},
	} {
		in := RequestIntent{requestFields: requestFields{Request: ag.Request{Contents: tc.contents}}}
		in.endWithUser()
		var got []string
		for _, c := range in.Request.Contents {
			got = append(got, c.Role+":"+c.Parts[len(c.Parts)-1].Text)
		}
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("%s: got %v want %v", name, got, tc.want)
		}
	}
	// Both formats apply it to a prefilled assistant turn.
	p := testPipeline{State: testStore(t)}
	for _, in := range []PreparedRequest{
		p.mustMessages(t, messages(user("List colors."), assistant("Red,"))),
		p.mustResponses(t, `{"model":"gemini-test","input":[{"role":"user","content":"List colors."},{"role":"assistant","content":"Red,"}]}`),
	} {
		contents := in.Request.Contents
		if len(contents) != 3 || contents[1].Parts[0].Text != "Red," || contents[2].Parts[0].Text != "Continue." || !warned(in.Warnings, "appended a user turn") {
			t.Fatalf("%s: %s %v", in.Format, raw(contents), in.Warnings)
		}
	}
}

// One client session maps to one upstream session, whichever way the client
// names it; without a name, a conversation is keyed by its first user message.
func TestSessionIDsFollowTheClientSession(t *testing.T) {
	s := testStore(t)
	session := func(headers http.Header, body string, responses bool) string {
		t.Helper()
		p := testPipeline{State: s, headers: headers}
		var in PreparedRequest
		if responses {
			in = p.mustResponses(t, body)
		} else {
			in = p.mustMessages(t, body)
		}
		if !nativeSessionID.MatchString(in.Request.SessionID) {
			t.Fatalf("session ID %q is not in the native format", in.Request.SessionID)
		}
		return in.Request.SessionID
	}
	responses := func(text string) string { return `{"model":"gemini-test","input":"` + text + `"}` }
	codex := http.Header{"Session-Id": {"01a173fc-cb86-72f0-a250-ce0969dcc4b5"}}
	if session(codex, responses("one"), true) != session(codex, responses("two"), true) {
		t.Fatal("one Codex session must map to one upstream session")
	}
	key := `{"model":"gemini-test","prompt_cache_key":"thread-7","input":"`
	if session(nil, key+`one"}`, true) != session(nil, key+`two"}`, true) {
		t.Fatal("prompt_cache_key must identify the session")
	}
	claude := func(text string) string {
		return `{"model":"gemini-test","max_tokens":100,"metadata":{"user_id":"{\"device_id\":\"d\",\"session_id\":\"98a88910\"}"},"messages":[{"role":"user","content":"` + text + `"}]}`
	}
	viaMetadata := session(nil, claude("one"), false)
	if viaMetadata != session(nil, claude("two"), false) || viaMetadata != session(http.Header{"X-Claude-Code-Session-Id": {"98a88910"}}, claude("three"), false) {
		t.Fatal("Claude Code metadata and header must identify the same session")
	}
	if claudeCodeSession([]byte(`{"user_id":"user_ab_account_cd_session_ef12"}`)) != "ef12" {
		t.Fatal("composite Claude Code user_id format")
	}
	first := session(nil, responses("same start"), true)
	if first != session(nil, `{"model":"gemini-test","input":[{"role":"user","content":"same start"},{"role":"assistant","content":"ok"},{"role":"user","content":"more"}]}`, true) {
		t.Fatal("follow-ups of one conversation must share a session")
	}
	if first == session(nil, responses("another start"), true) {
		t.Fatal("different conversations must not share a session")
	}
	a, b := &RequestIntent{}, &RequestIntent{}
	a.assignSession(http.Header{})
	b.assignSession(http.Header{})
	if a.Request.SessionID == b.Request.SessionID {
		t.Fatal("requests without any hint get distinct random sessions")
	}
}

var nativeSessionID = regexp.MustCompile(`^-\d{1,19}$`)
