// Package antigravity owns the provider's native wire format and transport.
package antigravity

import (
	"encoding/json"
	"slices"
	"sort"
	"strings"
)

type Blob struct {
	MIMEType string `json:"mimeType"`
	Data     string `json:"data"`
}
type File struct {
	MIMEType string `json:"mimeType"`
	URI      string `json:"fileUri"`
}
type Call struct {
	ID   string          `json:"id,omitempty"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}
type Result struct {
	ID       string          `json:"id,omitempty"`
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
	Parts    []Part          `json:"parts,omitempty"`
}
type Part struct {
	Text             string  `json:"text,omitempty"`
	Thought          bool    `json:"thought,omitempty"`
	Signature        string  `json:"thoughtSignature,omitempty"`
	InlineData       *Blob   `json:"inlineData,omitempty"`
	FileData         *File   `json:"fileData,omitempty"`
	FunctionCall     *Call   `json:"functionCall,omitempty"`
	FunctionResponse *Result `json:"functionResponse,omitempty"`
	// Unknown lists fields this bridge does not model. They are reported, not
	// fatal, so new upstream metadata does not break generation.
	Unknown []string `json:"-"`
}

func (p *Part) UnmarshalJSON(data []byte) error {
	type plain Part
	var value plain
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		return err
	}
	for key, v := range keys {
		switch key {
		case "text", "thought", "thoughtSignature", "inlineData", "fileData", "functionCall", "functionResponse":
		case "thought_signature":
			if value.Signature == "" {
				if err := json.Unmarshal(v, &value.Signature); err != nil {
					return err
				}
			}
		default:
			value.Unknown = append(value.Unknown, key)
		}
	}
	sort.Strings(value.Unknown)
	*p = Part(value)
	return nil
}

// Empty reports whether the part carries nothing this bridge can represent.
func (p Part) Empty() bool {
	return p.Text == "" && p.Signature == "" && p.InlineData == nil && p.FileData == nil && p.FunctionCall == nil && p.FunctionResponse == nil
}

type Content struct {
	Role  string `json:"role,omitempty"`
	Parts []Part `json:"parts"`
}
type Function struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}
type Tool struct {
	Functions    []Function      `json:"functionDeclarations,omitempty"`
	GoogleSearch json.RawMessage `json:"googleSearch,omitempty"`
}
type CallingConfig struct {
	Mode         string   `json:"mode"`
	AllowedNames []string `json:"allowedFunctionNames,omitempty"`
}
type ToolConfig struct {
	FunctionCallingConfig CallingConfig `json:"functionCallingConfig"`
}
type Thinking struct {
	Budget  *int   `json:"thinkingBudget,omitempty"`
	Level   string `json:"thinkingLevel,omitempty"`
	Include bool   `json:"includeThoughts"`
}
type ImageConfig struct {
	AspectRatio string `json:"aspectRatio,omitempty"`
	Size        string `json:"imageSize,omitempty"`
}
type Generation struct {
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"topP,omitempty"`
	TopK        *int            `json:"topK,omitempty"`
	MaxTokens   *int            `json:"maxOutputTokens,omitempty"`
	Stop        []string        `json:"stopSequences,omitempty"`
	Thinking    *Thinking       `json:"thinkingConfig,omitempty"`
	MIMEType    string          `json:"responseMimeType,omitempty"`
	Schema      json.RawMessage `json:"responseSchema,omitempty"`
	Modalities  []string        `json:"responseModalities,omitempty"`
	Image       *ImageConfig    `json:"imageConfig,omitempty"`
}
type Request struct {
	Contents   []Content   `json:"contents"`
	System     *Content    `json:"systemInstruction,omitempty"`
	Tools      []Tool      `json:"tools,omitempty"`
	ToolConfig *ToolConfig `json:"toolConfig,omitempty"`
	Generation Generation  `json:"generationConfig,omitempty"`
	SessionID  string      `json:"sessionId,omitempty"`
}
type Envelope struct {
	Project     string  `json:"project"`
	Model       string  `json:"model"`
	UserAgent   string  `json:"userAgent"`
	RequestID   string  `json:"requestId"`
	RequestType string  `json:"requestType"`
	Request     Request `json:"request"`
}
type Usage struct {
	Input        int              `json:"promptTokenCount"`
	Output       int              `json:"candidatesTokenCount"`
	Thoughts     int              `json:"thoughtsTokenCount"`
	Cached       int              `json:"cachedContentTokenCount"`
	Total        int              `json:"totalTokenCount"`
	InputDetails []ModalityTokens `json:"promptTokensDetails,omitempty"`
}
type ModalityTokens struct {
	Modality string `json:"modality"`
	Count    int    `json:"tokenCount"`
}
type Grounding struct {
	Queries []string `json:"webSearchQueries,omitempty"`
	Chunks  []struct {
		Web *struct {
			URI   string `json:"uri"`
			Title string `json:"title"`
		} `json:"web,omitempty"`
	} `json:"groundingChunks"`
	Supports []struct {
		Segment struct {
			Start int    `json:"startIndex"`
			End   int    `json:"endIndex"`
			Text  string `json:"text"`
		} `json:"segment"`
		Indices []int `json:"groundingChunkIndices"`
	} `json:"groundingSupports"`
}
type Candidate struct {
	Content   Content    `json:"content"`
	Finish    string     `json:"finishReason,omitempty"`
	Grounding *Grounding `json:"groundingMetadata,omitempty"`
}
type Response struct {
	Candidates     []Candidate     `json:"candidates"`
	Usage          *Usage          `json:"usageMetadata,omitempty"`
	PromptFeedback json.RawMessage `json:"promptFeedback,omitempty"`
	Error          json.RawMessage `json:"error,omitempty"`
}
type Chunk struct {
	Parts     []Part
	Usage     *Usage
	Finish    string
	Grounding *Grounding
}
type Catalog struct {
	Models          map[string]json.RawMessage `json:"models"`
	SearchModels    []string                   `json:"webSearchModelIds"`
	ImageModels     []string                   `json:"imageGenerationModelIds"`
	AgentModelSorts []struct {
		Groups []struct {
			ModelIDs []string `json:"modelIds"`
		} `json:"groups"`
	} `json:"agentModelSorts"`
}

