package antigravity

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestCatalogOffersVariantsAsOneModel(t *testing.T) {
	catalog := Catalog{
		Models: map[string]json.RawMessage{
			"gemini-x-low":   json.RawMessage(`{"displayName":"Gemini X (Low)","supportsThinking":true,"minThinkingBudget":32,"thinkingLevel":1}`),
			"gemini-x-high":  json.RawMessage(`{"displayName":"Gemini X (High)","supportsThinking":true,"minThinkingBudget":32,"thinkingLevel":3}`),
			"gemini-x-xhigh": json.RawMessage(`{"displayName":"Gemini X (XHigh)","supportsThinking":true,"minThinkingBudget":32,"thinkingLevel":100}`),
			// The level a model's ID ends in must be its own.
			"gemini-y-medium": json.RawMessage(`{"displayName":"Gemini Y","supportsThinking":true,"minThinkingBudget":128,"thinkingLevel":3}`),
			"gemini-y-tiered": json.RawMessage(`{"supportsThinking":true,"thinkingLevel":3}`),
			// A catalog model with the shared ID keeps its would-be variants apart.
			"gemini-z":      json.RawMessage(`{"supportsThinking":true,"thinkingLevel":3}`),
			"gemini-z-low":  json.RawMessage(`{"supportsThinking":true,"thinkingLevel":1}`),
			"gemini-unsure": json.RawMessage(`{"supportsThinking":true,"thinkingLevel":7}`),
			"gemini-budget": json.RawMessage(`{"supportsThinking":true,"minThinkingBudget":128,"thinkingBudget":1024}`),
			"gemini-plain":  json.RawMessage(`{}`),
			"image-a":       json.RawMessage(`{}`),
		},
		ImageModels: []string{"image-a"},
	}
	catalog.AgentModelSorts = []struct {
		Groups []struct {
			ModelIDs []string `json:"modelIds"`
		} `json:"groups"`
	}{{Groups: []struct {
		ModelIDs []string `json:"modelIds"`
	}{{ModelIDs: []string{"gemini-x-high", "gemini-x-xhigh", "gemini-y-medium", "gemini-x-low", "gemini-z-low"}}}}}
	catalog.DefaultModel = "gemini-x-low"
	if got, want := catalog.ListedModelIDs(), []string{"gemini-x", "gemini-y-medium", "gemini-z-low", "image-a"}; !slices.Equal(got, want) {
		t.Fatalf("listed models = %v, want %v", got, want)
	}
	levels := func(info ModelInfo) (out []string) {
		for _, l := range info.Levels {
			out = append(out, l.Name+"="+l.Model)
		}
		return out
	}
	x, ok := catalog.Info("gemini-x")
	if !ok || x.DisplayName != "Gemini X" || !x.Thinking || x.MinThinkingBudget != 32 ||
		!slices.Equal(levels(x), []string{"low=gemini-x-low", "high=gemini-x-high", "xhigh=gemini-x-xhigh"}) || x.Levels[2].Number != 100 {
		t.Fatalf("gemini-x = %+v", x)
	}
	for _, effort := range []struct{ effort, level string }{{"minimal", "low"}, {"low", "low"}, {"medium", "low"}, {"high", "high"}, {"xhigh", "xhigh"}, {"max", "xhigh"}, {"none", "low"}} {
		if got := x.Level(effort.effort).Name; got != effort.level {
			t.Errorf("effort %s picks %s, want %s", effort.effort, got, effort.level)
		}
	}
	if _, ok := catalog.Info("gemini-x-high"); ok {
		t.Fatal("a variant is offered under its own ID")
	}
	standard := []string{"low", "medium", "high"}
	for id, want := range map[string][]string{"gemini-y-medium": standard, "gemini-y-tiered": standard, "gemini-z": standard, "gemini-z-low": standard, "gemini-unsure": standard} {
		info, ok := catalog.Info(id)
		var names []string
		for _, l := range info.Levels {
			names = append(names, l.Name)
			if l.Model != id {
				t.Errorf("%s level %s served by %s", id, l.Name, l.Model)
			}
		}
		if !ok || !slices.Equal(names, want) {
			t.Errorf("%s: %+v", id, info)
		}
	}
	if b, _ := catalog.Info("gemini-budget"); !b.Thinking || len(b.Levels) != 0 || b.Budget != 1024 || b.MinThinkingBudget != 128 {
		t.Errorf("gemini-budget = %+v", b)
	}
	if p, _ := catalog.Info("gemini-plain"); p.Thinking || len(p.Levels) != 0 {
		t.Errorf("gemini-plain = %+v", p)
	}
	if i, _ := catalog.Info("image-a"); !i.Image {
		t.Errorf("image-a = %+v", i)
	}
}

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
