package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zclkkk/agy2api/internal/account"
	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

func TestPrintQuotaSummaryShowsSharedWindows(t *testing.T) {
	var summary ag.QuotaSummary
	if err := json.Unmarshal([]byte(`{
		"groups":[
			{"displayName":"Gemini Models","buckets":[
				{"window":"weekly","remainingFraction":0.24087813,"resetTime":"2026-11-16T17:46:38Z"},
				{"window":"5h","remainingFraction":1,"resetTime":"2026-11-11T19:46:38Z"}]},
			{"displayName":"Claude and GPT models","buckets":[
				{"window":"weekly","resetTime":"2026-11-18T00:00:00Z"},
				{"window":"5h","remainingFraction":0,"resetTime":"2026-11-11T19:46:38Z"}]},
			{"displayName":"Gemini 4 Argon","buckets":[
				{"window":"weekly","remainingFraction":0.99995255,"resetTime":"2026-11-17T15:32:33Z"}]}]
	}`), &summary); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := printQuotaSummary(&out, summary, time.FixedZone("UTC+8", 8*3600)); err != nil {
		t.Fatal(err)
	}
	// A full window hides its moving reset time; a missing fraction is not zero.
	want := []string{
		"GROUP                  WINDOW  REMAINING  RESETS",
		"Gemini Models          weekly  24.09%     2026-11-17 01:46 UTC+8",
		"Gemini Models          5h      100.00%    -",
		"Claude and GPT models  weekly  -          2026-11-18 08:00 UTC+8",
		"Claude and GPT models  5h      0.00%      2026-11-12 03:46 UTC+8",
		"Gemini 4 Argon         weekly  >99.99%    2026-11-17 23:32 UTC+8",
	}
	if got := strings.TrimRight(out.String(), "\n"); got != strings.Join(want, "\n") {
		t.Fatalf("output:\n%s", got)
	}
}

func TestPercentKeepsZeroAndFullExact(t *testing.T) {
	for fraction, want := range map[float64]string{
		0: "0.00%", 0.00004: "<0.01%", 0.0001: "0.01%", 0.5: "50.00%",
		0.9999: "99.99%", 0.99995255: ">99.99%", 1: "100.00%",
	} {
		if got := percent(fraction); got != want {
			t.Errorf("percent(%v) = %q, want %q", fraction, got, want)
		}
	}
}

func TestPrintAccountsAndQuota(t *testing.T) {
	created := time.Date(2026, 10, 28, 0, 5, 0, 0, time.UTC)
	accounts := []account.Account{
		{Info: account.Info{ID: "a3f9c2e1", APIKey: "agy_one", Email: "alice@example.com", Name: "Alice", CreatedAt: created}},
		{Info: account.Info{ID: "0b1c2d3e", APIKey: "agy_two", Email: "bob@example.com", CreatedAt: created}},
	}
	var out strings.Builder
	if err := printAccounts(&out, accounts, time.UTC); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"ID        EMAIL              NAME   API KEY  CREATED",
		"a3f9c2e1  alice@example.com  Alice  agy_one  2026-10-28 00:05",
		"0b1c2d3e  bob@example.com    -      agy_two  2026-10-28 00:05",
	}
	if got := strings.TrimRight(out.String(), "\n"); got != strings.Join(want, "\n") {
		t.Fatalf("accounts:\n%s", got)
	}

	// One account failing does not hide the others, but the command fails.
	out.Reset()
	err := printQuota(&out, accounts, func(a account.Account) (ag.QuotaSummary, error) {
		if a.ID == "a3f9c2e1" {
			return ag.QuotaSummary{}, errors.New("OAuth refresh failed")
		}
		return ag.QuotaSummary{Groups: []ag.QuotaGroup{{DisplayName: "Gemini", Buckets: []ag.QuotaBucket{{Window: "5h"}}}}}, nil
	}, time.UTC)
	want = []string{
		"a3f9c2e1  alice@example.com (Alice)",
		"error: OAuth refresh failed",
		"",
		"0b1c2d3e  bob@example.com",
		"GROUP   WINDOW  REMAINING  RESETS",
		"Gemini  5h      -          -",
	}
	if got := strings.TrimRight(out.String(), "\n"); got != strings.Join(want, "\n") || err == nil || !strings.Contains(err.Error(), "1 of 2") {
		t.Fatalf("quota (%v):\n%s", err, got)
	}
}
