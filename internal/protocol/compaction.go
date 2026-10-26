package protocol

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

// Compaction replaces a conversation's history with a summary the model
// writes. The summary travels in the compaction item's encrypted_content, as
// OpenAI's opaque compaction items do. It is encoded rather than encrypted:
// it holds only the client's own conversation, and keeping it free of local
// state lets a compacted conversation continue after the state store is lost.
const compactionPrefix = "agy2api.compaction.v1."

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

func sealCompaction(summary, model string) string {
	data, _ := json.Marshal(compactionCapsule{Summary: summary, Model: model, Created: time.Now().Unix()})
	return compactionPrefix + base64.RawURLEncoding.EncodeToString(data)
}

func openCompaction(token string) (string, bool) {
	encoded, ok := strings.CutPrefix(token, compactionPrefix)
	if !ok {
		return "", false
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", false
	}
	var c compactionCapsule
	if json.Unmarshal(data, &c) != nil || c.Summary == "" {
		return "", false
	}
	return c.Summary, true
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
