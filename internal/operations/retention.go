package operations

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// RetentionOptions controls cleanup after a verified database snapshot has
// been written. A zero duration disables that cleanup; negative durations are
// rejected so an operator cannot accidentally turn retention into a purge.
type RetentionOptions struct {
	BackupDirectory   string
	AttachmentRoot    string
	WorktreeRoot      string
	PinnedTasksPath   string
	EventRetention    time.Duration
	ArtifactRetention time.Duration
	BackupRetention   time.Duration
	Now               func() time.Time
}

var (
	retentionTaskID = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)
	inactiveStates  = []string{"queued", "preparing", "running", "awaiting_approval", "awaiting_auth", "verifying", "committing", "pushing", "paused"}
	artifactStates  = []string{"failed", "canceled"}
)

func applyRetention(ctx context.Context, db *sql.DB, options RetentionOptions) error {
	if db == nil {
		return errors.New("retention requires an open database")
	}
	if options.EventRetention < 0 || options.ArtifactRetention < 0 || options.BackupRetention < 0 {
		return errors.New("retention durations cannot be negative")
	}
	if options.EventRetention == 0 && options.ArtifactRetention == 0 && options.BackupRetention == 0 {
		return nil
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	pinned, err := loadPinnedTasks(options.PinnedTasksPath)
	if err != nil {
		return err
	}
	if options.EventRetention > 0 {
		if err := pruneEvents(ctx, db, now().UTC().Add(-options.EventRetention), pinned); err != nil {
			return err
		}
	}
	if options.ArtifactRetention > 0 {
		current := now().UTC()
		if err := pruneArtifacts(ctx, db, current.Add(-options.ArtifactRetention), current, options, pinned); err != nil {
			return err
		}
	}
	if options.BackupRetention > 0 {
		if err := pruneBackups(ctx, options.BackupDirectory, now().UTC().Add(-options.BackupRetention)); err != nil {
			return err
		}
	}
	return nil
}

func loadPinnedTasks(path string) ([]string, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("open pinned task list: %w", err)
	}
	defer file.Close()

	seen := make(map[string]struct{})
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024), 64*1024)
	for scanner.Scan() {
		value := strings.TrimSpace(scanner.Text())
		if value == "" || strings.HasPrefix(value, "#") {
			continue
		}
		if !retentionTaskID.MatchString(value) {
			return nil, fmt.Errorf("invalid pinned task ID %q", value)
		}
		seen[value] = struct{}{}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read pinned task list: %w", err)
	}
	values := make([]string, 0, len(seen))
	for value := range seen {
		values = append(values, value)
	}
	sort.Strings(values)
	return values, nil
}

func pruneEvents(ctx context.Context, db *sql.DB, cutoff time.Time, pinned []string) error {
	predicate, args := taskPredicate("t", inactiveStates, pinned)
	query := `DELETE FROM execution_events
		WHERE datetime(created_at) < datetime(?)
		  AND local_task_id IN (SELECT t.id FROM local_tasks t WHERE ` + predicate + `)`
	args = append([]any{cutoff.Format(time.RFC3339Nano)}, args...)
	if _, err := db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("prune inactive events: %w", err)
	}
	return nil
}

func pruneArtifacts(ctx context.Context, db *sql.DB, cutoff, updatedAt time.Time, options RetentionOptions, pinned []string) error {
	attachmentPaths, err := attachmentPaths(ctx, db, cutoff, pinned)
	if err != nil {
		return err
	}
	worktreePaths, err := worktreePaths(ctx, db, cutoff, pinned)
	if err != nil {
		return err
	}
	if len(attachmentPaths) > 0 && strings.TrimSpace(options.AttachmentRoot) == "" {
		return errors.New("attachment retention requires an attachment root")
	}
	if len(worktreePaths) > 0 && strings.TrimSpace(options.WorktreeRoot) == "" {
		return errors.New("worktree retention requires a worktree root")
	}
	for _, path := range attachmentPaths {
		if err := removeManagedPath(options.AttachmentRoot, path); err != nil {
			return fmt.Errorf("remove retained attachment %q: %w", path, err)
		}
	}
	for _, path := range worktreePaths {
		if err := removeManagedPath(options.WorktreeRoot, path); err != nil {
			return fmt.Errorf("remove retained worktree %q: %w", path, err)
		}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin artifact retention: %w", err)
	}
	defer tx.Rollback()
	predicate, args := taskPredicate("t", inactiveStates, pinned)
	query := `DELETE FROM attachments
		WHERE datetime(created_at) < datetime(?)
		  AND local_task_id IN (SELECT t.id FROM local_tasks t WHERE ` + predicate + `)`
	args = append([]any{cutoff.Format(time.RFC3339Nano)}, args...)
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("prune attachment records: %w", err)
	}

	predicate, predicateArgs := pinnedPredicate("t", pinned)
	query = `UPDATE local_tasks AS t
		SET worktree_path = '', updated_at = ?
		WHERE t.state IN (?, ?)
		  AND datetime(t.updated_at) < datetime(?)
		  AND ` + predicate
	updateArgs := []any{updatedAt.Format(time.RFC3339Nano), artifactStates[0], artifactStates[1], cutoff.Format(time.RFC3339Nano)}
	updateArgs = append(updateArgs, predicateArgs...)
	if _, err := tx.ExecContext(ctx, query, updateArgs...); err != nil {
		return fmt.Errorf("clear retained worktree records: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit artifact retention: %w", err)
	}
	return nil
}

