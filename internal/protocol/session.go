package protocol

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
)

// Headers that carry a client's own session ID: Codex sends Session-Id and
// Claude Code sends X-Claude-Code-Session-Id.
var sessionHeaders = []string{"Session-Id", "Session_id", "X-Claude-Code-Session-Id"}

// nativeSession formats a session ID the way the Antigravity client does:
// "-" followed by a non-negative 63-bit decimal number.
func nativeSession(seed string) string {
	var n uint64
	if seed == "" {
		var b [8]byte
		_, _ = rand.Read(b[:])
		n = binary.BigEndian.Uint64(b[:])
	} else {
		sum := sha256.Sum256([]byte("agy2api session\x00" + seed))
		n = binary.BigEndian.Uint64(sum[:8])
	}
	return "-" + strconv.FormatUint(n&math.MaxInt64, 10)
}

// assignSession sets the upstream session ID. A session ID sent by the client
// wins, so all requests of one client session share an upstream session.
// Otherwise the conversation is keyed by its first user message, which every
// follow-up request repeats.
func (in *Input) assignSession(headers http.Header) {
	client := in.ClientSession
	for _, name := range sessionHeaders {
		if v := strings.TrimSpace(headers.Get(name)); v != "" {
			client = v
			break
		}
	}
	if client != "" {
		in.Request.SessionID = nativeSession("client\x00" + client)
		return
	}
	seed := ""
	for _, c := range in.Request.Contents {
		if c.Role == "user" {
			seed = "user\x00" + string(raw(c.Parts))
			break
		}
	}
	in.Request.SessionID = nativeSession(seed)
}

// claudeCodeSession extracts the session from Messages metadata.user_id. Claude
// Code sends a JSON object with session_id; older versions used
// "user_<hash>_account_<id>_session_<id>".
func claudeCodeSession(metadata json.RawMessage) string {
	var m struct {
		UserID string `json:"user_id"`
	}
	if json.Unmarshal(metadata, &m) != nil || m.UserID == "" {
		return ""
	}
	var v struct {
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal([]byte(m.UserID), &v) == nil {
		return v.SessionID
	}
	if _, session, ok := strings.Cut(m.UserID, "_session_"); ok {
		return session
	}
	return ""
}
