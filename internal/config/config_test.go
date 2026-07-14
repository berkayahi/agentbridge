package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAcceptsGenericSafeProfile(t *testing.T) {
	cfg, err := Load(writeConfig(t, validYAML))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if got := cfg.Repositories["sample-app"].Delivery.AllowedRef; got != "refs/heads/staging" {
		t.Fatalf("allowed ref = %q, want refs/heads/staging", got)
	}
	if got := cfg.Repositories["sample-app"].Verification[0].Argv; len(got) != 3 || got[0] != "go" {
		t.Fatalf("verification argv = %#v", got)
	}
}

func TestLoadRequiresExplicitDefaultForMultipleRepositories(t *testing.T) {
	second := `
  second-app:
    checkout_path: /srv/agentbridge/checkouts/second-app
    remote: origin
    base_ref: refs/heads/staging
    verification:
      - argv: ["go", "test", "./..."]
    delivery:
      enabled: true
      allowed_ref: refs/heads/staging
`
	without := validYAML + second
	assertLoadError(t, without, "default_repository")
	unknown := "default_repository: missing\n" + without
	assertLoadError(t, unknown, "default_repository")
	withDefault := "default_repository: second-app\n" + without
	cfg, err := Load(writeConfig(t, withDefault))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultRepository != "second-app" {
		t.Fatalf("default_repository=%q", cfg.DefaultRepository)
	}
}

func TestLoadRequiresSafeProviderModels(t *testing.T) {
	for name, model := range map[string]string{
		"empty":              "",
		"whitespace":         "gpt 5.6 terra",
		"leading whitespace": " opus",
		"shell separator":    "opus;touch-pwned",
		"substitution":       "$(whoami)",
		"control":            "opus\nnext",
	} {
		t.Run(name, func(t *testing.T) {
			yml := strings.Replace(validYAML, "model: gpt-5.6-terra", "model: \""+model+"\"", 1)
			assertLoadError(t, yml, "model")
		})
	}
}

func TestLoadAcceptsCurrentAndFutureSafeProviderModels(t *testing.T) {
	for _, model := range []string{"gpt-5.6-terra", "gpt-5.6-sol", "gpt-6.1-terra"} {
		t.Run(model, func(t *testing.T) {
			yml := strings.Replace(validYAML, "gpt-5.6-terra", model, 1)
			if _, err := Load(writeConfig(t, yml)); err != nil {
				t.Fatalf("Load() error = %v", err)
			}
		})
	}
}

func TestLoadRejectsCodexModelsBelowTerraFloor(t *testing.T) {
	for _, model := range []string{"gpt-5.5-terra", "gpt-5.6-mini", "gpt-4.1", "opus"} {
		t.Run(model, func(t *testing.T) {
			yml := strings.Replace(validYAML, "gpt-5.6-terra", model, 1)
			assertLoadError(t, yml, "GPT-5.6 Terra")
		})
	}
}

func TestLoadRejectsUnknownYAMLField(t *testing.T) {
	yml := strings.Replace(validYAML, "listen: 127.0.0.1:8787", "listen: 127.0.0.1:8787\n  unexpected: true", 1)
	assertLoadError(t, yml, "unknown")
}

func TestLoadRejectsShellStringVerificationCommand(t *testing.T) {
	yml := strings.Replace(validYAML, "argv: [\"go\", \"test\", \"./...\"]", "argv: \"go test ./...\"", 1)
	assertLoadError(t, yml, "argv")
}

func TestLoadRejectsNonLoopbackServer(t *testing.T) {
	for _, address := range []string{"0.0.0.0:8787", "192.168.1.8:8787", ":8787"} {
		t.Run(address, func(t *testing.T) {
			yml := strings.Replace(validYAML, "127.0.0.1:8787", address, 1)
			assertLoadError(t, yml, "loopback")
		})
	}
}

func TestLoadAcceptsSupportedLoopbackServers(t *testing.T) {
	for _, address := range []string{"localhost:8787", `"[::1]:8787"`} {
		t.Run(address, func(t *testing.T) {
			yml := strings.Replace(validYAML, "127.0.0.1:8787", address, 1)
			if _, err := Load(writeConfig(t, yml)); err != nil {
				t.Fatalf("Load() error = %v", err)
			}
		})
	}
}

func TestLoadRejectsInvalidTailscaleIdentityAllowlist(t *testing.T) {
	tests := map[string]string{
		"surrounding whitespace": strings.Replace(validYAML, "    - operator@example.invalid", `    - " operator@example.invalid "`, 1),
		"duplicate identity": strings.Replace(
			validYAML,
			"    - operator@example.invalid",
			"    - operator@example.invalid\n    - operator@example.invalid",
			1,
		),
	}

	for name, yml := range tests {
		t.Run(name, func(t *testing.T) {
			assertLoadError(t, yml, "allowed_tailscale_identities")
		})
	}
}

func TestLoadRejectsInvalidTelegramPolicy(t *testing.T) {
	tests := map[string]string{
		"private chats not required": strings.Replace(validYAML, "private_chat_only: true", "private_chat_only: false", 1),
		"empty allowlist":            strings.Replace(validYAML, "allowed_user_ids: [123456789]", "allowed_user_ids: []", 1),
		"zero ID":                    strings.Replace(validYAML, "allowed_user_ids: [123456789]", "allowed_user_ids: [0]", 1),
		"duplicate ID":               strings.Replace(validYAML, "allowed_user_ids: [123456789]", "allowed_user_ids: [123456789, 123456789]", 1),
		"multiple operators":         strings.Replace(validYAML, "allowed_user_ids: [123456789]", "allowed_user_ids: [123456789, 987654321]", 1),
		"missing paired chat":        strings.Replace(validYAML, "paired_chat_id: 123456789", "paired_chat_id: 0", 1),
	}

	for name, yml := range tests {
		t.Run(name, func(t *testing.T) {
			assertLoadError(t, yml, "telegram")
		})
	}
}

