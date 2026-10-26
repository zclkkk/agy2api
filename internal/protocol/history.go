package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
	"github.com/zclkkk/agy2api/internal/state"
)

const recordVersion = 2

type StoredResponse struct {
	Version       int                 `json:"version,omitempty"`
	Model         string              `json:"model"`
	HistorySystem *ag.Content         `json:"history_system,omitempty"`
	Contents      []ag.Content        `json:"contents"`
	Response      json.RawMessage     `json:"response"`
	Items         []json.RawMessage   `json:"items"`
	Tools         map[string]ToolName `json:"tools,omitempty"`
}

// replayStore provides typed access to all records used by the protocol. Missing
// cache entries may degrade; storage failures are returned to the caller.
type replayStore struct{ State *state.Store }

// LoadResponse is shared by history continuation and the HTTP retrieval routes.
// Records are interpreted only in the current format.
func LoadResponse(ctx context.Context, s *state.Store, id string) (StoredResponse, error) {
	var value StoredResponse
	err := s.Get(ctx, "response", id, &value)
	if err == nil && value.Version != recordVersion {
		err = errors.New("unsupported stored response version")
	}
	return value, err
}

func (s replayStore) seal(model string, replay Replay) (string, error) {
	replay.Version, replay.Model, replay.Kind = recordVersion, model, "reasoning"
	return s.State.Seal("agy2api/replay/v2", replay)
}

func (s replayStore) open(model, token string) (Replay, error) {
	var replay Replay
	if err := s.State.Unseal("agy2api/replay/v2", token, &replay); err != nil {
		return Replay{}, err
	}
	if replay.Version != recordVersion || replay.Kind != "reasoning" {
		return Replay{}, errors.New("unsupported replay record")
	}
	if replay.Model != model {
		return Replay{}, errors.New("reasoning belongs to a different upstream model")
	}
	return replay, nil
}

type replayReference struct {
	Content, Part  int
	Token, Visible string
}

func (in *RequestIntent) deferReplay(token, visible string) {
	appendContent(&in.Request, "model", []ag.Part{{}})
	i := len(in.Request.Contents) - 1
	in.references = append(in.references, replayReference{Content: i, Part: len(in.Request.Contents[i].Parts) - 1, Token: token, Visible: visible})
}

