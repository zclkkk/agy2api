package antigravity

import (
	"encoding/json"
	"slices"
	"strings"
)

type Catalog struct {
	// Models holds the enabled Gemini models; Client.Models drops the rest.
	Models          map[string]json.RawMessage `json:"models"`
	ImageModels     []string                   `json:"imageGenerationModelIds"`
	AgentModelSorts []struct {
		Groups []struct {
			ModelIDs []string `json:"modelIds"`
		} `json:"groups"`
	} `json:"agentModelSorts"`
	// Models Antigravity assigns to editor roles.
	DefaultModel        string   `json:"defaultAgentModelId"`
	CommandModels       []string `json:"commandModelIds"`
	CommitMessageModels []string `json:"commitMessageModelIds"`
	QueryModels         []string `json:"mqueryModelIds"`
	WebSearchModels     []string `json:"webSearchModelIds"`

	// index is built by Client.Models; catalogs built otherwise index on use.
	index *catalogIndex
}

// ModelInfo describes a model as agy2api offers it.
type ModelInfo struct {
	DisplayName string
	Image       bool
	// Thinking is false for models without thinking; requests to them carry
	// no thinking configuration.
	Thinking          bool
	MinThinkingBudget int
	// Levels are the thinking levels the model offers, from least to most
	// thinking. They are empty for a model the catalog configures by budget.
	Levels []Level
	// Budget is the thinking budget the catalog configures a model with when
	// it names no level for it.
	Budget int
}

// Level is a thinking level a model offers and the catalog model serving it.
type Level struct {
	Name   string // reasoning effort name, such as "high"
	Number int    // thinkingLevel enum number
	Model  string // catalog model ID
}

// thinkingLevels are Gemini's thinking levels from least to most thinking.
// The catalog names levels by enum number. XHIGH has two numbers upstream;
// the catalog uses 100.
var thinkingLevels = []Level{{Name: "minimal", Number: 4}, {Name: "low", Number: 1}, {Name: "medium", Number: 2}, {Name: "high", Number: 3}, {Name: "xhigh", Number: 100}}

// IsThinkingLevel reports whether name is one of Gemini's thinking levels.
func IsThinkingLevel(name string) bool { return levelRank(name) >= 0 }

func levelRank(name string) int {
	return slices.IndexFunc(thinkingLevels, func(l Level) bool { return l.Name == name })
}

// Level picks the level a reasoning effort asks for: the effort itself when
// the model offers it, otherwise the closest offered level below it, or the
// lowest. "max" picks the highest level and "none" the lowest. The model must
// offer levels.
func (m ModelInfo) Level(effort string) Level {
	if effort == "max" {
		return m.Levels[len(m.Levels)-1]
	}
	rank := levelRank(effort)
	level := m.Levels[0]
	for _, l := range m.Levels {
		if levelRank(l.Name) <= rank {
			level = l
		}
	}
	return level
}

type catalogIndex struct {
	models  map[string]ModelInfo // offered model ID -> model
	offered map[string]string    // catalog model ID -> offered model ID
}

type catalogEntry struct {
	DisplayName       string `json:"displayName"`
	SupportsThinking  bool   `json:"supportsThinking"`
	MinThinkingBudget int    `json:"minThinkingBudget"`
	ThinkingLevel     int    `json:"thinkingLevel"`
	ThinkingBudget    int    `json:"thinkingBudget"`
}

