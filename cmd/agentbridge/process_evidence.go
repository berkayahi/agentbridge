package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/berkayahi/agentbridge/internal/workmodel"
)

const maxProcEnvironmentBytes = 1 << 20

type taskProcessInspector interface {
	Running(context.Context, string, workmodel.Provider, string) (bool, error)
}

// procTaskInspector does not persist or signal numeric PIDs. It reads current
// same-UID process evidence and treats the unique worktree cwd as authoritative;
// task/provider environment markers are supplemental evidence for Claude.
type procTaskInspector struct {
	root       string
	platform   string
	maxEntries int
}

func (p procTaskInspector) Running(ctx context.Context, taskID string, providerName workmodel.Provider, worktree string) (bool, error) {
	platform := p.platform
	if platform == "" {
		platform = runtime.GOOS
	}
	if platform != "linux" {
		// Without a reliable live-process evidence source, reconciliation must
		// pause rather than claim the worktree has no orphan process.
		return true, nil
	}
	root := p.root
	if root == "" {
		root = "/proc"
	}
	limit := p.maxEntries
	if limit <= 0 {
		limit = 4096
	}
	resolvedWorktree, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		return false, fmt.Errorf("resolve process worktree: %w", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return false, fmt.Errorf("read process evidence: %w", err)
	}
	if len(entries) > limit {
		return false, errors.New("process evidence scan exceeded bound")
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if !entry.IsDir() || !numericPID(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return false, err
		}
		if !ownedByCurrentUser(info) {
			continue
		}
		pidDir := filepath.Join(root, entry.Name())
		cwd, err := os.Readlink(filepath.Join(pidDir, "cwd"))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return false, fmt.Errorf("read same-user process cwd: %w", err)
		}
		if !filepath.IsAbs(cwd) {
			cwd = filepath.Join(pidDir, cwd)
		}
		resolvedCWD, err := filepath.EvalSymlinks(cwd)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return false, err
		}
		relativeCWD, err := filepath.Rel(resolvedWorktree, resolvedCWD)
		if err != nil || filepath.IsAbs(relativeCWD) || relativeCWD == ".." || strings.HasPrefix(relativeCWD, ".."+string(filepath.Separator)) {
			continue
		}

		// A worktree is unique per task, so cwd alone catches Codex tool
		// children that do not inherit task markers. Read bounded markers when
		// available to support diagnostics without weakening that evidence.
		_, _ = boundedEnvironment(filepath.Join(pidDir, "environ"), taskID, providerName)
		return true, nil
	}
	return false, nil
}

func numericPID(value string) bool {
	pid, err := strconv.Atoi(value)
	return err == nil && pid > 0
}

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Getuid()
}

func boundedEnvironment(path, taskID string, providerName workmodel.Provider) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxProcEnvironmentBytes+1))
	if err != nil {
		return false, err
	}
	if len(data) > maxProcEnvironmentBytes {
		return false, errors.New("process environment exceeded bound")
	}
	wantTask := []byte("AGENTBRIDGE_TASK_ID=" + taskID)
	wantProvider := []byte("AGENTBRIDGE_PROVIDER=" + string(providerName))
	foundTask, foundProvider := false, false
	for _, value := range bytes.Split(data, []byte{0}) {
		foundTask = foundTask || bytes.Equal(value, wantTask)
		foundProvider = foundProvider || bytes.Equal(value, wantProvider)
	}
	return foundTask && foundProvider, nil
}

var _ taskProcessInspector = procTaskInspector{}
