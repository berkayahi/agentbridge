package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/task"
)

func TestExecPTYRunsFakeLoginAndCapturesOnlyExpectedPrompt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY test requires a Unix pseudo-terminal")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-login")
	script := "#!/bin/sh\nprintf 'OAuth token: NEVER-CAPTURE\\nOpen https://auth.openai.com/device and enter ABCD-EFGH\\n'\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	var transcript strings.Builder
	runner := ExecPTY{}
	if err := runner.Run(context.Background(), path, nil, nil, func(chunk []byte) {
		transcript.WriteString(expectedPrompt(task.ProviderCodex, string(chunk)))
	}); err != nil {
		t.Fatal(err)
	}
	if got := transcript.String(); !strings.Contains(got, "https://auth.openai.com/device") || !strings.Contains(got, "ABCD-EFGH") {
		t.Fatalf("transcript = %q", got)
	}
	if strings.Contains(transcript.String(), "NEVER-CAPTURE") {
		t.Fatalf("unexpected provider output captured: %q", transcript.String())
	}
}

func TestExecPTYHonorsCancellationAndWaitsForChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY test requires a Unix pseudo-terminal")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-login")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nwhile :; do sleep 1; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := (ExecPTY{}).Run(ctx, path, nil, nil, func([]byte) {})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("canceled child took %s to exit", elapsed)
	}
}

func TestExecPTYCancellationKillsProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY test requires a Unix pseudo-terminal")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-login")
	pidPath := filepath.Join(dir, "child.pid")
	script := "#!/bin/sh\n(trap '' TERM HUP; while :; do sleep 1; done) &\necho $! > \"$1\"\nwait\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := (ExecPTY{}).Run(ctx, "/bin/sh", []string{path, pidPath}, nil, func([]byte) {})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v", err)
	}
	payload, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(payload)), "%d", &pid); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for processExists(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if processExists(pid) {
		t.Fatalf("descendant process %d survived cancellation", pid)
	}
}

func TestExpectedPromptRejectsOAuthTokensAndUnrecognizedOutput(t *testing.T) {
	t.Parallel()
	input := "Bearer oauth-super-secret\nrefresh_token=another-secret\nOpen https://auth.openai.com/device and enter WXYZ-1234\n"
	got := expectedPrompt(task.ProviderCodex, input)
	if !strings.Contains(got, "https://auth.openai.com/device") || !strings.Contains(got, "WXYZ-1234") {
		t.Fatalf("prompt = %q", got)
	}
	if strings.Contains(got, "oauth-super-secret") || strings.Contains(got, "another-secret") {
		t.Fatalf("secret copied into prompt: %q", got)
	}
}

func TestExpectedPromptRejectsUntrustedAndCredentialURLs(t *testing.T) {
	t.Parallel()
	input := "Open https://evil.example/callback?access_token=LEAK-ME and enter SAFE-CODE\n" +
		"Open https://credential@auth.openai.com/device and enter USER-CODE\n" +
		"Open https://auth.openai.com/device?Access_Token=ALSO-LEAK and enter QUERY-CODE\n" +
		"refresh token endpoint https://auth.openai.com/session/PATH-SECRET\n" +
		"Open https://auth.openai.com/device/TRAILING-SECRET and enter TRAIL-CODE\n" +
		"Open https://auth.openai.com/device and enter REAL-CODE\n"
	got := expectedPrompt(task.ProviderCodex, input)
	if strings.Contains(got, "evil.example") || strings.Contains(got, "LEAK-ME") || strings.Contains(got, "ALSO-LEAK") || strings.Contains(got, "credential") || strings.Contains(got, "USER-CODE") || strings.Contains(got, "QUERY-CODE") || strings.Contains(got, "PATH-SECRET") || strings.Contains(got, "TRAILING-SECRET") || strings.Contains(got, "TRAIL-CODE") {
		t.Fatalf("unsafe URL captured: %q", got)
	}
	if !strings.Contains(got, "https://auth.openai.com/device") || !strings.Contains(got, "REAL-CODE") {
		t.Fatalf("expected prompt missing: %q", got)
	}
}

func TestExecCommandRunnerMapsMissingExecutable(t *testing.T) {
	t.Parallel()
	_, err := (ExecCommandRunner{}).Run(context.Background(), filepath.Join(t.TempDir(), "missing"))
	if !errors.Is(err, ErrCommandMissing) {
		t.Fatalf("error = %v", err)
	}
}

func TestAuthProcessesUseConfiguredExecutableAndSharedClaudeConfig(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, "claude-state")
	script := filepath.Join(dir, "claude-test")
	contents := "#!/bin/sh\n" +
		"test \"$CLAUDE_CONFIG_DIR\" = \"" + configDir + "\" || exit 23\n" +
		"printf '%s\\n' '{\"loggedIn\":true}'\n"
	if err := os.WriteFile(script, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	environment := []string{"PATH=/usr/bin:/bin", "CLAUDE_CONFIG_DIR=" + configDir}
	runner := ExecCommandRunner{Executables: map[string]string{"claude": script}, Environment: environment}
	output, err := runner.Run(context.Background(), "claude", "auth", "status", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(output)); got != `{"loggedIn":true}` {
		t.Fatalf("output=%q", got)
	}

	ptyRunner := ExecPTY{Executables: map[string]string{"claude": script}, Environment: environment}
	var ptyOutput strings.Builder
	if err := ptyRunner.Run(context.Background(), "claude", nil, nil, func(value []byte) { ptyOutput.Write(value) }); err != nil {
		t.Fatal(err)
	}
}

func TestPTYEndOfStreamTreatsEIOAsNormal(t *testing.T) {
	t.Parallel()
	if !ignorablePTYReadError(fmt.Errorf("read pty: %w", syscall.EIO)) {
		t.Fatal("EIO should be treated as a normal PTY end-of-stream")
	}
}

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || !errors.Is(err, syscall.ESRCH)
}
