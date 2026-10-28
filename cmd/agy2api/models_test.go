package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

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
