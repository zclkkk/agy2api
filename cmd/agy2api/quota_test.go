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
				{"window":"5h","remainingFraction":0.8156261,"resetTime":"2026-11-11T19:46:38Z"}]},
			{"displayName":"Claude and GPT models","buckets":[
				{"window":"weekly","remainingFraction":1,"resetTime":"2026-11-18T00:00:00Z"},
				{"window":"5h","disabled":true}]},
			{"displayName":"Future group","buckets":[
				{"displayName":"Daily limit","remainingAmount":"42"},
				{"bucketId":"new-window"},{}]},
			{"buckets":[{"window":"weekly"}]}]
	}`), &summary); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	now := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	if err := printQuotaSummary(&out, summary, time.FixedZone("UTC+8", 8*3600), now); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"GROUP WINDOW REMAINING STATUS RESETS",
		"Gemini Models weekly 24.09% available 2026-11-17 01:46 UTC+8",
		"Gemini Models 5h 81.56% available 2026-11-12 03:46 UTC+8",
		"Claude and GPT models weekly 100.00% available -",
		"Claude and GPT models 5h - disabled -",
		"Future group Daily limit 42 available -",
		"Future group new-window - unknown -",
		"Future group - - unknown -",
		"- weekly - unknown -",
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != len(want) {
		t.Fatalf("rows:\n%s", out.String())
	}
	for i, line := range lines {
		if got := strings.Join(strings.Fields(line), " "); got != want[i] {
			t.Errorf("row %d = %q, want %q", i, got, want[i])
		}
	}
}

func TestQuotaDetailsDistinguishesZeroUnknownAndResetState(t *testing.T) {
	now := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, value, reset, remaining, status, formattedReset string
	}{
		{"zero", `"remainingFraction":0`, "2026-11-11T05:00:00Z", "0.00%", "exhausted", "2026-11-11 13:00 UTC+8"},
		{"missing", `"bucketId":"missing"`, "2026-11-11T05:00:00Z", "-", "unknown", "-"},
		{"null", `"remainingFraction":null`, "", "-", "unknown", "-"},
		{"small positive", `"remainingFraction":0.0004`, "", "0.04%", "available", "-"},
		{"tiny positive", `"remainingFraction":0.000001`, "", "<0.01%", "available", "-"},
		{"almost full", `"remainingFraction":0.999999`, "", ">99.99%", "available", "-"},
		{"full", `"remainingFraction":1`, "2026-11-11T05:00:00Z", "100.00%", "available", "-"},
		{"disabled", `"remainingFraction":1,"disabled":true`, "2026-11-11T05:00:00Z", "-", "disabled", "-"},
		{"past reset", `"remainingFraction":0`, "2026-11-10T23:00:00Z", "0.00%", "reset due", "2026-11-11 07:00 UTC+8"},
		{"reset now", `"remainingFraction":0.5`, "2026-11-11T00:00:00Z", "50.00%", "reset due", "2026-11-11 08:00 UTC+8"},
		{"invalid reset", `"remainingFraction":0.5`, "invalid", "50.00%", "available", "-"},
		{"absolute amount", `"remainingAmount":"9007199254740993"`, "2026-11-11T05:00:00Z", "9007199254740993", "available", "2026-11-11 13:00 UTC+8"},
		{"zero amount", `"remainingAmount":0`, "", "0", "exhausted", "-"},
		{"negative fraction", `"remainingFraction":-0.1`, "", "-", "unknown", "-"},
		{"fraction over one", `"remainingFraction":1.1`, "", "-", "unknown", "-"},
		{"negative amount", `"remainingAmount":"-1"`, "", "-", "unknown", "-"},
		{"fractional amount", `"remainingAmount":1.5`, "", "-", "unknown", "-"},
		{"ambiguous value", `"remainingFraction":0,"remainingAmount":"42"`, "", "-", "unknown", "-"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var bucket ag.QuotaBucket
			if err := json.Unmarshal([]byte(`{`+tc.value+`,"resetTime":"`+tc.reset+`"}`), &bucket); err != nil {
				t.Fatal(err)
			}
			remaining, status, reset := quotaDetails(bucket, time.FixedZone("UTC+8", 8*3600), now)
			if remaining != tc.remaining || status != tc.status || reset != tc.formattedReset {
				t.Fatalf("got (%q, %q, %q), want (%q, %q, %q)", remaining, status, reset, tc.remaining, tc.status, tc.formattedReset)
			}
		})
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
		"GROUP   WINDOW  REMAINING  STATUS   RESETS",
		"Gemini  5h      -          unknown  -",
	}
	if got := strings.TrimRight(out.String(), "\n"); got != strings.Join(want, "\n") || err == nil || !strings.Contains(err.Error(), "1 of 2") {
		t.Fatalf("quota (%v):\n%s", err, got)
	}
}
