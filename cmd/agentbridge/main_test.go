package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/berkayahi/agentbridge/internal/telegram"
)

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"version"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if got, want := stdout.String(), "agentbridge dev (commit unknown, built unknown)\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}
}

func TestRunTelegramPairPrintsOnlyNumericIdentity(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runWithPairer([]string{"pair", "telegram", "--config", "config.yaml"}, &stdout, &stderr, fakePairer{})
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	if got, want := stdout.String(), "telegram_user_id: 42\ntelegram_chat_id: 100\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if strings.Contains(stdout.String(), "username") || stderr.Len() != 0 {
		t.Fatalf("unsafe output: %q %q", stdout.String(), stderr.String())
	}
}

func TestRunTelegramPairDoesNotPretendTransportExists(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"pair", "telegram", "--config", "config.yaml"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "transport is not configured") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}

type fakePairer struct{}

func (fakePairer) Pair(context.Context) (telegram.Pairing, string, error) {
	return telegram.Pairing{UserID: 42, ChatID: 100}, "not-printed", nil
}

func TestRunVersionReturnsFailureWhenOutputCannotBeWritten(t *testing.T) {
	var stderr bytes.Buffer
	code := run([]string{"version"}, failingWriter{}, &stderr)

	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if got, want := stderr.String(), "agentbridge: failed to write version output\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

func TestRunInvalidArguments(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"invalid"}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if got := stdout.String(); got != "" {
		t.Fatalf("stdout = %q, want empty", got)
	}
	if got, want := stderr.String(), "usage: agentbridge version | agentbridge doctor --config <path> | agentbridge pair telegram --config <path>\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

func TestRunDoctorReportsOnlySafeSummary(t *testing.T) {
	clearProviderAPIKeyEnvironment(t)
	path := writeDoctorConfig(t, `server:
  listen: 127.0.0.1:8787
  allowed_tailscale_identities: [operator@example.invalid]
telegram:
  private_chat_only: true
  allowed_user_ids: [987654321]
providers:
  codex: {executable: /usr/local/bin/codex}
repositories:
  public-sample:
    checkout_path: /srv/agentbridge/checkouts/public-sample
    remote: origin
    base_ref: refs/heads/staging
    verification:
      - {argv: ["go", "test", "./..."], dir: .}
    deployment_url: https://private-deploy.example.invalid
    delivery: {enabled: true, allowed_ref: refs/heads/staging}
`)
	var stdout, stderr bytes.Buffer
	code := run([]string{"doctor", "--config", path}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	want := "configuration valid\nprofiles (1): public-sample\ndelivery: 1 enabled, 0 disabled\n"
	if got := stdout.String(); got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	for _, sensitive := range []string{"/srv/", "987654321", "private-deploy", "operator@", "codex"} {
		if strings.Contains(stdout.String(), sensitive) {
			t.Fatalf("stdout contains sensitive value %q: %q", sensitive, stdout.String())
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestRunDoctorReturnsConciseValidationError(t *testing.T) {
	clearProviderAPIKeyEnvironment(t)
	path := writeDoctorConfig(t, "server: {listen: 0.0.0.0:8787}\n")
	var stdout, stderr bytes.Buffer
	code := run([]string{"doctor", "--config", path}, &stdout, &stderr)

	if code == 0 {
		t.Fatal("code = 0, want nonzero")
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if got := stderr.String(); !strings.HasPrefix(got, "agentbridge: invalid configuration:") || strings.Contains(got, path) {
		t.Fatalf("stderr = %q, want concise error without path", got)
	}
}

func clearProviderAPIKeyEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"} {
		t.Setenv(name, "")
	}
}

func writeDoctorConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, io.ErrClosedPipe
}
