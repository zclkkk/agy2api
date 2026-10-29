// Package auth manages one Antigravity OAuth identity.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/zclkkk/agy2api/internal/securefile"
	"golang.org/x/oauth2"
)

// These identify the installed Antigravity OAuth application, not a user account.
const ClientID = "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"
const clientSecret = "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"

// RedirectURL hosts Google's authorization-code handoff page. No local callback
// listener is needed; the user copies the code into the process that began PKCE.
const RedirectURL = "https://antigravity.google/oauth-callback"

func OAuthConfig() oauth2.Config {
	return oauth2.Config{ClientID: ClientID, ClientSecret: clientSecret, RedirectURL: RedirectURL,
		Endpoint: oauth2.Endpoint{AuthURL: "https://accounts.google.com/o/oauth2/v2/auth", TokenURL: "https://oauth2.googleapis.com/token", AuthStyle: oauth2.AuthStyleInParams},
		Scopes:   []string{"https://www.googleapis.com/auth/cloud-platform", "https://www.googleapis.com/auth/userinfo.email", "https://www.googleapis.com/auth/userinfo.profile", "https://www.googleapis.com/auth/cclog", "https://www.googleapis.com/auth/experimentsandconfigs"}}
}

type Credentials struct {
	Token     oauth2.Token `json:"token"`
	ProjectID string       `json:"project_id"`
}

// UserInfoURL returns the signed-in Google account; the login scopes include
// its email and profile.
const UserInfoURL = "https://www.googleapis.com/oauth2/v2/userinfo"

// User is the Google account behind a login. ID is stable for the account;
// Email and Name are for display.
type User struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

func FetchUser(ctx context.Context, client *http.Client, accessToken string) (User, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, UserInfoURL, nil)
	if err != nil {
		return User{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	res, err := client.Do(req)
	if err != nil {
		return User{}, errors.New("could not read the Google account; check connectivity and run agy2api login again")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return User{}, fmt.Errorf("Google account lookup failed with HTTP %d", res.StatusCode)
	}
	var user User
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&user); err != nil || user.ID == "" || user.Email == "" {
		return User{}, errors.New("Google did not return the account ID and email")
	}
	return user, nil
}

func Save(path string, c Credentials) error {
	if c.Token.RefreshToken == "" {
		return errors.New("OAuth did not return a refresh token; sign in again with consent")
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return securefile.Write(path, data)
}

type Manager struct {
	mu     sync.Mutex
	path   string
	creds  Credentials
	client *http.Client
	config oauth2.Config
}

func Load(path string, client *http.Client) (*Manager, error) {
	creds, err := read(path)
	if err != nil {
		return nil, err
	}
	return &Manager{path: path, creds: creds, client: client, config: OAuthConfig()}, nil
}

func read(path string) (Credentials, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Credentials{}, fmt.Errorf("read credentials (run agy2api login first): %w", err)
	}
	var creds Credentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return Credentials{}, fmt.Errorf("decode credentials: %w", err)
	}
	if creds.Token.RefreshToken == "" || creds.ProjectID == "" {
		return Credentials{}, errors.New("credentials are incomplete; run agy2api login")
	}
	return creds, nil
}

func valid(t oauth2.Token) bool {
	return t.AccessToken != "" && !t.Expiry.IsZero() && time.Until(t.Expiry) > time.Minute
}

func (m *Manager) ProjectID() string { m.mu.Lock(); defer m.mu.Unlock(); return m.creds.ProjectID }

// Invalidate forces the next AccessToken call to refresh when token is still
// the cached access token, e.g. after the upstream rejected it with HTTP 401.
func (m *Manager) Invalidate(token string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.creds.Token.AccessToken == token {
		m.creds.Token.Expiry = time.Time{}
	}
}

// AccessToken returns a valid access token, refreshing it when needed.
//
// The credentials file is the authority and the manager's copy a cache: the
// file can change while the service runs, since login and logout do not wait
// for it. Before a refresh the manager rereads the file and adopts credentials
// a later login wrote, and it writes the refreshed token back only while the
// file still exists. An account removed while it is served keeps working from
// memory until the service restarts, and its directory is never recreated.
func (m *Manager) AccessToken(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if valid(m.creds.Token) {
		return m.creds.Token.AccessToken, nil
	}
	if stored, err := read(m.path); err == nil && stored.Token.RefreshToken != m.creds.Token.RefreshToken {
		m.creds = stored
		if valid(m.creds.Token) {
			return m.creds.Token.AccessToken, nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, oauth2.HTTPClient, m.client)
	previous := m.creds.Token
	expired := previous
	expired.Expiry = time.Now().Add(-time.Hour)
	token, err := m.config.TokenSource(ctx, &expired).Token()
	if err != nil {
		return "", errors.New("OAuth refresh failed; check connectivity or run agy2api login again")
	}
	if token.RefreshToken == "" {
		token.RefreshToken = previous.RefreshToken
	}
	next := m.creds
	next.Token = *token
	// The directory of a removed account is gone: the write fails on it and
	// is skipped, without recreating it.
	if err := Save(m.path, next); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("persist refreshed token: %w", err)
	}
	m.creds = next
	return token.AccessToken, nil
}
