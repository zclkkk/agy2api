package protocol

import (
	"encoding/json"
	"strings"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

type responsesEncoder struct{}

func (responsesEncoder) start(r *wireState) error {
	if err := r.event("response.created", map[string]any{"response": r.response("in_progress", nil)}); err != nil {
		return err
	}
	return r.event("response.in_progress", map[string]any{"response": r.response("in_progress", nil)})
}
func (r *wireState) toolItem(item map[string]any, b *outputBlock, arguments string) {
	item["call_id"] = b.CallID
	item["name"] = b.Public.Name
	if b.Public.Namespace != "" {
		item["namespace"] = b.Public.Namespace
	}
	if b.Public.Custom {
		item["type"] = "custom_tool_call"
		item["input"] = ""
		if arguments != "" {
			item["input"] = customInput(json.RawMessage(arguments))
		}
		return
	}
	item["type"] = "function_call"
	item["arguments"] = arguments
}

func (responsesEncoder) open(r *wireState, b *outputBlock) error {
	index := len(r.items)
	item := map[string]any{"id": b.ID, "status": "in_progress"}
	switch b.Kind {
	case "text":
		item["type"] = "message"
		item["role"] = "assistant"
		item["content"] = []any{}
	case "thinking":
		item["type"] = "reasoning"
		item["summary"] = []any{}
	case "tool":
		r.toolItem(item, b, "")
	case "image":
		item["type"] = "image_generation_call"
		item["result"] = nil
	}
	if err := r.event("response.output_item.added", map[string]any{"output_index": index, "item": item}); err != nil {
		return err
	}
	if b.Kind == "text" {
		return r.event("response.content_part.added", map[string]any{"item_id": b.ID, "output_index": index, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}, "logprobs": []any{}}})
	}
	if b.Kind == "thinking" {
		return r.event("response.reasoning_summary_part.added", map[string]any{"item_id": b.ID, "output_index": index, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": ""}})
	}
	return nil
}

func (responsesEncoder) delta(r *wireState, b *outputBlock, text string) error {
	if b.Kind == "tool" {
		event, value := "response.function_call_arguments.delta", string(b.Args)
		if b.Public.Custom {
			event, value = "response.custom_tool_call_input.delta", customInput(b.Args)
		}
		return r.event(event, map[string]any{"item_id": b.ID, "output_index": len(r.items), "delta": value})
	}
	event, index := "response.output_text.delta", "content_index"
	if b.Kind == "thinking" {
		event, index = "response.reasoning_summary_text.delta", "summary_index"
	}
	return r.event(event, map[string]any{"item_id": b.ID, "output_index": len(r.items), index: 0, "delta": text})
}
func (responsesEncoder) close(r *wireState, b *outputBlock) error {
	index := len(r.items)
	var item map[string]any
	item = map[string]any{"id": b.ID, "status": "completed"}
	switch b.Kind {
	case "text":
		annotations := b.Annotations
		part := map[string]any{"type": "output_text", "text": b.Text, "annotations": annotations, "logprobs": []any{}}
		item["type"] = "message"
		item["role"] = "assistant"
		item["content"] = []any{part}
		for i, annotation := range annotations {
			if err := r.event("response.output_text.annotation.added", map[string]any{"item_id": b.ID, "output_index": index, "content_index": 0, "annotation_index": i, "annotation": annotation}); err != nil {
				return err
			}
		}
		if err := r.event("response.output_text.done", map[string]any{"item_id": b.ID, "output_index": index, "content_index": 0, "text": b.Text, "logprobs": []any{}}); err != nil {
			return err
		}
		if err := r.event("response.content_part.done", map[string]any{"item_id": b.ID, "output_index": index, "content_index": 0, "part": part}); err != nil {
			return err
		}
	case "thinking":
		sealed := b.Sealed
		part := map[string]any{"type": "summary_text", "text": b.Text}
		item["type"] = "reasoning"
		item["summary"] = []any{part}
		item["encrypted_content"] = sealed
		if err := r.event("response.reasoning_summary_text.done", map[string]any{"item_id": b.ID, "output_index": index, "summary_index": 0, "text": b.Text}); err != nil {
			return err
		}
		if err := r.event("response.reasoning_summary_part.done", map[string]any{"item_id": b.ID, "output_index": index, "summary_index": 0, "part": part}); err != nil {
			return err
		}
	case "tool":
		r.toolItem(item, b, string(b.Args))
		var err error
		if b.Public.Custom {
			err = r.event("response.custom_tool_call_input.done", map[string]any{"item_id": b.ID, "output_index": index, "input": customInput(b.Args)})
		} else {
			err = r.event("response.function_call_arguments.done", map[string]any{"item_id": b.ID, "output_index": index, "arguments": string(b.Args)})
		}
		if err != nil {
			return err
		}
	case "image":
		item["type"] = "image_generation_call"
		item["result"] = b.Blob.Data
		item["output_format"] = strings.TrimPrefix(b.Blob.MIMEType, "image/")
	}
	if err := r.event("response.output_item.done", map[string]any{"output_index": index, "item": item}); err != nil {
		return err
	}
	r.items = append(r.items, item)
	if b.Kind == "text" && b.Sealed != "" {
		// A carrier after the text restores a signature on that exact native part.
		sealed := b.Sealed
		carrier := map[string]any{"id": ag.ID("rs_"), "type": "reasoning", "summary": []any{}, "encrypted_content": sealed, "status": "completed"}
		index := len(r.items)
		if err := r.event("response.output_item.added", map[string]any{"output_index": index, "item": map[string]any{"id": carrier["id"], "type": "reasoning", "summary": []any{}, "status": "in_progress"}}); err != nil {
			return err
		}
		if err := r.event("response.output_item.done", map[string]any{"output_index": index, "item": carrier}); err != nil {
			return err
		}
		r.items = append(r.items, carrier)
	}
	return nil
}

func (responsesEncoder) search(r *wireState, trace searchTrace) error {
	id := ag.ID("ws_agy_")
	index := len(r.items)
	item := map[string]any{"id": id, "type": "web_search_call", "status": "in_progress"}
	if err := r.event("response.output_item.added", map[string]any{"output_index": index, "item": item}); err != nil {
		return err
	}
	for _, name := range []string{"response.web_search_call.in_progress", "response.web_search_call.searching", "response.web_search_call.completed"} {
		if err := r.event(name, map[string]any{"item_id": id, "output_index": index}); err != nil {
			return err
		}
	}
	action := map[string]any{"type": "search"}
	if len(trace.Queries) > 0 {
		action["query"] = trace.Queries[0]
		action["queries"] = trace.Queries
	}
	item = map[string]any{"id": id, "type": "web_search_call", "status": "completed", "action": action}
	if err := r.event("response.output_item.done", map[string]any{"output_index": index, "item": item}); err != nil {
		return err
	}
	r.items = append(r.items, item)
	return nil
}

func (r *wireState) response(status string, incomplete any) map[string]any {
	metadata := r.input.Metadata
	if !present(metadata) {
		metadata = raw(map[string]any{})
	}
	var previous any
	if r.input.PreviousID != "" {
		previous = r.input.PreviousID
	}
	out := map[string]any{
		"id": r.id, "object": "response", "created_at": r.created,
		"status": status, "error": nil, "incomplete_details": incomplete,
		"model": r.input.Model, "output": r.items,
		"previous_response_id": previous, "store": r.input.Store, "metadata": metadata,
		"parallel_tool_calls": true, "tool_choice": "auto", "tools": []any{},
		"instructions": nil, "max_output_tokens": nil, "temperature": nil,
		"top_p": nil, "truncation": "disabled",
		"usage": r.responseUsage(),
	}
	for key, value := range r.input.Options {
		out[key] = value
	}

	return out
}

func (r *wireState) responseUsage() map[string]any {
	return map[string]any{
		"input_tokens":          r.usage.Input,
		"input_tokens_details":  map[string]int{"cached_tokens": r.usage.Cached, "cache_write_tokens": 0},
		"output_tokens":         r.usage.Output + r.usage.Thoughts,
		"output_tokens_details": map[string]int{"reasoning_tokens": r.usage.Thoughts},
		"total_tokens":          r.usage.Input + r.usage.Output + r.usage.Thoughts,
	}
}

func (responsesEncoder) result(r *wireState, status, _ string, incomplete any) json.RawMessage {
	return raw(r.response(status, incomplete))
}
func (responsesEncoder) finish(r *wireState, status, _ string, result json.RawMessage) error {
	return r.event("response."+status, map[string]any{"response": result})
}
func (responsesEncoder) fail(r *wireState, message string) error {
	response := r.response("failed", nil)
	response["error"] = map[string]any{"code": "upstream_error", "message": message}
	return r.event("response.failed", map[string]any{"response": response})
}
