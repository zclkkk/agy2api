package protocol

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
	"github.com/zclkkk/agy2api/internal/state"
)

const recordVersion = 2

type StoredResponse struct {
	Version       int               `json:"version,omitempty"`
	Model         string            `json:"model"`
	UpstreamModel string            `json:"upstream_model,omitempty"`
	HistorySystem *ag.Content       `json:"history_system,omitempty"`
	Contents      []ag.Content      `json:"contents"`
	Response      json.RawMessage   `json:"response"`
	Items         []json.RawMessage `json:"items"`
}

// replayStore provides typed access to all records used by the protocol. Missing
// cache entries may degrade; storage failures are returned to the caller.
type replayStore struct{ State *state.Store }

func (s replayStore) response(ctx context.Context, id string) (StoredResponse, error) {
	var value StoredResponse
	err := s.State.Get(ctx, "response", id, &value)
	if err == nil && value.Version > recordVersion {
		err = errors.New("unsupported stored response version")
	}
	return value, err
}

func (s replayStore) seal(model string, replay Replay) (string, error) {
	replay.Version, replay.Model, replay.Kind = recordVersion, model, "reasoning"
	token, err := s.State.Seal("agy2api/replay/v2", replay)
	if err != nil {
		return "", err
	}
	return "agy2." + strings.TrimPrefix(token, "agy1."), nil
}

func (s replayStore) open(model, token string) (Replay, error) {
	var replay Replay
	if strings.HasPrefix(token, "agy2.") {
		err := s.State.Unseal("agy2api/replay/v2", "agy1."+strings.TrimPrefix(token, "agy2."), &replay)
		if err != nil {
			return replay, err
		}
		if replay.Version != recordVersion || replay.Kind != "reasoning" {
			return Replay{}, errors.New("unsupported replay record")
		}
		if replay.Model != model {
			return Replay{}, errors.New("reasoning belongs to a different upstream model")
		}
		return replay, nil
	}
	// v1 carriers are authenticated against the selected upstream model, never
	// the public alias. New carriers carry authenticated model provenance.
	err := s.State.Unseal(model, token, &replay)
	return replay, err
}

type replayReference struct {
	Content, Part           int
	Token, Visible, ImageID string
}

func (in *RequestIntent) deferReplay(token, visible string) {
	appendContent(&in.Request, "model", []ag.Part{{}})
	i := len(in.Request.Contents) - 1
	in.references = append(in.references, replayReference{Content: i, Part: len(in.Request.Contents[i].Parts) - 1, Token: token, Visible: visible})
}

func (in *RequestIntent) deferImage(id, data string) {
	head := data[:min(len(data), 1024)]
	head = head[:len(head)/4*4]
	sample, _ := base64.StdEncoding.DecodeString(head)
	appendContent(&in.Request, "model", []ag.Part{{InlineData: &ag.Blob{MIMEType: http.DetectContentType(sample), Data: data}}})
	i := len(in.Request.Contents) - 1
	in.references = append(in.references, replayReference{Content: i, Part: len(in.Request.Contents[i].Parts) - 1, ImageID: id})
}

func (in *requestPlan) prependHistory(previous StoredResponse) error {
	contents := cloneContents(previous.Contents)
	if previous.Version < recordVersion || previous.UpstreamModel != in.UpstreamModel() {
		contents = stripSignatures(contents)
		in.warn("upstream model changed or legacy history has no model provenance; prior reasoning was dropped")
	}
	for i := range in.references {
		in.references[i].Content += len(contents)
	}
	in.Request.Contents = append(contents, in.Request.Contents...)
	if previous.Version < recordVersion {
		// v1 retained the public input items but lost system messages from native
		// history. Recover these messages without inheriting top-level instructions.
		for _, item := range previous.Items {
			var v ResponseItem
			if json.Unmarshal(item, &v) != nil {
				return errors.New("invalid legacy response history")
			}
			if (v.Type == "" || v.Type == "message") && (v.Role == "developer" || v.Role == "system") {
				parts, err := mediaParts(v.Content)
				if err != nil {
					return err
				}
				previous.HistorySystem = joinSystem(previous.HistorySystem, &ag.Content{Parts: parts})
			}
		}
	}
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
				if p.InlineData != nil {
					var record ImageRecord
					err := s.State.Get(ctx, "image", ref.ImageID, &record)
					if err != nil && !errors.Is(err, state.ErrNotFound) {
						return err
					}
					if err == nil && record.Model == in.UpstreamModel() && record.Digest == digest(raw(p.InlineData.Data)) {
						p.InlineData = &ag.Blob{MIMEType: record.MIMEType, Data: p.InlineData.Data}
						p.Signature = record.Signature
					} else {
						in.warn("image replay metadata for %s is missing or changed", ref.ImageID)
					}
				} else {
					replay, err := s.open(in.UpstreamModel(), ref.Token)
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
			}
			if p.FunctionCall != nil && p.Signature == "" && agyCallID(p.FunctionCall.ID) {
				var record SignatureRecord
				err := s.State.Get(ctx, "tool", p.FunctionCall.ID, &record)
				if err != nil && !errors.Is(err, state.ErrNotFound) {
					return err
				}
				if err == nil && record.Model == in.UpstreamModel() && record.Name == p.FunctionCall.Name && record.Digest == digest(p.FunctionCall.Args) {
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
