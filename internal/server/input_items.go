package server

import (
	"encoding/json"
	"net/url"
	"slices"
	"strconv"

	"github.com/zclkkk/agy2api/internal/protocol"
)

func inputItemsPage(items []json.RawMessage, query url.Values) (map[string]any, error) {
	limit := 20
	if query.Has("limit") {
		var err error
		limit, err = strconv.Atoi(query.Get("limit"))
		if err != nil || limit < 1 || limit > 100 {
			return nil, &protocol.Error{Param: "limit", Message: "limit must be an integer between 1 and 100"}
		}
	}
	order := "desc"
	if query.Has("order") {
		order = query.Get("order")
	}
	if order != "asc" && order != "desc" {
		return nil, &protocol.Error{Param: "order", Message: "order must be asc or desc"}
	}
	ordered := slices.Clone(items)
	if order == "desc" {
		slices.Reverse(ordered)
	}
	itemID := func(data json.RawMessage) string {
		var item struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(data, &item)
		return item.ID
	}
	if query.Has("after") {
		after := query.Get("after")
		index := slices.IndexFunc(ordered, func(item json.RawMessage) bool { return after != "" && itemID(item) == after })
		if index < 0 {
			return nil, &protocol.Error{Param: "after", Message: "after must identify an input item in this response"}
		}
		ordered = ordered[index+1:]
	}
	hasMore := len(ordered) > limit
	page := make([]json.RawMessage, min(len(ordered), limit))
	copy(page, ordered)
	first, last := "", ""
	if len(page) > 0 {
		first, last = itemID(page[0]), itemID(page[len(page)-1])
	}
	return map[string]any{"object": "list", "data": page, "has_more": hasMore, "first_id": first, "last_id": last}, nil
}
