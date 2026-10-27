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
			"selector order and image union",
			`{
				"agentModelSorts":[
					{"groups":[{"modelIds":["gemini-z-high","missing","gemini-z-low"]},{"modelIds":["gemini-z-high","image-z"]}]},
					{"groups":[{"modelIds":["gemini-z-low","gemini-a"]}]}
				],
				"imageGenerationModelIds":["image-z","missing","image-a","image-z"],
				"tieredModelIds":{"flash":["gemini-unlisted"]},
				"webSearchModelIds":["gemini-unlisted"]
			}`,
			[]string{"gemini-z-high", "gemini-z-low", "image-z", "gemini-a", "image-a"},
		},
		{"missing selector", `{}`, nil},
		{"empty selector", `{"agentModelSorts":[]}`, nil},
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
			for _, id := range []string{"gemini-z-high", "gemini-z-low", "gemini-a", "image-z", "image-a", "gemini-unlisted", "tab_preview"} {
				catalog.Models[id] = json.RawMessage(`{"recommended":true}`)
			}
			if got := catalog.ListedModelIDs(); !slices.Equal(got, tc.want) {
				t.Fatalf("listed models = %v, want %v", got, tc.want)
			}
			if _, ok := catalog.Info("gemini-unlisted"); !ok || len(catalog.Models) != 7 {
				t.Fatal("listing changed the full catalog")
			}
		})
	}
}
