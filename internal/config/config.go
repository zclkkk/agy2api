// Package config defines the single-account service configuration.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	Listen                string `json:"listen"`
	DataDir               string `json:"data_dir"`
	APIKey                string `json:"api_key"`
	UpstreamURL           string `json:"upstream_url"`
	UserAgent             string `json:"user_agent"`
	ProxyURL              string `json:"proxy_url"`
	SanitizeClientMarkers bool   `json:"sanitize_client_markers"`
	MaxBodyBytes          int64  `json:"max_body_bytes"`
	MaxConcurrentRequests int    `json:"max_concurrent_requests"`
	StateTTLHours         int    `json:"state_ttl_hours"`
	MaxStateEntries       int    `json:"max_state_entries"`
	MaxStateBytes         int64  `json:"max_state_bytes"`
}

func Default() Config {
	return Config{
		Listen: "127.0.0.1:8080", DataDir: "./data",
		UpstreamURL:           "https://daily-cloudcode-pa.googleapis.com",
		SanitizeClientMarkers: true,
		MaxBodyBytes:          32 << 20, MaxConcurrentRequests: 4,
		StateTTLHours: 168, MaxStateEntries: 10000, MaxStateBytes: 1 << 30,
	}
}

func Load(path string) (Config, error) {
	c := Default()
	if path != "" {
		f, err := os.Open(path)
		if err != nil {
			return c, fmt.Errorf("open config: %w", err)
		}
		defer f.Close()
		d := json.NewDecoder(io.LimitReader(f, 1<<20))
		d.DisallowUnknownFields()
		if err := d.Decode(&c); err != nil {
			return c, fmt.Errorf("decode config: %w", err)
		}
		if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
			return c, errors.New("config must contain one JSON object")
		}
	}
	for _, v := range []struct {
		name string
		dst  *string
	}{{"AGY2API_LISTEN", &c.Listen}, {"AGY2API_DATA_DIR", &c.DataDir}, {"AGY2API_API_KEY", &c.APIKey}, {"AGY2API_PROXY_URL", &c.ProxyURL}} {
		if s, ok := os.LookupEnv(v.name); ok {
			*v.dst = s
		}
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("listen must be host:port: %w", err)
	}
	ip := net.ParseIP(host)
	if c.APIKey == "" && host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("api_key is required when listening outside loopback")
	}
	if strings.TrimSpace(c.DataDir) == "" {
		return errors.New("data_dir must not be empty")
	}
	if c.MaxBodyBytes < 1024 || c.MaxBodyBytes > 256<<20 {
		return errors.New("max_body_bytes must be between 1024 and 268435456")
	}
	if c.MaxConcurrentRequests < 1 || c.MaxConcurrentRequests > 128 {
		return errors.New("max_concurrent_requests must be between 1 and 128")
	}
	if c.StateTTLHours < 1 || c.MaxStateEntries < 1 {
		return errors.New("state retention limits must be positive")
	}
	if c.MaxStateBytes < 64<<20 {
		return errors.New("max_state_bytes must be at least 67108864")
	}
	u, err := url.Parse(c.UpstreamURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("invalid upstream_url")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname())) {
		return errors.New("upstream_url requires HTTPS (HTTP allowed for local tests)")
	}
	if c.ProxyURL != "" {
		p, err := url.Parse(c.ProxyURL)
		if err != nil || p.Host == "" || (p.Scheme != "http" && p.Scheme != "https" && p.Scheme != "socks5" && p.Scheme != "socks5h") {
			return errors.New("invalid proxy_url")
		}
	}
	// An empty user_agent follows the latest Antigravity release.
	if strings.ContainsAny(c.UserAgent, "\r\n") {
		return errors.New("invalid user_agent")
	}
	return nil
}

func isLoopback(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || ip != nil && ip.IsLoopback()
}
func (c Config) CredentialsPath() string { return filepath.Join(c.DataDir, "credentials.json") }
