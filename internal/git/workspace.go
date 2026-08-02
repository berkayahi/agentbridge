package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	ErrInvalidProfile = errors.New("git workspace: invalid repository profile")
	ErrInvalidTaskID  = errors.New("git workspace: invalid task ID")
	ErrInvalidBaseSHA = errors.New("git workspace: invalid expected base SHA")
	ErrBaseMismatch   = errors.New("git workspace: configured base ref does not match expected SHA")
	ErrDirtyCheckout  = errors.New("git workspace: control checkout is dirty")
	ErrPathCollision  = errors.New("git workspace: path collision")
)

var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
var fullObjectID = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

type RepositoryProfile struct{ ControlCheckout, Remote, BaseRef, WorktreeRoot string }

func (p RepositoryProfile) Validate() error {
	if !filepath.IsAbs(p.ControlCheckout) || !filepath.IsAbs(p.WorktreeRoot) {
		return ErrInvalidProfile
	}
	if !safeName.MatchString(p.Remote) {
		return ErrInvalidProfile
	}
	if !validHeadRef(p.BaseRef) {
		return ErrInvalidProfile
	}
	return nil
}

// EnsureRemoteBaseRef makes Kovan's private work ref an implementation detail.
// It publishes only the checkout's committed HEAD, so local uncommitted files
// are never staged or committed as part of workspace preparation.
func EnsureRemoteBaseRef(ctx context.Context, runner Runner, checkout, remote, baseRef string) error {
	return EnsureRemoteBaseRefAt(ctx, runner, checkout, remote, baseRef, "")
}

// EnsureRemoteBaseRefAt validates or creates the configured delivery ref at an
// exact commit. An expected SHA is a caller-owned baseline fence: an existing
// ref may not drift through it, and a missing ref is created from that commit
// rather than from the control checkout's potentially stale HEAD.
func EnsureRemoteBaseRefAt(ctx context.Context, runner Runner, checkout, remote, baseRef, expectedBaseSHA string) error {
	if !filepath.IsAbs(checkout) || !safeName.MatchString(remote) || !validHeadRef(baseRef) {
		return ErrInvalidProfile
	}
	expectedBaseSHA = strings.ToLower(strings.TrimSpace(expectedBaseSHA))
	if expectedBaseSHA != "" && !fullObjectID.MatchString(expectedBaseSHA) {
		return ErrInvalidBaseSHA
	}
	result, err := runner.Run(ctx, checkout, "ls-remote", "--heads", remote, baseRef)
	if err != nil {
		return fmt.Errorf("inspect configured base ref: %w", err)
	}
	fields := strings.Fields(result.Stdout)
	if len(fields) > 0 {
		remoteSHA := strings.ToLower(fields[0])
		if expectedBaseSHA != "" && remoteSHA != expectedBaseSHA {
			return fmt.Errorf("%w: ref %s is %s, expected %s", ErrBaseMismatch, baseRef, remoteSHA, expectedBaseSHA)
		}
		return nil
	}
	source := "HEAD"
	if expectedBaseSHA != "" {
		source = expectedBaseSHA
		if _, err := runner.Run(ctx, checkout, "rev-parse", "--verify", expectedBaseSHA+"^{commit}"); err != nil {
			if _, fetchErr := runner.Run(ctx, checkout, "fetch", "--no-tags", remote, expectedBaseSHA); fetchErr != nil {
				return fmt.Errorf("fetch expected base %s: %w", expectedBaseSHA, fetchErr)
			}
			if _, resolveErr := runner.Run(ctx, checkout, "rev-parse", "--verify", expectedBaseSHA+"^{commit}"); resolveErr != nil {
				return fmt.Errorf("resolve expected base %s: %w", expectedBaseSHA, resolveErr)
			}
		}
	}
	if _, err := runner.Run(ctx, checkout, "push", remote, source+":"+baseRef); err != nil {
		return fmt.Errorf("prepare configured base ref: %w", err)
	}
	return nil
}

type Workspace struct{ BaseSHA, Path string }
type WorkspacePort interface {
	SaveWorkspace(context.Context, string, string, string) error
}
type WorkspaceManager struct {
	Git  Runner
	Port WorkspacePort
}

func (m WorkspaceManager) Prepare(ctx context.Context, profile RepositoryProfile, taskID string) (Workspace, error) {
	return m.PrepareAt(ctx, profile, taskID, "")
}

