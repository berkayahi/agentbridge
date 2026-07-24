package obsidian

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/berkayahi/agentbridge/internal/localcontrol"
)

var (
	ErrNoteChanged = errors.New("obsidian: note changed while projecting")
	ErrUnsafePath  = errors.New("obsidian: unsafe note path")
)

// Observer is the read-only portion of the authenticated local API needed by
// a projection host. It keeps Obsidian independent from HTTP and SQLite.
type Observer interface {
	Observe(context.Context, localcontrol.ObserveRequest) (localcontrol.ObserveResponse, error)
}

// SyncTaskFile observes one canonical task and atomically updates its Markdown
// note. A cursor gap triggers one full replay from cursor zero. The original
// body is retained by ApplyObserved, and a concurrent local edit aborts the
// rename instead of silently overwriting the user's work.
func SyncTaskFile(ctx context.Context, observer Observer, taskID, path string, state SyncState) (ParsedNote, error) {
	if observer == nil || strings.TrimSpace(taskID) == "" || !filepath.IsAbs(path) {
		return ParsedNote{}, ErrInvalidNote
	}
	original, mode, exists, err := readNote(path)
	if err != nil {
		return ParsedNote{}, err
	}
	parsed, err := Parse(string(original))
	if err != nil {
		return ParsedNote{}, err
	}
	if parsed.Managed && parsed.Metadata.CanonicalTaskID != taskID {
		return ParsedNote{}, fmt.Errorf("note task %q, requested task %q: %w", parsed.Metadata.CanonicalTaskID, taskID, ErrConflict)
	}
	after := uint64(0)
	if parsed.Managed {
		after = parsed.Metadata.LastAppliedCursor
	}
	queryAfter := after
	if parsed.Managed && after > 0 && parsed.Metadata.LastAppliedTaskCursor == 0 {
		// Upgrade old notes with a full task-scoped replay. Their global cursor
		// cannot establish the task-scoped sequence expected by the projection.
		queryAfter = 0
	}
	observed, err := observer.Observe(ctx, localcontrol.ObserveRequest{TaskID: taskID, AfterCursor: queryAfter, Limit: 200})
	if err != nil {
		return ParsedNote{}, err
	}
	projected, err := ApplyObserved(string(original), observed, state)
	if errors.Is(err, ErrCursorGap) && queryAfter > 0 {
		observed, err = observer.Observe(ctx, localcontrol.ObserveRequest{TaskID: taskID, Limit: 200})
		if err != nil {
			return ParsedNote{}, err
		}
		projected, err = ApplyObserved(string(original), observed, state)
	}
	if err != nil {
		return ParsedNote{}, err
	}
	if projected == string(original) {
		return Parse(projected)
	}
	if err := writeNoteIfUnchanged(path, original, exists, mode, []byte(projected)); err != nil {
		return ParsedNote{}, err
	}
	return Parse(projected)
}

func readNote(path string) ([]byte, os.FileMode, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0o600, false, nil
	}
	if err != nil {
		return nil, 0, false, fmt.Errorf("inspect Obsidian note: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, 0, false, ErrUnsafePath
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, false, fmt.Errorf("read Obsidian note: %w", err)
	}
	return contents, info.Mode().Perm(), true, nil
}

func writeNoteIfUnchanged(path string, original []byte, existed bool, mode os.FileMode, contents []byte) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if existed {
			return ErrNoteChanged
		}
	} else if err != nil {
		return fmt.Errorf("recheck Obsidian note: %w", err)
	} else {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return ErrUnsafePath
		}
		current, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("re-read Obsidian note: %w", readErr)
		}
		if !equalBytes(current, original) {
			return ErrNoteChanged
		}
	}
	directory := filepath.Dir(path)
	directoryInfo, err := os.Lstat(directory)
	if err != nil || !directoryInfo.IsDir() || directoryInfo.Mode()&os.ModeSymlink != 0 {
		return ErrUnsafePath
	}
	temporary, err := os.CreateTemp(directory, ".kovan-note-*")
	if err != nil {
		return fmt.Errorf("create projected note: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode.Perm()); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("set projected note mode: %w", err)
	}
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write projected note: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync projected note: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close projected note: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("install projected note: %w", err)
	}
	if directoryFile, err := os.Open(directory); err == nil {
		_ = directoryFile.Sync()
		_ = directoryFile.Close()
	}
	return nil
}

func equalBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
