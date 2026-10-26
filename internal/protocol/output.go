package protocol

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
	"github.com/zclkkk/agy2api/internal/state"
)

// outputBlock is one content block or output item being assembled.
type outputBlock struct {
	Kind, ID, CallID, Name, Text, Signature string
	Public                                  ToolName
	Args                                    json.RawMessage
	Parts                                   []ag.Part
	Blob                                    *ag.Blob
}

// Renderer turns the upstream stream into a Messages or Responses reply,
// streamed through emit or collected in Result, and records replay state.
type Renderer struct {
	format         string // "messages" or "responses"
	input          Input
	state          *state.Store
	emit           func(string, map[string]any) error
	id             string
	created        int64
	started        bool
	sequence       int
	items          []map[string]any
	active         *outputBlock
	toolUsed       bool
	usage          ag.Usage
	finish         string
	grounding      *ag.Grounding
	native         []ag.Part // the model turn, for stored history
	summary        strings.Builder
	bytes          int
	Result         json.RawMessage
	Warnings       []string
	textSignatures *textSignatureIndex
}

// model is the upstream model; replay state is bound to it.
func (r *Renderer) model() string { return r.input.UpstreamModel() }

func (r *Renderer) warn(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	for _, w := range r.Warnings {
		if w == message {
			return
		}
	}
	r.Warnings = append(r.Warnings, message)
}

// customInput returns the raw text argument of a custom (freeform) tool call.
func customInput(args json.RawMessage) string {
	var v struct {
		Input *string `json:"input"`
	}
	if json.Unmarshal(args, &v) == nil && v.Input != nil {
		return *v.Input
	}
	return string(args)
}

func NewRenderer(format string, in Input, s *state.Store, emit func(string, map[string]any) error) *Renderer {
	prefix := "resp_"
	if format == "messages" {
		prefix = "msg_"
	}
	r := &Renderer{format: format, input: in, state: s, emit: emit, id: ag.ID(prefix), created: time.Now().Unix(), items: []map[string]any{}}
	if format == "messages" {
		r.textSignatures = newTextSignatureIndex(in.UpstreamModel(), in.Request.SessionID, in.Request.System)
		for _, content := range in.Request.Contents {
			r.textSignatures.begin(content.Role)
			for _, part := range content.Parts {
				r.textSignatures.add(part)
			}
		}
		r.textSignatures.begin("model")
	}
	return r
}

func (r *Renderer) event(name string, fields map[string]any) error {
	if r.emit == nil {
		return nil
	}
	fields["type"] = name
	if r.format == "responses" {
		fields["sequence_number"] = r.sequence
		r.sequence++
	}
	return r.emit(name, fields)
}

func (r *Renderer) start() error {
	if r.started {
		return nil
	}
	r.started = true
	if r.format == "messages" {
		return r.event("message_start", map[string]any{"message": r.message(nil)})
	}
	if err := r.event("response.created", map[string]any{"response": r.response("in_progress", nil)}); err != nil {
		return err
	}
	return r.event("response.in_progress", map[string]any{"response": r.response("in_progress", nil)})
}

