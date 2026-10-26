package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
	"github.com/zclkkk/agy2api/internal/state"
)

type Format string

const (
	Messages  Format = "messages"
	Responses Format = "responses"
)

type Operation uint8

const (
	Generate Operation = iota
	CountTokens
	CompactResponse
)

// PreparedRequest is a snapshot ready for the upstream. It has no tool-choice
// intent or deferred replay references and is not mutated by output encoders.
type PreparedRequest struct {
	Format        Format
	Model         string
	Request       ag.Request
	Stream, Store bool
	PreviousID    string
	OriginalItems []json.RawMessage
	Metadata      json.RawMessage
	Options       map[string]json.RawMessage
	HideThinking  bool
	Tools         map[string]ToolName
	Compact       string
	Warnings      []string
	HistorySystem *ag.Content
}

func (in PreparedRequest) PublicTool(native string) ToolName {
	if name, ok := in.Tools[native]; ok {
		return name
	}
	return ToolName{Name: native}
}

// requestPlan is private working state, separate from decoded client intent.
type requestPlan struct {
	RequestIntent
	toolRecords map[string]SignatureRecord // replay records read while resolving history
}

// Planner is the only entry point from a decoded request to an upstream request.
// All endpoints share this ordering, including counts and compaction.
type Planner struct {
	State                 *state.Store
	SanitizeClientMarkers bool
	ResolveMedia          func(context.Context, *ag.Request) error
}

func (p Planner) Prepare(ctx context.Context, intent RequestIntent, catalog ag.Catalog, operation Operation, headers http.Header) (PreparedRequest, error) {
	in := requestPlan{RequestIntent: cloneIntent(intent)}
	finish := func() PreparedRequest {
		return PreparedRequest{Format: in.Format, Model: in.Model,
			Request: in.Request, Stream: in.Stream, Store: in.Store,
			PreviousID: in.PreviousID, OriginalItems: in.OriginalItems, Metadata: in.Metadata, Options: in.Options,
			HideThinking: in.HideThinking, Tools: in.Tools, Compact: in.Compact,
			Warnings: in.Warnings, HistorySystem: in.HistorySystem}
	}
	var previous *StoredResponse
	if in.PreviousID != "" {
		value, err := LoadResponse(ctx, p.State, in.PreviousID)
		if err != nil {
			if errors.Is(err, state.ErrNotFound) {
				err = invalid("previous_response_id", "previous response is missing, expired, or was not stored")
			}
			return finish(), err
		}
		previous = &value
	}
	if operation == CompactResponse {
		in.Compact, in.Stream, in.Store = "endpoint", false, false
	}
	if in.Compact != "" && operation != CountTokens {
		in.disableTools()
	}
	if err := in.plan(catalog); err != nil {
		return finish(), err
	}
	if previous != nil {
		if err := in.prependHistory(*previous); err != nil {
			return finish(), err
		}
	}
	if err := (replayStore{p.State}).resolveHistoryTools(ctx, &in); err != nil {
		return finish(), err
	}
	in.Request.System = joinSystem(in.Instructions, in.HistorySystem)
	if p.SanitizeClientMarkers {
		in.SanitizeClientMarkers()
	}
	if err := (replayStore{p.State}).restore(ctx, &in); err != nil {
		return finish(), err
	}
	// Stored history can supply the conversation when this input only updates
	// instructions or asks for compaction. Check after replay, before adding
	// synthetic prompts that would hide an empty or entirely dropped history.
	if len(in.Request.Contents) == 0 {
		return finish(), invalid("input", "at least one message is required")
	}
	if in.Compact != "" && operation != CountTokens {
		appendContent(&in.Request, "user", []ag.Part{{Text: compactionPrompt}})
	}
	if p.ResolveMedia != nil {
		if err := p.ResolveMedia(ctx, &in.Request); err != nil {
			return finish(), err
		}
	}
	if err := in.finalize(headers); err != nil {
		return finish(), err
	}
	if err := validateIntent(&in.RequestIntent); err != nil {
		return finish(), err
	}
	if in.Format == Messages && operation == Generate {
		if err := (replayStore{p.State}).restoreTextSignatures(ctx, &in); err != nil {
			return finish(), err
		}
	}
	// Tool policy has already been resolved against current declarations. The
	// output/replay identity map also needs historical tools still in context.
	for native, identity := range in.historyTools {
		in.Tools[native] = identity
	}
	return finish(), nil
}

func joinSystem(parts ...*ag.Content) *ag.Content {
	var result *ag.Content
	for _, part := range parts {
		if part == nil {
			continue
		}
		if result == nil {
			result = &ag.Content{}
		}
		result.Parts = append(result.Parts, part.Parts...)
	}
	return result
}

func (in *RequestIntent) historySystem(text string) {
	var req ag.Request
	req.System = in.HistorySystem
	system(&req, text)
	in.HistorySystem = req.System
	system(&in.Request, text)
}

func cloneIntent(in RequestIntent) RequestIntent {
	tools := make(map[string]ToolName, len(in.Tools))
	for name, tool := range in.Tools {
		tools[name] = tool
	}
	in.Tools = tools
	in.historyTools = maps.Clone(in.historyTools)
	in.historyCalls = append([]historyCall(nil), in.historyCalls...)
	in.Request.Contents = cloneContents(in.Request.Contents)
	in.Request.System = joinSystem(in.Request.System)
	in.Instructions = joinSystem(in.Instructions)
	in.HistorySystem = joinSystem(in.HistorySystem)
	in.Request.Tools = append([]ag.Tool(nil), in.Request.Tools...)
	for i := range in.Request.Tools {
		in.Request.Tools[i].Functions = append([]ag.Function(nil), in.Request.Tools[i].Functions...)
	}
	in.references = append([]replayReference(nil), in.references...)
	in.OriginalItems = append([]json.RawMessage(nil), in.OriginalItems...)
	in.Warnings = append([]string(nil), in.Warnings...)
	return in
}

func cloneContents(contents []ag.Content) []ag.Content {
	out := append([]ag.Content(nil), contents...)
	for i := range out {
		out[i].Parts = append([]ag.Part(nil), out[i].Parts...)
		for j := range out[i].Parts {
			p := &out[i].Parts[j]
			if p.FunctionCall != nil {
				v := *p.FunctionCall
				p.FunctionCall = &v
			}
			if p.FunctionResponse != nil {
				v := *p.FunctionResponse
				v.Parts = append([]ag.Part(nil), v.Parts...)
				p.FunctionResponse = &v
			}
		}
	}
	return out
}