// PrepareAt creates a detached worktree only when the configured base ref
// resolves to expectedBaseSHA. Passing an empty SHA retains the generic
// profile-driven behavior used by existing AgentBridge clients.
func (m WorkspaceManager) PrepareAt(ctx context.Context, profile RepositoryProfile, taskID, expectedBaseSHA string) (Workspace, error) {
	if err := profile.Validate(); err != nil {
		return Workspace{}, err
	}
	if !safeName.MatchString(taskID) {
		return Workspace{}, ErrInvalidTaskID
	}
	expectedBaseSHA = strings.ToLower(strings.TrimSpace(expectedBaseSHA))
	if expectedBaseSHA != "" && !fullObjectID.MatchString(expectedBaseSHA) {
		return Workspace{}, ErrInvalidBaseSHA
	}
	if m.Port == nil {
		return Workspace{}, fmt.Errorf("%w: persistence port is required", ErrInvalidProfile)
	}
	status, err := m.Git.Run(ctx, profile.ControlCheckout, "status", "--porcelain=v1", "-z")
	if err != nil {
		return Workspace{}, err
	}
	if status.Stdout != "" {
		return Workspace{}, ErrDirtyCheckout
	}
	path := filepath.Join(profile.WorktreeRoot, taskID)
	if _, err := os.Lstat(path); err == nil {
		return Workspace{}, ErrPathCollision
	} else if !errors.Is(err, os.ErrNotExist) {
		return Workspace{}, err
	}
	branch := strings.TrimPrefix(profile.BaseRef, "refs/heads/")
	tracking := "refs/remotes/" + profile.Remote + "/" + branch
	refspec := "+" + profile.BaseRef + ":" + tracking
	if err := EnsureRemoteBaseRefAt(ctx, m.Git, profile.ControlCheckout, profile.Remote, profile.BaseRef, expectedBaseSHA); err != nil {
		return Workspace{}, err
	}
	if _, err := m.Git.Run(ctx, profile.ControlCheckout, "fetch", "--no-tags", profile.Remote, refspec); err != nil {
		return Workspace{}, fmt.Errorf("fetch configured base ref: %w", err)
	}
	resolved, err := m.Git.Run(ctx, profile.ControlCheckout, "rev-parse", "--verify", tracking+"^{commit}")
	if err != nil {
		return Workspace{}, fmt.Errorf("resolve configured base ref: %w", err)
	}
	sha := strings.TrimSpace(resolved.Stdout)
	if expectedBaseSHA != "" && strings.ToLower(sha) != expectedBaseSHA {
		return Workspace{}, fmt.Errorf("%w: fetched ref %s is %s, expected %s", ErrBaseMismatch, profile.BaseRef, sha, expectedBaseSHA)
	}
	if err := os.MkdirAll(profile.WorktreeRoot, 0o700); err != nil {
		return Workspace{}, fmt.Errorf("create worktree root: %w", err)
	}
	if _, err := m.Git.Run(ctx, profile.ControlCheckout, "worktree", "add", "--detach", path, sha); err != nil {
		return Workspace{}, fmt.Errorf("add detached worktree: %w", err)
	}
	if err := m.Port.SaveWorkspace(ctx, taskID, sha, path); err != nil {
		_, _ = m.Git.Run(context.Background(), profile.ControlCheckout, "worktree", "remove", "--force", path)
		return Workspace{}, fmt.Errorf("persist workspace: %w", err)
	}
	return Workspace{BaseSHA: sha, Path: path}, nil
}

func (m WorkspaceManager) Cleanup(ctx context.Context, profile RepositoryProfile, path string) error {
	if err := profile.Validate(); err != nil {
		return err
	}
	rel, err := filepath.Rel(profile.WorktreeRoot, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ErrPathCollision
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	_, err = m.Git.Run(ctx, profile.ControlCheckout, "worktree", "remove", path)
	return err
}

func validHeadRef(ref string) bool {
	const prefix = "refs/heads/"
	branch := strings.TrimPrefix(ref, prefix)
	if branch == ref || branch == "" || strings.ContainsAny(branch, " ~^:?*[\\") || strings.Contains(branch, "..") || strings.Contains(branch, "//") || strings.Contains(branch, "@{") || strings.HasSuffix(branch, "/") || strings.HasSuffix(branch, ".") {
		return false
	}
	for _, part := range strings.Split(branch, "/") {
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}
