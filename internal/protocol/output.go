package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
	"github.com/zclkkk/agy2api/internal/state"
)

// outputBlock is one content block or output item being assembled.
type outputBlock struct {
	Kind, ID, CallID, Name, Text, Signature string
	Public                                  ToolName
	Args                                    json.RawMessage
	Parts                                   []ag.Part
	Sealed                                  string
}

// Renderer turns the upstream stream into a Messages or Responses reply,
// streamed through emit or collected in Result, and records replay state.
type Renderer struct {
	wireState
	encoder        wireEncoder
	state          *state.Store
	started        bool
	active         *outputBlock
	toolCalls      int
	finish         string
	native         []ag.Part // the model turn, for stored history
	summary        strings.Builder
	bytes          int
	Result         json.RawMessage
	Warnings       []string
	textSignatures *textSignatureIndex
}

// model is the upstream model; replay state is bound to it.
func (r *Renderer) model() string { return r.input.Model }

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

func NewRenderer(in PreparedRequest, s *state.Store, emit func(string, map[string]any) error) *Renderer {
	prefix := "resp_"
	if in.Format == Messages {
		prefix = "msg_"
	}
	r := &Renderer{wireState: wireState{input: in, emit: emit, id: ag.ID(prefix), created: time.Now().Unix(), items: []map[string]any{}}, state: s, encoder: responsesEncoder{}}
	if in.Format == Messages {
		r.encoder = messagesEncoder{}
		r.textSignatures = newTextSignatureIndex(in.Model, in.Request.SessionID, in.Request.System)
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

func (r *Renderer) start() error {
	if r.started {
		return nil
	}
	r.started = true
	return r.encoder.start(&r.wireState)
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
	// These endpoints produce text, reasoning and client-executed tool calls.
	// Reject unsupported media before emitting any content from this chunk.
	for _, part := range chunk.Parts {
		if part.InlineData != nil || part.FileData != nil {
			return errors.New("upstream returned media output, which is not supported by the Responses or Messages endpoints")
		}
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
		kind := "text"
		switch {
		case part.FunctionCall != nil:
			kind = "tool"
		case part.Thought:
			kind = "thinking"
		}
		if part.Text == "" && part.FunctionCall == nil {
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
		if r.active == nil || r.active.Kind != kind || kind == "tool" {
			if err := r.closeBlock(ctx); err != nil {
				return err
			}
			if kind == "tool" {
				prefix := "call_agy_"
				if r.input.Format == Messages {
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
				if err := r.encoder.open(&r.wireState, r.active); err != nil {
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
		if kind == "tool" {
			args, err := objectArguments(part.FunctionCall.Args)
			if err != nil {
				return errors.New("upstream returned malformed function arguments")
			}
			b.Args = args
			if err := r.encoder.delta(&r.wireState, b, ""); err != nil {
				return err
			}
			continue
		}
		b.Text += part.Text
		if err := r.encoder.delta(&r.wireState, b, part.Text); err != nil {
			return err
		}
	}
	return nil
}

func (r *Renderer) open(kind string) error {
	prefix := "msg_"
	if kind == "thinking" {
		prefix = "rs_"
	}
	r.active = &outputBlock{Kind: kind, ID: ag.ID(prefix)}
	return r.encoder.open(&r.wireState, r.active)
}

// Records become available before their corresponding completed blocks are sent.
func (r *Renderer) closeBlock(ctx context.Context) error {
	b := r.active
	if b == nil {
		return nil
	}
	if err := r.recordBlock(ctx, b); err != nil {
		return err
	}
	if err := r.encoder.close(&r.wireState, b); err != nil {
		return err
	}
	if b.Kind == "tool" {
		r.toolCalls++
	}
	r.native = append(r.native, b.Parts...)
	r.active = nil
	return nil
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
	case "STOP", "FINISH_REASON_UNSPECIFIED":
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
	if len(r.items) == 0 && status == "completed" {
		r.warn("upstream completed without content")
	}
	if r.input.SingleCall && r.toolCalls > 1 {
		control := "parallel_tool_calls=false"
		if r.input.Format == Messages {
			control = "disable_parallel_tool_use"
		}
		r.warn("%s is not enforced; %d tool calls were returned", control, r.toolCalls)
	}
	if stop == "end_turn" && r.toolCalls > 0 {
		stop = "tool_use"
	}
	r.Result = r.encoder.result(&r.wireState, status, stop, incomplete)
	contents := append([]ag.Content(nil), r.input.Request.Contents...)
	contents = append(contents, ag.Content{Role: "model", Parts: r.native})
	if err := r.recordResponse(ctx, contents); err != nil {
		return err
	}
	return r.encoder.finish(&r.wireState, status, stop, r.Result)
}
func (r *Renderer) Fail(message string) error {
	if !r.started {
		return nil
	}
	return r.encoder.fail(&r.wireState, message)
}
