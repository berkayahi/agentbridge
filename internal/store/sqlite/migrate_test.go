package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestMigrationUpgradesLegacyTimestampsOnce(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	seedLegacyDatabase(t, path)

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open(legacy): %v", err)
	}
	assertTimestampMigration(t, db)
	if err := db.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}

	db, err = Open(ctx, path)
	if err != nil {
		t.Fatalf("Open(migrated): %v", err)
	}
	defer db.Close()
	assertTimestampMigration(t, db)
}

func TestTimestampMigrationRejectsNonUTCDataTransactionally(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalid-legacy.db")
	seedLegacyDatabase(t, path)
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec("UPDATE tasks SET created_at = '2026-07-14T11:00:00+03:00'"); err != nil {
		t.Fatal(err)
	}
	_ = legacy.Close()

	if db, err := Open(context.Background(), path); err == nil {
		_ = db.Close()
		t.Fatal("Open() accepted legacy timestamp outside the documented UTC-Z contract")
	}
	check, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var versions int
	if err := check.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 1 {
		t.Fatalf("recorded migration count = %d, want only migration 1", versions)
	}
	var created string
	if err := check.QueryRow("SELECT created_at FROM tasks WHERE id = 'legacy-task'").Scan(&created); err != nil {
		t.Fatal(err)
	}
	if created != "2026-07-14T11:00:00+03:00" {
		t.Fatalf("failed migration modified timestamp to %q", created)
	}
}

func seedLegacyDatabase(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	initial, err := migrations.ReadFile("migrations/001_initial.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(initial)); err != nil {
		t.Fatalf("apply legacy schema: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO tasks (
			id, repo_profile_id, title, prompt, state, provider,
			telegram_chat_id, telegram_message_id, base_sha, worktree_path,
			created_at, updated_at
		) VALUES ('legacy-task', 'repo', 'Legacy', 'work', 'queued', 'codex', 1, 1, 'abc', '/tmp/work',
		          '2026-07-14T08:00:00Z', '2026-07-14T08:00:00.1Z');
		INSERT INTO task_events (id, task_id, event_type, visibility, redacted_payload, created_at)
		VALUES ('first', 'legacy-task', 'task_created', 'internal', '{}', '2026-07-14T08:00:00Z'),
		       ('second', 'legacy-task', 'provider_message', 'internal', '{}', '2026-07-14T08:00:00.1Z');
		INSERT INTO repository_leases (repo_profile_id, owner_id, acquired_at, heartbeat_at, expires_at)
		VALUES ('repo', 'worker', '2026-07-14T07:59:59.01Z', '2026-07-14T07:59:59.001Z', '2026-07-14T08:00:00Z');
	`); err != nil {
		t.Fatalf("seed legacy rows: %v", err)
	}
}

func assertTimestampMigration(t *testing.T, db *Store) {
	t.Helper()
	var versions int
	if err := db.db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&versions); err != nil {
		t.Fatalf("read schema migrations: %v", err)
	}
	if versions != 2 {
		t.Fatalf("schema migration count = %d, want 2", versions)
	}
	wantValues := map[string]string{
		"task created":    "2026-07-14T08:00:00.000000000Z",
		"task updated":    "2026-07-14T08:00:00.100000000Z",
		"lease acquired":  "2026-07-14T07:59:59.010000000Z",
		"lease heartbeat": "2026-07-14T07:59:59.001000000Z",
	}
	queries := map[string]string{
		"task created":    "SELECT created_at FROM tasks WHERE id = 'legacy-task'",
		"task updated":    "SELECT updated_at FROM tasks WHERE id = 'legacy-task'",
		"lease acquired":  "SELECT acquired_at FROM repository_leases WHERE repo_profile_id = 'repo'",
		"lease heartbeat": "SELECT heartbeat_at FROM repository_leases WHERE repo_profile_id = 'repo'",
	}
	for name, query := range queries {
		var got string
		if err := db.db.QueryRow(query).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != wantValues[name] {
			t.Fatalf("%s = %q, want lossless %q", name, got, wantValues[name])
		}
	}

	rows, err := db.db.Query(`
		SELECT created_at FROM tasks
		UNION ALL SELECT updated_at FROM tasks
		UNION ALL SELECT created_at FROM task_events
		UNION ALL SELECT acquired_at FROM repository_leases
		UNION ALL SELECT heartbeat_at FROM repository_leases
		UNION ALL SELECT expires_at FROM repository_leases`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		if len(value) != len("2026-07-14T08:00:00.000000000Z") {
			t.Fatalf("timestamp %q was not normalized to nine fractional digits", value)
		}
		if _, err := time.Parse(timestampLayout, value); err != nil {
			t.Fatalf("timestamp %q is not fixed-width UTC: %v", value, err)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	events, err := db.Events(context.Background(), "legacy-task")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].ID != "first" || events[1].ID != "second" {
		t.Fatalf("migrated event order = %#v", events)
	}

	base := time.Date(2026, time.July, 14, 8, 0, 0, 0, time.UTC)
	var expired int
	if err := db.db.QueryRow(
		"SELECT expires_at <= ? FROM repository_leases WHERE repo_profile_id = 'repo'",
		timestamp(base.Add(100*time.Millisecond)),
	).Scan(&expired); err != nil {
		t.Fatal(err)
	}
	if expired != 1 {
		t.Fatal("normalized lease expiry did not compare chronologically")
	}
}
