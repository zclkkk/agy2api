package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
	"github.com/zclkkk/agy2api/internal/auth"
	"github.com/zclkkk/agy2api/internal/config"
	"golang.org/x/oauth2"
)

type loginTransport func(*http.Request) (*http.Response, error)

func (f loginTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func loginURL(t *testing.T, output string) *url.URL {
	t.Helper()
	for line := range strings.SplitSeq(output, "\n") {
		if strings.HasPrefix(line, "https://accounts.google.com/") {
			u, err := url.Parse(line)
			if err != nil {
				t.Fatal(err)
			}
			return u
		}
	}
	t.Fatal("authorization URL missing from prompt")
	return nil
}

func TestLoginCodeExchangeAndRefresh(t *testing.T) {
	for _, failure := range []string{"", "exchange", "project", "refresh token"} {
		t.Run("failure="+failure, func(t *testing.T) {
			cfg := config.Default()
			cfg.DataDir = t.TempDir()
			if err := auth.Save(cfg.CredentialsPath(), auth.Credentials{ProjectID: "old-project", Token: oauth2.Token{RefreshToken: "old-refresh"}}); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(cfg.CredentialsPath())
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			exchanges, projects, refreshes := 0, 0, 0
			client := &http.Client{Transport: loginTransport(func(r *http.Request) (*http.Response, error) {
				status, body := 200, ""
				switch r.URL.Host {
				case "oauth2.googleapis.com":
					if r.Method != http.MethodPost || r.URL.Path != "/token" {
						t.Fatal("unexpected token request")
					}
					if err := r.ParseForm(); err != nil {
						t.Fatal(err)
					}
					if r.Form.Get("client_id") != auth.ClientID {
						t.Fatal("wrong OAuth client")
					}
					if r.Form.Get("grant_type") == "refresh_token" {
						refreshes++
						if r.Form.Get("refresh_token") != "fixture-refresh" {
							t.Fatal("new login did not provide a reusable refresh token")
						}
						body = `{"access_token":"fixture-refreshed","expires_in":3600,"token_type":"Bearer"}`
						break
					}
					exchanges++
					q := loginURL(t, output.String()).Query()
					if q.Get("client_id") != auth.ClientID || q.Get("redirect_uri") != auth.RedirectURL || q.Get("response_type") != "code" || q.Get("access_type") != "offline" || q.Get("prompt") != "consent" || q.Get("state") == "" {
						t.Fatal("authorization link has incorrect parameters")
					}
					verifier := r.Form.Get("code_verifier")
					sum := sha256.Sum256([]byte(verifier))
					if len(verifier) < 43 || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(sum[:]) {
						t.Fatal("exchange is not bound to the authorization link by PKCE")
					}
					if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "4/fixture-code" || r.Form.Get("redirect_uri") != auth.RedirectURL {
						t.Fatal("incorrect code exchange")
					}
					body = `{"access_token":"fixture-access","refresh_token":"fixture-refresh","expires_in":3600,"token_type":"Bearer"}`
					if failure == "exchange" {
						status, body = 400, `{"error":"invalid_grant","error_description":"rejected 4/fixture-code"}`
					} else if failure == "refresh token" {
						body = `{"access_token":"fixture-access","expires_in":3600,"token_type":"Bearer"}`
					}
				case "daily-cloudcode-pa.googleapis.com":
					projects++
					if r.URL.Path != "/v1internal:loadCodeAssist" || r.Header.Get("Authorization") != "Bearer fixture-access" {
						t.Fatal("incorrect project discovery")
					}
					body = `{"cloudaicompanionProject":"fixture-project"}`
					if failure == "project" {
						status, body = 403, `{"error":{"message":"denied fixture-access"}}`
					}
				default:
					t.Fatal("unexpected network destination")
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})}
			err = login(context.Background(), cfg, client, ag.NewUserAgent(), strings.NewReader("  4/fixture-code \r\n"), &output)
			if (err != nil) != (failure != "") {
				t.Fatalf("login error: %v", err)
			}
			combined := output.String()
			if err != nil {
				combined += err.Error()
			}
			for _, secret := range []string{"4/fixture-code", "fixture-access", "fixture-refresh"} {
				if strings.Contains(combined, secret) {
					t.Fatal("login echoed a code or token")
				}
			}
			after, readErr := os.ReadFile(cfg.CredentialsPath())
			if readErr != nil {
				t.Fatal(readErr)
			}
			if failure != "" {
				if !bytes.Equal(before, after) || strings.Contains(output.String(), "Login saved") {
					t.Fatal("failed login replaced working credentials or claimed success")
				}
				return
			}
			var saved auth.Credentials
			if err := json.Unmarshal(after, &saved); err != nil {
				t.Fatal(err)
			}
			if saved.ProjectID != "fixture-project" || saved.Token.RefreshToken != "fixture-refresh" || exchanges != 1 || projects != 1 {
				t.Fatal("successful login did not save complete credentials")
			}
			info, err := os.Stat(cfg.CredentialsPath())
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("credentials are not private")
			}
			manager, err := auth.Load(cfg.CredentialsPath(), client)
			if err != nil {
				t.Fatal(err)
			}
			manager.Invalidate(saved.Token.AccessToken)
			if token, err := manager.AccessToken(context.Background()); err != nil || token != "fixture-refreshed" || refreshes != 1 {
				t.Fatalf("new credentials cannot refresh: %v", err)
			}
		})
	}
}

func TestLoginAttemptsHaveSeparatePKCE(t *testing.T) {
	var prior url.Values
	for range 2 {
		var output bytes.Buffer
		if err := login(context.Background(), config.Default(), nil, ag.NewUserAgent(), strings.NewReader(""), &output); err == nil {
			t.Fatal("empty input accepted")
		}
		current := loginURL(t, output.String()).Query()
		if prior != nil && (prior.Get("state") == current.Get("state") || prior.Get("code_challenge") == current.Get("code_challenge")) {
			t.Fatal("login attempts reused session material")
		}
		prior = current
	}
}

func TestReadAuthorizationCodeRejectsInvalidInput(t *testing.T) {
	for _, input := range []string{"", " \n", "http://localhost:51121/oauth-callback?code=secret", "https://antigravity.google/oauth-callback?code=secret", "4/code with spaces", "4/code\x00", strings.Repeat("x", 17<<10)} {
		if _, err := readAuthorizationCode(context.Background(), strings.NewReader(input)); err == nil {
			t.Fatal("invalid code input accepted")
		}
	}
}

func TestReadAuthorizationCodeCancellation(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readAuthorizationCode(ctx, r); !errors.Is(err, context.Canceled) {
		t.Fatalf("blocked on stdin after cancellation: %v", err)
	}
}

func TestLoginRejectsRemovedFlags(t *testing.T) {
	for _, flag := range []string{"--manual", "--port", "--callback-port", "--token"} {
		err := run(context.Background(), []string{"login", flag})
		if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
			t.Fatalf("removed flag %q accepted: %v", flag, err)
		}
	}
}
