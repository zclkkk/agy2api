// Package server exposes the public HTTP APIs.
package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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

// Account is one hosted account. Requests authenticated with its key use its
// upstream identity, state, concurrency slots and model catalog.
type Account struct {
	ID       string
	Key      string
	Upstream *ag.Client
	State    *state.Store

	logger        *slog.Logger
	slots         chan struct{}
	catalogMu     sync.Mutex
	catalog       ag.Catalog
	catalogExpiry time.Time
}

type Server struct {
	cfg       config.Config
	logger    *slog.Logger
	accounts  []*Account
	keys      map[[sha256.Size]byte]*Account
	media     *http.Client
	heartbeat time.Duration
}

// New serves the accounts. Each must have its own API key: a shared key would
// route one account's clients to another.
func New(cfg config.Config, accounts []*Account, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if len(accounts) == 0 {
		return nil, errors.New("no accounts; run agy2api login first")
	}
	s := &Server{cfg: cfg, logger: logger, accounts: accounts, keys: make(map[[sha256.Size]byte]*Account), media: mediaClient(), heartbeat: heartbeatPeriod}
	for _, a := range accounts {
		if a.Key == "" {
			return nil, fmt.Errorf("account %s has no API key", a.ID)
		}
		sum := sha256.Sum256([]byte(a.Key))
		if other, ok := s.keys[sum]; ok {
			return nil, fmt.Errorf("accounts %s and %s have the same API key; log one of them out", other.ID, a.ID)
		}
		s.keys[sum] = a
		a.logger = logger.With("account", a.ID)
		a.slots = make(chan struct{}, cfg.MaxConcurrentRequests)
	}
	return s, nil
}

// authenticate returns the account whose key the request carries.
func (s *Server) authenticate(r *http.Request) *Account {
	key := r.Header.Get("X-Api-Key")
	if key == "" {
		key = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	if key == "" {
		return nil
	}
	return s.keys[sha256.Sum256([]byte(key))]
}

type accountKey struct{}

// accountOf returns the account an authenticated request belongs to.
func accountOf(r *http.Request) *Account { return r.Context().Value(accountKey{}).(*Account) }

// requestLog collects per-request details for the completion log line.
type requestLog struct {
	mu       sync.Mutex
	account  string
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
	mux.HandleFunc("POST /v1/responses", s.generation(protocol.Responses, protocol.Generate))
	mux.HandleFunc("POST /v1/responses/compact", s.generation(protocol.Responses, protocol.CompactResponse))
	mux.HandleFunc("POST /v1/messages", s.generation(protocol.Messages, protocol.Generate))
	mux.HandleFunc("POST /v1/messages/count_tokens", s.counter(protocol.Messages))
	mux.HandleFunc("POST /v1/responses/input_tokens", s.counter(protocol.Responses))
	mux.HandleFunc("POST /v1/images/generations", s.images(false))
	mux.HandleFunc("POST /v1/images/edits", s.images(true))
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
		info := &requestLog{}
		ctx := context.WithValue(r.Context(), logKey{}, info)
		if r.URL.Path != "/healthz" {
			a := s.authenticate(r)
			if a == nil {
				s.error(w, r, 401, "authentication_error", "invalid API key", "")
				return
			}
			info.account = a.ID
			ctx = context.WithValue(ctx, accountKey{}, a)
		}
		started := time.Now()
		mux.ServeHTTP(w, r.WithContext(ctx))
		attrs := []any{"request_id", id, "method", r.Method, "path", r.URL.Path, "status", w.status, "duration_ms", time.Since(started).Milliseconds()}
		if info.account != "" {
			attrs = append(attrs, "account", info.account)
		}
		if info.model != "" {
			attrs = append(attrs, "model", info.model)
		}
		if len(info.warnings) > 0 {
			attrs = append(attrs, "warnings", info.warnings)
		}
		s.logger.Info("request completed", attrs...)
	})
}

func (s *Server) readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes))
	if err != nil {
		s.writeError(w, r, err)
		return nil, false
	}
	return data, true
}

// acquire waits for one of the account's generation slots instead of
// rejecting bursts outright. The caller runs release when done.
func (s *Server) acquire(w http.ResponseWriter, r *http.Request) (release func(), ok bool) {
	slots := accountOf(r).slots
	release = func() { <-slots }
	select {
	case slots <- struct{}{}:
		return release, true
	default:
	}
	timer := time.NewTimer(queueWait)
	defer timer.Stop()
	select {
	case slots <- struct{}{}:
		return release, true
	case <-r.Context().Done():
		return nil, false
	case <-timer.C:
		w.Header().Set("Retry-After", "5")
		s.error(w, r, 429, "rate_limit_error", "concurrency limit reached", "")
		return nil, false
	}
}

