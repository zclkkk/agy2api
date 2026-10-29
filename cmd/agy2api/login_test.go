package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/zclkkk/agy2api/internal/account"
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

// A login creates an account for a new Google account and renews the
// credentials of a known one. A failure at any step changes nothing.
func TestLoginCreatesAndRenewsAccounts(t *testing.T) {
	for _, tc := range []struct {
		failure  string
		existing bool
	}{
		{"", false}, {"", true}, {"exchange", true}, {"project", true}, {"user", true}, {"refresh token", true}, {"user", false},
	} {
		t.Run(fmt.Sprintf("failure=%s,existing=%v", tc.failure, tc.existing), func(t *testing.T) {
			cfg := config.Default()
			cfg.DataDir = t.TempDir()
			var before account.Account
			var beforeCreds []byte
			if tc.existing {
				var err error
				before, _, err = account.Login(cfg.DataDir, auth.User{ID: "104", Email: "old@example.com"}, auth.Credentials{ProjectID: "fixture-project", Token: oauth2.Token{RefreshToken: "old-refresh"}})
				if err != nil {
					t.Fatal(err)
				}
				if beforeCreds, err = os.ReadFile(before.CredentialsPath()); err != nil {
					t.Fatal(err)
				}
			}
			var output bytes.Buffer
			exchanges, projects, users, refreshes := 0, 0, 0, 0
			failure := tc.failure
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
				case "www.googleapis.com":
					users++
					if r.URL.String() != auth.UserInfoURL || r.Header.Get("Authorization") != "Bearer fixture-access" {
						t.Fatal("incorrect user lookup")
					}
					body = `{"id":"104","email":"alice@example.com","name":"Alice"}`
					if failure == "user" {
						status, body = 401, `{"error":"invalid_token"}`
					}
				default:
					t.Fatal("unexpected network destination")
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})}
			err := login(context.Background(), cfg, client, ag.NewUserAgent(), strings.NewReader("  4/fixture-code \r\n"), &output)
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
			accounts, listErr := account.List(cfg.DataDir)
			if listErr != nil {
				t.Fatal(listErr)
			}
			if failure != "" {
				if strings.Contains(output.String(), "Logged in") || strings.Contains(output.String(), "Renewed") {
					t.Fatal("failed login claimed success")
				}
				if !tc.existing {
					if len(accounts) != 0 {
						t.Fatal("failed login created an account")
					}
					return
				}
				after, _ := os.ReadFile(before.CredentialsPath())
				if len(accounts) != 1 || accounts[0].Info != before.Info || !bytes.Equal(after, beforeCreds) {
					t.Fatal("failed login changed the account")
				}
				return
			}
			if len(accounts) != 1 || accounts[0].ID != account.ID("104") || accounts[0].Email != "alice@example.com" || accounts[0].Name != "Alice" || exchanges != 1 || projects != 1 || users != 1 {
				t.Fatalf("login stored %+v", accounts)
			}
			a := accounts[0]
			if tc.existing {
				if a.APIKey != before.APIKey || !strings.Contains(output.String(), "Renewed") || strings.Contains(output.String(), a.APIKey) {
					t.Fatalf("renewal changed or printed the key: %s", output.String())
				}
			} else if !strings.Contains(output.String(), "Logged in as alice@example.com (Alice).") || !strings.Contains(output.String(), a.ID) || !strings.Contains(output.String(), a.APIKey) {
				t.Fatalf("new account output: %s", output.String())
			}
			manager, err := auth.Load(a.CredentialsPath(), client)
			if err != nil {
				t.Fatal(err)
			}
			if manager.ProjectID() != "fixture-project" {
				t.Fatal("login did not save the project")
			}
			// The saved access token is still valid; invalidating it forces the
			// refresh token from this login to be used.
			saved, _ := os.ReadFile(a.CredentialsPath())
			if !strings.Contains(string(saved), "fixture-access") {
				t.Fatal("login did not save the new token")
			}
			manager.Invalidate("fixture-access")
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

func TestLoginRejectsUnsupportedFlags(t *testing.T) {
	for _, flag := range []string{"--manual", "--port", "--callback-port", "--token"} {
		err := run(context.Background(), []string{"login", flag})
		if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
			t.Fatalf("unsupported flag %q accepted: %v", flag, err)
		}
	}
}
