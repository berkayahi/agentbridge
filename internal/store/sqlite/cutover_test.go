package sqlite

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestCutoverCreatesVerifiedBackupAndMapsLegacyTask(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.db.Exec(`
		INSERT INTO tasks (id, repo_profile_id, title, prompt, state, provider, telegram_chat_id, telegram_message_id, base_sha, worktree_path, created_at, updated_at)
		VALUES ('task-1', 'repository-1', 'Legacy task', 'Repair this', 'queued', 'codex', 1, 1, '', '', '2026-07-01T00:00:00.000000000Z', '2026-07-01T00:00:00.000000000Z')`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	result, err := Cutover(context.Background(), path, "test-build")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{result.BackupPath, result.ManifestPath} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o", name, info.Mode().Perm())
		}
	}

	check, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var tasks, executions, legacyTables int
	if err := check.QueryRow("SELECT COUNT(*) FROM local_tasks").Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if err := check.QueryRow("SELECT COUNT(*) FROM executions").Scan(&executions); err != nil {
		t.Fatal(err)
	}
	if err := check.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'tasks'").Scan(&legacyTables); err != nil {
		t.Fatal(err)
	}
	if tasks != 1 || executions != 1 || legacyTables != 0 {
		t.Fatalf("tasks=%d executions=%d legacy tables=%d", tasks, executions, legacyTables)
	}
	if err := check.Close(); err != nil {
		t.Fatal(err)
	}
	v2, err := OpenV2(context.Background(), path)
	if err != nil {
		t.Fatalf("OpenV2(after cutover) = %v", err)
	}
	if err := v2.Close(); err != nil {
		t.Fatal(err)
	}
}
