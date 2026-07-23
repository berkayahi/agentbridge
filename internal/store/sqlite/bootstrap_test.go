package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestOpenV2FreshBootstrap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agentbridge-v2.db")
	store, err := OpenV2(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	check, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	for _, table := range []string{"migration_ledger", "local_tasks", "executions", "sessions", "repository_bindings", "git_checkpoints", "git_operations", "command_inbox", "command_results"} {
		var count int
		if err := check.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("missing v2 table %q", table)
		}
	}
	if _, err := Open(context.Background(), path); err == nil {
		t.Fatal("legacy Open accepted an existing v2 database")
	}
	if _, err := OpenV2(context.Background(), path); err != nil && !errors.Is(err, ErrMigrationRequired) {
		t.Fatalf("OpenV2(existing) = %v", err)
	}
}
