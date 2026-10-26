package protocol

import (
	"strings"
	"unicode/utf8"
)

func (r *Renderer) annotations(text string) []any {
	result := []any{}
	if r.grounding == nil {
		return result
	}
	for _, support := range r.grounding.Supports {
		start, end := support.Segment.Start, support.Segment.End
		if support.Segment.Text != "" {
			start = strings.Index(text, support.Segment.Text)
			end = start + len(support.Segment.Text)
		}
		if start < 0 || end < start || end > len(text) {
			continue
		}
		for _, idx := range support.Indices {
			if idx < 0 || idx >= len(r.grounding.Chunks) {
				continue
			}
			w := r.grounding.Chunks[idx].Web
			if w == nil {
				continue
			}
			result = append(result, map[string]any{"type": "url_citation", "start_index": utf8.RuneCountInString(text[:start]), "end_index": utf8.RuneCountInString(text[:end]), "url": w.URI, "title": w.Title})
		}
	}
	return result
}

func (r *Renderer) messageCitations(text string) ([]any, error) {
	result := []any{}
	for _, v := range r.annotations(text) {
		a := v.(map[string]any)
		start, end := a["start_index"].(int), a["end_index"].(int)
		sealed, err := r.state.Seal(r.model(), a)
		if err != nil {
			return nil, err
		}
		result = append(result, map[string]any{"type": "web_search_result_location", "url": a["url"], "title": a["title"], "cited_text": string([]rune(text)[start:end]), "encrypted_index": sealed})
	}
	return result, nil
}

// searchTrace reports the search the model ran: a web_search_call item for
// Responses, and for Messages the server_tool_use and web_search_tool_result
// blocks that Anthropic clients such as Claude Code read source links from.
// They follow the answer because grounding arrives with the last chunk.

func (r *Renderer) searchTrace() error {
	if !r.input.Search || r.grounding == nil {
		return nil
	}
	trace := searchTrace{Queries: r.grounding.Queries}
	seen := map[string]bool{}
	for _, chunk := range r.grounding.Chunks {
		w := chunk.Web
		if w == nil || w.URI == "" || seen[w.URI] {
			continue
		}
		seen[w.URI] = true
		page := searchPage{URL: w.URI, Title: w.Title}
		if r.input.Format == Messages {
			token, err := r.state.Seal(r.model(), map[string]string{"url": w.URI, "title": w.Title})
			if err != nil {
				return err
			}
			page.Token = token
		}
		trace.Pages = append(trace.Pages, page)
	}
	return r.encoder.search(&r.wireState, trace)
}
