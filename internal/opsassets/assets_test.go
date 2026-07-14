package opsassets

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate operations asset test")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func readAsset(t *testing.T, name string) string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(repositoryRoot(t), filepath.FromSlash(name)))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(contents)
}

func TestServiceIsHardenedAndRestartable(t *testing.T) {
	service := readAsset(t, "deploy/systemd/agentbridge.service")
	required := []string{
		"Restart=always",
		"RestartSec=5s",
		"StartLimitIntervalSec=60s",
		"UMask=0077",
		"NoNewPrivileges=true",
		"PrivateTmp=true",
		"WorkingDirectory=%h/.local/share/agentbridge",
		"ExecStartPre=%h/.local/bin/agentbridge doctor --config %h/.config/agentbridge/config.yaml",
		"AGENTBRIDGE_LISTEN=127.0.0.1:8787",
		"LoadCredential=telegram_bot_token:",
	}
	for _, property := range required {
		if !strings.Contains(service, property) {
			t.Errorf("service is missing %q", property)
		}
	}
	if strings.Contains(service, "claude_oauth_token") {
		t.Error("Claude subscription sessions must remain owned by Claude Code")
	}
}

func TestUserUnitsAvoidPrivateDevicesCapabilityFailure(t *testing.T) {
	for _, asset := range []string{
		"deploy/systemd/agentbridge.service",
		"deploy/systemd/agentbridge-backup.service",
	} {
		unit := readAsset(t, asset)
		if strings.Contains(unit, "PrivateDevices=true") {
			t.Errorf("%s enables PrivateDevices, which fails with status 218/CAPABILITIES in an unprivileged systemd user manager", asset)
		}
	}
}

func TestInstallerUsesPathsReferencedByUnits(t *testing.T) {
	installer := readAsset(t, "deploy/install.sh")
	for _, fragment := range []string{
		"$HOME/.local/bin",
		"$HOME/.local/lib/agentbridge",
		"$HOME/.local/share/agentbridge",
		"$HOME/.cache/agentbridge",
		"$HOME/.config/systemd/user",
	} {
		if !strings.Contains(installer, fragment) {
			t.Errorf("installer does not provision unit path %q", fragment)
		}
	}
}

func TestAuthRecoveryIsInteractiveAndDashboardSupervised(t *testing.T) {
	installer := readAsset(t, "deploy/install.sh")
	credentialGuide := readAsset(t, "examples/credentials/README.md")
	for name, contents := range map[string]string{
		"installer":        installer,
		"credential guide": credentialGuide,
	} {
		if strings.Contains(contents, "claude_oauth_token") {
			t.Errorf("%s must not copy Claude subscription sessions into AgentBridge credentials", name)
		}
	}

	guide := readAsset(t, "docs/auth-recovery.md")
	for _, fragment := range []string{
		"Tailscale-only dashboard",
		"codex login --device-auth",
		"claude auth login --claudeai",
		"CLI owns",
	} {
		if !strings.Contains(guide, fragment) {
			t.Errorf("auth recovery guide is missing %q", fragment)
		}
	}
	if strings.Contains(guide, "setup-token") || strings.Contains(guide, "claude_oauth_token") {
		t.Error("auth recovery guide must not transfer setup tokens into AgentBridge")
	}
}

func TestBackupUsesSQLiteOnlineBackupAndSafeRetention(t *testing.T) {
	script := readAsset(t, "scripts/backup.sh")
	required := []string{
		".backup",
		"PRAGMA integrity_check",
		"EVENT_RETENTION_DAYS:-30",
		"ARTIFACT_RETENTION_DAYS:-7",
		"PINNED_TASKS_FILE",
		"state NOT IN ('queued','preparing','running','awaiting_approval','awaiting_auth','verifying','committing','pushing','paused')",
	}
	for _, fragment := range required {
		if !strings.Contains(script, fragment) {
			t.Errorf("backup and retention script is missing %q", fragment)
		}
	}
	if strings.Contains(script, "cp \"$DATABASE_PATH\"") {
		t.Error("live SQLite database must not be copied directly")
	}
}

func TestPublicAssetsContainNoPrivateDeploymentData(t *testing.T) {
	assets := []string{
		"README.md",
		"CONTRIBUTING.md",
		"CODE_OF_CONDUCT.md",
		"SECURITY.md",
		"deploy/systemd/agentbridge.service",
		"deploy/systemd/agentbridge-backup.service",
		"deploy/systemd/agentbridge-backup.timer",
		"deploy/install.sh",
		"deploy/uninstall.sh",
		"scripts/backup.sh",
		"scripts/restore-check.sh",
		"scripts/pi-smoke.sh",
		"docs/architecture.md",
		"docs/operations.md",
		"docs/auth-recovery.md",
		"docs/upgrade.md",
		"docs/incident-response.md",
		"examples/config.local.yaml",
		"examples/credentials/README.md",
		".github/ISSUE_TEMPLATE/bug.yml",
		".github/ISSUE_TEMPLATE/feature.yml",
		".github/ISSUE_TEMPLATE/config.yml",
		".github/PULL_REQUEST_TEMPLATE.md",
		".github/dependabot.yml",
	}
	forbidden := []string{
		"berkay",
		"ceptemenu",
		"banabi",
		"/users/",
		"/home/",
		"openai_api_key",
		"anthropic_api_key",
		"anthropic_auth_token",
		"tailscale funnel",
	}
	for _, asset := range assets {
		contents := strings.ToLower(readAsset(t, asset))
		for _, fragment := range forbidden {
			if strings.Contains(contents, fragment) {
				t.Errorf("%s contains forbidden public-data fragment %q", asset, fragment)
			}
		}
	}
}

func TestOperationsDocumentsRejectPublicExposureAndProviderAutoUpdate(t *testing.T) {
	operations := readAsset(t, "docs/operations.md")
	if !strings.Contains(operations, "127.0.0.1:8787") {
		t.Error("operations guide must keep the dashboard on loopback")
	}
	if !strings.Contains(operations, "Do not enable public ingress") {
		t.Error("operations guide must reject public ingress")
	}
	upgrade := readAsset(t, "docs/upgrade.md")
	if !strings.Contains(upgrade, "Never auto-update Codex CLI or Claude Code") {
		t.Error("upgrade guide must forbid provider auto-update")
	}
}
