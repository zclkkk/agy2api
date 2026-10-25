package main

import (
	"bufio"
	"context"
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
	"github.com/zclkkk/agy2api/internal/auth"
	"github.com/zclkkk/agy2api/internal/config"
	"github.com/zclkkk/agy2api/internal/server"
	"github.com/zclkkk/agy2api/internal/state"
	"golang.org/x/oauth2"
)

var version = "dev"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "agy2api:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Println("agy2api — single-account Antigravity API\n\nCommands:\n  login   [--config path] [--manual] [--port 51121]\n  serve   [--config path]\n  models  [--config path]\n  version\n\nAPIs: /v1/responses and /v1/messages")
		return nil
	}
	if args[0] == "version" || args[0] == "--version" {
		fmt.Println("agy2api", version)
		return nil
	}
	command := args[0]
	if command != "login" && command != "serve" && command != "models" {
		return fmt.Errorf("unknown command %q", command)
	}
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	configPath := f.String("config", "", "JSON configuration file")
	var manual bool
	var port int
	if command == "login" {
		f.BoolVar(&manual, "manual", false, "paste the full callback URL instead of starting a local callback listener")
		f.IntVar(&port, "port", 51121, "OAuth loopback callback port")
	}
	if err := f.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	client, err := ag.NewHTTPClient(cfg.ProxyURL)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	if command == "login" {
		return login(ctx, cfg, client, manual, port)
	}
	manager, err := auth.Load(cfg.CredentialsPath(), client)
	if err != nil {
		return err
	}
	upstream := &ag.Client{HTTP: client, BaseURL: cfg.UpstreamURL, UserAgent: cfg.UserAgent, Auth: manager}
	if command == "models" {
		catalog, err := upstream.Models(ctx)
		if err != nil {
			return err
		}
		ids := make([]string, 0, len(catalog.Models))
		for id := range catalog.Models {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			fmt.Println(id)
		}
		return nil
	}
	store, err := state.Open(cfg.DataDir, time.Duration(cfg.StateTTLHours)*time.Hour, cfg.MaxStateEntries, cfg.MaxStateBytes)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.BindAccount(ctx, manager.ProjectID()); err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	api := server.New(cfg, upstream, store, logger)
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	srv := &http.Server{Handler: api.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20, BaseContext: func(net.Listener) context.Context { return ctx }}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(listener) }()
	logger.Info("agy2api listening", "address", listener.Addr().String(), "version", version)
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			_ = srv.Close()
			return err
		}
		return nil
	}
}

type callbackResult struct {
	code string
	err  error
}

func login(parent context.Context, cfg config.Config, client *http.Client, manual bool, port int) error {
	if port < 1 || port > 65535 {
		return errors.New("port must be between 1 and 65535")
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	defer cancel()
	redirect := fmt.Sprintf("http://localhost:%d/oauth-callback", port)
	oauth := auth.OAuthConfig(redirect)
	stateValue := ag.ID("state_")
	verifier := oauth2.GenerateVerifier()
	link := oauth.AuthCodeURL(stateValue, oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("prompt", "consent"), oauth2.S256ChallengeOption(verifier))
	result := make(chan callbackResult, 1)
	accept := func(raw string) (string, error) {
		u, err := url.Parse(raw)
		if err != nil {
			return "", errors.New("invalid callback URL")
		}
		if u.Scheme != "http" || u.Host != fmt.Sprintf("localhost:%d", port) || u.Path != "/oauth-callback" {
			return "", errors.New("callback URL does not match this login")
		}
		if subtle.ConstantTimeCompare([]byte(u.Query().Get("state")), []byte(stateValue)) != 1 {
			return "", errors.New("OAuth state mismatch")
		}
		if u.Query().Get("error") != "" {
			return "", errors.New("OAuth authorization was denied")
		}
		code := u.Query().Get("code")
		if code == "" {
			return "", errors.New("callback has no authorization code")
		}
		return code, nil
	}
	if !manual {
		listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			return fmt.Errorf("OAuth callback listener: %w (use --manual on a remote machine)", err)
		}
		mux := http.NewServeMux()
		mux.HandleFunc("GET /oauth-callback", func(w http.ResponseWriter, r *http.Request) {
			code, err := accept(redirect + "?" + r.URL.RawQuery)
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			select {
			case result <- callbackResult{code: code}:
				fmt.Fprintln(w, "Authorization received. You may close this tab.")
			default:
				http.Error(w, "Authorization already received", 409)
			}
		})
		srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			err := srv.Serve(listener)
			if !errors.Is(err, http.ErrServerClosed) {
				select {
				case result <- callbackResult{err: err}:
				default:
				}
			}
		}()
		defer srv.Close()
	}
	fmt.Fprintln(os.Stderr, "Open this URL in your browser:\n\n"+link+"\n")
	if manual {
		fmt.Fprintln(os.Stderr, "After authorization, paste the complete localhost callback URL here (the browser may show a connection error):")
		go func() {
			scanner := bufio.NewScanner(os.Stdin)
			scanner.Buffer(make([]byte, 4096), 16<<10)
			if !scanner.Scan() {
				result <- callbackResult{err: errors.New("no callback URL received")}
				return
			}
			code, err := accept(strings.TrimSpace(scanner.Text()))
			result <- callbackResult{code: code, err: err}
		}()
	}
	var callback callbackResult
	select {
	case callback = <-result:
	case <-ctx.Done():
		return ctx.Err()
	}
	if callback.err != nil {
		return callback.err
	}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, client)
	token, err := oauth.Exchange(ctx, callback.code, oauth2.VerifierOption(verifier))
	if err != nil {
		return errors.New("OAuth token exchange failed; verify the account and try again")
	}
	upstream := &ag.Client{HTTP: client, BaseURL: cfg.UpstreamURL, UserAgent: cfg.UserAgent}
	project, err := upstream.DiscoverProject(ctx, token.AccessToken)
	if err != nil {
		return err
	}
	if err := auth.Save(cfg.CredentialsPath(), auth.Credentials{Token: *token, ProjectID: project}); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Login saved. Start the service with agy2api serve.")
	return nil
}
