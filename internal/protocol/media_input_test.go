package protocol

import (
	"context"
	"strings"
	"testing"
)

func TestDocumentMetadataContentSourcesAndSearchResults(t *testing.T) {
	in, err := (Decoder{}).Messages([]byte(`{
		"model":"gemini-test","max_tokens":100,
		"messages":[
			{"role":"user","content":[
				{"type":"document","title":"Spec","context":"Draft from March","source":{"type":"content","content":[{"type":"text","text":"Part one"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YWJj"}}]}},
				{"type":"search_result","source":"https://example.com/a","title":"Result A","content":[{"type":"text","text":"First"},{"type":"text","text":"Second"}]}]},
			{"role":"assistant","content":[{"type":"tool_use","id":"call1","name":"search","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call1","content":[{"type":"search_result","source":"https://example.com/b","title":"Result B","content":[{"type":"text","text":"Found"}]}]}]}
		]}`), false)
	if err != nil {
		t.Fatal(err)
	}
	parts := in.Request.Contents[0].Parts
	if len(parts) != 4 || parts[0].Text != "Document title: Spec\nDocument context: Draft from March" || parts[1].Text != "Part one" ||
		parts[2].InlineData == nil || parts[3].Text != "Search result: Result A\nSource: https://example.com/a\n\nFirst\n\nSecond" {
		t.Fatalf("parts = %s", raw(parts))
	}
	result := in.Request.Contents[2].Parts[0].FunctionResponse
	if result == nil || !strings.Contains(string(result.Response), `Search result: Result B\nSource: https://example.com/b\n\nFound`) {
		t.Fatalf("tool search result = %s", raw(result))
	}
	for name, block := range map[string]string{
		"search result without source": `{"type":"search_result","title":"t","content":[{"type":"text","text":"x"}]}`,
		"search result with an image":  `{"type":"search_result","source":"s","title":"t","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YWJj"}}]}`,
		"content document with a file": `{"type":"document","source":{"type":"content","content":[{"type":"document","source":{"type":"text","data":"x"}}]}}`,
		"file source":                  `{"type":"document","source":{"type":"file","file_id":"file_1"}}`,
	} {
		body := `{"model":"gemini-test","max_tokens":10,"messages":[{"role":"user","content":[` + block + `]}]}`
		if _, err := (Decoder{}).Messages([]byte(body), false); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestMessagesTextDocumentsUseSourceData(t *testing.T) {
	in, err := (Decoder{}).Messages([]byte(`{
		"model":"gemini-test","max_tokens":100,
		"messages":[
			{"role":"user","content":[{"type":"document","source":{"type":"text","media_type":"text/plain","data":"Document body\n第二行"}},{"type":"text","text":"Summarize it."}]},
			{"role":"assistant","content":[{"type":"tool_use","id":"call1","name":"read","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call1","content":[{"type":"document","source":{"type":"text","media_type":"text/plain","data":"Tool document"}}]}]}
		]}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if got := in.Request.Contents[0].Parts[0].Text; got != "Document body\n第二行" {
		t.Fatalf("document body = %q", got)
	}
	result := in.Request.Contents[2].Parts[0].FunctionResponse
	if result == nil || string(result.Response) != `{"is_error":false,"output":"Tool document"}` {
		t.Fatalf("tool document was lost: %+v", result)
	}
}

func TestImageInputAndClientToolImagesRemainSupported(t *testing.T) {
	d := testPipeline{State: testStore(t)}
	for _, format := range []Format{Responses, Messages} {
		t.Run(string(format), func(t *testing.T) {
			var in PreparedRequest
			var err error
			if format == Responses {
				in, err = d.Responses(context.Background(), []byte(`{
					"model":"gemini-test","tools":[{"type":"function","name":"generate_image","parameters":{"type":"object"}}],
					"input":[
						{"role":"user","content":[{"type":"input_text","text":"Compare these images."},{"type":"input_image","image_url":"data:image/png;base64,YWJj"}]},
						{"type":"function_call","call_id":"call_client","name":"generate_image","arguments":"{}"},
						{"type":"function_call_output","call_id":"call_client","output":[{"type":"input_image","image_url":"data:image/png;base64,ZGVm"}]}
					]}`))
			} else {
				in, err = d.Messages(context.Background(), []byte(`{
					"model":"gemini-test","max_tokens":100,"tools":[{"name":"generate_image","input_schema":{"type":"object"}}],
					"messages":[
						{"role":"user","content":[{"type":"text","text":"Compare these images."},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YWJj"}}]},
						{"role":"assistant","content":[{"type":"tool_use","id":"toolu_client","name":"generate_image","input":{}}]},
						{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_client","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"ZGVm"}}]}]}
					]}`), false)
			}
			if err != nil {
				t.Fatal(err)
			}
			contents := in.Request.Contents
			if len(contents) != 3 {
				t.Fatalf("conversation changed: %s", raw(contents))
			}
			image := contents[0].Parts[1].InlineData
			if image == nil || image.MIMEType != "image/png" || image.Data != "YWJj" {
				t.Fatal("user image was lost")
			}
			call := contents[1].Parts[0]
			if call.FunctionCall.Name != "generate_image" || call.Signature != SkipSignature {
				t.Fatal("ordinary function identity or signature was lost")
			}
			result := contents[2].Parts[0].FunctionResponse
			if result == nil || result.Name != "generate_image" || len(result.Parts) != 1 || result.Parts[0].InlineData == nil || result.Parts[0].InlineData.Data != "ZGVm" {
				t.Fatal("client tool image was lost")
			}
		})
	}
}
