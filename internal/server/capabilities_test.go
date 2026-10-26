package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

func TestUnsupportedCapabilitiesAreRejectedBeforeExecution(t *testing.T) {
	_, s := newService(t, nil, func(w http.ResponseWriter) {
		// The image model is identified by catalog metadata, not its name.
		io.WriteString(w, `{"models":{"gemini-test":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"},"gemini-canvas":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"}},"imageGenerationModelIds":["gemini-canvas"],"webSearchModelIds":["gemini-test"]}`)
	}, func(w http.ResponseWriter, r *http.Request) {
		t.Error("unsupported capability reached upstream generation")
		http.Error(w, "unexpected generation", 500)
	})
	responses := map[string]string{
		"image model":   `"model":"gemini-canvas"`,
		"tool type":     `"tools":[{"type":"unsupported_tool"}]`,
		"tool choice":   `"tool_choice":{"type":"unsupported_tool"}`,
		"allowed tools": `"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"unsupported_tool"}]}`,
		"nested tool":   `"tools":[{"type":"namespace","name":"files","tools":[{"type":"unsupported_tool"}]}]`,
		"disabled tool": `"tools":[{"type":"unsupported_tool"}],"tool_choice":"none"`,
		"history item":  `"input":[{"type":"unsupported_item"},{"role":"user","content":"continue"}]`,
	}
	messages := map[string]string{
		"image model":   `"model":"gemini-canvas"`,
		"tool type":     `"tools":[{"type":"unsupported_tool","name":"tool"}]`,
		"tool choice":   `"tool_choice":{"type":"unsupported_tool"}`,
		"disabled tool": `"tools":[{"type":"unsupported_tool","name":"tool"}],"tool_choice":{"type":"none"}`,
		"history block": `"messages":[{"role":"assistant","content":[{"type":"unsupported_block"}]},{"role":"user","content":"continue"}]`,
	}
	for _, group := range []struct {
		paths []string
		base  string
		cases map[string]string
	}{
		{[]string{"/v1/responses", "/v1/responses/input_tokens", "/v1/responses/compact"}, `{"model":"gemini-test","input":"hello"}`, responses},
		{[]string{"/v1/messages", "/v1/messages/count_tokens"}, helloMessages, messages},
	} {
		for _, path := range group.paths {
			for name, fields := range group.cases {
				t.Run(path+"/"+name, func(t *testing.T) {
					var body, overrides map[string]any
					if err := json.Unmarshal([]byte(group.base), &body); err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal([]byte("{"+fields+"}"), &overrides); err != nil {
						t.Fatal(err)
					}
					for key, value := range overrides {
						body[key] = value
					}
					for _, stream := range []bool{false, true} {
						body["stream"] = stream
						data, err := json.Marshal(body)
						if err != nil {
							t.Fatal(err)
						}
						res, out := send(t, s, path, string(data), nil)
						if res.StatusCode != http.StatusBadRequest || !strings.Contains(out, "not supported") && !strings.Contains(out, "unsupported") {
							t.Fatalf("stream=%v: expected explicit 400, got %d %s", stream, res.StatusCode, out)
						}
						if !strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
							t.Fatal("validation error opened an SSE stream")
						}
					}
				})
			}
		}
	}
}

func TestSameNamedFunctionsRemainClientTools(t *testing.T) {
	_, s := newService(t, nil, staticModels, func(w http.ResponseWriter, r *http.Request) {
		var envelope ag.Envelope
		if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
			t.Error(err)
			return
		}
		if envelope.RequestType != ag.KindAgent || envelope.Model != "gemini-test" || len(envelope.Request.Tools) != 1 {
			t.Errorf("function request was rerouted: %+v", envelope)
			return
		}
		tool := envelope.Request.Tools[0]
		if tool.GoogleSearch != nil || len(tool.Functions) != 1 {
			t.Errorf("function became a hosted tool: %+v", tool)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		frame(w, chunk([]ag.Part{{FunctionCall: &ag.Call{Name: tool.Functions[0].Name, Args: json.RawMessage(`{}`)}, Signature: "tool-signature"}}, "STOP", nil))
	})
	for _, name := range []string{"web_search", "WebSearch", "image_generation", "generate_image"} {
		for _, path := range []string{"/v1/responses", "/v1/messages"} {
			body := `{"model":"gemini-test","input":"run the tool","tools":[{"type":"function","name":"` + name + `","parameters":{"type":"object"}}],"tool_choice":{"type":"function","name":"` + name + `"}}`
			want := `"type":"function_call"`
			if path == "/v1/messages" {
				body = `{"model":"gemini-test","max_tokens":100,"messages":[{"role":"user","content":"run the tool"}],"tools":[{"name":"` + name + `","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"` + name + `"}}`
				want = `"type":"tool_use"`
			}
			res, out := send(t, s, path, body, nil)
			if res.StatusCode != 200 || !strings.Contains(out, want) || !strings.Contains(out, `"name":"`+name+`"`) {
				t.Fatalf("%s %s: %d %s", path, name, res.StatusCode, out)
			}
		}
	}
}

func TestUnexpectedImageOutputFailsWithoutSuccess(t *testing.T) {
	for _, partial := range []bool{false, true} {
		s, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			if partial {
				frame(w, chunk([]ag.Part{{Text: "starting"}}, "", nil))
			}
			frame(w, chunk([]ag.Part{{InlineData: &ag.Blob{MIMEType: "image/png", Data: "aGVsbG8="}}}, "STOP", nil))
		})
		for _, path := range []string{"/v1/responses", "/v1/messages"} {
			for _, stream := range []bool{false, true} {
				res, out := post(t, s, path, map[string]any{"model": "gemini-test", "input": "hello", "messages": []map[string]string{{"role": "user", "content": "hello"}}, "max_tokens": 100, "stream": stream})
				if !stream || !partial {
					if res.StatusCode != http.StatusBadGateway {
						t.Fatalf("%s: expected 502, got %d %s", path, res.StatusCode, out)
					}
				} else if !strings.Contains(string(out), "event: response.failed") && !strings.Contains(string(out), "event: error") {
					t.Fatalf("%s: missing terminal SSE error: %s", path, out)
				}
				if strings.Contains(string(out), "image_generation_call") || strings.Contains(string(out), "event: response.completed") || strings.Contains(string(out), "event: message_stop") {
					t.Fatalf("unsupported output reported success: %s", out)
				}
			}
		}
	}
}
