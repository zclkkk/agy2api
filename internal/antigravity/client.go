package antigravity

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type TokenSource interface {
	AccessToken(context.Context) (string, error)
	ProjectID() string
}

// invalidator is implemented by token sources that can drop a rejected token.
type invalidator interface{ Invalidate(token string) }
type Client struct {
	HTTP    *http.Client
	BaseURL string
	// UserAgent returns the User-Agent header; nil sends the fallback version.
	UserAgent func() string
	Auth      TokenSource
}
type HTTPError struct {
	Status     int
	Message    string
	RetryAfter string
	// Detail is the upstream's own error text, for the local log only. It can
	// echo request content, so it is never returned to API clients.
	Detail string
}

func (e *HTTPError) Error() string { return e.Message }

func NewHTTPClient(proxy string) (*http.Client, error) {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = 15 * time.Second
	t.ResponseHeaderTimeout = 90 * time.Second
	t.IdleConnTimeout = 60 * time.Second
	t.MaxIdleConnsPerHost = 8
	if proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil {
			return nil, err
		}
		t.Proxy = http.ProxyURL(u)
	}
	return &http.Client{Transport: t, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}

func ID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b[:])
}

func (c *Client) post(ctx context.Context, method string, payload any) (*http.Response, error) {
	token, err := c.Auth.AccessToken(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := c.postToken(ctx, method, payload, token)
	// A token can be revoked before its expiry; refresh once and retry.
	var status *HTTPError
	if inv, ok := c.Auth.(invalidator); ok && errors.As(err, &status) && status.Status == http.StatusUnauthorized {
		inv.Invalidate(token)
		if token, err = c.Auth.AccessToken(ctx); err != nil {
			return nil, err
		}
		return c.postToken(ctx, method, payload, token)
	}
	return resp, err
}

func (c *Client) postToken(ctx context.Context, method string, payload any, token string) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/v1internal:"+method, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	agent := "antigravity/" + FallbackVersion
	if c.UserAgent != nil {
		agent = c.UserAgent()
	}
	r.Header.Set("User-Agent", agent)
	resp, err := c.HTTP.Do(r)
	if err != nil {
		return nil, fmt.Errorf("Antigravity connection failed: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, &HTTPError{Status: resp.StatusCode, Message: upstreamErrorMessage(resp.StatusCode, data), RetryAfter: upstreamRetryAfter(resp.Header.Get("Retry-After"), data), Detail: upstreamErrorDetail(data)}
	}
	return resp, nil
}

var upstreamEnum = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,79}$`)

func upstreamRetryAfter(header string, data []byte) string {
	if header != "" {
		return header
	}
	var body struct {
		Error struct {
			Details []struct {
				Type  string `json:"@type"`
				Delay string `json:"retryDelay"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &body) != nil {
		return ""
	}
	for _, detail := range body.Error.Details {
		if detail.Type != "type.googleapis.com/google.rpc.RetryInfo" {
			continue
		}
		delay, err := time.ParseDuration(detail.Delay)
		if err == nil && delay >= 0 {
			return strconv.FormatInt(int64(math.Ceil(delay.Seconds())), 10)
		}
	}
	return ""
}

func upstreamErrorDetail(data []byte) string {
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	detail := strings.TrimSpace(string(data))
	if json.Unmarshal(data, &body) == nil && body.Error.Message != "" {
		detail = body.Error.Message
	}
	if len(detail) > 500 {
		detail = detail[:500] + "…"
	}
	return detail
}

