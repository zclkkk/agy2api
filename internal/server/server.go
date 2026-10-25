// Package server exposes the two public HTTP protocols.
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
	"github.com/zclkkk/agy2api/internal/config"
	"github.com/zclkkk/agy2api/internal/protocol"
	"github.com/zclkkk/agy2api/internal/state"
)

type Backend interface {
	Generate(context.Context, string, ag.Request, bool, func(ag.Chunk) error) error
	Count(context.Context, string, ag.Request) (int, error)
	Models(context.Context) (ag.Catalog, error)
}
type Server struct {
	cfg           config.Config
	backend       Backend
	state         *state.Store
	logger        *slog.Logger
	slots         chan struct{}
	catalogMu     sync.Mutex
	catalog       ag.Catalog
	catalogExpiry time.Time
	media         *http.Client
}

func New(cfg config.Config, backend Backend, store *state.Store, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{cfg: cfg, backend: backend, state: store, logger: logger, slots: make(chan struct{}, cfg.MaxConcurrentRequests), media: mediaClient()}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("POST /v1/responses", s.generate)
	mux.HandleFunc("POST /v1/messages", s.generate)
	mux.HandleFunc("POST /v1/messages/count_tokens", s.count)
	mux.HandleFunc("POST /v1/responses/input_tokens", s.count)
	mux.HandleFunc("GET /v1/models", s.models)
	mux.HandleFunc("GET /v1/models/{id}", s.model)
	mux.HandleFunc("GET /v1/responses/{id}", s.retrieve)
	mux.HandleFunc("DELETE /v1/responses/{id}", s.delete)
	mux.HandleFunc("GET /v1/responses/{id}/input_items", s.inputItems)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.error(w, r, 404, "not_found_error", "endpoint not found", "")
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := ag.ID("req_")
		w.Header().Set("X-Request-Id", id)
		w.Header().Set("Request-Id", id)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.URL.Path != "/healthz" && s.cfg.APIKey != "" {
			key := r.Header.Get("X-Api-Key")
			if key == "" {
				key = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			}
			a, b := sha256.Sum256([]byte(key)), sha256.Sum256([]byte(s.cfg.APIKey))
			if subtle.ConstantTimeCompare(a[:], b[:]) != 1 {
				s.error(w, r, 401, "authentication_error", "invalid API key", "")
				return
			}
		}
		started := time.Now()
		mux.ServeHTTP(w, r)
		s.logger.Info("request completed", "request_id", id, "method", r.Method, "path", r.URL.Path, "duration_ms", time.Since(started).Milliseconds())
	})
}

func (s *Server) decode(w http.ResponseWriter, r *http.Request, count bool) (protocol.Input, error) {
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return protocol.Input{}, err
	}
	d := protocol.Decoder{State: s.state}
	if strings.HasPrefix(r.URL.Path, "/v1/messages") {
		return d.Messages(r.Context(), data, count)
	}
	return d.Responses(r.Context(), data)
}

func (s *Server) enter(w http.ResponseWriter, r *http.Request) bool {
	select {
	case s.slots <- struct{}{}:
		return true
	default:
		w.Header().Set("Retry-After", "1")
		s.error(w, r, 429, "rate_limit_error", "concurrency limit reached", "")
		return false
	}
}

