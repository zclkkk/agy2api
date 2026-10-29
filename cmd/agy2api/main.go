package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zclkkk/agy2api/internal/account"
	ag "github.com/zclkkk/agy2api/internal/antigravity"
	"github.com/zclkkk/agy2api/internal/auth"
	"github.com/zclkkk/agy2api/internal/config"
	"github.com/zclkkk/agy2api/internal/server"
	"github.com/zclkkk/agy2api/internal/state"
)

var version = "dev"

// Only test builds replace this at link time; deployments use Antigravity.
var upstreamURL = ag.BaseURL

const usage = `agy2api — serves Antigravity accounts as OpenAI and Anthropic APIs

Commands:
  login     [--config path]       add an account, or renew its credentials
  logout    [--config path] ID    remove an account with its key and state
  accounts  [--config path]       list the accounts and their API keys
  quota     [--config path]       show each account's models and quota
  serve     [--config path]       serve every account
  version

APIs: /v1/responses, /v1/messages and /v1/images. Each request carries the
API key of the account it uses. Restart serve after changing accounts.`

var commands = map[string]bool{"login": true, "logout": true, "accounts": true, "quota": true, "serve": true, "version": true}

const noAccounts = "no accounts; run agy2api login to add one"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "agy2api:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "help" && len(args) == 2 {
		if !commands[args[1]] {
			return fmt.Errorf("unknown command %q", args[1])
		}
		return run(ctx, []string{args[1], "--help"})
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		if len(args) > 1 {
			return errors.New("unexpected arguments after help")
		}
		fmt.Println(usage)
		return nil
	}
	if args[0] == "--version" {
		if len(args) != 1 {
			return errors.New("unexpected arguments after --version")
		}
		fmt.Println("agy2api", version)
		return nil
	}
	command := args[0]
	if !commands[command] {
		return fmt.Errorf("unknown command %q", command)
	}
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	var configPath string
	if command != "version" {
		f.StringVar(&configPath, "config", "", "TOML configuration file")
	}
	if err := f.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if command == "logout" && f.NArg() != 1 {
		return errors.New("logout takes one account ID; run agy2api accounts to list them")
	}
	if command != "logout" && f.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if command == "version" {
		fmt.Println("agy2api", version)
		return nil
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	switch command {
	case "logout":
		a, err := account.Remove(cfg.DataDir, f.Arg(0))
		if err != nil {
			return err
		}
		name := a.ID
		if a.Email != "" {
			name += " " + label(a)
		}
		fmt.Printf("Removed account %s. Restart agy2api serve to apply.\n", name)
		return nil
	case "accounts":
		accounts, err := account.List(cfg.DataDir)
		if err != nil {
			return err
		}
		if len(accounts) == 0 {
			fmt.Fprintln(os.Stderr, "agy2api:", noAccounts)
			return nil
		}
		return printAccounts(os.Stdout, accounts, time.Local)
	}
	client, err := ag.NewHTTPClient(cfg.ProxyURL)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	agent := ag.NewUserAgent()
	switch command {
	case "login":
		return login(ctx, cfg, client, agent, os.Stdin, os.Stderr)
	case "quota":
		return quota(ctx, cfg, client, agent)
	}
	return serve(ctx, cfg, client, agent)
}

// upstreamFor is the Antigravity client that acts as the account.
func upstreamFor(a account.Account, client *http.Client, agent *ag.UserAgent) (*ag.Client, error) {
	manager, err := auth.Load(a.CredentialsPath(), client, a.Lock())
	if err != nil {
		return nil, err
	}
	return &ag.Client{HTTP: client, BaseURL: upstreamURL, UserAgent: agent.String, Auth: manager}, nil
}

// openAccount prepares an account for serving. Its state database must belong
// to the project of its credentials.
func openAccount(ctx context.Context, cfg config.Config, a account.Account, client *http.Client, agent *ag.UserAgent) (*server.Account, error) {
	upstream, err := upstreamFor(a, client, agent)
	if err != nil {
		return nil, err
	}
	store, err := state.Open(a.Dir, time.Duration(cfg.StateTTLHours)*time.Hour, state.EntryLimit, cfg.MaxStateBytes)
	if err != nil {
		return nil, err
	}
	if err := store.BindAccount(ctx, upstream.Auth.ProjectID()); err != nil {
		store.Close()
		return nil, err
	}
	return &server.Account{ID: a.ID, Key: a.APIKey, Upstream: upstream, State: store}, nil
}

// serve starts only when every account opens, so a damaged account is
// noticed at startup rather than when its clients fail.
func serve(ctx context.Context, cfg config.Config, client *http.Client, agent *ag.UserAgent) error {
	list, err := account.List(cfg.DataDir)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		return errors.New(noAccounts)
	}
	accounts := make([]*server.Account, 0, len(list))
	defer func() {
		for _, a := range accounts {
			a.State.Close()
		}
	}()
	for _, a := range list {
		hosted, err := openAccount(ctx, cfg, a, client, agent)
		if err != nil {
			return fmt.Errorf("account %s: %w", a.ID, err)
		}
		accounts = append(accounts, hosted)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	api, err := server.New(cfg, accounts, logger)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	go agent.Run(ctx, client, func(version string, err error) {
		if err != nil {
			logger.Warn("Antigravity version check failed", "user_agent", agent.String(), "error", err.Error())
			return
		}
		logger.Info("Antigravity version checked", "latest", version, "user_agent", agent.String())
	})
	srv := &http.Server{Handler: api.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20, BaseContext: func(net.Listener) context.Context { return ctx }}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(listener) }()
	logger.Info("agy2api listening", "address", listener.Addr().String(), "version", version, "accounts", len(accounts))
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