func TestLoadRejectsUnsafeDeliveryRefs(t *testing.T) {
	for _, ref := range []string{
		"refs/heads/main",
		"refs/heads/master",
		"refs/heads/production",
		"refs/tags/v1.0.0",
		"staging",
	} {
		t.Run(ref, func(t *testing.T) {
			yml := strings.Replace(validYAML, "allowed_ref: refs/heads/staging", "allowed_ref: "+ref, 1)
			assertLoadError(t, yml, "allowed_ref")
		})
	}
}

func TestDeliveryRejectsInvalidGitBranchRefs(t *testing.T) {
	refs := []string{
		"refs/heads/",
		"refs/heads//feature",
		"refs/heads/feature/",
		"refs/heads/.feature",
		"refs/heads/feature/.hidden",
		"refs/heads/feature.lock",
		"refs/heads/feature.",
		"refs/heads/feature branch",
		"refs/heads/feature~1",
		"refs/heads/feature^2",
		"refs/heads/feature:one",
		"refs/heads/feature?one",
		"refs/heads/feature*one",
		"refs/heads/feature[one",
		`refs/heads/feature\one`,
		"refs/heads/feature..one",
		"refs/heads/feature@{one",
		"refs/heads/feature\nnewline",
		"refs/heads/feature\x01control",
		"refs/heads/feature\x7fdelete",
		"refs/tags/v1.0.0",
		"feature",
	}

	for _, ref := range refs {
		t.Run(strings.ReplaceAll(ref, "/", "_"), func(t *testing.T) {
			err := (DeliveryPolicy{Enabled: true, AllowedRef: ref}).validate()
			if err == nil {
				t.Fatalf("delivery ref %q accepted, want rejection", ref)
			}
		})
	}
}

func TestLoadAllowsDeliveryDisabledWithoutRef(t *testing.T) {
	yml := strings.Replace(validYAML, "enabled: true\n      allowed_ref: refs/heads/staging", "enabled: false", 1)
	cfg, err := Load(writeConfig(t, yml))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Repositories["sample-app"].Delivery.Enabled {
		t.Fatal("delivery enabled, want disabled")
	}
}

func TestLoadRejectsInvalidVerificationWorkingDirectory(t *testing.T) {
	for _, dir := range []string{"../outside", "/srv/sample-app", "sub/../../outside"} {
		t.Run(dir, func(t *testing.T) {
			yml := strings.Replace(validYAML, "dir: .", "dir: "+dir, 1)
			assertLoadError(t, yml, "dir")
		})
	}
}

func TestLoadRejectsRelativePathsAndInvalidProfileNames(t *testing.T) {
	tests := map[string]struct {
		yml  string
		want string
	}{
		"provider executable": {strings.Replace(validYAML, "/usr/local/bin/codex", "codex", 1), "absolute"},
		"checkout path":       {strings.Replace(validYAML, "/srv/agentbridge/checkouts/sample-app", "checkouts/sample-app", 1), "absolute"},
		"profile name":        {strings.Replace(validYAML, "sample-app:", "bad profile!:", 1), "profile name"},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			assertLoadError(t, test.yml, test.want)
		})
	}
}

func TestLoadRejectsInvalidDeploymentURL(t *testing.T) {
	yml := strings.Replace(validYAML, "https://deploy.example.invalid/sample-app", "file:///srv/sample-app", 1)
	assertLoadError(t, yml, "deployment_url")
}

func TestRejectAPIKeyEnvironment(t *testing.T) {
	for _, name := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"} {
		t.Run(name, func(t *testing.T) {
			for _, key := range forbiddenAPIKeyEnvironment {
				t.Setenv(key, "")
			}
			t.Setenv(name, "not-a-real-secret")
			if err := RejectAPIKeyEnvironment(); err == nil || strings.Contains(err.Error(), "not-a-real-secret") {
				t.Fatalf("RejectAPIKeyEnvironment() error = %v", err)
			}
		})
	}
}

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertLoadError(t *testing.T, yml, contains string) {
	t.Helper()
	_, err := Load(writeConfig(t, yml))
	if err == nil {
		t.Fatal("Load() error = nil, want validation error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(contains)) {
		t.Fatalf("Load() error = %q, want it to contain %q", err, contains)
	}
}

const validYAML = `server:
  listen: 127.0.0.1:8787
  allowed_tailscale_identities:
    - operator@example.invalid
telegram:
  private_chat_only: true
  allowed_user_ids: [123456789]
  paired_chat_id: 123456789
providers:
  codex:
    executable: /usr/local/bin/codex
    model: gpt-5.6-terra
repositories:
  sample-app:
    checkout_path: /srv/agentbridge/checkouts/sample-app
    remote: origin
    base_ref: refs/heads/staging
    verification:
      - argv: ["go", "test", "./..."]
        dir: .
    deployment_url: https://deploy.example.invalid/sample-app
    delivery:
      enabled: true
      allowed_ref: refs/heads/staging
`
