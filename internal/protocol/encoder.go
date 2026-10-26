package protocol

import (
	"encoding/json"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

// wireState is the complete input available to encoders. It deliberately has no
// database, provider client, or native-history accumulator.
type wireState struct {
	input    PreparedRequest
	emit     func(string, map[string]any) error
	id       string
	created  int64
	sequence int
	items    []map[string]any
	usage    ag.Usage
}

type searchPage struct{ URL, Title, Token string }
type searchTrace struct {
	Queries []string
	Pages   []searchPage
}

type wireEncoder interface {
	start(*wireState) error
	open(*wireState, *outputBlock) error
	delta(*wireState, *outputBlock, string) error
	close(*wireState, *outputBlock) error
	search(*wireState, searchTrace) error
	result(*wireState, string, string, any) json.RawMessage
	finish(*wireState, string, string, json.RawMessage) error
	fail(*wireState, string) error
}

func (r *wireState) event(name string, fields map[string]any) error {
	if r.emit == nil {
		return nil
	}
	fields["type"] = name
	if r.input.Format == Responses {
		fields["sequence_number"] = r.sequence
		r.sequence++
	}
	return r.emit(name, fields)
}
