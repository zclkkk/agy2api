package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"AGY2API_LISTEN", "AGY2API_DATA_DIR", "AGY2API_PROXY_URL"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTOMLDefaultsAndOverrides(t *testing.T) {
	clearConfigEnv(t)
	for _, path := range []string{"", writeConfig(t, "# Defaults only\n"), "../../config.example.toml"} {
		got, err := Load(path)
		if err != nil || got != Default() {
			t.Fatalf("defaults from %q: %+v, %v", path, got, err)
		}
	}
	body := `# All supported fields, with comments and readable integers.
listen = "0.0.0.0:9000"
data_dir = './private data'
proxy_url = "socks5h://127.0.0.1:1080"
max_body_bytes = 67_108_864
max_concurrent_requests = 2
state_ttl_hours = 24
max_state_bytes = 134_217_728
sanitize_client_markers = false
`
	want := Config{Listen: "0.0.0.0:9000", DataDir: "./private data", ProxyURL: "socks5h://127.0.0.1:1080", MaxBodyBytes: 64 << 20, MaxConcurrentRequests: 2, StateTTLHours: 24, MaxStateBytes: 128 << 20}
	got, err := Load(writeConfig(t, body))
	if err != nil || got != want {
		t.Fatalf("TOML overrides: %+v, %v", got, err)
	}
	// Every request needs an account's key, so any address may be served.
	if got, err := Load(writeConfig(t, "listen = '0.0.0.0:8080'")); err != nil || got.Listen != "0.0.0.0:8080" {
		t.Fatalf("public listen address: %v", err)
	}
	for _, enabled := range []bool{true, false} {
		value := "true"
		if !enabled {
			value = "false"
		}
		got, err := Load(writeConfig(t, "sanitize_client_markers = "+value))
		if err != nil || got.SanitizeClientMarkers != enabled {
			t.Fatalf("explicit boolean %v: %+v, %v", enabled, got, err)
		}
	}
}

func TestEnvironmentOverridesTOMLIncludingEmptyValues(t *testing.T) {
	clearConfigEnv(t)
	path := writeConfig(t, `listen = "0.0.0.0:9000"
data_dir = "./from-file"
proxy_url = "http://127.0.0.1:8081"
max_concurrent_requests = 2
`)
	t.Setenv("AGY2API_LISTEN", "127.0.0.1:9001")
	t.Setenv("AGY2API_DATA_DIR", "./from-env")
	t.Setenv("AGY2API_PROXY_URL", "socks5://127.0.0.1:1080")
	got, err := Load(path)
	if err != nil || got.Listen != "127.0.0.1:9001" || got.DataDir != "./from-env" || got.ProxyURL != "socks5://127.0.0.1:1080" || got.MaxConcurrentRequests != 2 {
		t.Fatalf("environment precedence: %+v, %v", got, err)
	}
	t.Setenv("AGY2API_PROXY_URL", "")
	got, err = Load(path)
	if err != nil || got.ProxyURL != "" {
		t.Fatalf("empty environment values did not override: %v", err)
	}
	t.Setenv("AGY2API_DATA_DIR", "")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "data_dir") {
		t.Fatalf("effective configuration was not validated: %v", err)
	}
}

func TestInvalidConfigIsRejected(t *testing.T) {
	clearConfigEnv(t)
	for _, tc := range []struct{ name, body, errorText string }{
		{"unknown", "max_concurrent_request = 4", "max_concurrent_request"},
		{"nested", "[server]\nlisten = '127.0.0.1:8000'", "unknown config fields"},
		{"unsupported upstream", "upstream_url = 'http://localhost:8080'", "upstream_url"},
		{"unsupported agent", "user_agent = 'antigravity/2.24.0'", "user_agent"},
		{"unsupported count", "max_state_entries = 10000", "max_state_entries"},
		{"json", `{"listen":"127.0.0.1:8000"}`, "decode config"},
		{"duplicate", "listen = '127.0.0.1:8000'\nlisten = '127.0.0.1:8001'", "line 2"},
		{"type", "max_concurrent_requests = '4'", "line 1"},
		{"syntax", "listen = \"unterminated", "line 1"},
		{"trailing data", "max_concurrent_requests = 2\ninvalid!", "line 2"},
		{"empty data dir", "data_dir = ''", "data_dir"},
		{"removed api key", "api_key = 'key'", "api_key"},
		{"bad port", "listen = '127.0.0.1:65536'", "port"},
		{"small body", "max_body_bytes = 1023", "max_body_bytes"},
		{"large body", "max_body_bytes = 268435457", "max_body_bytes"},
		{"zero concurrency", "max_concurrent_requests = 0", "max_concurrent_requests"},
		{"large concurrency", "max_concurrent_requests = 129", "max_concurrent_requests"},
		{"zero ttl", "state_ttl_hours = 0", "state_ttl_hours"},
		{"overflow ttl", "state_ttl_hours = 2562048", "state_ttl_hours"},
		{"small state", "max_state_bytes = 67108863", "max_state_bytes"},
		{"bad proxy", "proxy_url = 'ftp://localhost'", "proxy_url"},
		{"oversized", "#" + strings.Repeat(" ", 1<<20), "exceeds 1 MiB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.errorText) {
				t.Fatalf("got %v; expected %q", err, tc.errorText)
			}
		})
	}
}

func TestConfigErrorsDoNotPrintSourceExcerpts(t *testing.T) {
	clearConfigEnv(t)
	for _, badLine := range []string{"unknown = 1", "max_concurrent_requests = 'wrong'"} {
		_, err := Load(writeConfig(t, "proxy_url = 'http://user:fixture-secret-not-for-errors@127.0.0.1:8080'\n"+badLine))
		if err == nil || strings.Contains(err.Error(), "fixture-secret-not-for-errors") {
			t.Fatalf("error exposed configuration source: %v", err)
		}
	}
}
