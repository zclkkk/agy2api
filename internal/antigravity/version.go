package antigravity

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// FallbackVersion is reported until the release manifest has been read.
	// Antigravity serves its newest models only to recent client versions.
	FallbackVersion = "2.24.0"
	versionRefresh  = 6 * time.Hour
)

// Test builds replace this at link time to keep SDK validation offline.
var manifestURL = "https://antigravity-hub-auto-updater-974169037036.us-central1.run.app/manifest/latest-arm64-mac.yml"

var semver = regexp.MustCompile(`^\d{1,4}\.\d{1,4}\.\d{1,6}$`)

// UserAgent reports "antigravity/<version>" and keeps the version current
// with the official release manifest, never below the built-in fallback.
type UserAgent struct {
	mu       sync.RWMutex
	latest   string
	manifest string // overrides manifestURL in tests
}

func NewUserAgent() *UserAgent { return &UserAgent{} }

// String returns the User-Agent header value.
func (u *UserAgent) String() string {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return "antigravity/" + newerVersion(FallbackVersion, u.latest)
}

// Refresh reads the latest version from the release manifest. On failure the
// previous version stays in use.
func (u *UserAgent) Refresh(ctx context.Context, client *http.Client) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	source := manifestURL
	if u.manifest != "" {
		source = u.manifest
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "electron-builder")
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("read Antigravity release manifest: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Antigravity release manifest returned HTTP %d", resp.StatusCode)
	}
	version, err := manifestVersion(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", err
	}
	u.mu.Lock()
	u.latest = version
	u.mu.Unlock()
	return version, nil
}

// Run refreshes the version now and then periodically until ctx ends.
func (u *UserAgent) Run(ctx context.Context, client *http.Client, report func(version string, err error)) {
	ticker := time.NewTicker(versionRefresh)
	defer ticker.Stop()
	for {
		version, err := u.Refresh(ctx, client)
		if report != nil && ctx.Err() == nil {
			report(version, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// manifestVersion reads the top-level "version:" line of the YAML manifest.
func manifestVersion(r io.Reader) (string, error) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		value, ok := strings.CutPrefix(scanner.Text(), "version:")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if !semver.MatchString(value) {
			return "", errors.New("Antigravity release manifest has an invalid version")
		}
		return value, nil
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", errors.New("Antigravity release manifest has no version")
}

// newerVersion compares dotted numeric versions; an empty version loses.
func newerVersion(a, b string) string {
	if b == "" {
		return a
	}
	if a == "" {
		return b
	}
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			if x > y {
				return a
			}
			return b
		}
	}
	return a
}