func (s *Server) generate(w http.ResponseWriter, r *http.Request) {
	if !s.enter(w, r) {
		return
	}
	defer func() { <-s.slots }()
	in, err := s.decode(w, r, false)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.checkModel(r.Context(), &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.resolveMedia(r.Context(), &in.Request); err != nil {
		s.writeError(w, r, err)
		return
	}
	format := "responses"
	if r.URL.Path == "/v1/messages" {
		format = "messages"
	}
	streamStarted := false
	var emit func(string, map[string]any) error
	if in.Stream {
		emit = func(event string, value map[string]any) error {
			data, err := json.Marshal(value)
			if err != nil {
				return err
			}
			if !streamStarted {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("X-Accel-Buffering", "no")
				w.WriteHeader(200)
				streamStarted = true
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
				return err
			}
			return http.NewResponseController(w).Flush()
		}
	}
	native, err := (protocol.Decoder{State: s.state}).NativeRequest(r.Context(), in.Model, in.Request)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if native.SessionID == "" {
		native.SessionID = ag.ID("session-")
	}
	in.Request.SessionID = native.SessionID
	renderer := protocol.NewRenderer(format, in, s.state, emit)
	err = s.backend.Generate(r.Context(), in.Model, native, in.Search, func(chunk ag.Chunk) error { return renderer.Add(r.Context(), chunk) })
	if err == nil {
		err = renderer.Finish(r.Context())
	}
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		if streamStarted {
			_ = renderer.Fail(publicError(err))
			s.logger.Warn("stream failed", "request_id", w.Header().Get("X-Request-Id"), "error", publicError(err))
			return
		}
		s.writeError(w, r, err)
		return
	}
	if !in.Stream {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write(renderer.Result)
	}
}

func (s *Server) count(w http.ResponseWriter, r *http.Request) {
	if !s.enter(w, r) {
		return
	}
	defer func() { <-s.slots }()
	in, err := s.decode(w, r, true)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.checkModel(r.Context(), &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.resolveMedia(r.Context(), &in.Request); err != nil {
		s.writeError(w, r, err)
		return
	}
	native, err := (protocol.Decoder{State: s.state}).NativeRequest(r.Context(), in.Model, in.Request)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	n, err := s.backend.Count(r.Context(), in.Model, native)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/messages") {
		writeJSON(w, 200, map[string]int{"input_tokens": n})
	} else {
		writeJSON(w, 200, map[string]any{"object": "response.input_tokens", "input_tokens": n})
	}
}

func (s *Server) getCatalog(ctx context.Context) (ag.Catalog, error) {
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()
	if time.Now().Before(s.catalogExpiry) {
		return s.catalog, nil
	}
	catalog, err := s.backend.Models(ctx)
	if err != nil {
		return ag.Catalog{}, err
	}
	s.catalog = catalog
	s.catalogExpiry = time.Now().Add(5 * time.Minute)
	return catalog, nil
}
func (s *Server) checkModel(ctx context.Context, in *protocol.Input) error {
	catalog, err := s.getCatalog(ctx)
	if err != nil {
		return err
	}
	if _, ok := catalog.Models[in.Model]; !ok {
		return &protocol.Error{Message: "model is not available to this account", Param: "model"}
	}
	if strings.Contains(strings.ToLower(in.Model), "claude") && in.Request.Generation.Thinking != nil && in.Request.Generation.MaxTokens == nil {
		var limits struct {
			MaxOutputTokens int `json:"maxOutputTokens"`
		}
		_ = json.Unmarshal(catalog.Models[in.Model], &limits)
		if limits.MaxOutputTokens <= 0 {
			return &protocol.Error{Message: "specify max_output_tokens when this Claude model's output limit is not advertised", Param: "max_output_tokens"}
		}
		if budget := in.Request.Generation.Thinking.Budget; budget != nil && *budget >= limits.MaxOutputTokens {
			return &protocol.Error{Message: "thinking budget exceeds this model's output limit", Param: "reasoning"}
		}
		in.Request.Generation.MaxTokens = &limits.MaxOutputTokens
	}
	if in.Search {
		found := false
		for _, id := range catalog.SearchModels {
			if id == in.Model {
				found = true
				break
			}
		}
		if !found {
			return &protocol.Error{Message: "web search is not advertised for this model", Param: "tools"}
		}
	}
	return nil
}

