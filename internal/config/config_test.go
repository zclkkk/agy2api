package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSanitizeClientMarkersDefaultsAndExplicitOverride(t *testing.T) {
	t.Setenv("AGY2API_LISTEN", "127.0.0.1:8080")
	t.Setenv("AGY2API_DATA_DIR", t.TempDir())
	t.Setenv("AGY2API_API_KEY", "")
	t.Setenv("AGY2API_PROXY_URL", "")
	if !Default().SanitizeClientMarkers {
		t.Fatal("client marker sanitization must default to enabled")
	}
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"existing config", `{}`, true},
		{"enabled", `{"sanitize_client_markers":true}`, true},
		{"disabled", `{"sanitize_client_markers":false}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.SanitizeClientMarkers != tc.want {
				t.Fatalf("got %v, want %v", cfg.SanitizeClientMarkers, tc.want)
			}
		})
	}
}
