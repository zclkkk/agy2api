// Package config defines the single-account service configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

type Config struct {
	Listen                string `toml:"listen"`
	DataDir               string `toml:"data_dir"`
	APIKey                string `toml:"api_key"`
	ProxyURL              string `toml:"proxy_url"`
	MaxBodyBytes          int64  `toml:"max_body_bytes"`
	MaxConcurrentRequests int    `toml:"max_concurrent_requests"`
	StateTTLHours         int    `toml:"state_ttl_hours"`
	MaxStateBytes         int64  `toml:"max_state_bytes"`
	SanitizeClientMarkers bool   `toml:"sanitize_client_markers"`
}

func Default() Config {
	return Config{
		Listen: "127.0.0.1:8080", DataDir: "./data",
		SanitizeClientMarkers: true,
		MaxBodyBytes:          32 << 20, MaxConcurrentRequests: 4,
		StateTTLHours: 168, MaxStateBytes: 1 << 30,
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
		const maxConfigBytes = 1 << 20
		data, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
		if err != nil {
			return c, fmt.Errorf("read config: %w", err)
		}
		if len(data) > maxConfigBytes {
			return c, errors.New("config exceeds 1 MiB")
		}
		if err := toml.NewDecoder(bytes.NewReader(data)).DisallowUnknownFields().Decode(&c); err != nil {
			var unknown *toml.StrictMissingError
			if errors.As(err, &unknown) {
				var keys []string
				for _, field := range unknown.Errors {
					keys = append(keys, strings.Join(field.Key(), "."))
				}
				return c, fmt.Errorf("unknown config fields: %s", strings.Join(keys, ", "))
			}
			var decode *toml.DecodeError
			if errors.As(err, &decode) {
				row, column := decode.Position()
				// Do not print the contextual source excerpt: it may contain a key.
				return c, fmt.Errorf("decode config at line %d, column %d: %w", row, column, err)
			}
			return c, fmt.Errorf("decode config: %w", err)
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
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("listen must be host:port: %w", err)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return errors.New("listen port must be between 0 and 65535")
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
	const maxTTLHours = int64(1<<63-1) / int64(time.Hour)
	if c.StateTTLHours < 1 || int64(c.StateTTLHours) > maxTTLHours {
		return fmt.Errorf("state_ttl_hours must be between 1 and %d", maxTTLHours)
	}
	if c.MaxStateBytes < 64<<20 {
		return errors.New("max_state_bytes must be at least 67108864")
	}
	if c.ProxyURL != "" {
		p, err := url.Parse(c.ProxyURL)
		if err != nil || p.Host == "" || (p.Scheme != "http" && p.Scheme != "https" && p.Scheme != "socks5" && p.Scheme != "socks5h") {
			return errors.New("invalid proxy_url")
		}
	}
	return nil
}

func (c Config) CredentialsPath() string { return filepath.Join(c.DataDir, "credentials.json") }
