package protocol

import (
	"errors"
	"time"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

// Compaction is carried by the client in an authenticated, encrypted capsule.
// Only the instance key is needed to restore it; no per-capsule record is stored.
const compactionScope = "agy2api/compaction/v1"

const compactionPrompt = `The conversation so far is being compacted. Write a summary that will replace all of the messages above. Work will continue from this summary alone, without access to the earlier messages, so it must be complete and specific.

Include:
- The user's requests, goals, constraints and preferences, quoting the user where the wording matters.
- What has been done: decisions and their reasons, files, commands and tools used, results, and errors with how they were resolved.
- The current state of the work: which requested steps are already complete, with the results of the latest tool calls, what remains to be done, and the immediate next step.
- Exact details needed to continue, such as names, paths, identifiers, values and short code excerpts.

Reply with the summary only. Do not call tools.`

type compactionCapsule struct {
	Summary string `json:"summary"`
	Model   string `json:"model"`
	Created int64  `json:"created"`
}

func (s replayStore) sealCompaction(summary, model string) (string, error) {
	return s.State.Seal(compactionScope, compactionCapsule{Summary: summary, Model: model, Created: time.Now().Unix()})
}

func (s replayStore) openCompaction(token string) (string, error) {
	var c compactionCapsule
	if err := s.State.Unseal(compactionScope, token, &c); err != nil {
		return "", err
	}
	if c.Summary == "" {
		return "", errors.New("compaction summary is empty")
	}
	return c.Summary, nil
}

func (in *RequestIntent) deferCompaction(token string) {
	appendContent(&in.Request, "user", []ag.Part{{}})
	i := len(in.Request.Contents) - 1
	in.references = append(in.references, replayReference{Content: i, Part: len(in.Request.Contents[i].Parts) - 1, Token: token, Compaction: true})
}

// summaryPart is the user-turn text that stands in for compacted history.
func summaryPart(summary string) ag.Part {
	return ag.Part{Text: "<conversation_summary>\nEarlier messages of this conversation were replaced by this summary. Continue the work from it; do not repeat steps it reports as complete.\n\n" + summary + "\n</conversation_summary>"}
}

// disableTools prevents tool calls while the model writes a compaction summary.
// Tools stay declared because the history may contain calls to them, but the
// model may not call any.
func (in *RequestIntent) disableTools() {
	in.Request.Generation.MIMEType = ""
	in.Request.Generation.Schema = nil
	in.Choice = ToolChoice{Mode: "none"}
}
