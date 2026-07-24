package localcontrol

import (
	"context"
	"fmt"
)

// DiffFile is one file a bee touched.
type DiffFile struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	Binary  bool   `json:"binary,omitempty"`
}

// TaskDiff is what a bee has changed in her own isolated worktree, so a keeper
// can judge her work before committing it. It is read-only evidence: nothing
// here can move a task or touch a repository.
type TaskDiff struct {
	Files []DiffFile `json:"files"`
	Patch string     `json:"patch,omitempty"`
	// Truncated says so out loud rather than quietly showing a partial patch as
	// if it were the whole change.
	Truncated bool `json:"truncated"`
}

type TaskDiffResponse struct {
	Task TaskView `json:"task"`
	Diff TaskDiff `json:"diff"`
}

// Differ reads a task's worktree. Only the runtime can do this: the authority
// never opens a filesystem path itself.
type Differ interface {
	Diff(ctx context.Context, view TaskView) (TaskDiff, error)
}

// TaskDiff reports what the bee has changed. It takes no command lock and
// performs no transition: looking at her work must never disturb it.
func (s *Service) TaskDiff(ctx context.Context, taskID string) (TaskDiffResponse, error) {
	if s == nil {
		return TaskDiffResponse{}, ErrNotConfigured
	}
	if !validID(taskID) {
		return TaskDiffResponse{}, ErrInvalidRequest
	}
	view, err := s.taskView(ctx, taskID)
	if err != nil {
		return TaskDiffResponse{}, err
	}
	differ, ok := s.executor.(Differ)
	if !ok {
		return TaskDiffResponse{}, fmt.Errorf("runtime cannot report a diff: %w", ErrNotConfigured)
	}
	diff, err := differ.Diff(ctx, view)
	if err != nil {
		return TaskDiffResponse{}, err
	}
	if diff.Files == nil {
		diff.Files = []DiffFile{}
	}
	return TaskDiffResponse{Task: view, Diff: diff}, nil
}