// prepare decodes a request and shapes it into the native request sent upstream.
func (s *Server) prepare(w http.ResponseWriter, r *http.Request, data []byte, format protocol.Format, operation protocol.Operation) (protocol.PreparedRequest, bool) {
	var intent protocol.RequestIntent
	var err error
	decoder := protocol.Decoder{}
	if format == protocol.Messages {
		intent, err = decoder.Messages(data, operation == protocol.CountTokens)
	} else {
		intent, err = decoder.Responses(data)
	}
	if err != nil {
		s.note(r, intent.Model, intent.Warnings...)
		s.writeError(w, r, err)
		return protocol.PreparedRequest{}, false
	}
	a := accountOf(r)
	catalog, err := a.getCatalog(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return protocol.PreparedRequest{}, false
	}
	planner := protocol.Planner{State: a.State, SanitizeClientMarkers: s.cfg.SanitizeClientMarkers, ResolveMedia: s.resolveMedia}
	prepared, err := planner.Prepare(r.Context(), intent, catalog, operation, r.Header)
	s.note(r, intent.Model, prepared.Warnings...)
	if err != nil {
		s.writeError(w, r, err)
		return prepared, false
	}
	return prepared, true
}

func (s *Server) generation(format protocol.Format, operation protocol.Operation) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { s.generate(w, r, format, operation) }
}

func (s *Server) counter(format protocol.Format) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { s.count(w, r, format) }
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

func (s *Server) generate(w http.ResponseWriter, r *http.Request, format protocol.Format, operation protocol.Operation) {
	// The body is read before queueing so slow uploads do not hold a slot.
	data, ok := s.readBody(w, r)
	if !ok {
		return
	}
	release, ok := s.acquire(w, r)
	if !ok {
		return
	}
	defer release()
	in, ok := s.prepare(w, r, data, format, operation)
	if !ok {
		return
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
	a := accountOf(r)
	renderer := protocol.NewRenderer(in, a.State, emit)
	err := a.Upstream.Generate(r.Context(), in.Model, in.Request, ag.KindAgent, func(chunk ag.Chunk) error {
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
func (s *Server) count(w http.ResponseWriter, r *http.Request, format protocol.Format) {
	data, ok := s.readBody(w, r)
	if !ok {
		return
	}
	in, ok := s.prepare(w, r, data, format, protocol.CountTokens)
	if !ok {
		return
	}
	n, err := accountOf(r).Upstream.Count(r.Context(), in.Request)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if format == protocol.Messages {
		writeJSON(w, 200, map[string]int{"input_tokens": n})
	} else {
		writeJSON(w, 200, map[string]any{"object": "response.input_tokens", "input_tokens": n})
	}
}

// getCatalog caches the account's model catalog. When a refresh fails, the
// previous catalog keeps serving and the refresh is retried shortly.
func (a *Account) getCatalog(ctx context.Context) (ag.Catalog, error) {
	a.catalogMu.Lock()
	defer a.catalogMu.Unlock()
	now := time.Now()
	if now.Before(a.catalogExpiry) {
		return a.catalog, nil
	}
	catalog, err := a.Upstream.Models(ctx)
	if err != nil {
		if a.catalog.Models != nil {
			a.catalogExpiry = now.Add(catalogRetry)
			a.logger.Warn("model catalog refresh failed; serving the cached catalog", "error", err.Error())
			return a.catalog, nil
		}
		return ag.Catalog{}, err
	}
	a.catalog = catalog
	a.catalogExpiry = now.Add(catalogTTL)
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
	catalog, err := accountOf(r).getCatalog(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	ids := make([]string, 0)
	data := make([]any, 0)
	for _, id := range catalog.ListedModelIDs() {
		if served(catalog, id, anthropic(r)) {
			ids = append(ids, id)
			data = append(data, modelObject(id, catalog.Models[id], anthropic(r)))
		}
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
	catalog, err := accountOf(r).getCatalog(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	id := r.PathValue("id")
	if !served(catalog, id, anthropic(r)) {
		s.error(w, r, 404, "not_found_error", "model not found", "model")
		return
	}
	writeJSON(w, 200, modelObject(id, catalog.Models[id], anthropic(r)))
}

// served reports whether the requesting API format can use the model. Image
// generation models only serve the OpenAI Images API.
func served(catalog ag.Catalog, id string, anthropicFormat bool) bool {
	info, ok := catalog.Info(id)
	return ok && !(anthropicFormat && info.Image)
}

func (s *Server) stored(w http.ResponseWriter, r *http.Request) (protocol.StoredResponse, bool) {
	record, err := protocol.LoadResponse(r.Context(), accountOf(r).State, r.PathValue("id"))
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
	// A client asking for an event stream cannot use the stored JSON instead.
	query := r.URL.Query()
	if stream := query.Get("stream"); (stream != "" && stream != "false") || query.Has("starting_after") {
		s.error(w, r, 400, "invalid_request_error", "streaming retrieval is not supported; retrieve the stored response without stream or starting_after", "stream")
		return
	}
	record, ok := s.stored(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(record.Response)
}
func (s *Server) delete(w http.ResponseWriter, r *http.Request) {
	deleted, err := accountOf(r).State.Delete(r.Context(), "response", r.PathValue("id"))
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
	page, err := inputItemsPage(record.Items, r.URL.Query())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, 200, page)
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
