package main

import (
	"context"
	"testing"
)

func TestHelpAndVersionDoNotLoadConfiguration(t *testing.T) {
	t.Setenv("AGY2API_LISTEN", "invalid")
	for _, args := range [][]string{
		nil, {"-h"}, {"--help"}, {"help"}, {"help", "login"},
		{"help", "serve"}, {"help", "models"}, {"help", "version"},
		{"login", "--help"}, {"serve", "-h"}, {"models", "--help"},
		{"version"}, {"--version"}, {"version", "--help"},
	} {
		if err := run(context.Background(), args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
}

func TestCLIRejectsUnexpectedArguments(t *testing.T) {
	for _, args := range [][]string{
		{"help", "unknown"}, {"help", "serve", "extra"}, {"--help", "extra"},
		{"--version", "extra"}, {"version", "extra"}, {"version", "--unknown"},
		{"version", "--config", "config.toml"}, {"serve", "extra"},
	} {
		if err := run(context.Background(), args); err == nil {
			t.Fatalf("unexpected arguments accepted: %v", args)
		}
	}
}
