package antigravity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestUserAgentFollowsManifest(t *testing.T) {
	body := "version: 2.30.1\nfiles:\n  - url: https://example.invalid/a.zip\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "electron-builder" {
			t.Errorf("manifest request User-Agent = %q", r.Header.Get("User-Agent"))
		}
		_, _ = w.Write([]byte(body))
	}))
	defer up.Close()
	for _, tc := range []struct{ configured, before, after string }{
		{"", "antigravity/" + FallbackVersion, "antigravity/2.30.1"},
		{"antigravity/2.24.0", "antigravity/2.24.0", "antigravity/2.30.1"},
		{"antigravity/3.0.0", "antigravity/3.0.0", "antigravity/3.0.0"}, // configured version is a floor
		{"custom-agent/1.0", "custom-agent/1.0", "custom-agent/1.0"},    // custom values are sent as is
	} {
		agent := NewUserAgent(tc.configured)
		agent.manifest = up.URL
		if got := agent.String(); got != tc.before {
			t.Fatalf("%q before refresh = %q, want %q", tc.configured, got, tc.before)
		}
		if _, err := agent.Refresh(context.Background(), up.Client()); err != nil {
			t.Fatal(err)
		}
		if got := agent.String(); got != tc.after {
			t.Fatalf("%q after refresh = %q, want %q", tc.configured, got, tc.after)
		}
	}
}

func TestUserAgentKeepsVersionWhenManifestFails(t *testing.T) {
	var failing atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() {
			http.Error(w, "unavailable", 503)
			return
		}
		_, _ = w.Write([]byte("version: 2.40.0\n"))
	}))
	defer up.Close()
	agent := NewUserAgent("")
	agent.manifest = up.URL
	if _, err := agent.Refresh(context.Background(), up.Client()); err != nil {
		t.Fatal(err)
	}
	failing.Store(true)
	if _, err := agent.Refresh(context.Background(), up.Client()); err == nil {
		t.Fatal("expected an error from a failing manifest")
	}
	if got := agent.String(); got != "antigravity/2.40.0" {
		t.Fatalf("version after a failed refresh = %q", got)
	}
}

func TestManifestVersionParsing(t *testing.T) {
	for _, tc := range []struct {
		body, want string
		ok         bool
	}{
		{"version: 2.26.0\npath: a.zip\n", "2.26.0", true},
		{"path: a.zip\nversion: '2.27.1'\n", "2.27.1", true},
		{"version: latest\n", "", false},
		{"files: []\n", "", false},
	} {
		got, err := manifestVersion(strings.NewReader(tc.body))
		if (err == nil) != tc.ok || got != tc.want {
			t.Fatalf("manifestVersion(%q) = %q, %v", tc.body, got, err)
		}
	}
	if newerVersion("2.9.0", "2.10.0") != "2.10.0" || newerVersion("2.24.0", "") != "2.24.0" || newerVersion("3.0.0", "2.99.99") != "3.0.0" {
		t.Fatal("newerVersion compares numerically")
	}
}
