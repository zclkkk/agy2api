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
	"sort"
	"syscall"
	"time"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
	"github.com/zclkkk/agy2api/internal/auth"
	"github.com/zclkkk/agy2api/internal/config"
	"github.com/zclkkk/agy2api/internal/server"
	"github.com/zclkkk/agy2api/internal/state"
)

var version = "dev"

// Only test builds replace this at link time; deployments use Antigravity.
var upstreamURL = ag.BaseURL

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
		switch args[1] {
		case "login", "serve", "models", "version":
			return run(ctx, []string{args[1], "--help"})
		default:
			return fmt.Errorf("unknown command %q", args[1])
		}
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		if len(args) > 1 {
			return errors.New("unexpected arguments after help")
		}
		fmt.Println("agy2api — single-account Antigravity API\n\nCommands:\n  login   [--config path]\n  serve   [--config path]\n  models  [--config path]\n  version\n\nAPIs: /v1/responses and /v1/messages")
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
	if command != "login" && command != "serve" && command != "models" && command != "version" {
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
	if f.NArg() != 0 {
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
	client, err := ag.NewHTTPClient(cfg.ProxyURL)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	agent := ag.NewUserAgent()
	if command == "login" {
		return login(ctx, cfg, client, agent, os.Stdin, os.Stderr)
	}
	manager, err := auth.Load(cfg.CredentialsPath(), client)
	if err != nil {
		return err
	}
	upstream := &ag.Client{HTTP: client, BaseURL: upstreamURL, UserAgent: agent.String, Auth: manager}
	if command == "models" {
		if _, err := agent.Refresh(ctx, client); err != nil {
			fmt.Fprintln(os.Stderr, "agy2api: using", agent.String()+":", err)
		}
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
	store, err := state.Open(cfg.DataDir, time.Duration(cfg.StateTTLHours)*time.Hour, state.EntryLimit, cfg.MaxStateBytes)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.BindAccount(ctx, manager.ProjectID()); err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	go agent.Run(ctx, client, func(version string, err error) {
		if err != nil {
			logger.Warn("Antigravity version check failed", "user_agent", agent.String(), "error", err.Error())
			return
		}
		logger.Info("Antigravity version checked", "latest", version, "user_agent", agent.String())
	})
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
