package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/berkayahi/agentbridge/internal/config"
)

func writeHive(t *testing.T, body string) string {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

const oneRepository = `mode: standalone
default_repository: platform
server:
  listen: 127.0.0.1:8787
providers:
  codex:
    executable: /opt/homebrew/bin/codex
    model: gpt-5.6-terra
repositories:
  platform:
    checkout_path: /Users/keeper/kovan-hive/checkouts/platform
    remote: origin
    base_ref: refs/heads/hive/landing
    verification:
      - argv: ["go", "test", "./..."]
        dir: .
    delivery:
      enabled: true
      allowed_ref: refs/heads/hive/landing
`

func TestAddRepositoryKeepsWhatWasAlreadyThere(t *testing.T) {
	path := writeHive(t, oneRepository)

	err := config.AddRepository(path, "agentbridge", config.RepositoryProfile{
		CheckoutPath: "/Users/keeper/kovan-hive/checkouts/agentbridge",
		Remote:       "origin",
		BaseRef:      "refs/heads/hive/landing",
		Verification: []config.VerificationCommand{{Argv: []string{"go", "test", "./..."}, Dir: "."}},
		Delivery:     config.DeliveryPolicy{Enabled: true, AllowedRef: "refs/heads/hive/landing"},
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("the file must still load: %v", err)
	}
	if len(reloaded.Repositories) != 2 {
		t.Fatalf("both repositories must be present: %+v", reloaded.Repositories)
	}
	// The existing profile has to survive untouched: a keeper adding one
	// repository must not silently lose the verification of another.
	platform := reloaded.Repositories["platform"]
	if platform.CheckoutPath != "/Users/keeper/kovan-hive/checkouts/platform" {
		t.Fatalf("existing checkout changed: %+v", platform)
	}
	if len(platform.Verification) != 1 || platform.Verification[0].Argv[0] != "go" {
		t.Fatalf("existing verification changed: %+v", platform.Verification)
	}
	if reloaded.Providers["codex"].Executable != "/opt/homebrew/bin/codex" {
		t.Fatalf("provider configuration changed: %+v", reloaded.Providers)
	}
}

// Registering over an existing id would silently change where that id points,
// and every task ever dispatched against it resolves through that entry.
func TestAddRepositoryRefusesAnIdThatAlreadyExists(t *testing.T) {
	path := writeHive(t, oneRepository)
	err := config.AddRepository(path, "platform", config.RepositoryProfile{
		CheckoutPath: "/somewhere/else",
		Remote:       "origin",
		BaseRef:      "refs/heads/hive/landing",
	})
	if !errors.Is(err, config.ErrRepositoryExists) {
		t.Fatalf("err = %v, want already configured", err)
	}
}

// A profile can be individually plausible and still make the file invalid, so the
// whole configuration is validated and nothing is written when it would not load.
func TestAddRepositoryLeavesTheFileUntouchedWhenItWouldNotLoad(t *testing.T) {
	path := writeHive(t, oneRepository)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	err = config.AddRepository(path, "broken", config.RepositoryProfile{
		CheckoutPath: "relative/path",
		Remote:       "origin",
		BaseRef:      "refs/heads/main",
	})
	if err == nil {
		t.Fatal("a repository that cannot be validated must be refused")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("a refused registration must not have changed the file")
	}
}

func TestAddRepositoryRefusesARelativeConfigurationPath(t *testing.T) {
	if err := config.AddRepository("config.yaml", "x", config.RepositoryProfile{}); err == nil ||
		!strings.Contains(err.Error(), "absolute") {
		t.Fatalf("err = %v, want an absolute-path refusal", err)
	}
}