// ListedModelIDs follows the upstream agent selector, then appends image models.
// Models remains the full Gemini catalog for explicit lookups and requests.
func (c Catalog) ListedModelIDs() []string {
	ids := make([]string, 0)
	seen := make(map[string]bool)
	add := func(id string) {
		if _, ok := c.Models[id]; !ok || seen[id] {
			return
		}
		seen[id] = true
		ids = append(ids, id)
	}
	for _, order := range c.AgentModelSorts {
		for _, group := range order.Groups {
			for _, id := range group.ModelIDs {
				add(id)
			}
		}
	}
	for _, id := range c.ImageModels {
		add(id)
	}
	return ids
}

// ModelInfo is the subset of catalog metadata used to shape requests.
type ModelInfo struct {
	MinThinkingBudget int  `json:"minThinkingBudget"`
	Search            bool `json:"-"`
	Image             bool `json:"-"`
}

func (c Catalog) Info(id string) (ModelInfo, bool) {
	raw, ok := c.Models[id]
	if !ok {
		return ModelInfo{}, false
	}
	var info ModelInfo
	_ = json.Unmarshal(raw, &info)
	info.Search = slices.Contains(c.SearchModels, id)
	info.Image = slices.Contains(c.ImageModels, id) || (len(c.ImageModels) == 0 && strings.Contains(id, "image"))
	return info, true
}

// GeminiProvider is the catalog apiProvider of the Gemini models this bridge serves.
const GeminiProvider = "API_PROVIDER_GOOGLE_GEMINI"

// Request types understood by streamGenerateContent.
const (
	KindAgent  = "agent"
	KindSearch = "web_search"
	KindImage  = "image_gen"
)
