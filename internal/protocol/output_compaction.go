package protocol

import (
	"context"
	"strings"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

func (r *Renderer) finishCompaction(ctx context.Context) error {
	summary := strings.TrimSpace(r.summary.String())
	if summary == "" {
		return &ag.HTTPError{Status: 502, Message: "Antigravity returned an empty compaction summary; retry the request"}
	}
	item := map[string]any{"id": ag.ID("cmp_"), "type": "compaction", "encrypted_content": sealCompaction(summary, r.input.Model)}
	if r.input.Compact == "endpoint" {
		r.Result = raw(map[string]any{"id": r.id, "object": "response.compaction", "created_at": r.created, "output": []any{item}, "usage": r.responseUsage()})
		return nil
	}
	index := len(r.items)
	if err := r.event("response.output_item.added", map[string]any{"output_index": index, "item": item}); err != nil {
		return err
	}
	if err := r.event("response.output_item.done", map[string]any{"output_index": index, "item": item}); err != nil {
		return err
	}
	r.items = append(r.items, item)
	response := r.response("completed", nil)
	r.Result = raw(response)
	contents := []ag.Content{{Role: "user", Parts: []ag.Part{summaryPart(summary)}}}
	if err := r.recordResponse(ctx, contents); err != nil {
		return err
	}

	return r.event("response.completed", map[string]any{"response": response})
}
