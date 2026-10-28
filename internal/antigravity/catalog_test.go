package antigravity

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestCatalogListedModelIDs(t *testing.T) {
	for _, tc := range []struct {
		name, metadata string
		want           []string
	}{
		{
			"selector, roles, then images",
			`{
				"agentModelSorts":[
					{"groups":[{"modelIds":["gemini-z-high","missing","gemini-z-low"]},{"modelIds":["gemini-z-high","image-z"]}]},
					{"groups":[{"modelIds":["gemini-z-low","gemini-a"]}]}
				],
				"defaultAgentModelId":"gemini-z-high",
				"commandModelIds":["gemini-command","missing"],
				"commitMessageModelIds":["gemini-commit"],
				"mqueryModelIds":["gemini-query","gemini-commit"],
				"webSearchModelIds":["gemini-search"],
				"imageGenerationModelIds":["image-z","missing","image-a","image-z"],
				"tieredModelIds":{"flash":["gemini-unlisted"]},
				"tabModelIds":["tab_preview"]
			}`,
			[]string{"gemini-z-high", "gemini-z-low", "image-z", "gemini-a", "gemini-command", "gemini-commit", "gemini-query", "gemini-search", "image-a"},
		},
		{"missing selector", `{}`, nil},
		{"empty selector", `{"agentModelSorts":[]}`, nil},
		{"roles without selector", `{"defaultAgentModelId":"gemini-a","commandModelIds":["gemini-command"]}`, []string{"gemini-a", "gemini-command"}},
		{"missing selector with images", `{"imageGenerationModelIds":["image-z","image-a"]}`, []string{"image-z", "image-a"}},
		{"empty groups", `{"agentModelSorts":[{}, {"groups":[{}, {"modelIds":[]}]}],"imageGenerationModelIds":["image-a"]}`, []string{"image-a"}},
		{"no matching candidates", `{"agentModelSorts":[{"groups":[{"modelIds":["missing"]}]}],"imageGenerationModelIds":["missing"]}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var catalog Catalog
			if err := json.Unmarshal([]byte(tc.metadata), &catalog); err != nil {
				t.Fatal(err)
			}
			catalog.Models = make(map[string]json.RawMessage)
			ids := []string{"gemini-z-high", "gemini-z-low", "gemini-a", "gemini-command", "gemini-commit", "gemini-query", "gemini-search", "image-z", "image-a", "gemini-unlisted", "tab_preview"}
			for _, id := range ids {
				catalog.Models[id] = json.RawMessage(`{"recommended":true}`)
			}
			if got := catalog.ListedModelIDs(); !slices.Equal(got, tc.want) {
				t.Fatalf("listed models = %v, want %v", got, tc.want)
			}
			if _, ok := catalog.Info("gemini-unlisted"); !ok || len(catalog.Models) != len(ids) {
				t.Fatal("listing changed the full catalog")
			}
		})
	}
}