func (in *requestPlan) prependHistory(previous StoredResponse) error {
	for native, identity := range previous.Tools {
		name, err := in.registerHistoryTool(identity)
		if err != nil {
			return err
		}
		if name != native {
			return invalid("previous_response_id", "stored tool identity is inconsistent")
		}
	}
	contents := cloneContents(previous.Contents)
	if previous.Model != in.Model {
		contents = stripSignatures(contents)
		in.warn("model changed; prior reasoning was dropped")
	}
	for i := range in.references {
		in.references[i].Content += len(contents)
	}
	for i := range in.historyCalls {
		in.historyCalls[i].Content += len(contents)
	}
	in.Request.Contents = append(contents, in.Request.Contents...)
	in.HistorySystem = joinSystem(previous.HistorySystem, in.HistorySystem)
	var output struct {
		Output []json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(previous.Response, &output); err != nil {
		return err
	}
	items := append([]json.RawMessage(nil), previous.Items...)
	items = append(items, output.Output...)
	in.OriginalItems = append(items, in.OriginalItems...)
	return nil
}

// resolveHistoryTools completes the identity of each call in the client's
// input and names it. An omitted or empty namespace is unspecified. A matching
// replay record confirms the full identity, including an ordinary tool's empty
// namespace. Otherwise current declarations may resolve it. Explicit nonempty
// namespaces are preserved; signature eligibility is checked separately.
func (s replayStore) resolveHistoryTools(ctx context.Context, in *requestPlan) error {
	in.toolRecords = map[string]SignatureRecord{}
	fromState, fromDeclarations := 0, 0
	for _, h := range in.historyCalls {
		call := in.Request.Contents[h.Content].Parts[h.Part].FunctionCall
		identity := h.Tool
		resolved := identity.Namespace != ""
		if agyCallID(call.ID) {
			var record SignatureRecord
			err := s.State.Get(ctx, "tool", call.ID, &record)
			if err != nil && !errors.Is(err, state.ErrNotFound) {
				return err
			}
			if err == nil {
				in.toolRecords[call.ID] = record
				if !resolved && record.Tool.Name == identity.Name &&
					record.Tool.Custom == identity.Custom && record.Digest == digest(call.Args) {
					identity = record.Tool
					resolved = true
					if identity.Namespace != "" {
						fromState++
					}
				}
			}
		}
		if !resolved {
			if declared, ok := in.Tools[toolSymbol(identity)]; !ok || declared != identity {
				var candidate ToolName
				matches := 0
				for _, t := range in.Tools {
					if t.Namespace != "" && t.Name == identity.Name && t.Custom == identity.Custom {
						candidate, matches = t, matches+1
					}
				}
				switch {
				case matches == 1:
					identity = candidate
					fromDeclarations++
				case matches > 1:
					in.warn("namespace of historical tool call %s is ambiguous: several declared tools match", call.ID)
				}
			}
		}
		native, err := in.registerHistoryTool(identity)
		if err != nil {
			return err
		}
		call.Name = native
	}
	if fromState > 0 {
		in.warn("filled the missing namespace of %d historical tool call(s) from replay state", fromState)
	}
	if fromDeclarations > 0 {
		in.warn("filled the missing namespace of %d historical tool call(s) from declared tools", fromDeclarations)
	}
	return nil
}

func (s replayStore) restore(ctx context.Context, in *requestPlan) error {
	refs := map[[2]int]replayReference{}
	for _, ref := range in.references {
		refs[[2]int{ref.Content, ref.Part}] = ref
	}
	var restored ag.Request
	for i, c := range in.Request.Contents {
		var parts []ag.Part
		for j, p := range c.Parts {
			if ref, ok := refs[[2]int{i, j}]; ok {
				replay, err := s.open(in.Model, ref.Token)
				if err != nil {
					in.warn("dropped a reasoning item: %v", err)
					continue
				}
				if ref.Visible != "" {
					var visible strings.Builder
					for _, part := range replay.Parts {
						visible.WriteString(part.Text)
					}
					if visible.String() != ref.Visible {
						in.warn("dropped a thinking block because its text changed")
						continue
					}
				}
				if replay.PreviousText != "" {
					prev := parts
					if len(prev) == 0 && len(restored.Contents) > 0 && restored.Contents[len(restored.Contents)-1].Role == "model" {
						prev = restored.Contents[len(restored.Contents)-1].Parts
					}
					if len(prev) == 0 || digest(raw(prev[len(prev)-1].Text)) != replay.PreviousText {
						in.warn("dropped a text signature because the signed text changed")
						continue
					}
					prev[len(prev)-1].Signature = replay.Signature
				} else {
					parts = append(parts, replay.Parts...)
				}
				continue
			}
			if p.FunctionCall != nil && p.Signature == "" && agyCallID(p.FunctionCall.ID) {
				record, found := in.toolRecords[p.FunctionCall.ID]
				if !found {
					// Calls from stored history were not resolved from client input.
					err := s.State.Get(ctx, "tool", p.FunctionCall.ID, &record)
					if err != nil && !errors.Is(err, state.ErrNotFound) {
						return err
					}
					found = err == nil
				}
				// The full identity, not the short-hashed name, decides whether a
				// signature belongs to this call; a mismatch only declines it.
				identity, known := in.historyTools[p.FunctionCall.Name]
				if found && known && record.Model == in.Model && record.Tool == identity && record.Name == p.FunctionCall.Name && record.Digest == digest(p.FunctionCall.Args) {
					p.Signature = record.Signature
				} else {
					in.warn("tool replay state for %s is missing, expired or changed; using a placeholder signature", p.FunctionCall.ID)
				}
			}
			if p.FunctionResponse != nil && p.FunctionResponse.Name == "" {
				name, err := toolName(p.FunctionResponse.ID, restored.Contents)
				if err != nil {
					return err
				}
				p.FunctionResponse.Name = name
			}
			parts = append(parts, p)
		}
		appendContent(&restored, c.Role, parts)
	}
	in.Request.Contents = restored.Contents
	in.references = nil
	return nil
}
