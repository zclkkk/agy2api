package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zclkkk/agy2api/internal/account"
	"github.com/zclkkk/agy2api/internal/auth"
	"github.com/zclkkk/agy2api/internal/state"
	"golang.org/x/oauth2"
)

func TestHelpAndVersionDoNotLoadConfiguration(t *testing.T) {
	t.Setenv("AGY2API_LISTEN", "invalid")
	args := [][]string{nil, {"-h"}, {"--help"}, {"help"}, {"version"}, {"--version"}, {"version", "--help"}}
	for command := range commands {
		args = append(args, []string{"help", command}, []string{command, "--help"})
	}
	for _, args := range args {
		if err := run(context.Background(), args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
}

func TestCLIRejectsUnexpectedArguments(t *testing.T) {
	for _, args := range [][]string{
		{"help", "unknown"}, {"help", "serve", "extra"}, {"--help", "extra"},
		{"--version", "extra"}, {"version", "extra"}, {"version", "--unknown"},
		{"version", "--config", "config.toml"}, {"serve", "extra"}, {"login", "extra"},
		{"accounts", "extra"}, {"quota", "extra"}, {"logout"}, {"logout", "a", "b"},
		{"models"}, {"help", "models"},
	} {
		if err := run(context.Background(), args); err == nil {
			t.Fatalf("unexpected arguments accepted: %v", args)
		}
	}
}

// testData is a data directory configured through a config file.
func testData(t *testing.T) (dataDir, configPath string) {
	t.Helper()
	dataDir = t.TempDir()
	configPath = filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(configPath, []byte("listen = '127.0.0.1:0'\ndata_dir = '"+dataDir+"'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return dataDir, configPath
}

func addAccount(t *testing.T, dataDir, userID, email string) account.Account {
	t.Helper()
	a, _, err := account.Login(dataDir, auth.User{ID: userID, Email: email}, auth.Credentials{ProjectID: "project-" + userID, Token: oauth2.Token{AccessToken: "a", RefreshToken: "r", Expiry: time.Now().Add(time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestLogoutRemovesTheAccount(t *testing.T) {
	dataDir, configPath := testData(t)
	a := addAccount(t, dataDir, "104", "a@example.com")
	b := addAccount(t, dataDir, "105", "b@example.com")
	if err := run(context.Background(), []string{"logout", "--config", configPath, a.ID}); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"logout", "--config", configPath, a.ID}); err == nil {
		t.Fatal("removed a missing account")
	}
	accounts, err := account.List(dataDir)
	if err != nil || len(accounts) != 1 || accounts[0].ID != b.ID {
		t.Fatalf("remaining %+v err %v", accounts, err)
	}
}

// serve opens every account before listening; the context is canceled so a
// start that should have failed returns instead of serving.
func TestServeRejectsUnusableAccounts(t *testing.T) {
	for name, tc := range map[string]struct {
		prepare func(t *testing.T, dataDir string)
		want    string
	}{
		"no accounts": {func(*testing.T, string) {}, "no accounts"},
		"damaged account": {func(t *testing.T, dataDir string) {
			a := addAccount(t, dataDir, "104", "a@example.com")
			os.Remove(a.CredentialsPath())
		}, "account " + account.ID("104")},
		"shared key": {func(t *testing.T, dataDir string) {
			a := addAccount(t, dataDir, "104", "a@example.com")
			b := addAccount(t, dataDir, "105", "b@example.com")
			info, _ := os.ReadFile(filepath.Join(b.Dir, "account.json"))
			os.WriteFile(filepath.Join(b.Dir, "account.json"), []byte(strings.Replace(string(info), b.APIKey, a.APIKey, 1)), 0600)
		}, "same API key"},
		"state of another project": {func(t *testing.T, dataDir string) {
			// The state database is bound to the account's project; credentials
			// naming another project cannot use it.
			a := addAccount(t, dataDir, "104", "a@example.com")
			store, err := state.Open(a.Dir, time.Hour, 10, 64<<20)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.BindAccount(context.Background(), "project-104"); err != nil {
				t.Fatal(err)
			}
			store.Close()
			creds, _ := os.ReadFile(a.CredentialsPath())
			os.WriteFile(a.CredentialsPath(), []byte(strings.Replace(string(creds), "project-104", "project-other", 1)), 0600)
		}, "another Antigravity account"},
	} {
		t.Run(name, func(t *testing.T) {
			dataDir, configPath := testData(t)
			tc.prepare(t, dataDir)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := run(ctx, []string{"serve", "--config", configPath})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}
