package isolation

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func PrepareCommand(cmd *exec.Cmd, policy Policy) error {
	if cmd == nil {
		return errors.New("isolation: nil command")
	}
	policy = policy.normalized()
	if !policy.RequiresSandbox() {
		return nil
	}
	if err := policy.Validate(); err != nil {
		return err
	}
	if policy.WorktreeRoot != "" && !containedPath(policy.WorktreeRoot, cmd.Dir) {
		return fmt.Errorf("%w: command directory", ErrInvalidPolicy)
	}
	for _, path := range policy.WritablePaths {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("%w: writable path unavailable", ErrInvalidPolicy)
		}
	}
	if _, err := policy.Enforce(); err != nil {
		return err
	}
	return preparePlatformCommand(cmd, policy)
}

func ApplyStartedProcess(process *os.Process, policy Policy) error {
	if process == nil {
		return errors.New("isolation: process is nil")
	}
	if policy.Limits.Empty() {
		return nil
	}
	return applyProcessLimits(process, policy.Limits)
}

func containedPath(root, candidate string) bool {
	if root == "" || candidate == "" || !filepath.IsAbs(root) || !filepath.IsAbs(candidate) {
		return false
	}
	root = filepath.Clean(root)
	candidate = filepath.Clean(candidate)
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	if resolved, err := filepath.EvalSymlinks(candidate); err == nil {
		candidate = resolved
	}
	rel, err := filepath.Rel(root, candidate)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func invalidPlatformPolicy(policy Policy) error {
	if policy.Tier == TierWeak && (policy.Network.Mode != "" || !policy.Limits.Empty() || policy.WorktreeRoot != "") {
		return fmt.Errorf("%w: weak tier has unenforced process boundary", ErrCapabilityUnavailable)
	}
	return nil
}
