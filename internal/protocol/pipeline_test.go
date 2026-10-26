package protocol

import (
	"context"
	"encoding/json"
	"net/http"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
	"github.com/zclkkk/agy2api/internal/state"
)

// testPipeline exercises the same decode/plan boundary as HTTP requests.
// A catalog and headers may be supplied for routing and session tests.
type testPipeline struct {
	State   *state.Store
	catalog *ag.Catalog
	headers http.Header
}

func (p testPipeline) prepare(ctx context.Context, in RequestIntent, err error, operation Operation) (PreparedRequest, error) {
	if err != nil {
		return PreparedRequest{}, err
	}
	catalog := testCatalog
	if p.catalog != nil {
		catalog = *p.catalog
	}
	// Synthetic models used by replay tests do not need account discovery.
	if _, ok := catalog.Models[in.Model]; !ok {
		catalog.Models = map[string]json.RawMessage{}
		catalog.Models[in.Model] = json.RawMessage(`{}`)
	}
	return (Planner{State: p.State}).Prepare(ctx, in, catalog, operation, p.headers)
}

func (p testPipeline) Responses(ctx context.Context, body []byte) (PreparedRequest, error) {
	in, err := (Decoder{}).Responses(body)
	return p.prepare(ctx, in, err, Generate)
}

func (p testPipeline) Messages(ctx context.Context, body []byte, count bool) (PreparedRequest, error) {
	in, err := (Decoder{}).Messages(body, count)
	operation := Generate
	if count {
		operation = CountTokens
	}
	return p.prepare(ctx, in, err, operation)
}
