package protocol

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/zclkkk/agy2api/internal/state"
)

// A compaction item seals its summary with the instance key: it reveals
// nothing, survives a restart, and anything else in its place fails the
// request instead of being dropped.
func TestCompactionItems(t *testing.T) {
	dir := t.TempDir()
	s, err := state.Open(dir, time.Hour, 100, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	token, err := (replayStore{s}).sealCompaction("the private summary", "gemini-test")
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, "agy2."))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("the private summary")) {
		t.Fatal("compaction contains a plaintext summary")
	}
	sealed[len(sealed)-1] ^= 1
	tampered := "agy2." + base64.RawURLEncoding.EncodeToString(sealed)
	reasoning, err := s.Seal("agy2api/replay/v2", compactionCapsule{Summary: "wrong scope"})
	if err != nil {
		t.Fatal(err)
	}
	request := func(s *state.Store, token string) (PreparedRequest, error) {
		return testPipeline{State: s}.Responses(map[string]any{
			"model": "gemini-other",
			"input": []any{map[string]string{"type": "compaction", "encrypted_content": token}, map[string]string{"role": "user", "content": "continue"}},
		})
	}
	for name, bad := range map[string]string{
		"empty": "", "foreign": "gAAAA-openai", "not base64": "agy2.!!", "not sealed": "agy2.e30",
		"tampered": tampered, "reasoning token": reasoning,
	} {
		if _, err := request(s, bad); err == nil || !strings.Contains(err.Error(), "compaction") {
			t.Errorf("%s compaction item accepted: %v", name, err)
		}
	}
	if _, err := request(testStore(t), token); err == nil {
		t.Fatal("another instance opened the summary")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err = state.Open(dir, time.Hour, 100, 1<<20); err != nil {
		t.Fatal(err)
	}
	in, err := request(s, token)
	if err != nil || !strings.Contains(in.Request.Contents[0].Parts[0].Text, "the private summary") {
		t.Fatalf("summary not restored after restart: %v", err)
	}
}
