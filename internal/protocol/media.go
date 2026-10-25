package protocol

import (
	"encoding/base64"
	"encoding/json"
	"mime"
	"net/url"
	"path/filepath"
	"strings"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

type Source struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
	Text      string `json:"text,omitempty"`
}
type Block struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	Thinking     string          `json:"thinking,omitempty"`
	Signature    string          `json:"signature,omitempty"`
	ID           string          `json:"id,omitempty"`
	Name         string          `json:"name,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	Content      json.RawMessage `json:"content,omitempty"`
	IsError      bool            `json:"is_error,omitempty"`
	Source       *Source         `json:"source,omitempty"`
	ImageURL     string          `json:"image_url,omitempty"`
	Detail       string          `json:"detail,omitempty"`
	FileData     string          `json:"file_data,omitempty"`
	FileURL      string          `json:"file_url,omitempty"`
	Filename     string          `json:"filename,omitempty"`
	FileID       string          `json:"file_id,omitempty"`
	Annotations  json.RawMessage `json:"annotations,omitempty"`
	Logprobs     json.RawMessage `json:"logprobs,omitempty"`
	Citations    json.RawMessage `json:"citations,omitempty"`
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

func blocks(content json.RawMessage) ([]Block, error) {
	var text string
	if json.Unmarshal(content, &text) == nil {
		return []Block{{Type: "text", Text: text}}, nil
	}
	var out []Block
	if err := strict(content, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func mediaParts(content json.RawMessage, param string) ([]ag.Part, error) {
	items, err := blocks(content)
	if err != nil {
		return nil, err
	}
	parts := make([]ag.Part, 0, len(items))
	for _, b := range items {
		p, err := mediaPart(b)
		if err != nil {
			return nil, err
		}
		parts = append(parts, p)
	}
	return parts, nil
}

func mediaPart(b Block) (ag.Part, error) {
	if present(b.CacheControl) {
		return ag.Part{}, unsupported("cache_control")
	}
	switch b.Type {
	case "text", "input_text", "output_text":
		return ag.Part{Text: b.Text}, nil
	case "image", "document":
		if b.Source == nil {
			return ag.Part{}, invalid("source", "media source is required")
		}
		s := b.Source
		switch s.Type {
		case "base64":
			return inline(s.MediaType, s.Data)
		case "url":
			return uriPart(s.URL, s.MediaType)
		case "text":
			if b.Type == "document" {
				return ag.Part{Text: s.Text}, nil
			}
		}
		return ag.Part{}, unsupported("source.type")
	case "input_image":
		if b.FileID != "" {
			return ag.Part{}, unsupported("file_id")
		}
		if b.Detail != "" && b.Detail != "auto" {
			return ag.Part{}, unsupported("image detail other than auto")
		}
		return uriPart(b.ImageURL, "")
	case "input_file":
		if b.FileID != "" {
			return ag.Part{}, unsupported("file_id")
		}
		if b.FileURL != "" {
			return uriPart(b.FileURL, mime.TypeByExtension(filepath.Ext(b.Filename)))
		}
		if strings.HasPrefix(b.FileData, "data:") {
			return uriPart(b.FileData, "")
		}
		mt := mime.TypeByExtension(filepath.Ext(b.Filename))
		if mt == "" {
			mt = "application/octet-stream"
		}
		return inline(mt, b.FileData)
	default:
		return ag.Part{}, unsupported("content type " + b.Type)
	}
}

func inline(mt, data string) (ag.Part, error) {
	if mt == "" || data == "" {
		return ag.Part{}, invalid("source", "media_type and base64 data are required")
	}
	if _, err := base64.StdEncoding.DecodeString(data); err != nil {
		return ag.Part{}, invalid("source.data", "invalid base64 media")
	}
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = mt[:i]
	}
	return ag.Part{InlineData: &ag.Blob{MIMEType: mt, Data: data}}, nil
}

func uriPart(uri, mt string) (ag.Part, error) {
	if strings.HasPrefix(uri, "data:") {
		header, data, ok := strings.Cut(strings.TrimPrefix(uri, "data:"), ",")
		if !ok || !strings.HasSuffix(header, ";base64") {
			return ag.Part{}, invalid("media", "expected a base64 data URL")
		}
		return inline(strings.TrimSuffix(header, ";base64"), data)
	}
	u, err := url.Parse(uri)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "gs") {
		return ag.Part{}, invalid("media", "expected an HTTPS URL, gs:// URI, or base64 data URL")
	}
	return ag.Part{FileData: &ag.File{MIMEType: mt, URI: uri}}, nil
}