func anthropic(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, "/v1/messages") || r.Header.Get("Anthropic-Version") != ""
}
func modelObject(id string, value json.RawMessage, claude bool) map[string]any {
	if !claude {
		return map[string]any{"id": id, "object": "model", "created": 0, "owned_by": "antigravity"}
	}
	var info struct {
		DisplayName string `json:"displayName"`
	}
	_ = json.Unmarshal(value, &info)
	if info.DisplayName == "" {
		info.DisplayName = id
	}
	return map[string]any{"id": id, "type": "model", "display_name": info.DisplayName, "created_at": "1970-01-01T00:00:00Z"}
}
func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	catalog, err := s.getCatalog(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	ids := make([]string, 0, len(catalog.Models))
	for id := range catalog.Models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	data := make([]any, 0, len(ids))
	for _, id := range ids {
		data = append(data, modelObject(id, catalog.Models[id], anthropic(r)))
	}
	out := map[string]any{"data": data}
	if anthropic(r) {
		out["has_more"] = false
		out["first_id"] = nil
		out["last_id"] = nil
		if len(ids) > 0 {
			out["first_id"] = ids[0]
			out["last_id"] = ids[len(ids)-1]
		}
	} else {
		out["object"] = "list"
	}
	writeJSON(w, 200, out)
}
func (s *Server) model(w http.ResponseWriter, r *http.Request) {
	catalog, err := s.getCatalog(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	id := r.PathValue("id")
	v, ok := catalog.Models[id]
	if !ok {
		s.error(w, r, 404, "not_found_error", "model not found", "model")
		return
	}
	writeJSON(w, 200, modelObject(id, v, anthropic(r)))
}

func (s *Server) stored(w http.ResponseWriter, r *http.Request) (protocol.StoredResponse, bool) {
	var record protocol.StoredResponse
	err := s.state.Get(r.Context(), "response", r.PathValue("id"), &record)
	if errors.Is(err, state.ErrNotFound) {
		s.error(w, r, 404, "not_found_error", "response not found or expired", "")
		return record, false
	}
	if err != nil {
		s.writeError(w, r, err)
		return record, false
	}
	return record, true
}
func (s *Server) retrieve(w http.ResponseWriter, r *http.Request) {
	record, ok := s.stored(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(record.Response)
}
func (s *Server) delete(w http.ResponseWriter, r *http.Request) {
	deleted, err := s.state.Delete(r.Context(), "response", r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if !deleted {
		s.error(w, r, 404, "not_found_error", "response not found or expired", "")
		return
	}
	writeJSON(w, 200, map[string]any{"id": r.PathValue("id"), "object": "response.deleted", "deleted": true})
}
func (s *Server) inputItems(w http.ResponseWriter, r *http.Request) {
	record, ok := s.stored(w, r)
	if !ok {
		return
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": record.Items, "has_more": false, "first_id": nil, "last_id": nil})
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var validation *protocol.Error
	if errors.As(err, &validation) {
		s.error(w, r, 400, "invalid_request_error", validation.Message, validation.Param)
		return
	}
	var limit *http.MaxBytesError
	if errors.As(err, &limit) {
		s.error(w, r, 413, "request_too_large", "request body exceeds configured limit", "")
		return
	}
	var upstream *ag.HTTPError
	if errors.As(err, &upstream) {
		status := upstream.Status
		kind := "api_error"
		if status == 429 {
			kind = "rate_limit_error"
		}
		if status == 401 || status == 403 {
			status = 502
		}
		if status < 400 || status >= 600 {
			status = 502
		}
		if upstream.RetryAfter != "" {
			w.Header().Set("Retry-After", upstream.RetryAfter)
		}
		s.error(w, r, status, kind, upstream.Message, "")
		return
	}
	s.logger.Warn("request failed", "request_id", w.Header().Get("X-Request-Id"), "error", err.Error())
	s.error(w, r, 502, "api_error", publicError(err), "")
}
func publicError(err error) string {
	var v *protocol.Error
	if errors.As(err, &v) {
		return v.Message
	}
	var u *ag.HTTPError
	if errors.As(err, &u) {
		return u.Message
	}
	return "generation failed; inspect the local service log"
}
func (s *Server) error(w http.ResponseWriter, r *http.Request, status int, kind, message, param string) {
	if anthropic(r) {
		writeJSON(w, status, map[string]any{"type": "error", "error": map[string]string{"type": kind, "message": message}, "request_id": w.Header().Get("X-Request-Id")})
		return
	}
	if kind == "authentication_error" {
		kind = "invalid_request_error"
	}
	var field any
	if param != "" {
		field = param
	}
	writeJSON(w, status, map[string]any{"error": map[string]any{"type": kind, "message": message, "param": field, "code": nil}})
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
