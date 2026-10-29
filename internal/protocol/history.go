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
	replay.Version, replay.Model = recordVersion, model
	return s.State.Seal("agy2api/replay/v2", replay)
}

func (s replayStore) open(model, token string) (Replay, error) {
	var replay Replay
	if err := s.State.Unseal("agy2api/replay/v2", token, &replay); err != nil {
		return Replay{}, err
	}
	if replay.Version != recordVersion {
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
	Compaction     bool
}

func (in *RequestIntent) deferReplay(token, visible string) {
	appendContent(&in.Request, "model", []ag.Part{{}})
	i := len(in.Request.Contents) - 1
	in.references = append(in.references, replayReference{Content: i, Part: len(in.Request.Contents[i].Parts) - 1, Token: token, Visible: visible})
}

func (in *RequestIntent) prependHistory(previous StoredResponse) error {
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

// nameHistoryCalls names each call in the client's input after the tool the
// client identified. Whether a call's signature can be replayed is checked
// against its record when the history is restored.
func (in *RequestIntent) nameHistoryCalls() error {
	for _, h := range in.historyCalls {
		call := in.Request.Contents[h.Content].Parts[h.Part].FunctionCall
		native, err := in.registerHistoryTool(h.Tool)
		if err != nil {
			return err
		}
		call.Name = native
	}
	return nil
}

func (s replayStore) restore(ctx context.Context, in *RequestIntent) error {
	refs := map[[2]int]replayReference{}
	for _, ref := range in.references {
		refs[[2]int{ref.Content, ref.Part}] = ref
	}
	var restored ag.Request
	for i, c := range in.Request.Contents {
		var parts []ag.Part
		for j, p := range c.Parts {
			if ref, ok := refs[[2]int{i, j}]; ok {
				if ref.Compaction {
					summary, err := s.openCompaction(ref.Token)
					if err != nil {
						return invalid("input", "compaction item cannot be restored; provide a valid compaction item or the original history")
					}
					parts = append(parts, summaryPart(summary))
					continue
				}
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
				var record SignatureRecord
				err := s.State.Get(ctx, "tool", p.FunctionCall.ID, &record)
				if err != nil && !errors.Is(err, state.ErrNotFound) {
					return err
				}
				found := err == nil
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
