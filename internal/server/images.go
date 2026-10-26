package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
	"github.com/zclkkk/agy2api/internal/protocol"
)

func (s *Server) images(edit bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := s.readImages(w, r, edit)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		in, err := (protocol.Decoder{}).Images(data, edit)
		s.note(r, in.Model, in.Warnings...)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		catalog, err := s.getCatalog(r.Context())
		if err == nil {
			err = in.ValidateModel(catalog)
		}
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		if !s.acquire(w, r) {
			return
		}
		defer func() { <-s.slots }()
		if err := s.resolveMedia(r.Context(), &in.Request); err != nil {
			s.writeError(w, r, err)
			return
		}
		if err := in.ValidateInputs(); err != nil {
			s.writeError(w, r, err)
			return
		}
		response := protocol.ImagesResponse{Created: time.Now().Unix(), OutputFormat: in.OutputFormat}
		outputBytes := 0
		for i := 0; i < in.Count; i++ {
			if r.Context().Err() != nil {
				return
			}
			collector := protocol.NewImageCollector(in)
			err := s.upstream.Generate(r.Context(), in.Model, in.Request, ag.KindImage, collector.Add)
			if err != nil {
				if r.Context().Err() == nil {
					s.writeError(w, r, err)
				}
				return
			}
			image, size, usage, err := collector.Finish()
			if err != nil {
				s.writeError(w, r, err)
				return
			}
			outputBytes += len(image.Base64)
			if outputBytes > protocol.MaxImageOutputBytes {
				s.writeError(w, r, errors.New("image batch exceeds 64 MiB"))
				return
			}
			response.Append(image, size, usage)
		}
		writeJSON(w, http.StatusOK, response)
	}
}

// readImages normalizes the Images HTTP encodings into a single JSON request.
// Multipart is consumed in memory under the same total request limit as JSON;
// uploaded files never need temporary disk storage.
func (s *Server) readImages(w http.ResponseWriter, r *http.Request, edit bool) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)
	kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return nil, &protocol.Error{Param: "Content-Type", Message: "expected application/json or multipart/form-data"}
	}
	if kind == "application/json" {
		return io.ReadAll(r.Body)
	}
	if !edit || kind != "multipart/form-data" {
		return nil, &protocol.Error{Param: "Content-Type", Message: "generations require JSON; edits accept JSON or multipart/form-data"}
	}
	reader, err := r.MultipartReader()
	if err != nil {
		return nil, &protocol.Error{Param: "body", Message: "invalid multipart body"}
	}
	fields := map[string]json.RawMessage{}
	var images []map[string]string
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			var limit *http.MaxBytesError
			if errors.As(err, &limit) {
				return nil, limit
			}
			return nil, &protocol.Error{Param: "body", Message: "invalid multipart body"}
		}
		name := part.FormName()
		value, err := io.ReadAll(part)
		_ = part.Close()
		if err != nil {
			var limit *http.MaxBytesError
			if errors.As(err, &limit) {
				return nil, limit
			}
			return nil, &protocol.Error{Param: "body", Message: "incomplete multipart body"}
		}
		if name == "image" || name == "image[]" {
			if len(images) == 16 {
				return nil, &protocol.Error{Param: "image", Message: "editing accepts at most 16 input images"}
			}
			images = append(images, map[string]string{"image_url": "data:" + http.DetectContentType(value) + ";base64," + base64.StdEncoding.EncodeToString(value)})
			continue
		}
		if name == "" || name == "images" || part.FileName() != "" {
			return nil, &protocol.Error{Param: name, Message: "unsupported multipart field"}
		}
		if _, exists := fields[name]; exists {
			return nil, &protocol.Error{Param: name, Message: "duplicate multipart field"}
		}
		switch name {
		case "n", "output_compression", "partial_images", "stream":
			if !json.Valid(value) {
				return nil, &protocol.Error{Param: name, Message: "invalid multipart scalar value"}
			}
			fields[name] = value
		default:
			fields[name], _ = json.Marshal(string(value))
		}
	}
	fields["images"], _ = json.Marshal(images)
	return json.Marshal(fields)
}
