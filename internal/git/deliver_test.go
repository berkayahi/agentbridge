package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type verifierFunc func(context.Context, string) error

func (f verifierFunc) Verify(ctx context.Context, path string) error { return f(ctx, path) }

type recordingGit struct {
	runner Runner
	calls  [][]string
}

func (r *recordingGit) Run(ctx context.Context, dir string, args ...string) (RunResult, error) {
	r.calls = append(r.calls, slices.Clone(args))
	return r.runner.Run(ctx, dir, args...)
}

func TestDeliverVerifiedChangesToConfiguredRef(t *testing.T) {
	fixture, profile, workspace := preparedDeliveryFixture(t)
	if err := os.WriteFile(filepath.Join(workspace.Path, "change.txt"), []byte("safe change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recording := &recordingGit{}
	delivery := Delivery{Git: recording, Verifier: verifierFunc(func(context.Context, string) error { return nil })}
	result, err := delivery.Deliver(context.Background(), DeliveryRequest{Profile: profile, Workspace: workspace, CommitMessage: "feat: add safe change"})
	if err != nil {
		t.Fatal(err)
	}
	if result.NoChanges || result.CommitSHA == "" {
		t.Fatalf("result = %#v", result)
	}
	if got := strings.TrimSpace(gitOutput(t, fixture.control, "ls-remote", fixture.remote, "refs/heads/staging")); !strings.HasPrefix(got, result.CommitSHA) {
		t.Fatalf("remote = %s commit = %s", got, result.CommitSHA)
	}
	for _, args := range recording.calls {
		if slices.Contains(args, "--force") || slices.Contains(args, "--force-with-lease") {
			t.Fatalf("unsafe argv: %v", args)
		}
	}
	last := recording.calls[len(recording.calls)-1]
	if !slices.Equal(last, []string{"push", "origin", "HEAD:refs/heads/staging"}) {
		t.Fatalf("push argv = %v", last)
	}
}

func TestDeliverSkipsNoChangesAndVerificationFailure(t *testing.T) {
	_, profile, workspace := preparedDeliveryFixture(t)
	recording := &recordingGit{}
	delivery := Delivery{Git: recording, Verifier: verifierFunc(func(context.Context, string) error { return nil })}
	result, err := delivery.Deliver(context.Background(), DeliveryRequest{Profile: profile, Workspace: workspace, CommitMessage: "feat: nothing"})
	if err != nil || !result.NoChanges {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if containsGitCommand(recording.calls, "commit") || containsGitCommand(recording.calls, "push") {
		t.Fatalf("unexpected calls: %v", recording.calls)
	}
	if err := os.WriteFile(filepath.Join(workspace.Path, "change"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	recording.calls = nil
	delivery.Verifier = verifierFunc(func(context.Context, string) error { return errors.New("failed") })
	if _, err := delivery.Deliver(context.Background(), DeliveryRequest{Profile: profile, Workspace: workspace, CommitMessage: "feat: blocked"}); !errors.Is(err, ErrVerificationFailed) {
		t.Fatalf("err = %v", err)
	}
	if len(recording.calls) != 0 {
		t.Fatalf("git ran after failed verification: %v", recording.calls)
	}
}

func TestDeliverRejectsUnsafePolicyMessageAndSecrets(t *testing.T) {
	_, profile, workspace := preparedDeliveryFixture(t)
	for _, ref := range []string{"refs/heads/main", "refs/heads/master", "refs/heads/production", "refs/tags/v1", "refs/heads/arbitrary"} {
		unsafe := profile
		unsafe.AllowedRef = ref
		if _, err := (Delivery{Git: &recordingGit{}, Verifier: verifierFunc(func(context.Context, string) error { return nil })}).Deliver(context.Background(), DeliveryRequest{Profile: unsafe, Workspace: workspace, CommitMessage: "feat: safe"}); !errors.Is(err, ErrUnsafeDelivery) {
			t.Fatalf("ref %s: %v", ref, err)
		}
	}
	if err := os.WriteFile(filepath.Join(workspace.Path, "secret.env"), []byte("OPENAI_API_KEY=sk-secret-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	delivery := Delivery{Git: &recordingGit{}, Verifier: verifierFunc(func(context.Context, string) error { return nil })}
	if _, err := delivery.Deliver(context.Background(), DeliveryRequest{Profile: profile, Workspace: workspace, CommitMessage: "bad message"}); !errors.Is(err, ErrInvalidCommitMessage) {
		t.Fatalf("message err = %v", err)
	}
	if _, err := delivery.Deliver(context.Background(), DeliveryRequest{Profile: profile, Workspace: workspace, CommitMessage: "feat: secret"}); !errors.Is(err, ErrSecretDetected) {
		t.Fatalf("secret err = %v", err)
	}
}

func TestDeliverDetectsRemoteRace(t *testing.T) {
	fixture, profile, workspace := preparedDeliveryFixture(t)
	if err := os.WriteFile(filepath.Join(workspace.Path, "local.txt"), []byte("local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(fixture.root, "other")
	runGit(t, fixture.root, "clone", "--branch", "staging", fixture.remote, other)
	runGit(t, other, "config", "user.name", "Other")
	runGit(t, other, "config", "user.email", "other@example.invalid")
	if err := os.WriteFile(filepath.Join(other, "remote.txt"), []byte("remote\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, other, "add", "remote.txt")
	runGit(t, other, "commit", "-m", "feat: remote race")
	runGit(t, other, "push", "origin", "HEAD:refs/heads/staging")
	delivery := Delivery{Git: &recordingGit{}, Verifier: verifierFunc(func(context.Context, string) error { return nil })}
	if _, err := delivery.Deliver(context.Background(), DeliveryRequest{Profile: profile, Workspace: workspace, CommitMessage: "feat: local"}); !errors.Is(err, ErrDeliveryConflict) {
		t.Fatalf("err = %v", err)
	}
}

func TestPrePushHookUsesExactProfileRef(t *testing.T) {
	profile := DeliveryProfile{RepositoryProfile: RepositoryProfile{ControlCheckout: "/tmp/control", WorktreeRoot: "/tmp/worktrees", Remote: "origin", BaseRef: "refs/heads/feature/demo"}, Enabled: true, AllowedRef: "refs/heads/feature/demo"}
	hook, err := PrePushHook(profile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hook, "refs/heads/feature/demo") || strings.Contains(hook, "refs/heads/staging") {
		t.Fatalf("hook = %s", hook)
	}
}

func preparedDeliveryFixture(t *testing.T) (struct{ root, control, remote string }, DeliveryProfile, Workspace) {
	t.Helper()
	base := newGitFixture(t, "staging")
	remote := strings.TrimSpace(gitOutput(t, base.control, "remote", "get-url", "origin"))
	runGit(t, base.control, "config", "user.name", "Test")
	runGit(t, base.control, "config", "user.email", "test@example.invalid")
	port := &savedWorkspace{}
	repo := RepositoryProfile{ControlCheckout: base.control, Remote: "origin", BaseRef: "refs/heads/staging", WorktreeRoot: filepath.Join(base.root, "worktrees")}
	workspace, err := (WorkspaceManager{Git: Runner{}, Port: port}).Prepare(context.Background(), repo, "delivery-task")
	if err != nil {
		t.Fatal(err)
	}
	return struct{ root, control, remote string }{base.root, base.control, remote}, DeliveryProfile{RepositoryProfile: repo, Enabled: true, AllowedRef: "refs/heads/staging"}, workspace
}
func containsGitCommand(calls [][]string, command string) bool {
	for _, args := range calls {
		if len(args) > 0 && args[0] == command {
			return true
		}
	}
	return false
}

var _ = exec.ErrNotFound
