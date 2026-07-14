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
	ErrDirtyCheckout  = errors.New("git workspace: control checkout is dirty")
	ErrPathCollision  = errors.New("git workspace: path collision")
)

var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

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

type Workspace struct{ BaseSHA, Path string }
type WorkspacePort interface {
	SaveWorkspace(context.Context, string, string, string) error
}
type WorkspaceManager struct {
	Git  Runner
	Port WorkspacePort
}

func (m WorkspaceManager) Prepare(ctx context.Context, profile RepositoryProfile, taskID string) (Workspace, error) {
	if err := profile.Validate(); err != nil {
		return Workspace{}, err
	}
	if !safeName.MatchString(taskID) {
		return Workspace{}, ErrInvalidTaskID
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
	if _, err := m.Git.Run(ctx, profile.ControlCheckout, "fetch", "--no-tags", profile.Remote, refspec); err != nil {
		return Workspace{}, fmt.Errorf("fetch configured base ref: %w", err)
	}
	resolved, err := m.Git.Run(ctx, profile.ControlCheckout, "rev-parse", "--verify", tracking+"^{commit}")
	if err != nil {
		return Workspace{}, fmt.Errorf("resolve configured base ref: %w", err)
	}
	sha := strings.TrimSpace(resolved.Stdout)
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
