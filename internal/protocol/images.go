package protocol

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

// ImageIntent describes one independent Images API operation. It has no
// conversation history, tools or replay state.
type ImageIntent struct {
	Model        string
	Request      ag.Request
	Count        int
	Size         string
	OutputFormat string
	Compression  int
	Warnings     []string
}

type imageReference struct {
	URL    string `json:"image_url"`
	FileID string `json:"file_id"`
}

type imageRequest struct {
	Model          string           `json:"model"`
	Prompt         string           `json:"prompt"`
	N              *int             `json:"n"`
	Size           string           `json:"size"`
	OutputFormat   string           `json:"output_format"`
	Compression    *int             `json:"output_compression"`
	ResponseFormat string           `json:"response_format"`
	Quality        string           `json:"quality"`
	Background     string           `json:"background"`
	Moderation     string           `json:"moderation"`
	Stream         bool             `json:"stream"`
	PartialImages  int              `json:"partial_images"`
	Images         []imageReference `json:"images"`
	Mask           json.RawMessage  `json:"mask"`
	InputFidelity  string           `json:"input_fidelity"`
	Style          string           `json:"style"`
}

func (Decoder) Images(data []byte, edit bool) (ImageIntent, error) {
	in := ImageIntent{Count: 1, OutputFormat: "png", Compression: 100}
	var r imageRequest
	ignored, err := decodeRequest(data, &r, map[string]bool{"user": true, "safety_identifier": true})
	if err != nil {
		return in, err
	}
	if len(ignored) > 0 {
		in.Warnings = append(in.Warnings, "ignored request fields: "+strings.Join(ignored, ", "))
	}
	in.Model = r.Model
	if strings.TrimSpace(r.Model) == "" {
		return in, invalid("model", "model is required; use an image generation model listed by /v1/models")
	}
	if strings.TrimSpace(r.Prompt) == "" || utf8.RuneCountInString(r.Prompt) > 32000 {
		return in, invalid("prompt", "prompt must contain between 1 and 32000 characters")
	}
	if r.N != nil {
		in.Count = *r.N
	}
	if in.Count < 1 || in.Count > 10 {
		return in, invalid("n", "n must be between 1 and 10")
	}
	switch r.Size {
	case "", "auto":
	case "1024x1024":
		in.Size = r.Size
		in.Request.Generation.Image = &ag.ImageConfig{AspectRatio: "1:1", Size: "1K"}
	default:
		return in, invalid("size", "supported sizes are auto and 1024x1024")
	}
	if r.OutputFormat != "" {
		in.OutputFormat = r.OutputFormat
	}
	if in.OutputFormat != "png" && in.OutputFormat != "jpeg" {
		return in, invalid("output_format", "supported output formats are png and jpeg")
	}
	if r.Compression != nil {
		if in.OutputFormat != "jpeg" || *r.Compression < 0 || *r.Compression > 100 {
			return in, invalid("output_compression", "output_compression requires jpeg output and a value between 0 and 100")
		}
		in.Compression = *r.Compression
	}
	if r.ResponseFormat != "" && r.ResponseFormat != "b64_json" {
		return in, invalid("response_format", "only b64_json output is supported")
	}
	for _, option := range []struct{ name, value string }{
		{"quality", r.Quality}, {"background", r.Background}, {"moderation", r.Moderation},
	} {
		if option.value != "" && option.value != "auto" {
			return in, invalid(option.name, option.name+" only supports auto")
		}
	}
	switch {
	case r.Stream:
		return in, unsupported("stream")
	case r.PartialImages != 0:
		return in, unsupported("partial_images")
	case present(r.Mask):
		return in, unsupported("mask")
	case r.InputFidelity != "":
		return in, unsupported("input_fidelity")
	case r.Style != "":
		return in, unsupported("style")
	}
	if edit && (len(r.Images) == 0 || len(r.Images) > 16) {
		return in, invalid("images", "editing requires between 1 and 16 input images")
	}
	if !edit && len(r.Images) != 0 {
		return in, invalid("images", "input images require /v1/images/edits")
	}
	parts := []ag.Part{{Text: r.Prompt}}
	for _, ref := range r.Images {
		if ref.FileID != "" {
			return in, unsupported("images.file_id")
		}
		if !strings.HasPrefix(ref.URL, "data:") && !strings.HasPrefix(ref.URL, "https://") {
			return in, invalid("images.image_url", "expected an HTTPS URL or base64 data URL")
		}
		part, err := uriPart(ref.URL, "")
		if err != nil {
			return in, err
		}
		parts = append(parts, part)
	}
	in.Request.Contents = []ag.Content{{Role: "user", Parts: parts}}
	in.Request.System = &ag.Content{Parts: []ag.Part{{Text: "Generate exactly one final image for the user's request."}}}
	return in, nil
}

// ValidateModel checks the catalog without the conversation planner, which
// deliberately excludes image generation models.
func (in ImageIntent) ValidateModel(catalog ag.Catalog) error {
	info, ok := catalog.Info(in.Model)
	if !ok || !info.Image {
		return invalid("model", "model must be an available image generation model listed by /v1/models")
	}
	return nil
}
