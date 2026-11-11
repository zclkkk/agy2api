package antigravity

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestQuotaSummaryReadsIndependentWindowsAndValuePresence(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1internal:retrieveUserQuotaSummary" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("User-Agent") != "antigravity/hub/test" {
			t.Error("missing account or client identity")
		}
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || len(payload) != 1 || payload["project"] != "test-project" {
			t.Errorf("payload = %v, error = %v", payload, err)
		}
		fmt.Fprint(w, `{"description":"Shared limits","groups":[
			{"displayName":"Gemini Models","buckets":[
				{"bucketId":"gemini-weekly","displayName":"Weekly Limit Remaining","window":"weekly","remainingFraction":0.24087813,"resetTime":"2026-11-16T17:46:38Z"},
				{"bucketId":"gemini-5h","window":"5h","remainingFraction":0.8156261,"resetTime":"2026-11-11T19:46:38Z"}]},
			{"displayName":"Other models","buckets":[
				{"remainingFraction":0}, {}, {"remainingFraction":null}, {"disabled":true},
				{"remainingAmount":"9007199254740993"}, {"remainingAmount":0}]}]}`)
	}))
	defer up.Close()
	c := Client{HTTP: up.Client(), BaseURL: up.URL, Auth: testToken{}, UserAgent: func() string { return "antigravity/hub/test" }}
	summary, err := c.QuotaSummary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Groups) != 2 || len(summary.Groups[0].Buckets) != 2 || len(summary.Groups[1].Buckets) != 6 {
		t.Fatalf("groups = %+v", summary.Groups)
	}
	gemini := summary.Groups[0]
	weekly, fiveHour := gemini.Buckets[0], gemini.Buckets[1]
	if gemini.DisplayName != "Gemini Models" || weekly.ID != "gemini-weekly" || weekly.DisplayName != "Weekly Limit Remaining" || weekly.Window != "weekly" || weekly.RemainingFraction == nil || *weekly.RemainingFraction != 0.24087813 || weekly.ResetTime != "2026-11-16T17:46:38Z" {
		t.Fatalf("weekly = %+v", weekly)
	}
	if fiveHour.Window != "5h" || fiveHour.RemainingFraction == nil || *fiveHour.RemainingFraction != 0.8156261 || fiveHour.ResetTime != "2026-11-11T19:46:38Z" {
		t.Fatalf("five hour = %+v", fiveHour)
	}
	b := summary.Groups[1].Buckets
	if b[0].RemainingFraction == nil || *b[0].RemainingFraction != 0 || b[1].RemainingFraction != nil || b[2].RemainingFraction != nil || !b[3].Disabled {
		t.Fatalf("zero, missing, null or disabled lost: %+v", b)
	}
	for i, want := range []int64{9007199254740993, 0} {
		if b[i+4].RemainingAmount == nil {
			t.Fatal("missing amount")
		}
		if got, err := b[i+4].RemainingAmount.Int64(); err != nil || got != want {
			t.Fatalf("amount = %d, %v; want %d", got, err, want)
		}
	}
}

func TestQuotaSummaryErrorsDoNotFallBackToModelMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		status           int
	}{
		{"invalid JSON", `{`, "invalid JSON", 200},
		{"wrong schema", `{"models":{"m":{"quotaInfo":{"remainingFraction":1}}}}`, "no quota buckets", 200},
		{"empty groups", `{"groups":[]}`, "no quota buckets", 200},
		{"empty buckets", `{"groups":[{"displayName":"Gemini Models","buckets":[]}]}`, "no quota buckets", 200},
		{"wrong fraction type", `{"groups":[{"buckets":[{"remainingFraction":"bad"}]}]}`, "invalid JSON", 200},
		{"unavailable", `{"error":{"status":"PERMISSION_DENIED","message":"private-account"}}`, "HTTP 403 (PERMISSION_DENIED)", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/v1internal:retrieveUserQuotaSummary" {
					t.Errorf("unexpected fallback: %s", r.URL)
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer up.Close()
			c := Client{HTTP: up.Client(), BaseURL: up.URL, Auth: testToken{}}
			_, err := c.QuotaSummary(context.Background())
			if calls != 1 || err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "private-account") {
				t.Fatalf("calls = %d, error = %v", calls, err)
			}
		})
	}
}