func upstreamErrorMessage(status int, data []byte) string {
	message := fmt.Sprintf("Antigravity returned HTTP %d", status)
	// Structured status/reason enums aid diagnosis without exposing raw error
	// messages, project IDs, account metadata, or echoed request content.
	var body struct {
		Error struct {
			Status  string `json:"status"`
			Details []struct {
				Reason string `json:"reason"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &body) != nil {
		return message
	}
	var codes []string
	if upstreamEnum.MatchString(body.Error.Status) {
		codes = append(codes, body.Error.Status)
	}
	for _, d := range body.Error.Details {
		if upstreamEnum.MatchString(d.Reason) {
			codes = append(codes, d.Reason)
		}
	}
	if len(codes) > 0 {
		message += " (" + strings.Join(codes, ": ") + ")"
	}
	return message
}

func readJSON(r *http.Response, dst any) error {
	defer r.Body.Close()
	data, err := io.ReadAll(io.LimitReader(r.Body, (32<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 32<<20 {
		return errors.New("upstream response exceeds 32 MiB")
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return errors.New("invalid JSON from Antigravity")
	}
	return nil
}

func (c *Client) Models(ctx context.Context) (Catalog, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var catalog Catalog
	r, err := c.post(ctx, "fetchAvailableModels", map[string]string{"project": c.Auth.ProjectID()})
	if err != nil {
		return catalog, err
	}
	err = readJSON(r, &catalog)
	if err == nil && catalog.Models == nil {
		err = errors.New("Antigravity returned no model catalog")
	}
	// Only Gemini models are served. Third-party models (Claude, GPT-OSS) and
	// internal entries would each need their own request rules.
	for id, data := range catalog.Models {
		var info struct {
			Provider string `json:"apiProvider"`
		}
		if json.Unmarshal(data, &info) != nil || info.Provider != GeminiProvider {
			delete(catalog.Models, id)
		}
	}
	return catalog, err
}

// Count returns Antigravity's token estimate. The endpoint takes no model, so the
// estimate is the same for every model.
func (c *Client) Count(ctx context.Context, request Request) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	request.SessionID = ""
	request.ToolConfig = nil
	request.Generation = Generation{}
	r, err := c.post(ctx, "countTokens", struct {
		Request Request `json:"request"`
	}{request})
	if err != nil {
		return 0, err
	}
	var out struct {
		Total *int `json:"totalTokens"`
	}
	if err = readJSON(r, &out); err != nil {
		return 0, err
	}
	if out.Total == nil || *out.Total < 0 {
		return 0, errors.New("Antigravity returned no token count")
	}
	return *out.Total, nil
}

// Generate consumes the upstream stream in order. Success requires a finish reason;
// EOF and [DONE] alone are not proof that generation completed.
func (c *Client) Generate(ctx context.Context, model string, request Request, kind string, emit func(Chunk) error) error {
	if kind == "" {
		kind = KindAgent
	}
	if kind == KindImage && len(request.Generation.Modalities) == 0 {
		request.Generation.Modalities = []string{"TEXT", "IMAGE"}
	}
	if request.SessionID == "" {
		request.SessionID = ID("session-")
	}
	r, err := c.post(ctx, "streamGenerateContent?alt=sse", Envelope{Project: c.Auth.ProjectID(), Model: model, UserAgent: "antigravity", RequestID: ID("agent-"), RequestType: kind, Request: request})
	if err != nil {
		return err
	}
	defer r.Body.Close()
	finished := false
	consume := func(data []byte) error {
		if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
			return nil
		}
		var wrapped struct {
			Response json.RawMessage `json:"response"`
			Error    json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(data, &wrapped); err != nil {
			return errors.New("malformed Antigravity stream event")
		}
		if len(wrapped.Error) > 0 && string(wrapped.Error) != "null" {
			return errors.New("Antigravity reported an error during generation")
		}
		if len(wrapped.Response) > 0 {
			data = wrapped.Response
		}
		var event Response
		if err := json.Unmarshal(data, &event); err != nil {
			return errors.New("malformed Antigravity response")
		}
		if len(event.Error) > 0 && string(event.Error) != "null" {
			return errors.New("Antigravity reported an error during generation")
		}
		if len(event.Candidates) > 1 {
			return errors.New("multiple upstream candidates are unsupported")
		}
		chunk := Chunk{Usage: event.Usage}
		if len(event.Candidates) == 1 {
			candidate := event.Candidates[0]
			chunk.Parts = candidate.Content.Parts
			chunk.Finish = candidate.Finish
			chunk.Grounding = candidate.Grounding
			if candidate.Finish != "" {
				finished = true
			}
		} else if len(event.PromptFeedback) > 0 {
			var feedback struct {
				BlockReason string `json:"blockReason"`
			}
			_ = json.Unmarshal(event.PromptFeedback, &feedback)
			if feedback.BlockReason != "" {
				return errors.New("Antigravity blocked the request")
			}
		}
		return emit(chunk)
	}
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		data, err := io.ReadAll(io.LimitReader(r.Body, (32<<20)+1))
		if err != nil {
			return err
		}
		if len(data) > 32<<20 {
			return errors.New("upstream response too large")
		}
		if err := consume(data); err != nil {
			return err
		}
	} else if err := DecodeSSE(r.Body, consume); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !finished {
		return errors.New("Antigravity stream ended before a finish reason")
	}
	return nil
}

// groundingRedirect prefixes the links Gemini search returns in place of the
// pages it cites. A variable so tests can point it at a local server.
var groundingRedirect = "https://vertexaisearch.cloud.google.com/grounding-api-redirect/"

// ResolveGrounding replaces search redirect links with the pages they point
// to, so clients can show where a citation comes from. Each link costs one
// HEAD request without credentials; links not resolved within a few seconds
// keep their redirect form.
func (c *Client) ResolveGrounding(ctx context.Context, g *Grounding) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var links []string
	seen := map[string]bool{}
	for _, chunk := range g.Chunks {
		if chunk.Web != nil && strings.HasPrefix(chunk.Web.URI, groundingRedirect) && !seen[chunk.Web.URI] {
			seen[chunk.Web.URI] = true
			links = append(links, chunk.Web.URI)
		}
	}
	targets := map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, 8)
	for _, link := range links {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			if target := c.redirectTarget(ctx, link); target != "" {
				mu.Lock()
				targets[link] = target
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	for _, chunk := range g.Chunks {
		if chunk.Web != nil && targets[chunk.Web.URI] != "" {
			chunk.Web.URI = targets[chunk.Web.URI]
		}
	}
}

// redirectTarget returns the Location of a redirect response, or "". The HTTP
// client does not follow redirects, so the target is never fetched.
func (c *Client) redirectTarget(ctx context.Context, link string) string {
	r, err := http.NewRequestWithContext(ctx, http.MethodHead, link, nil)
	if err != nil {
		return ""
	}
	resp, err := c.HTTP.Do(r)
	if err != nil {
		return ""
	}
	resp.Body.Close()
	if resp.StatusCode < 300 || resp.StatusCode >= 400 {
		return ""
	}
	target, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || (target.Scheme != "https" && target.Scheme != "http") || target.Host == "" {
		return ""
	}
	return target.String()
}

func DecodeSSE(r io.Reader, emit func([]byte) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 32<<20)
	var data, raw bytes.Buffer
	flush := func() error {
		if data.Len() == 0 {
			return nil
		}
		b := bytes.TrimSuffix(data.Bytes(), []byte("\n"))
		err := emit(b)
		data.Reset()
		return err
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event:") || strings.HasPrefix(line, "id:") || strings.HasPrefix(line, "retry:") {
			continue
		}
		if value, ok := strings.CutPrefix(line, "data:"); ok {
			value = strings.TrimPrefix(value, " ")
			if data.Len()+len(value) > 32<<20 {
				return errors.New("SSE event too large")
			}
			data.WriteString(value)
			data.WriteByte('\n')
		} else {
			// Some upstream failures are sent as raw JSON after SSE data.
			if raw.Len()+len(line) > 1<<20 {
				return errors.New("unexpected upstream stream data")
			}
			raw.WriteString(line)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read Antigravity stream: %w", err)
	}
	if err := flush(); err != nil {
		return err
	}
	if raw.Len() > 0 {
		return emit(raw.Bytes())
	}
	return nil
}

func (c *Client) DiscoverProject(ctx context.Context, token string) (string, error) {
	var load struct {
		Project json.RawMessage `json:"cloudaicompanionProject"`
		Allowed []struct {
			ID      string `json:"id"`
			Default bool   `json:"isDefault"`
		} `json:"allowedTiers"`
		Current struct {
			ID string `json:"id"`
		} `json:"currentTier"`
	}
	r, err := c.postToken(ctx, "loadCodeAssist", map[string]any{"metadata": map[string]string{"ideType": "ANTIGRAVITY"}}, token)
	if err != nil {
		return "", err
	}
	if err := readJSON(r, &load); err != nil {
		return "", err
	}
	if project := projectID(load.Project); project != "" {
		return project, nil
	}
	tier := load.Current.ID
	if tier == "" {
		tier = "free-tier"
	}
	for _, v := range load.Allowed {
		if v.Default {
			tier = v.ID
			break
		}
	}
	for i := 0; i < 5; i++ {
		if i > 0 {
			timer := time.NewTimer(2 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return "", ctx.Err()
			case <-timer.C:
			}
		}
		r, err := c.postToken(ctx, "onboardUser", map[string]any{"tier_id": tier, "metadata": map[string]string{"ide_type": "ANTIGRAVITY", "ide_name": "antigravity"}}, token)
		if err != nil {
			return "", err
		}
		var out struct {
			Done     bool `json:"done"`
			Response struct {
				Project json.RawMessage `json:"cloudaicompanionProject"`
			} `json:"response"`
		}
		if err := readJSON(r, &out); err != nil {
			return "", err
		}
		if out.Done {
			if id := projectID(out.Response.Project); id != "" {
				return id, nil
			}
			return "", errors.New("onboarding returned no project")
		}
	}
	return "", errors.New("Antigravity onboarding is still pending; try login again")
}

func projectID(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &obj)
	return obj.ID
}
