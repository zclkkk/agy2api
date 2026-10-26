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
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
	"github.com/zclkkk/agy2api/internal/config"
	"github.com/zclkkk/agy2api/internal/protocol"
	"github.com/zclkkk/agy2api/internal/state"
)

const (
	catalogTTL      = 5 * time.Minute
	catalogRetry    = 30 * time.Second
	queueWait       = 2 * time.Minute
	heartbeatPeriod = 15 * time.Second
)

type Server struct {
	cfg           config.Config
	upstream      *ag.Client
	state         *state.Store
	logger        *slog.Logger
	slots         chan struct{}
	catalogMu     sync.Mutex
	catalog       ag.Catalog
	catalogExpiry time.Time
	media         *http.Client
	heartbeat     time.Duration
}

func New(cfg config.Config, upstream *ag.Client, store *state.Store, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{cfg: cfg, upstream: upstream, state: store, logger: logger, slots: make(chan struct{}, cfg.MaxConcurrentRequests), media: mediaClient(), heartbeat: heartbeatPeriod}
}

// requestLog collects per-request details for the completion log line.
type requestLog struct {
	mu       sync.Mutex
	model    string
	warnings []string
}
type logKey struct{}

func (s *Server) note(r *http.Request, model string, warnings ...string) {
	l, _ := r.Context().Value(logKey{}).(*requestLog)
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if model != "" {
		l.model = model
	}
	l.warnings = append(l.warnings, warnings...)
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}
func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("POST /v1/responses", s.generate)
	mux.HandleFunc("POST /v1/responses/compact", s.generate)
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
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		id := ag.ID("req_")
		w := &statusWriter{ResponseWriter: rw}
		w.Header().Set("X-Request-Id", id)
		w.Header().Set("Request-Id", id)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.URL.Path != "/healthz" {
			if s.cfg.APIKey == "" {
				// Without a key only this machine may call the service. Browsers
				// send Origin on cross-site requests and DNS rebinding changes Host.
				if !localRequest(r) {
					s.error(w, r, 403, "permission_error", "only local clients may use this service without an api_key", "")
					return
				}
			} else {
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
		}
		started := time.Now()
		info := &requestLog{}
		mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), logKey{}, info)))
		attrs := []any{"request_id", id, "method", r.Method, "path", r.URL.Path, "status", w.status, "duration_ms", time.Since(started).Milliseconds()}
		if info.model != "" {
			attrs = append(attrs, "model", info.model)
		}
		if len(info.warnings) > 0 {
			attrs = append(attrs, "warnings", info.warnings)
		}
		s.logger.Info("request completed", attrs...)
	})
}

func localRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	if !loopbackHost(host) {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || !loopbackHost(u.Hostname()) {
			return false
		}
	}
	return true
}

func loopbackHost(host string) bool {
	host = strings.TrimSuffix(strings.Trim(host, "[]"), ".")
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes))
	if err != nil {
		s.writeError(w, r, err)
		return nil, false
	}
	return data, true
}

func (s *Server) decode(r *http.Request, data []byte, count bool) (protocol.Input, error) {
	d := protocol.Decoder{State: s.state}
	if strings.HasPrefix(r.URL.Path, "/v1/messages") {
		return d.Messages(r.Context(), data, count)
	}
	return d.Responses(r.Context(), data)
}

// acquire waits for a generation slot instead of rejecting bursts outright.
func (s *Server) acquire(w http.ResponseWriter, r *http.Request) bool {
	select {
	case s.slots <- struct{}{}:
		return true
	default:
	}
	timer := time.NewTimer(queueWait)
	defer timer.Stop()
	select {
	case s.slots <- struct{}{}:
		return true
	case <-r.Context().Done():
		return false
	case <-timer.C:
		w.Header().Set("Retry-After", "5")
		s.error(w, r, 429, "rate_limit_error", "concurrency limit reached", "")
		return false
	}
}

// prepare decodes a request and shapes it into the native request sent upstream.
func (s *Server) prepare(w http.ResponseWriter, r *http.Request, data []byte, count bool) (protocol.Input, bool) {
	in, err := s.decode(r, data, count)
	s.note(r, in.Model, in.Warnings...)
	if err != nil {
		s.writeError(w, r, err)
		return in, false
	}
	in.Warnings = nil
	if s.cfg.SanitizeClientMarkers {
		in.SanitizeClientMarkers()
	}
	if r.URL.Path == "/v1/responses/compact" {
		in.Compact, in.Stream, in.Store = "endpoint", false, false
	}
	if in.Compact != "" && !count {
		in.PrepareCompaction()
	}
	catalog, err := s.getCatalog(r.Context())
	if err == nil {
		err = in.Prepare(catalog)
	}
	if err == nil {
		err = s.resolveMedia(r.Context(), &in.Request)
	}
	if err == nil {
		err = in.Finalize(r.Header)
	}
	// Only Messages replies store text signatures this way.
	if err == nil && !count && r.URL.Path == "/v1/messages" {
		err = (protocol.Decoder{State: s.state}).RestoreTextSignatures(r.Context(), &in)
	}
	s.note(r, "", in.Warnings...)
	if err != nil {
		s.writeError(w, r, err)
		return in, false
	}
	return in, true
}