func (r *Renderer) Add(ctx context.Context, chunk ag.Chunk) error {
	r.bytes += len(raw(chunk))
	if r.bytes > 64<<20 {
		return errors.New("upstream output exceeds 64 MiB")
	}
	if u := chunk.Usage; u != nil {
		if u.Input > 0 {
			r.usage.Input = u.Input
		}
		if u.Output > 0 {
			r.usage.Output = u.Output
		}
		if u.Thoughts > 0 {
			r.usage.Thoughts = u.Thoughts
		}
		if u.Cached > 0 {
			r.usage.Cached = u.Cached
		}
		if u.Total > 0 {
			r.usage.Total = u.Total
		}
	}
	if chunk.Finish != "" {
		r.finish = chunk.Finish
	}
	if chunk.Grounding != nil {
		r.grounding = chunk.Grounding
	}
	if err := r.start(); err != nil {
		return err
	}
	for _, part := range chunk.Parts {
		if len(part.Unknown) > 0 {
			r.warn("ignored upstream part fields: %s", strings.Join(part.Unknown, ", "))
		}
		if part.Empty() {
			continue
		}
		if r.input.Compact != "" {
			// The summary is returned whole inside the compaction item.
			if !part.Thought {
				r.summary.WriteString(part.Text)
			}
			continue
		}
		if part.FileData != nil {
			return errors.New("upstream returned a content type that cannot be represented by this endpoint")
		}
		kind := "text"
		switch {
		case part.FunctionCall != nil:
			kind = "tool"
		case part.InlineData != nil:
			kind = "image"
		case part.Thought:
			kind = "thinking"
		}
		if part.Text == "" && part.FunctionCall == nil && part.InlineData == nil {
			if part.Signature != "" {
				if r.active == nil {
					if err := r.open("thinking"); err != nil {
						return err
					}
				}
				r.active.Signature = part.Signature
				r.active.Parts = append(r.active.Parts, part)
			}
			continue
		}
		if kind == "image" && r.format == "messages" {
			return errors.New("generated images require the Responses endpoint")
		}
		if r.active == nil || r.active.Kind != kind || kind == "tool" || kind == "image" {
			if err := r.closeBlock(ctx); err != nil {
				return err
			}
			if kind == "tool" {
				prefix := "call_agy_"
				if r.format == "messages" {
					prefix = "toolu_agy_"
				}
				call := *part.FunctionCall
				call.ID = ag.ID(prefix)
				part.FunctionCall = &call
				public := r.input.PublicTool(call.Name)
				itemPrefix := "fc_"
				if public.Custom {
					itemPrefix = "ctc_"
				}
				r.active = &outputBlock{Kind: kind, ID: ag.ID(itemPrefix), CallID: call.ID, Name: call.Name, Public: public, Args: call.Args}
				if err := r.startBlock(); err != nil {
					return err
				}
			} else {
				if err := r.open(kind); err != nil {
					return err
				}
			}
		}
		b := r.active
		b.Parts = append(b.Parts, part)
		if part.Signature != "" {
			b.Signature = part.Signature
		}
		if kind == "image" {
			b.Blob = part.InlineData
			continue
		}
		if kind == "tool" {
			args, err := objectArguments(part.FunctionCall.Args)
			if err != nil {
				return errors.New("upstream returned malformed function arguments")
			}
			b.Args = args
			switch {
			case r.format == "messages":
				err = r.event("content_block_delta", map[string]any{"index": len(r.items), "delta": map[string]any{"type": "input_json_delta", "partial_json": string(args)}})
			case b.Public.Custom:
				err = r.event("response.custom_tool_call_input.delta", map[string]any{"item_id": b.ID, "output_index": len(r.items), "delta": customInput(args)})
			default:
				err = r.event("response.function_call_arguments.delta", map[string]any{"item_id": b.ID, "output_index": len(r.items), "delta": string(args)})
			}
			if err != nil {
				return err
			}
			continue
		}
		b.Text += part.Text
		if kind == "thinking" && r.input.HideThinking {
			continue
		}
		if r.format == "messages" {
			typeName, key := "text_delta", "text"
			if kind == "thinking" {
				typeName, key = "thinking_delta", "thinking"
			}
			if err := r.event("content_block_delta", map[string]any{"index": len(r.items), "delta": map[string]any{"type": typeName, key: part.Text}}); err != nil {
				return err
			}
		} else {
			event, index := "response.output_text.delta", "content_index"
			if kind == "thinking" {
				event, index = "response.reasoning_summary_text.delta", "summary_index"
			}
			if err := r.event(event, map[string]any{"item_id": b.ID, "output_index": len(r.items), index: 0, "delta": part.Text}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Renderer) open(kind string) error {
	prefix := "msg_"
	if kind == "thinking" {
		prefix = "rs_"
	}
	if kind == "image" {
		prefix = "ig_"
	}
	r.active = &outputBlock{Kind: kind, ID: ag.ID(prefix)}
	return r.startBlock()
}

// toolItem fills a Responses function or custom tool call item. arguments is
// empty while the call is in progress.
func (r *Renderer) toolItem(item map[string]any, b *outputBlock, arguments string) {
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

func (r *Renderer) startBlock() error {
	b := r.active
	index := len(r.items)
	if r.format == "messages" {
		var content map[string]any
		switch b.Kind {
		case "text":
			content = map[string]any{"type": "text", "text": ""}
		case "thinking":
			content = map[string]any{"type": "thinking", "thinking": "", "signature": ""}
		case "tool":
			content = map[string]any{"type": "tool_use", "id": b.CallID, "name": b.Public.Name, "input": map[string]any{}}
		}
		return r.event("content_block_start", map[string]any{"index": index, "content_block": content})
	}
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

func (r *Renderer) closeBlock(ctx context.Context) error {
	b := r.active
	if b == nil {
		return nil
	}
	index := len(r.items)
	var item map[string]any
	// Stream text deltas form one signed native part, not separate historical turns.
	if b.Kind == "text" || b.Kind == "thinking" {
		b.Parts = []ag.Part{{Text: b.Text, Thought: b.Kind == "thinking", Signature: b.Signature}}
	}
	if b.Kind == "tool" {
		if err := r.state.Put(ctx, "tool", b.CallID, SignatureRecord{Model: r.model(), Name: b.Name, Digest: digest(b.Args), Signature: b.Signature}); err != nil {
			return err
		}
		b.Parts = []ag.Part{{FunctionCall: &ag.Call{ID: b.CallID, Name: b.Name, Args: b.Args}, Signature: b.Signature}}
	}
	if b.Kind == "image" {
		if b.Blob == nil || !strings.HasPrefix(b.Blob.MIMEType, "image/") {
			return errors.New("upstream returned a non-image media output")
		}
		if _, err := base64.StdEncoding.DecodeString(b.Blob.Data); err != nil {
			return errors.New("upstream returned invalid image data")
		}
		if err := r.state.Put(ctx, "image", b.ID, ImageRecord{Model: r.model(), MIMEType: b.Blob.MIMEType, Digest: digest(raw(b.Blob.Data)), Signature: b.Signature}); err != nil {
			return err
		}
	}
	if r.textSignatures != nil {
		if b.Kind == "text" && b.Signature != "" {
			if err := r.state.Put(ctx, "text_signature", r.textSignatures.key(b.Text), b.Signature); err != nil {
				return err
			}
		}
		for _, part := range b.Parts {
			r.textSignatures.add(part)
		}
	}
	if r.format == "messages" {
		switch b.Kind {
		case "text":
			item = map[string]any{"type": "text", "text": b.Text}
			citations, err := r.messageCitations(b.Text)
			if err != nil {
				return err
			}
			if len(citations) > 0 {
				item["citations"] = citations
				for _, citation := range citations {
					if err := r.event("content_block_delta", map[string]any{"index": index, "delta": map[string]any{"type": "citations_delta", "citation": citation}}); err != nil {
						return err
					}
				}
			}
		case "thinking":
			sealed, err := r.state.Seal(r.model(), Replay{Parts: b.Parts})
			if err != nil {
				return err
			}
			visible := b.Text
			if r.input.HideThinking {
				visible = ""
			}
			item = map[string]any{"type": "thinking", "thinking": visible, "signature": sealed}
			if err := r.event("content_block_delta", map[string]any{"index": index, "delta": map[string]any{"type": "signature_delta", "signature": sealed}}); err != nil {
				return err
			}
		case "tool":
			item = map[string]any{"type": "tool_use", "id": b.CallID, "name": b.Public.Name, "input": b.Args}
		}
		if err := r.event("content_block_stop", map[string]any{"index": index}); err != nil {
			return err
		}
	} else {
		item = map[string]any{"id": b.ID, "status": "completed"}
		switch b.Kind {
		case "text":
			annotations := r.annotations(b.Text)
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
			sealed, err := r.state.Seal(r.model(), Replay{Parts: b.Parts})
			if err != nil {
				return err
			}
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
	}
	r.items = append(r.items, item)
	r.toolUsed = r.toolUsed || b.Kind == "tool"
	r.native = append(r.native, b.Parts...)
	r.active = nil
	if b.Kind == "text" && b.Signature != "" && r.format == "responses" {
		// A carrier after the text restores a signature on that exact native part.
		sealed, err := r.state.Seal(r.model(), Replay{PreviousText: digest(raw(b.Text)), Signature: b.Signature})
		if err != nil {
			return err
		}
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

func (r *Renderer) annotations(text string) []any {
	result := []any{}
	if r.grounding == nil {
		return result
	}
	for _, support := range r.grounding.Supports {
		start, end := support.Segment.Start, support.Segment.End
		if support.Segment.Text != "" {
			start = strings.Index(text, support.Segment.Text)
			end = start + len(support.Segment.Text)
		}
		if start < 0 || end < start || end > len(text) {
			continue
		}
		for _, idx := range support.Indices {
			if idx < 0 || idx >= len(r.grounding.Chunks) {
				continue
			}
			w := r.grounding.Chunks[idx].Web
			if w == nil {
				continue
			}
			result = append(result, map[string]any{"type": "url_citation", "start_index": utf8.RuneCountInString(text[:start]), "end_index": utf8.RuneCountInString(text[:end]), "url": w.URI, "title": w.Title})
		}
	}
	return result
}

func (r *Renderer) messageCitations(text string) ([]any, error) {
	result := []any{}
	for _, v := range r.annotations(text) {
		a := v.(map[string]any)
		start, end := a["start_index"].(int), a["end_index"].(int)
		sealed, err := r.state.Seal(r.model(), a)
		if err != nil {
			return nil, err
		}
		result = append(result, map[string]any{"type": "web_search_result_location", "url": a["url"], "title": a["title"], "cited_text": string([]rune(text)[start:end]), "encrypted_index": sealed})
	}
	return result, nil
}

func (r *Renderer) searchTrace() error {
	if r.format != "responses" || !r.input.Search || r.grounding == nil {
		return nil
	}
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
	if len(r.grounding.Queries) > 0 {
		action["query"] = r.grounding.Queries[0]
		action["queries"] = r.grounding.Queries
	}
	item = map[string]any{"id": id, "type": "web_search_call", "status": "completed", "action": action}
	if err := r.event("response.output_item.done", map[string]any{"output_index": index, "item": item}); err != nil {
		return err
	}
	r.items = append(r.items, item)
	return nil
}

func (r *Renderer) message(stop any) map[string]any {
	return map[string]any{"id": r.id, "type": "message", "role": "assistant", "model": r.input.Model, "content": r.items, "stop_reason": stop, "stop_sequence": nil, "usage": map[string]any{"input_tokens": max(0, r.usage.Input-r.usage.Cached), "output_tokens": r.usage.Output + r.usage.Thoughts, "cache_read_input_tokens": r.usage.Cached}}
}
func (r *Renderer) response(status string, incomplete any) map[string]any {
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
	var options map[string]json.RawMessage
	_ = json.Unmarshal(r.input.Options, &options)
	for _, key := range []string{"instructions", "max_output_tokens", "temperature", "top_p", "tool_choice", "tools", "reasoning", "text"} {
		if v, ok := options[key]; ok {
			out[key] = v
		}
	}
	return out
}

func (r *Renderer) responseUsage() map[string]any {
	return map[string]any{
		"input_tokens":          r.usage.Input,
		"input_tokens_details":  map[string]int{"cached_tokens": r.usage.Cached, "cache_write_tokens": 0},
		"output_tokens":         r.usage.Output + r.usage.Thoughts,
		"output_tokens_details": map[string]int{"reasoning_tokens": r.usage.Thoughts},
		"total_tokens":          r.usage.Input + r.usage.Output + r.usage.Thoughts,
	}
}

func (r *Renderer) Finish(ctx context.Context) error {
	if !r.started {
		if err := r.start(); err != nil {
			return err
		}
	}
	status, stop := "completed", "end_turn"
	var incomplete any
	switch reason := strings.ToUpper(r.finish); reason {
	case "STOP", "FINISH_REASON_UNSPECIFIED", "NO_IMAGE":
	case "MAX_TOKENS":
		status = "incomplete"
		stop = "max_tokens"
		incomplete = map[string]string{"reason": "max_output_tokens"}
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "LANGUAGE",
		"IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT", "IMAGE_RECITATION", "IMAGE_OTHER":
		status = "incomplete"
		stop = "refusal"
		incomplete = map[string]string{"reason": "content_filter"}
	case "MALFORMED_FUNCTION_CALL", "UNEXPECTED_TOOL_CALL", "TOO_MANY_TOOL_CALLS", "MALFORMED_RESPONSE", "MISSING_THOUGHT_SIGNATURE":
		// The model produced an unusable turn; a retry usually succeeds.
		return &ag.HTTPError{Status: 502, Message: "Antigravity ended generation with " + reason + "; retry the request"}
	default:
		r.warn("treated upstream finish reason %s as a normal stop", reason)
	}
	if r.input.Compact != "" {
		if status != "completed" {
			return &ag.HTTPError{Status: 502, Message: "Antigravity did not finish the compaction summary (" + r.finish + "); retry the request"}
		}
		return r.finishCompaction(ctx)
	}
	if err := r.closeBlock(ctx); err != nil {
		return err
	}
	if err := r.searchTrace(); err != nil {
		return err
	}
	if len(r.items) == 0 && status == "completed" {
		r.warn("upstream completed without content")
	}
	if r.format == "messages" {
		if stop == "end_turn" && r.toolUsed {
			stop = "tool_use"
		}
		r.Result = raw(r.message(stop))
		if err := r.event("message_delta", map[string]any{"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]any{"input_tokens": max(0, r.usage.Input-r.usage.Cached), "output_tokens": r.usage.Output + r.usage.Thoughts, "cache_read_input_tokens": r.usage.Cached}}); err != nil {
			return err
		}
		return r.event("message_stop", map[string]any{})
	}
	response := r.response(status, incomplete)
	r.Result = raw(response)
	if r.input.Store {
		contents := append([]ag.Content(nil), r.input.Request.Contents...)
		contents = append(contents, ag.Content{Role: "model", Parts: r.native})
		items := append([]json.RawMessage(nil), r.input.OriginalItems...)
		if err := r.state.Put(ctx, "response", r.id, StoredResponse{Model: r.input.Model, Contents: contents, Response: r.Result, Items: items}); err != nil {
			return err
		}
	}
	return r.event("response."+status, map[string]any{"response": response})
}

// finishCompaction returns the summary as one compaction item. The
// /v1/responses/compact endpoint returns a response.compaction object; a
// compaction_trigger request returns an ordinary response.
func (r *Renderer) finishCompaction(ctx context.Context) error {
	summary := strings.TrimSpace(r.summary.String())
	if summary == "" {
		return &ag.HTTPError{Status: 502, Message: "Antigravity returned an empty compaction summary; retry the request"}
	}
	item := map[string]any{"id": ag.ID("cmp_"), "type": "compaction", "encrypted_content": sealCompaction(summary, r.input.Model)}
	if r.input.Compact == "endpoint" {
		r.Result = raw(map[string]any{"id": r.id, "object": "response.compaction", "created_at": r.created, "output": []any{item}, "usage": r.responseUsage()})
		return nil
	}
	index := len(r.items)
	if err := r.event("response.output_item.added", map[string]any{"output_index": index, "item": item}); err != nil {
		return err
	}
	if err := r.event("response.output_item.done", map[string]any{"output_index": index, "item": item}); err != nil {
		return err
	}
	r.items = append(r.items, item)
	response := r.response("completed", nil)
	r.Result = raw(response)
	if r.input.Store {
		// A continuation of this response starts from the summary alone.
		contents := []ag.Content{{Role: "user", Parts: []ag.Part{summaryPart(summary)}}}
		items := append([]json.RawMessage(nil), r.input.OriginalItems...)
		if err := r.state.Put(ctx, "response", r.id, StoredResponse{Model: r.input.Model, Contents: contents, Response: r.Result, Items: items}); err != nil {
			return err
		}
	}
	return r.event("response.completed", map[string]any{"response": response})
}

func (r *Renderer) Fail(message string) error {
	if !r.started {
		return nil
	}
	if r.format == "messages" {
		return r.event("error", map[string]any{"error": map[string]any{"type": "api_error", "message": message}})
	}
	resp := r.response("failed", nil)
	resp["error"] = map[string]any{"code": "upstream_error", "message": message}
	return r.event("response.failed", map[string]any{"response": resp})
}
