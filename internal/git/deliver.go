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
	ErrDeliveryDisabled     = errors.New("git delivery: disabled")
	ErrUnsafeDelivery       = errors.New("git delivery: unsafe policy")
	ErrVerificationFailed   = errors.New("git delivery: verification failed")
	ErrInvalidCommitMessage = errors.New("git delivery: invalid Conventional Commit message")
	ErrSecretDetected       = errors.New("git delivery: potential secret detected")
	ErrDeliveryConflict     = errors.New("git delivery: remote ref changed")
)

type DeliveryProfile struct {
	RepositoryProfile
	Enabled    bool
	AllowedRef string
}

func (p DeliveryProfile) Validate() error {
	if err := p.RepositoryProfile.Validate(); err != nil {
		return err
	}
	if !p.Enabled || !validHeadRef(p.AllowedRef) || p.AllowedRef != p.BaseRef || productionRef(p.AllowedRef) {
		return ErrUnsafeDelivery
	}
	return nil
}

type DeliveryRequest struct {
	Profile       DeliveryProfile
	Workspace     Workspace
	CommitMessage string
}
type DeliveryResult struct {
	NoChanges          bool
	CommitSHA, PushRef string
}
type GitRunner interface {
	Run(context.Context, string, ...string) (RunResult, error)
}
type Verifier interface {
	Verify(context.Context, string) error
}
type Delivery struct {
	Git      GitRunner
	Verifier Verifier
}

func (d Delivery) Deliver(ctx context.Context, request DeliveryRequest) (DeliveryResult, error) {
	if !request.Profile.Enabled {
		return DeliveryResult{}, ErrDeliveryDisabled
	}
	if err := request.Profile.Validate(); err != nil {
		return DeliveryResult{}, err
	}
	if d.Git == nil || d.Verifier == nil {
		return DeliveryResult{}, ErrUnsafeDelivery
	}
	if !validCommitMessage(request.CommitMessage) {
		return DeliveryResult{}, ErrInvalidCommitMessage
	}
	if err := validateWorkspace(request.Profile, request.Workspace); err != nil {
		return DeliveryResult{}, err
	}
	if err := d.Verifier.Verify(ctx, request.Workspace.Path); err != nil {
		return DeliveryResult{}, fmt.Errorf("%w: %v", ErrVerificationFailed, err)
	}
	status, err := d.Git.Run(ctx, request.Workspace.Path, "status", "--porcelain=v1", "-z")
	if err != nil {
		return DeliveryResult{}, err
	}
	if status.Stdout == "" {
		return DeliveryResult{NoChanges: true}, nil
	}
	if _, err := d.Git.Run(ctx, request.Workspace.Path, "add", "-A"); err != nil {
		return DeliveryResult{}, err
	}
	if err := d.scanChangedFiles(ctx, request.Workspace.Path); err != nil {
		return DeliveryResult{}, err
	}
	if _, err := d.Git.Run(ctx, request.Workspace.Path, "commit", "-m", request.CommitMessage); err != nil {
		return DeliveryResult{}, err
	}

	branch := strings.TrimPrefix(request.Profile.AllowedRef, "refs/heads/")
	tracking := "refs/remotes/" + request.Profile.Remote + "/" + branch
	refspec := "+" + request.Profile.AllowedRef + ":" + tracking
	if _, err := d.Git.Run(ctx, request.Workspace.Path, "fetch", "--no-tags", request.Profile.Remote, refspec); err != nil {
		return DeliveryResult{}, fmt.Errorf("%w: fetch allowed ref: %v", ErrDeliveryConflict, err)
	}
	remote, err := d.Git.Run(ctx, request.Workspace.Path, "rev-parse", "--verify", tracking+"^{commit}")
	if err != nil {
		return DeliveryResult{}, fmt.Errorf("%w: resolve allowed ref", ErrDeliveryConflict)
	}
	remoteSHA := strings.TrimSpace(remote.Stdout)
	if _, err := d.Git.Run(ctx, request.Workspace.Path, "merge-base", "--is-ancestor", remoteSHA, request.Workspace.BaseSHA); err != nil {
		return DeliveryResult{}, ErrDeliveryConflict
	}
	commit, err := d.Git.Run(ctx, request.Workspace.Path, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return DeliveryResult{}, err
	}
	commitSHA := strings.TrimSpace(commit.Stdout)
	refspec = "HEAD:" + request.Profile.AllowedRef
	if _, err := d.Git.Run(ctx, request.Workspace.Path, "push", request.Profile.Remote, refspec); err != nil {
		return DeliveryResult{}, ErrDeliveryConflict
	}
	return DeliveryResult{CommitSHA: commitSHA, PushRef: request.Profile.AllowedRef}, nil
}

func (d Delivery) scanChangedFiles(ctx context.Context, worktree string) error {
	files, err := d.Git.Run(ctx, worktree, "diff", "--cached", "--name-only", "-z", "--diff-filter=ACMR")
	if err != nil {
		return err
	}
	if strings.Contains(files.Stdout, "…[TRUNCATED]") {
		return fmt.Errorf("%w: changed file list exceeded bound", ErrSecretDetected)
	}
	for _, name := range strings.Split(files.Stdout, "\x00") {
		if name == "" {
			continue
		}
		path, err := containedPath(worktree, name)
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if info.Size() > 32<<20 {
			return fmt.Errorf("%w: changed file is too large to scan", ErrSecretDetected)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if secretPattern.Match(content) {
			return fmt.Errorf("%w in %s", ErrSecretDetected, name)
		}
	}
	return nil
}

var conventionalCommit = regexp.MustCompile(`^(build|chore|ci|docs|feat|fix|perf|refactor|revert|style|test)(\([a-zA-Z0-9._/-]+\))?!?: [^\r\n]+$`)
var secretPattern = regexp.MustCompile(`(?i)(-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----|\b(?:OPENAI_API_KEY|ANTHROPIC_API_KEY|CLAUDE_CODE_OAUTH_TOKEN|TELEGRAM_BOT_TOKEN|GITHUB_TOKEN)\s*=|\bgh[pousr]_[A-Za-z0-9]{20,}|\bgithub_pat_[A-Za-z0-9_]{20,}|\b[0-9]{8,12}:[A-Za-z0-9_-]{30,})`)

func validCommitMessage(message string) bool { return conventionalCommit.MatchString(message) }
func productionRef(ref string) bool {
	switch strings.ToLower(ref) {
	case "refs/heads/main", "refs/heads/master", "refs/heads/production":
		return true
	}
	return false
}
func validateWorkspace(profile DeliveryProfile, workspace Workspace) error {
	if !regexp.MustCompile(`^[0-9a-fA-F]{40,64}$`).MatchString(workspace.BaseSHA) {
		return ErrUnsafeDelivery
	}
	_, err := containedPath(profile.WorktreeRoot, workspace.Path)
	return err
}
func containedPath(root, name string) (string, error) {
	path := name
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, name)
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", ErrUnsafeDelivery
	}
	return path, nil
}

func PrePushHook(profile DeliveryProfile) (string, error) {
	if err := profile.Validate(); err != nil {
		return "", err
	}
	return fmt.Sprintf(`#!/bin/sh
allowed=%q
while read local_ref local_sha remote_ref remote_sha; do
  if [ "$remote_ref" != "$allowed" ]; then
    echo "agentbridge: rejected push to $remote_ref" >&2
    exit 1
  fi
done
`, profile.AllowedRef), nil
}
