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

func TestPrintModelsShowsQuota(t *testing.T) {
	var catalog ag.Catalog
	if err := json.Unmarshal([]byte(`{
		"models":{
			"gemini-a":{"quotaInfo":{"remainingFraction":0.9968903,"resetTime":"2026-10-28T00:05:48Z"}},
			"gemini-spent":{"quotaInfo":{"resetTime":"2026-10-28T00:05:48Z"}},
			"gemini-unmetered":{},
			"gemini-canvas":{"quotaInfo":{"remainingFraction":1,"resetTime":"invalid"}}
		},
		"agentModelSorts":[{"groups":[{"modelIds":["gemini-a","gemini-spent"]}]}],
		"commandModelIds":["gemini-unmetered"],
		"imageGenerationModelIds":["gemini-canvas"]
	}`), &catalog); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := printModels(&out, catalog, time.FixedZone("UTC+8", 8*3600)); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"MODEL             QUOTA   RESETS",
		"gemini-a          99.7%   2026-10-28 08:05",
		"gemini-spent      0.0%    2026-10-28 08:05",
		"gemini-unmetered  -       -",
		"gemini-canvas     100.0%  -",
	}
	if got := strings.Split(strings.TrimRight(out.String(), "\n"), "\n"); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("output:\n%s", out.String())
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
	err := printQuota(&out, accounts, func(a account.Account) (ag.Catalog, error) {
		if a.ID == "a3f9c2e1" {
			return ag.Catalog{}, errors.New("OAuth refresh failed")
		}
		var c ag.Catalog
		err := json.Unmarshal([]byte(`{"models":{"gemini-a":{}},"agentModelSorts":[{"groups":[{"modelIds":["gemini-a"]}]}]}`), &c)
		return c, err
	}, time.UTC)
	want = []string{
		"a3f9c2e1  alice@example.com (Alice)",
		"error: OAuth refresh failed",
		"",
		"0b1c2d3e  bob@example.com",
		"MODEL     QUOTA  RESETS",
		"gemini-a  -      -",
	}
	if got := strings.TrimRight(out.String(), "\n"); got != strings.Join(want, "\n") || err == nil || !strings.Contains(err.Error(), "1 of 2") {
		t.Fatalf("quota (%v):\n%s", err, got)
	}
}
