package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
	"github.com/zclkkk/agy2api/internal/auth"
	"github.com/zclkkk/agy2api/internal/config"
	"golang.org/x/oauth2"
)

func login(parent context.Context, cfg config.Config, client *http.Client, agent *ag.UserAgent, input io.Reader, output io.Writer) error {
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	defer cancel()
	oauth := auth.OAuthConfig()
	verifier := oauth2.GenerateVerifier()
	link := oauth.AuthCodeURL(ag.ID("state_"), oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("prompt", "consent"), oauth2.S256ChallengeOption(verifier))
	if _, err := fmt.Fprintln(output, "Open this URL in your browser:\n\n"+link+"\n\nAfter signing in, copy the authorization code from the Antigravity page and paste it here, then press Enter:"); err != nil {
		return err
	}
	code, err := readAuthorizationCode(ctx, input)
	if err != nil {
		return err
	}
	// The hosted page returns only the code to the user, not state. Google's
	// token endpoint checks the PKCE verifier to bind it to this login attempt.
	ctx = context.WithValue(ctx, oauth2.HTTPClient, client)
	token, err := oauth.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("OAuth token exchange failed; run agy2api login again and use the code from its new link")
	}
	upstream := &ag.Client{HTTP: client, BaseURL: upstreamURL, UserAgent: agent.String}
	project, err := upstream.DiscoverProject(ctx, token.AccessToken)
	if err != nil {
		return err
	}
	if err := auth.Save(cfg.CredentialsPath(), auth.Credentials{Token: *token, ProjectID: project}); err != nil {
		return err
	}
	_, err = fmt.Fprintln(output, "Login saved. Start agy2api serve with the same --config option.")
	return err
}

func readAuthorizationCode(ctx context.Context, input io.Reader) (string, error) {
	type result struct {
		code string
		err  error
	}
	// Stdin may block while the user authenticates. A buffered result lets the
	// reader finish if the CLI has already returned on timeout or cancellation.
	ready := make(chan result, 1)
	go func() {
		scanner := bufio.NewScanner(input)
		scanner.Buffer(make([]byte, 4096), 16<<10)
		if !scanner.Scan() {
			err := errors.New("no authorization code received")
			if scanner.Err() != nil {
				err = errors.New("could not read authorization code (maximum 16 KiB)")
			}
			ready <- result{err: err}
			return
		}
		code := strings.TrimSpace(scanner.Text())
		switch {
		case code == "":
			ready <- result{err: errors.New("authorization code must not be empty")}
		case strings.Contains(code, "://"):
			ready <- result{err: errors.New("paste only the authorization code from the Antigravity page, not a URL")}
		case strings.ContainsFunc(code, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }):
			ready <- result{err: errors.New("authorization code must be a single value without whitespace")}
		default:
			ready <- result{code: code}
		}
	}()
	select {
	case value := <-ready:
		return value.code, value.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