func attachmentPaths(ctx context.Context, db *sql.DB, cutoff time.Time, pinned []string) ([]string, error) {
	predicate, args := taskPredicate("t", inactiveStates, pinned)
	query := `SELECT a.storage_path
		FROM attachments a JOIN local_tasks t ON t.id = a.local_task_id
		WHERE datetime(a.created_at) < datetime(?) AND ` + predicate
	args = append([]any{cutoff.Format(time.RFC3339Nano)}, args...)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list retained attachments: %w", err)
	}
	defer rows.Close()
	var values []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, fmt.Errorf("scan retained attachment: %w", err)
		}
		if strings.TrimSpace(path) != "" {
			values = append(values, path)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read retained attachments: %w", err)
	}
	return values, nil
}

func worktreePaths(ctx context.Context, db *sql.DB, cutoff time.Time, pinned []string) ([]string, error) {
	predicate, args := pinnedPredicate("t", pinned)
	query := `SELECT t.worktree_path FROM local_tasks t
		WHERE t.state IN (?, ?) AND datetime(t.updated_at) < datetime(?) AND ` + predicate
	args = append([]any{artifactStates[0], artifactStates[1], cutoff.Format(time.RFC3339Nano)}, args...)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list retained worktrees: %w", err)
	}
	defer rows.Close()
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, fmt.Errorf("scan retained worktree: %w", err)
		}
		if strings.TrimSpace(value) != "" {
			values = append(values, value)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read retained worktrees: %w", err)
	}
	return values, nil
}

func taskPredicate(alias string, states, pinned []string) (string, []any) {
	clauses := []string{alias + ".state NOT IN (" + placeholders(len(states)) + ")"}
	args := make([]any, 0, len(states)+len(pinned))
	for _, state := range states {
		args = append(args, state)
	}
	pinnedClause, pinnedArgs := pinnedPredicate(alias, pinned)
	if pinnedClause != "1=1" {
		clauses = append(clauses, pinnedClause)
		args = append(args, pinnedArgs...)
	}
	return strings.Join(clauses, " AND "), args
}

func pinnedPredicate(alias string, pinned []string) (string, []any) {
	if len(pinned) == 0 {
		return "1=1", nil
	}
	args := make([]any, 0, len(pinned))
	for _, id := range pinned {
		args = append(args, id)
	}
	return alias + ".id NOT IN (" + placeholders(len(pinned)) + ")", args
}

func placeholders(count int) string {
	values := make([]string, count)
	for index := range values {
		values[index] = "?"
	}
	return strings.Join(values, ", ")
}

func removeManagedPath(root, storedPath string) error {
	root = filepath.Clean(strings.TrimSpace(root))
	if root == "." || root == "" {
		return errors.New("managed root is empty")
	}
	rootInfo, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect managed root: %w", err)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return errors.New("managed root must be a real directory")
	}
	candidate := strings.TrimSpace(storedPath)
	if candidate == "" {
		return nil
	}
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(root, candidate)
	}
	candidate = filepath.Clean(candidate)
	relative, err := filepath.Rel(root, candidate)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return errors.New("path escapes managed root")
	}
	if err := rejectSymlinkParents(root, relative); err != nil {
		return err
	}
	info, err := os.Lstat(candidate)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect managed path: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		if err := os.Remove(candidate); err != nil {
			return fmt.Errorf("remove managed file: %w", err)
		}
		return nil
	}
	if err := os.RemoveAll(candidate); err != nil {
		return fmt.Errorf("remove managed directory: %w", err)
	}
	return nil
}

func rejectSymlinkParents(root, relative string) error {
	parent := filepath.Dir(relative)
	if parent == "." {
		return nil
	}
	current := root
	for _, part := range strings.Split(parent, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect managed parent: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("managed path crosses a symlink")
		}
		if !info.IsDir() {
			return errors.New("managed path parent is not a directory")
		}
	}
	return nil
}

func pruneBackups(ctx context.Context, directory string, cutoff time.Time) error {
	if strings.TrimSpace(directory) == "" {
		return errors.New("backup retention requires a backup directory")
	}
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("list retained backups: %w", err)
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "agentbridge-") || !strings.HasSuffix(name, ".db") {
			continue
		}
		backupPath := filepath.Join(directory, name)
		info, err := os.Lstat(backupPath)
		if err != nil {
			return fmt.Errorf("inspect backup %q: %w", name, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
			continue
		}
		manifest := filepath.Join(directory, name+".manifest.json")
		manifestInfo, err := os.Lstat(manifest)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect backup manifest %q: %w", name, err)
		}
		if manifestInfo.Mode()&os.ModeSymlink != 0 || !manifestInfo.Mode().IsRegular() {
			return fmt.Errorf("backup manifest %q is not a regular file", name)
		}
		if err := os.Remove(backupPath); err != nil {
			return fmt.Errorf("remove backup %q: %w", name, err)
		}
		if err := os.Remove(manifest); err != nil {
			return fmt.Errorf("remove backup manifest %q: %w", name, err)
		}
	}
	return nil
}
