package server

import (
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"testing"
)

func TestInputItemsPage(t *testing.T) {
	items := []json.RawMessage{
		json.RawMessage(`{"id":"a","type":"message"}`),
		json.RawMessage(`{"id":"b","type":"message"}`),
		json.RawMessage(`{"id":"c","type":"message"}`),
	}
	for _, tc := range []struct {
		query string
		ids   []string
		more  bool
	}{
		{"", []string{"c", "b", "a"}, false},
		{"order=asc&limit=1", []string{"a"}, true},
		{"order=asc&limit=1&after=a", []string{"b"}, true},
		{"order=desc&limit=1&after=c", []string{"b"}, true},
		{"order=desc&after=b", []string{"a"}, false},
		{"order=desc&after=a", []string{}, false},
	} {
		t.Run(tc.query, func(t *testing.T) {
			query, _ := url.ParseQuery(tc.query)
			page, err := inputItemsPage(items, query)
			if err != nil {
				t.Fatal(err)
			}
			data := page["data"].([]json.RawMessage)
			var ids []string
			for _, raw := range data {
				var item struct{ ID string }
				_ = json.Unmarshal(raw, &item)
				ids = append(ids, item.ID)
			}
			if data == nil || !slices.Equal(ids, tc.ids) || page["has_more"] != tc.more {
				t.Fatalf("page=%v", page)
			}
			first, last := "", ""
			if len(ids) > 0 {
				first, last = ids[0], ids[len(ids)-1]
			}
			if page["first_id"] != first || page["last_id"] != last {
				t.Fatal("incorrect page cursors")
			}
		})
	}
	if string(items[0]) != `{"id":"a","type":"message"}` {
		t.Fatal("pagination mutated stored input")
	}
	for _, value := range []string{"limit=0", "limit=101", "limit=abc", "limit=", "order=wrong", "order=", "after=missing", "after="} {
		query, _ := url.ParseQuery(value)
		if _, err := inputItemsPage(items, query); err == nil {
			t.Fatalf("accepted %s", value)
		}
	}
	var many []json.RawMessage
	for i := 0; i < 25; i++ {
		many = append(many, json.RawMessage(fmt.Sprintf(`{"id":"item_%02d"}`, i)))
	}
	page, err := inputItemsPage(many, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(page["data"].([]json.RawMessage)) != 20 || page["has_more"] != true || page["first_id"] != "item_24" || page["last_id"] != "item_05" {
		t.Fatalf("default pagination: %v", page)
	}
}
