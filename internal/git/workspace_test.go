package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type savedWorkspace struct {
	task, base, path string
	calls            int
}

func (s *savedWorkspace) SaveWorkspace(_ context.Context, task, base, path string) error {
	s.task, s.base, s.path = task, base, path
	s.calls++
	return nil
}

func TestWorkspacePrepareUsesConfiguredBaseWithoutMutatingControl(t *testing.T) {
	fixture := newGitFixture(t, "staging")
	original := gitOutput(t, fixture.control, "rev-parse", "HEAD")
	port := &savedWorkspace{}
	manager := WorkspaceManager{Git: Runner{}, Port: port}
	profile := RepositoryProfile{ControlCheckout: fixture.control, Remote: "origin", BaseRef: "refs/heads/staging", WorktreeRoot: filepath.Join(fixture.root, "worktrees")}
	workspace, err := manager.Prepare(context.Background(), profile, "task-123")
	if err != nil {
		t.Fatal(err)
	}
	if workspace.BaseSHA != strings.TrimSpace(gitOutput(t, fixture.control, "rev-parse", "refs/remotes/origin/staging")) {
		t.Fatalf("base = %s", workspace.BaseSHA)
	}
	if port.calls != 1 || port.path != workspace.Path || port.base != workspace.BaseSHA {
		t.Fatalf("persisted = %#v", port)
	}
	if got := gitOutput(t, fixture.control, "rev-parse", "HEAD"); got != original {
		t.Fatalf("control HEAD changed: %s -> %s", original, got)
	}
	if got := strings.TrimSpace(gitOutput(t, workspace.Path, "rev-parse", "HEAD")); got != workspace.BaseSHA {
		t.Fatalf("worktree HEAD = %s", got)
	}
	if err := manager.Cleanup(context.Background(), profile, workspace.Path); err != nil {
		t.Fatal(err)
	}
	if err := manager.Cleanup(context.Background(), profile, workspace.Path); err != nil {
		t.Fatalf("idempotent cleanup: %v", err)
	}
}

func TestWorkspaceSupportsConfiguredFeatureBranch(t *testing.T) {
	fixture := newGitFixture(t, "feature/demo")
	manager := WorkspaceManager{Git: Runner{}, Port: &savedWorkspace{}}
	profile := RepositoryProfile{ControlCheckout: fixture.control, Remote: "origin", BaseRef: "refs/heads/feature/demo", WorktreeRoot: filepath.Join(fixture.root, "worktrees")}
	if _, err := manager.Prepare(context.Background(), profile, "feature-task"); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceRejectsUnsafeInputsDirtyCheckoutAndCollisions(t *testing.T) {
	fixture := newGitFixture(t, "staging")
	base := RepositoryProfile{ControlCheckout: fixture.control, Remote: "origin", BaseRef: "refs/heads/staging", WorktreeRoot: filepath.Join(fixture.root, "worktrees")}
	manager := WorkspaceManager{Git: Runner{}, Port: &savedWorkspace{}}
	for _, mutate := range []func(*RepositoryProfile){
		func(p *RepositoryProfile) { p.BaseRef = "refs/heads/../main" },
		func(p *RepositoryProfile) { p.Remote = "--upload-pack=bad" },
		func(p *RepositoryProfile) { p.WorktreeRoot = "relative" },
	} {
		profile := base
		mutate(&profile)
		if _, err := manager.Prepare(context.Background(), profile, "safe-task"); !errors.Is(err, ErrInvalidProfile) {
			t.Fatalf("profile %#v: %v", profile, err)
		}
	}
	if _, err := manager.Prepare(context.Background(), base, "../escape"); !errors.Is(err, ErrInvalidTaskID) {
		t.Fatalf("unsafe task: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fixture.control, "dirty.txt"), []byte("dirty"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Prepare(context.Background(), base, "dirty-task"); !errors.Is(err, ErrDirtyCheckout) {
		t.Fatalf("dirty checkout: %v", err)
	}
	if err := os.Remove(filepath.Join(fixture.control, "dirty.txt")); err != nil {
		t.Fatal(err)
	}
	collision := filepath.Join(base.WorktreeRoot, "collision")
	if err := os.MkdirAll(collision, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Prepare(context.Background(), base, "collision"); !errors.Is(err, ErrPathCollision) {
		t.Fatalf("collision: %v", err)
	}
}

func TestWorkspaceMissingRefAndCancellation(t *testing.T) {
	fixture := newGitFixture(t, "staging")
	manager := WorkspaceManager{Git: Runner{}, Port: &savedWorkspace{}}
	profile := RepositoryProfile{ControlCheckout: fixture.control, Remote: "origin", BaseRef: "refs/heads/missing", WorktreeRoot: filepath.Join(fixture.root, "worktrees")}
	if _, err := manager.Prepare(context.Background(), profile, "missing"); err == nil {
		t.Fatal("missing ref accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	profile.BaseRef = "refs/heads/staging"
	if _, err := manager.Prepare(ctx, profile, "canceled"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestRunnerBoundsAndRedactsOutputAndSummary(t *testing.T) {
	script := filepath.Join(t.TempDir(), "fake-git")
	secret := "ghp_abcdefghijklmnopqrstuvwxyz123456"
	body := "#!/bin/sh\nprintf '%s%s' '" + secret + "' '01234567890123456789'\nprintf '%s' '" + secret + "' >&2\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	result, err := (Runner{Executable: script, MaxOutputBytes: 16}).Run(context.Background(), t.TempDir(), secret)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Stdout+result.Stderr+result.Summary, secret) {
		t.Fatalf("secret leaked: %#v", result)
	}
	if !strings.Contains(result.Stdout, "TRUNCATED") {
		t.Fatalf("unbounded output: %q", result.Stdout)
	}
}

type gitFixture struct{ root, control string }

func newGitFixture(t *testing.T, branch string) gitFixture {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	remote := filepath.Join(root, "remote.git")
	control := filepath.Join(root, "control")
	runGit(t, root, "init", "--bare", remote)
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "init", "-b", branch)
	runGit(t, source, "config", "user.name", "Test")
	runGit(t, source, "config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "add", "README.md")
	runGit(t, source, "commit", "-m", "fixture")
	runGit(t, source, "remote", "add", "origin", remote)
	runGit(t, source, "push", "origin", "HEAD:refs/heads/"+branch)
	runGit(t, root, "clone", "--branch", branch, remote, control)
	return gitFixture{root: root, control: control}
}
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}
