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

func TestLoadRejectsInvalidTelegramPolicy(t *testing.T) {
	tests := map[string]string{
		"private chats not required": strings.Replace(validYAML, "private_chat_only: true", "private_chat_only: false", 1),
		"empty allowlist":            strings.Replace(validYAML, "allowed_user_ids: [123456789]", "allowed_user_ids: []", 1),
		"zero ID":                    strings.Replace(validYAML, "allowed_user_ids: [123456789]", "allowed_user_ids: [0]", 1),
		"duplicate ID":               strings.Replace(validYAML, "allowed_user_ids: [123456789]", "allowed_user_ids: [123456789, 123456789]", 1),
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
providers:
  codex:
    executable: /usr/local/bin/codex
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
