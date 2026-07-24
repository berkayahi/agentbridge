package config_test

import (
	"strings"
	"testing"

	"github.com/berkayahi/agentbridge/internal/config"
)

// A Mac controller that only drives the owner-only local API needs neither a
// Telegram bot nor a Tailscale dashboard allowlist. Requiring them forces an
// operator to invent credentials for adapters they are not running.
func desktopOnlyConfig() config.Config {
	return config.Config{
		Mode:              "standalone",
		DefaultRepository: "first-flight",
		Server: config.ServerConfig{
			Listen: "127.0.0.1:8787",
		},
		Providers: map[string]config.ProviderConfig{
			"codex": {Executable: "/usr/local/bin/codex", Model: "gpt-5.6-terra"},
		},
		Repositories: map[string]config.RepositoryProfile{
			"first-flight": {
				CheckoutPath: "/srv/checkouts/first-flight",
				Remote:       "origin",
				BaseRef:      "refs/heads/staging",
				Verification: []config.VerificationCommand{{Argv: []string{"go", "test", "./..."}, Dir: "."}},
			},
		},
	}
}

func TestStandaloneWithoutTelegramOrTailscaleIsValid(t *testing.T) {
	if err := desktopOnlyConfig().Validate(); err != nil {
		t.Fatalf("desktop-only config must validate, got %v", err)
	}
}

// A half-written Telegram block is a mistake, not a desktop-only install, so it
// must still fail loudly rather than silently dropping remote access.
func TestPartialTelegramStillFails(t *testing.T) {
	cases := map[string]config.TelegramConfig{
		"only paired chat":   {PairedChatID: 4242},
		"only allowed user":  {AllowedUserIDs: []int64{4242}},
		"only private flag":  {PrivateChatOnly: true},
		"users without chat": {PrivateChatOnly: true, AllowedUserIDs: []int64{4242}},
	}
	for name, telegram := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := desktopOnlyConfig()
			cfg.Telegram = telegram
			if err := cfg.Validate(); err == nil {
				t.Fatal("a partially configured Telegram block must fail validation")
			}
		})
	}
}

// A fully configured Telegram controller still requires its Tailscale
// allowlist, because that config does expose the dashboard.
func TestTelegramControllerStillRequiresTailscaleAllowlist(t *testing.T) {
	cfg := desktopOnlyConfig()
	cfg.Telegram = config.TelegramConfig{PrivateChatOnly: true, AllowedUserIDs: []int64{4242}, PairedChatID: 4242}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "allowed_tailscale_identities") {
		t.Fatalf("err = %v, want an allowed_tailscale_identities failure", err)
	}
}

func TestDesktopOnlyModeIsReported(t *testing.T) {
	if !desktopOnlyConfig().DesktopOnly() {
		t.Fatal("a standalone config without Telegram must report DesktopOnly")
	}
	cfg := desktopOnlyConfig()
	cfg.Telegram = config.TelegramConfig{PrivateChatOnly: true, AllowedUserIDs: []int64{4242}, PairedChatID: 4242}
	cfg.Server.AllowedTailscaleIdentities = []string{"operator@example.invalid"}
	if cfg.DesktopOnly() {
		t.Fatal("a Telegram controller must not report DesktopOnly")
	}
}
