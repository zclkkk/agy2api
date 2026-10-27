package protocol

import (
	"encoding/json"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

// responseInputItem gives accepted input the public shape returned by input_items.
// Preserve caller-supplied IDs and extension fields; generated IDs are stored
// with the response so retrieval and previous_response_id reuse the same items.
func responseInputItem(data json.RawMessage, item ResponseItem) (json.RawMessage, error) {
	var out map[string]json.RawMessage
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	kind := item.Type
	if kind == "" {
		kind = "message"
	}
	if kind == "compaction_summary" {
		kind = "compaction"
	}
	out["type"] = raw(kind)
	prefix := "item_"
	switch kind {
	case "message":
		prefix = "msg_"
		content, err := responseMessageContent(item.Content, item.Role == "assistant")
		if err != nil {
			return nil, err
		}
		out["content"] = content
	case "function_call":
		prefix = "fc_"
		if item.Arguments == "" {
			out["arguments"] = raw("{}")
		}
	case "custom_tool_call":
		prefix = "ctc_"
		out["input"] = raw(item.Input)
	case "function_call_output", "custom_tool_call_output":
		if !present(item.Output) {
			out["output"] = raw("")
		}
	case "reasoning":
		prefix = "rs_"
		if !present(item.Summary) {
			out["summary"] = raw([]any{})
		}
	case "compaction":
		prefix = "cmp_"
	}
	if item.ID == "" {
		out["id"] = raw(ag.ID(prefix))
	}
	if kind != "compaction" && item.Status == "" {
		out["status"] = raw("completed")
	}
	return raw(out), nil
}

func responseMessageContent(content json.RawMessage, assistant bool) (json.RawMessage, error) {
	var text string
	var parts []json.RawMessage
	if json.Unmarshal(content, &text) == nil {
		parts = []json.RawMessage{raw(map[string]string{"type": "text", "text": text})}
	} else if err := json.Unmarshal(content, &parts); err != nil {
		return nil, invalid("content", "content must be a string or an array of content blocks")
	}
	for i, part := range parts {
		var block map[string]json.RawMessage
		if err := json.Unmarshal(part, &block); err != nil {
			return nil, err
		}
		var kind string
		_ = json.Unmarshal(block["type"], &kind)
		switch kind {
		case "text", "input_text", "output_text":
			if assistant {
				block["type"] = raw("output_text")
				if !present(block["annotations"]) {
					block["annotations"] = raw([]any{})
				}
			} else {
				block["type"] = raw("input_text")
			}
			parts[i] = raw(block)
		}
	}
	return raw(parts), nil
}