// sseWriter serializes stream writes so heartbeats can share the connection.
type sseWriter struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	started bool
	closed  bool
	last    time.Time
}

func (s *sseWriter) send(event string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		s.w.Header().Set("Content-Type", "text/event-stream")
		s.w.Header().Set("Cache-Control", "no-cache")
		s.w.Header().Set("X-Accel-Buffering", "no")
		s.w.WriteHeader(200)
		s.started = true
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return err
	}
	s.last = time.Now()
	return http.NewResponseController(s.w).Flush()
}

// ping keeps idle streams alive while the model is still working, e.g. while
// it generates a large tool call that arrives as one upstream part.
func (s *sseWriter) ping(messages bool, idle time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || s.closed || time.Since(s.last) < idle {
		return
	}
	frame := ": keep-alive\n\n"
	if messages {
		frame = "event: ping\ndata: {\"type\":\"ping\"}\n\n"
	}
	if _, err := io.WriteString(s.w, frame); err == nil {
		s.last = time.Now()
		_ = http.NewResponseController(s.w).Flush()
	}
}

func (s *sseWriter) close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

func (s *Server) generate(w http.ResponseWriter, r *http.Request) {
	// The body is read before queueing so slow uploads do not hold a slot.
	data, ok := s.readBody(w, r)
	if !ok || !s.acquire(w, r) {
		return
	}
	defer func() { <-s.slots }()
	in, ok := s.prepare(w, r, data, false)
	if !ok {
		return
	}
	format := "responses"
	if r.URL.Path == "/v1/messages" {
		format = "messages"
	}
	sse := &sseWriter{w: w}
	var emit func(string, map[string]any) error
	if in.Stream {
		emit = func(event string, value map[string]any) error {
			data, err := json.Marshal(value)
			if err != nil {
				return err
			}
			return sse.send(event, data)
		}
		stop := make(chan struct{})
		defer close(stop)
		defer sse.close()
		go func() {
			ticker := time.NewTicker(s.heartbeat / 3)
			defer ticker.Stop()
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					sse.ping(format == "messages", s.heartbeat)
				}
			}
		}()
	}
	renderer := protocol.NewRenderer(format, in, s.state, emit)
	err := s.upstream.Generate(r.Context(), in.UpstreamModel(), in.Request, in.Kind, func(chunk ag.Chunk) error {
		if chunk.Grounding != nil {
			s.upstream.ResolveGrounding(r.Context(), chunk.Grounding)
		}
		return renderer.Add(r.Context(), chunk)
	})
	if err == nil {
		err = renderer.Finish(r.Context())
	}
	s.note(r, "", renderer.Warnings...)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		sse.mu.Lock()
		started := sse.started
		sse.mu.Unlock()
		if started {
			_ = renderer.Fail(publicError(err))
			s.logger.Warn("stream failed", "request_id", w.Header().Get("X-Request-Id"), "error", err.Error())
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

// count does not take a generation slot: it is short and clients call it often.
func (s *Server) count(w http.ResponseWriter, r *http.Request) {
	data, ok := s.readBody(w, r)
	if !ok {
		return
	}
	in, ok := s.prepare(w, r, data, true)
	if !ok {
		return
	}
	n, err := s.upstream.Count(r.Context(), in.Request)
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

// getCatalog caches the model catalog. When a refresh fails, the previous
// catalog keeps serving and the refresh is retried shortly.
func (s *Server) getCatalog(ctx context.Context) (ag.Catalog, error) {
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()
	now := time.Now()
	if now.Before(s.catalogExpiry) {
		return s.catalog, nil
	}
	catalog, err := s.upstream.Models(ctx)
	if err != nil {
		if s.catalog.Models != nil {
			s.catalogExpiry = now.Add(catalogRetry)
			s.logger.Warn("model catalog refresh failed; serving the cached catalog", "error", err.Error())
			return s.catalog, nil
		}
		return ag.Catalog{}, err
	}
	s.catalog = catalog
	s.catalogExpiry = now.Add(catalogTTL)
	return catalog, nil
}

func anthropic(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, "/v1/messages") || r.Header.Get("Anthropic-Version") != ""
}
func modelObject(id string, value json.RawMessage, anthropicFormat bool) map[string]any {
	if !anthropicFormat {
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
		if upstream.Detail != "" {
			s.logger.Warn("upstream error", "request_id", w.Header().Get("X-Request-Id"), "status", upstream.Status, "detail", upstream.Detail)
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
	if kind == "authentication_error" || kind == "permission_error" {
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
