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

func TestHeadlessDeviceServiceHasNoControllerTransports(t *testing.T) {
	service := readAsset(t, "deploy/systemd/agentbridge-device-agent.service")
	for _, property := range []string{
		"Restart=always",
		"ExecStartPre=%h/.local/bin/agentbridge doctor --config %h/.config/agentbridge/config.yaml",
		"ExecStart=%h/.local/bin/agentbridge serve --config %h/.config/agentbridge/config.yaml",
		"AGENTBRIDGE_DATA_DIR=%h/.local/share/agentbridge",
	} {
		if !strings.Contains(service, property) {
			t.Errorf("headless service is missing %q", property)
		}
	}
	for _, forbidden := range []string{"LoadCredential=telegram_bot_token:", "AGENTBRIDGE_LISTEN=", "agentbridge local-api"} {
		if strings.Contains(service, forbidden) {
			t.Errorf("headless service contains controller transport %q", forbidden)
		}
	}
}

func TestUserUnitsAvoidCapabilityBoundingSetFailures(t *testing.T) {
	for _, asset := range []string{
		"deploy/systemd/agentbridge.service",
		"deploy/systemd/agentbridge-device-agent.service",
		"deploy/systemd/agentbridge-backup.service",
	} {
		unit := readAsset(t, asset)
		for _, property := range []string{
			"PrivateDevices=true",
			"ProtectKernelModules=true",
			"ProtectKernelLogs=true",
		} {
			if strings.Contains(unit, property) {
				t.Errorf("%s sets %s, which fails with status 218/CAPABILITIES in an unprivileged systemd user manager", asset, property)
			}
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

func TestPiSmokeFindsPerUserInstalledCommands(t *testing.T) {
	script := readAsset(t, "scripts/pi-smoke.sh")
	if !strings.Contains(script, `PATH="$HOME/.local/bin:$PATH"`) {
		t.Error("Pi smoke script must add the per-user install directory to PATH")
	}
	for _, fragment := range []string{
		"AGENTBRIDGE_PI_NONCE",
		"AGENTBRIDGE_PI_ATTESTATION_PATH",
		"AGENTBRIDGE_SERVICE_NAME",
		"manifest_artifact_digest",
		"preflight_pass",
		"chmod 0700 -- \"$attestation_dir\"",
		"vertical_slice",
		"reconnect",
	} {
		if !strings.Contains(script, fragment) {
			t.Errorf("Pi smoke script is missing controlled evidence field %q", fragment)
		}
	}
	acceptance := readAsset(t, "scripts/pi-acceptance.sh")
	for _, fragment := range []string{
		"agentbridge.pi.vertical-slice.v1",
		"agentbridge.pi.acceptance.v1",
		"AGENTBRIDGE_SERVICE_NAME",
		"preflight_sha256",
		"duplicate_commit_receipts",
		"RUN_PI_ACCEPTANCE",
	} {
		if !strings.Contains(acceptance, fragment) {
			t.Errorf("Pi acceptance verifier is missing %q", fragment)
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

func TestBackupWrapperDelegatesToSchemaAwareV2Command(t *testing.T) {
	script := readAsset(t, "scripts/backup.sh")
	for _, fragment := range []string{
		`exec "$agentbridge_bin" backup --database "$database" --output "$backup_dir"`,
		"command -v",
		"umask 077",
	} {
		if !strings.Contains(script, fragment) {
			t.Errorf("backup wrapper is missing %q", fragment)
		}
	}
	if strings.Contains(script, "sqlite3") || strings.Contains(script, "cp \"$DATABASE_PATH\"") {
		t.Error("backup wrapper must not query or copy the live SQLite database")
	}

	implementation := readAsset(t, "internal/operations/backup.go")
	for _, fragment := range []string{
		`"VACUUM INTO '"`,
		`"PRAGMA integrity_check"`,
		`"agentbridge-2.0"`,
		"SchemaFingerprint",
	} {
		if !strings.Contains(implementation, fragment) {
			t.Errorf("v2 backup implementation is missing %q", fragment)
		}
	}
	retention := readAsset(t, "internal/operations/retention.go")
	backupCommand := readAsset(t, "cmd/agentbridge/backup.go")
	for _, fragment := range []string{
		"applyRetention",
		"PINNED_TASKS_FILE",
		"execution_events",
		"attachments",
		"worktree_path",
	} {
		if !strings.Contains(retention+backupCommand+script, fragment) {
			t.Errorf("v2 retention implementation is missing %q", fragment)
		}
	}
}

func TestPublicAssetsContainNoPrivateDeploymentData(t *testing.T) {
	assets := []string{
		"README.md",
		"CONTRIBUTING.md",
		"CODE_OF_CONDUCT.md",
		"SECURITY.md",
		"deploy/systemd/agentbridge.service",
		"deploy/systemd/agentbridge-device-agent.service",
		"deploy/systemd/agentbridge-backup.service",
		"deploy/systemd/agentbridge-backup.timer",
		"deploy/install.sh",
		"deploy/uninstall.sh",
		"scripts/backup.sh",
		"scripts/restore-check.sh",
		"scripts/pi-smoke.sh",
		"scripts/pi-acceptance.sh",
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
