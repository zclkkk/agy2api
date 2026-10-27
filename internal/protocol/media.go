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
	Type      string          `json:"type"`
	MediaType string          `json:"media_type,omitempty"`
	Data      string          `json:"data,omitempty"`
	URL       string          `json:"url,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
}
type Block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	// Source is an object for images and documents and a URL string for search results.
	Source    json.RawMessage `json:"source,omitempty"`
	Title     string          `json:"title,omitempty"`
	Context   string          `json:"context,omitempty"`
	ImageURL  string          `json:"image_url,omitempty"`
	Detail    string          `json:"detail,omitempty"`
	FileData  string          `json:"file_data,omitempty"`
	FileURL   string          `json:"file_url,omitempty"`
	Filename  string          `json:"filename,omitempty"`
	FileID    string          `json:"file_id,omitempty"`
	Citations json.RawMessage `json:"citations,omitempty"`
	ToolName  string          `json:"tool_name,omitempty"`
}

func (b Block) mediaSource() (Source, error) {
	var s Source
	if !present(b.Source) || json.Unmarshal(b.Source, &s) != nil {
		return s, invalid("source", "media source must be an object")
	}
	return s, nil
}

// blocks decodes string or block-array content.
func blocks(content json.RawMessage) ([]Block, error) {
	var text string
	if json.Unmarshal(content, &text) == nil {
		return []Block{{Type: "text", Text: text}}, nil
	}
	var out []Block
	if err := json.Unmarshal(content, &out); err != nil {
		return nil, invalid("content", "content must be a string or an array of content blocks")
	}
	return out, nil
}

func (in *RequestIntent) warnContentOptions(content json.RawMessage) {
	items, err := blocks(content)
	if err != nil {
		return
	} // The normal content decoder reports malformed input.
	for _, b := range items {
		in.warnBlockOptions(b)
		if b.Type == "tool_result" {
			children, _ := blocks(b.Content)
			for _, child := range children {
				in.warnBlockOptions(child)
			}
		}
	}
}

// warnBlockOptions reports accepted block options whose guarantee does not
// hold. cache_control needs no report: caching is never guaranteed.
func (in *RequestIntent) warnBlockOptions(b Block) {
	switch b.Type {
	case "document", "search_result":
		var citations struct {
			Enabled bool `json:"enabled"`
		}
		if json.Unmarshal(b.Citations, &citations) == nil && citations.Enabled {
			in.warn("%s citations.enabled is ignored; citations are not produced", b.Type)
		}
	case "input_image":
		if b.Detail != "" && b.Detail != "auto" {
			in.warn("image detail is not applied; images use the model's default resolution")
		}
	}
}

func mediaParts(content json.RawMessage) ([]ag.Part, error) {
	items, err := blocks(content)
	if err != nil {
		return nil, err
	}
	parts := make([]ag.Part, 0, len(items))
	for _, b := range items {
		p, err := blockParts(b)
		if err != nil {
			return nil, err
		}
		parts = append(parts, p...)
	}
	return parts, nil
}

// blockParts converts one content block, which may need several parts.
func blockParts(b Block) ([]ag.Part, error) {
	switch b.Type {
	case "document":
		return documentParts(b)
	case "search_result":
		return searchResultParts(b)
	}
	p, err := mediaPart(b)
	if err != nil {
		return nil, err
	}
	return []ag.Part{p}, nil
}

// documentParts passes a document's title and context to the model before
// its content, as Anthropic does.
func documentParts(b Block) ([]ag.Part, error) {
	s, err := b.mediaSource()
	if err != nil {
		return nil, err
	}
	var parts []ag.Part
	var header []string
	if b.Title != "" {
		header = append(header, "Document title: "+b.Title)
	}
	if b.Context != "" {
		header = append(header, "Document context: "+b.Context)
	}
	if len(header) > 0 {
		parts = append(parts, ag.Part{Text: strings.Join(header, "\n")})
	}
	switch s.Type {
	case "base64", "url":
		p, err := sourcePart(s)
		if err != nil {
			return nil, err
		}
		return append(parts, p), nil
	case "text":
		return append(parts, ag.Part{Text: s.Data}), nil
	case "content":
		if !present(s.Content) {
			return nil, invalid("source.content", "content document requires content")
		}
		children, err := blocks(s.Content)
		if err != nil {
			return nil, err
		}
		for _, child := range children {
			if child.Type != "text" && child.Type != "image" {
				return nil, unsupported("document content type " + child.Type)
			}
			p, err := mediaPart(child)
			if err != nil {
				return nil, err
			}
			parts = append(parts, p)
		}
		return parts, nil
	}
	return nil, unsupported("source.type")
}

// searchResultParts presents a search result as text. Citations to it are not
// produced; a request for them is reported by warnBlockOptions.
func searchResultParts(b Block) ([]ag.Part, error) {
	var source string
	if json.Unmarshal(b.Source, &source) != nil || source == "" {
		return nil, invalid("source", "search_result requires a source string")
	}
	if !present(b.Content) {
		return nil, invalid("content", "search_result requires content")
	}
	children, err := blocks(b.Content)
	if err != nil {
		return nil, err
	}
	texts := make([]string, 0, len(children))
	for _, child := range children {
		if child.Type != "text" {
			return nil, unsupported("search_result content type " + child.Type)
		}
		texts = append(texts, child.Text)
	}
	header := "Search result"
	if b.Title != "" {
		header += ": " + b.Title
	}
	return []ag.Part{{Text: header + "\nSource: " + source + "\n\n" + strings.Join(texts, "\n\n")}}, nil
}

func sourcePart(s Source) (ag.Part, error) {
	switch s.Type {
	case "base64":
		return inline(s.MediaType, s.Data)
	case "url":
		return uriPart(s.URL, s.MediaType)
	}
	return ag.Part{}, unsupported("source.type")
}

func mediaPart(b Block) (ag.Part, error) {
	switch b.Type {
	case "text", "input_text", "output_text":
		return ag.Part{Text: b.Text}, nil
	case "tool_reference":
		// Claude Code's tool search result; the referenced tool is declared.
		name := b.ToolName
		if name == "" {
			name = b.Name
		}
		return ag.Part{Text: "Tool " + name + " is now available."}, nil
	case "image":
		s, err := b.mediaSource()
		if err != nil {
			return ag.Part{}, err
		}
		return sourcePart(s)
	case "input_image":
		if b.FileID != "" {
			return ag.Part{}, unsupported("file_id")
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