// buildIndex groups the catalog into the models agy2api offers. Antigravity
// offers one model at several thinking levels as catalog models whose IDs
// end in their own level, such as gemini-x-low and gemini-x-high. agy2api
// offers them as one model, gemini-x, and a request's reasoning effort picks
// the variant. A catalog model with the shared ID itself keeps the variants
// apart. Other models are offered under their own IDs; those the catalog
// names a level for accept low, medium and high.
func (c Catalog) buildIndex() *catalogIndex {
	entries := make(map[string]catalogEntry, len(c.Models))
	for id, raw := range c.Models {
		var e catalogEntry
		_ = json.Unmarshal(raw, &e)
		entries[id] = e
	}
	x := &catalogIndex{models: make(map[string]ModelInfo), offered: make(map[string]string)}
	variants := make(map[string][]Level)
	for id, e := range entries {
		if base, level, ok := variantOf(id, e); ok {
			if _, taken := entries[base]; !taken {
				variants[base] = append(variants[base], level)
				x.offered[id] = base
				continue
			}
		}
		info := ModelInfo{DisplayName: e.DisplayName, Image: c.image(id), Thinking: e.SupportsThinking, MinThinkingBudget: e.MinThinkingBudget}
		if e.SupportsThinking && e.ThinkingLevel != 0 {
			for _, name := range []string{"low", "medium", "high"} {
				l := thinkingLevels[levelRank(name)]
				l.Model = id
				info.Levels = append(info.Levels, l)
			}
		} else if e.SupportsThinking {
			info.Budget = e.ThinkingBudget
		}
		x.models[id] = info
		x.offered[id] = id
	}
	for base, levels := range variants {
		slices.SortFunc(levels, func(a, b Level) int { return levelRank(a.Name) - levelRank(b.Name) })
		lowest := entries[levels[0].Model]
		x.models[base] = ModelInfo{DisplayName: sharedName(lowest.DisplayName, levels[0].Name, base), Thinking: true, MinThinkingBudget: lowest.MinThinkingBudget, Levels: levels}
	}
	return x
}

// variantOf reports whether a catalog model is one level of a model offered
// at several levels: its ID ends in the level the catalog names for it.
func variantOf(id string, e catalogEntry) (string, Level, bool) {
	i := slices.IndexFunc(thinkingLevels, func(l Level) bool { return l.Number == e.ThinkingLevel })
	if i < 0 {
		return "", Level{}, false
	}
	level := thinkingLevels[i]
	base, ok := strings.CutSuffix(id, "-"+level.Name)
	level.Model = id
	return base, level, ok && base != ""
}

// sharedName drops the level from a variant's display name, as in
// "Gemini X (High)", falling back to the shared ID.
func sharedName(display, level, base string) string {
	i := strings.LastIndex(display, " (")
	if i < 0 || !strings.HasSuffix(display, ")") || !strings.EqualFold(display[i+2:len(display)-1], level) {
		return base
	}
	return display[:i]
}

func (c Catalog) image(id string) bool {
	return slices.Contains(c.ImageModels, id) || (len(c.ImageModels) == 0 && strings.Contains(id, "image"))
}

func (c Catalog) indexed() *catalogIndex {
	if c.index != nil {
		return c.index
	}
	return c.buildIndex()
}

// Info describes a model agy2api offers. Variants of a model offered at
// several levels are reached through the shared ID only.
func (c Catalog) Info(id string) (ModelInfo, bool) {
	info, ok := c.indexed().models[id]
	return info, ok
}

// ListedModelIDs lists the models Antigravity itself uses: the agent selector,
// then the models it assigns to the default agent, commands and background
// tasks, then image generation models. Variants appear once, under the shared
// ID. Other catalog models remain available to explicit lookups and requests.
func (c Catalog) ListedModelIDs() []string {
	x := c.indexed()
	ids := make([]string, 0)
	seen := make(map[string]bool)
	add := func(list ...string) {
		for _, id := range list {
			if offered, ok := x.offered[id]; ok && !seen[offered] {
				seen[offered] = true
				ids = append(ids, offered)
			}
		}
	}
	for _, order := range c.AgentModelSorts {
		for _, group := range order.Groups {
			add(group.ModelIDs...)
		}
	}
	add(c.DefaultModel)
	add(c.CommandModels...)
	add(c.CommitMessageModels...)
	add(c.QueryModels...)
	add(c.WebSearchModels...)
	add(c.ImageModels...)
	return ids
}
