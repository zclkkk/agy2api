package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zclkkk/agy2api/internal/config"
)

func TestNewRejectsUnusableAccounts(t *testing.T) {
	for name, accounts := range map[string][]*Account{
		"none":       nil,
		"empty key":  {{ID: "aaaaaaaa"}},
		"shared key": {{ID: "aaaaaaaa", Key: "same"}, {ID: "bbbbbbbb", Key: "same"}},
	} {
		if _, err := New(config.Default(), accounts, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// accountUpstream is an upstream that offers model and answers with text.
// While hold is set, generation waits for release.
func accountUpstream(t *testing.T, model, text string, hold *atomic.Bool, release chan struct{}) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1internal:fetchAvailableModels":
			io.WriteString(w, `{"models":{"`+model+`":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"}},"agentModelSorts":[{"groups":[{"modelIds":["`+model+`"]}]}]}`)
		default:
			if hold != nil && hold.Load() {
				<-release
			}
			answer(text, "STOP")(w, r)
		}
	}))
	t.Cleanup(up.Close)
	return up
}

func TestAccountsAreIsolatedByKey(t *testing.T) {
	var hold atomic.Bool
	release := make(chan struct{})
	a := testAccount(t, "aaaaaaaa", "key-a", accountUpstream(t, "gemini-a", "from a", &hold, release))
	b := testAccount(t, "bbbbbbbb", "key-b", accountUpstream(t, "gemini-b", "from b", nil, nil))
	cfg := config.Default()
	cfg.MaxConcurrentRequests = 1
	var logs bytes.Buffer
	s := startServer(t, newServer(t, cfg, slog.New(slog.NewJSONHandler(&logs, nil)), a, b), "unused")
	// Cleanups run in reverse order: the held request is released before the
	// servers close, so a failing test cannot hang on it.
	unhold := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unhold)
	send := func(timeout time.Duration, method, path string, headers map[string]string, body string) (int, string, error) {
		req, _ := http.NewRequest(method, s.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		res, err := (&http.Client{Timeout: timeout}).Do(req)
		if err != nil {
			return 0, "", err
		}
		defer res.Body.Close()
		data, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(data), nil
	}
	call := func(method, path string, headers map[string]string, body string) (int, string) {
		t.Helper()
		status, data, err := send(10*time.Second, method, path, headers, body)
		if err != nil {
			t.Fatal(err)
		}
		return status, data
	}
	keyA, keyB := map[string]string{"Authorization": "Bearer key-a"}, map[string]string{"X-Api-Key": "key-b"}

	for name, tc := range map[string]struct {
		path    string
		headers map[string]string
		want    int
	}{
		"no key":        {"/v1/models", nil, 401},
		"unknown key":   {"/v1/models", map[string]string{"Authorization": "Bearer key-c"}, 401},
		"health":        {"/healthz", nil, 200},
		"bearer key":    {"/v1/models", keyA, 200},
		"x-api-key":     {"/v1/models", keyB, 200},
		"unknown route": {"/v1/chat/completions", nil, 401},
	} {
		if status, body := call(http.MethodGet, tc.path, tc.headers, ""); status != tc.want {
			t.Errorf("%s: %d %s", name, status, body)
		}
	}

	// Each key sees its own account's catalog.
	if _, body := call(http.MethodGet, "/v1/models", keyB, ""); !strings.Contains(body, "gemini-b") || strings.Contains(body, "gemini-a") {
		t.Fatalf("account b lists %s", body)
	}
	if status, body := call(http.MethodPost, "/v1/responses", keyB, `{"model":"gemini-a","input":"hi"}`); status != 400 {
		t.Fatalf("account b served account a's model: %d %s", status, body)
	}

	// Stored responses belong to the account that made them.
	status, body := call(http.MethodPost, "/v1/responses", keyA, `{"model":"gemini-a","input":"hi"}`)
	var created struct{ ID string }
	_ = json.Unmarshal([]byte(body), &created)
	if status != 200 || created.ID == "" || !strings.Contains(body, "from a") {
		t.Fatalf("account a: %d %s", status, body)
	}
	for _, tc := range []struct {
		method  string
		headers map[string]string
		want    int
	}{
		{http.MethodGet, keyB, 404}, {http.MethodDelete, keyB, 404}, {http.MethodGet, keyA, 200},
	} {
		if status, body := call(tc.method, "/v1/responses/"+created.ID, tc.headers, ""); status != tc.want {
			t.Fatalf("%s with %v: %d %s", tc.method, tc.headers, status, body)
		}
	}

	// A compaction item is sealed with the account's key.
	status, body = call(http.MethodPost, "/v1/responses/compact", keyA, `{"model":"gemini-a","input":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"}]}`)
	var compacted struct{ Output []json.RawMessage }
	_ = json.Unmarshal([]byte(body), &compacted)
	if status != 200 || len(compacted.Output) != 1 {
		t.Fatalf("compact: %d %s", status, body)
	}
	for model, tc := range map[string]struct {
		headers map[string]string
		want    int
	}{"gemini-a": {keyA, 200}, "gemini-b": {keyB, 400}} {
		request := `{"model":"` + model + `","store":false,"input":[` + string(compacted.Output[0]) + `,{"role":"user","content":"next"}]}`
		if status, body := call(http.MethodPost, "/v1/responses", tc.headers, request); status != tc.want {
			t.Fatalf("compaction item with %v: %d %s", tc.headers, status, body)
		}
	}

	// A busy account does not hold up another: account a's only slot is held
	// until account b has been served, which would otherwise wait in the queue.
	hold.Store(true)
	done := make(chan int, 1)
	go func() {
		status, _, _ := send(time.Minute, http.MethodPost, "/v1/responses", keyA, `{"model":"gemini-a","input":"slow","store":false}`)
		done <- status
	}()
	time.Sleep(100 * time.Millisecond)
	if status, body, err := send(3*time.Second, http.MethodPost, "/v1/responses", keyB, `{"model":"gemini-b","input":"hi","store":false}`); err != nil || status != 200 || !strings.Contains(body, "from b") {
		t.Fatalf("account b waited for account a: %d %s %v", status, body, err)
	}
	unhold()
	if status := <-done; status != 200 {
		t.Fatalf("held request: %d", status)
	}

	// Request log lines name the account.
	for _, id := range []string{`"account":"aaaaaaaa"`, `"account":"bbbbbbbb"`} {
		if !strings.Contains(logs.String(), id) {
			t.Fatalf("logs lack %s", id)
		}
	}
}
