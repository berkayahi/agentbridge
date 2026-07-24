package operations

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/store/sqlite"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

func TestBackupSnapshotsBeforeRetentionAndHonorsPins(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database := filepath.Join(root, "agentbridge.db")
	attachmentRoot := filepath.Join(root, "attachments")
	worktreeRoot := filepath.Join(root, "worktrees")
	backupRoot := filepath.Join(root, "backups")
	for _, path := range []string{attachmentRoot, worktreeRoot, backupRoot} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	old := now.Add(-48 * time.Hour)
	data, err := sqlite.OpenV2(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range []struct {
		id    string
		state workmodel.State
	}{
		{id: "expired", state: workmodel.Failed},
		{id: "active", state: workmodel.Running},
		{id: "pinned", state: workmodel.Failed},
	} {
		if err := data.CreateTask(ctx, workmodel.Task{
			ID: task.id, RepoProfileID: "repo", Title: task.id, Prompt: "retention", State: task.state,
			Provider: workmodel.CodexSubscription, CreatedAt: old, UpdatedAt: old,
		}, workmodel.Event{
			ID: task.id + "-created", TaskID: task.id, Type: workmodel.EventTaskCreated,
			Visibility: workmodel.VisibilityInternal, Payload: []byte(`{"task":"created"}`), CreatedAt: old,
		}); err != nil {
			t.Fatal(err)
		}
		attachment := filepath.Join(attachmentRoot, task.id+".txt")
		if err := os.WriteFile(attachment, []byte(task.id), 0o600); err != nil {
			t.Fatal(err)
		}
		worktree := filepath.Join(worktreeRoot, task.id)
		if err := os.MkdirAll(worktree, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(worktree, "result.txt"), []byte(task.id), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := data.SaveWorkspace(ctx, task.id, "base", worktree); err != nil {
			t.Fatal(err)
		}
		if err := data.SaveAttachment(ctx, workmodel.Attachment{
			ID: task.id + "-attachment", TaskID: task.id, Kind: "result", Name: task.id + ".txt",
			MediaType: "text/plain", StoragePath: attachment, SizeBytes: int64(len(task.id)), SHA256: task.id, CreatedAt: old,
		}); err != nil {
			t.Fatal(err)
		}
		if err := data.AppendEvent(ctx, workmodel.Event{
			ID: task.id + "-old-event", TaskID: task.id, Type: workmodel.EventProviderMessage,
			Visibility: workmodel.VisibilityUser, Payload: []byte(`{"message":"old"}`), CreatedAt: old,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := data.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := openDatabase(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.ExecContext(ctx, `UPDATE local_tasks SET updated_at = ?, worktree_path = ? WHERE id = ?`, old.Format(time.RFC3339Nano), filepath.Join(worktreeRoot, "expired"), "expired")
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.ExecContext(ctx, `UPDATE local_tasks SET updated_at = ?, worktree_path = ? WHERE id = ?`, old.Format(time.RFC3339Nano), filepath.Join(worktreeRoot, "active"), "active")
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.ExecContext(ctx, `UPDATE local_tasks SET updated_at = ?, worktree_path = ? WHERE id = ?`, old.Format(time.RFC3339Nano), filepath.Join(worktreeRoot, "pinned"), "pinned")
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	pinnedPath := filepath.Join(root, "pinned-task-ids")
	if err := os.WriteFile(pinnedPath, []byte("# keep this task\npinned\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldBackup := filepath.Join(backupRoot, "agentbridge-old.db")
	if err := os.WriteFile(oldBackup, []byte("old backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldBackup+".manifest.json", []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(oldBackup, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(oldBackup+".manifest.json", old, old); err != nil {
		t.Fatal(err)
	}

	result, err := Backup(ctx, BackupOptions{
		Database: database, Output: backupRoot, AttachmentRoot: attachmentRoot, WorktreeRoot: worktreeRoot,
		PinnedTasksPath: pinnedPath, EventRetention: 24 * time.Hour, ArtifactRetention: 24 * time.Hour,
		BackupRetention: 24 * time.Hour, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldBackup); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old backup exists after retention: %v", err)
	}
	if _, err := os.Stat(result.Database); err != nil {
		t.Fatal(err)
	}

	check, err := openDatabase(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	for _, task := range []struct {
		id       string
		preserve bool
	}{
		{id: "expired", preserve: false},
		{id: "active", preserve: true},
		{id: "pinned", preserve: true},
	} {
		var events, attachments int
		if err := check.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_events WHERE local_task_id = ?`, task.id).Scan(&events); err != nil {
			t.Fatal(err)
		}
		if err := check.QueryRowContext(ctx, `SELECT COUNT(*) FROM attachments WHERE local_task_id = ?`, task.id).Scan(&attachments); err != nil {
			t.Fatal(err)
		}
		if task.preserve && (events == 0 || attachments == 0) {
			t.Fatalf("task %s was pruned: events=%d attachments=%d", task.id, events, attachments)
		}
		if !task.preserve && (events != 0 || attachments != 0) {
			t.Fatalf("task %s was retained: events=%d attachments=%d", task.id, events, attachments)
		}
	}
	for _, task := range []string{"expired", "active", "pinned"} {
		var worktree string
		if err := check.QueryRowContext(ctx, `SELECT worktree_path FROM local_tasks WHERE id = ?`, task).Scan(&worktree); err != nil {
			t.Fatal(err)
		}
		wantEmpty := task == "expired"
		if (worktree == "") != wantEmpty {
			t.Fatalf("task %s worktree = %q, want empty=%v", task, worktree, wantEmpty)
		}
	}
	for _, task := range []string{"expired", "active", "pinned"} {
		_, err := os.Stat(filepath.Join(attachmentRoot, task+".txt"))
		removed := errors.Is(err, os.ErrNotExist)
		if removed != (task == "expired") {
			t.Fatalf("attachment %s removed=%v", task, removed)
		}
		_, err = os.Stat(filepath.Join(worktreeRoot, task))
		removed = errors.Is(err, os.ErrNotExist)
		if removed != (task == "expired") {
			t.Fatalf("worktree %s removed=%v", task, removed)
		}
	}

	snapshot, err := openDatabase(ctx, result.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	var snapshotEvents int
	if err := snapshot.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_events WHERE local_task_id = 'expired'`).Scan(&snapshotEvents); err != nil {
		t.Fatal(err)
	}
	if snapshotEvents == 0 {
		t.Fatal("verified backup was created after retention instead of before it")
	}
}

func TestRemoveManagedPathRejectsEscapesAndSymlinkParents(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(outsideFile, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeManagedPath(root, outsideFile); err == nil {
		t.Fatal("path escape was accepted")
	}
	if _, err := os.Stat(outsideFile); err != nil {
		t.Fatalf("outside file changed after escape rejection: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := removeManagedPath(root, filepath.Join("link", "keep.txt")); err == nil {
		t.Fatal("symlink parent was accepted")
	}
	if _, err := os.Stat(outsideFile); err != nil {
		t.Fatalf("outside file changed after symlink rejection: %v", err)
	}
}
