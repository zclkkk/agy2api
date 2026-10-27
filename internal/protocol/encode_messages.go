package protocol

import (
	"encoding/json"
	"errors"
)

type messagesEncoder struct{}

func (messagesEncoder) start(r *wireState) error {
	return r.event("message_start", map[string]any{"message": r.message(nil)})
}

func (messagesEncoder) open(r *wireState, b *outputBlock) error {
	var block map[string]any
	switch b.Kind {
	case "text":
		block = map[string]any{"type": "text", "text": ""}
	case "thinking":
		block = map[string]any{"type": "thinking", "thinking": "", "signature": ""}
	case "tool":
		block = map[string]any{"type": "tool_use", "id": b.CallID, "name": b.Public.Name, "input": map[string]any{}}
	default:
		return errors.New("unsupported output block type")
	}
	return r.event("content_block_start", map[string]any{"index": len(r.items), "content_block": block})
}

func (messagesEncoder) delta(r *wireState, b *outputBlock, text string) error {
	var delta map[string]any
	switch b.Kind {
	case "tool":
		delta = map[string]any{"type": "input_json_delta", "partial_json": string(b.Args)}
	case "thinking":
		if r.input.HideThinking {
			return nil
		}
		delta = map[string]any{"type": "thinking_delta", "thinking": text}
	default:
		delta = map[string]any{"type": "text_delta", "text": text}
	}
	return r.event("content_block_delta", map[string]any{"index": len(r.items), "delta": delta})
}

func (messagesEncoder) close(r *wireState, b *outputBlock) error {
	index := len(r.items)
	var item map[string]any
	switch b.Kind {
	case "text":
		item = map[string]any{"type": "text", "text": b.Text}
	case "thinking":
		visible := b.Text
		if r.input.HideThinking {
			visible = ""
		}
		item = map[string]any{"type": "thinking", "thinking": visible, "signature": b.Sealed}
		if err := r.event("content_block_delta", map[string]any{"index": index, "delta": map[string]any{"type": "signature_delta", "signature": b.Sealed}}); err != nil {
			return err
		}
	case "tool":
		item = map[string]any{"type": "tool_use", "id": b.CallID, "name": b.Public.Name, "input": b.Args}
	}
	if err := r.event("content_block_stop", map[string]any{"index": index}); err != nil {
		return err
	}
	r.items = append(r.items, item)
	return nil
}

func (r *wireState) messageUsage() map[string]any {
	// Gemini caches implicitly and reports no cache writes.
	return map[string]any{"input_tokens": max(0, r.usage.Input-r.usage.Cached), "output_tokens": r.usage.Output + r.usage.Thoughts,
		"cache_creation_input_tokens": 0, "cache_read_input_tokens": r.usage.Cached}
}

func (r *wireState) message(stop any) map[string]any {
	return map[string]any{"id": r.id, "type": "message", "role": "assistant", "model": r.input.Model, "content": r.items, "stop_reason": stop, "stop_sequence": nil, "usage": r.messageUsage()}
}

func (messagesEncoder) result(r *wireState, _, stop string, _ any) json.RawMessage {
	return raw(r.message(stop))
}

func (messagesEncoder) finish(r *wireState, _, stop string, _ json.RawMessage) error {
	if err := r.event("message_delta", map[string]any{"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": r.messageUsage()}); err != nil {
		return err
	}
	return r.event("message_stop", map[string]any{})
}

func (messagesEncoder) fail(r *wireState, message string) error {
	return r.event("error", map[string]any{"error": map[string]any{"type": "api_error", "message": message}})
}
