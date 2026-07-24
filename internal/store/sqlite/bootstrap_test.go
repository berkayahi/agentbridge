package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
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
	for _, table := range []string{"migration_ledger", "local_projects", "local_boards", "local_task_contexts", "local_control_events", "local_command_idempotency", "local_devices", "local_pairing_challenges", "local_task_devices", "local_device_link_counters", "local_device_commands", "local_tasks", "executions", "sessions", "repository_bindings", "git_checkpoints", "git_operations", "command_inbox", "command_results"} {
		var count int
		if err := check.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("missing v2 table %q", table)
		}
	}
	var ledgerRows int
	if err := check.QueryRow("SELECT COUNT(*) FROM migration_ledger").Scan(&ledgerRows); err != nil {
		t.Fatal(err)
	}
	if ledgerRows != 9 {
		t.Fatalf("v2 migration ledger rows = %d, want 9", ledgerRows)
	}
	if _, err := Open(context.Background(), path); err == nil {
		t.Fatal("legacy Open accepted an existing v2 database")
	}
	if _, err := OpenV2(context.Background(), path); err != nil && !errors.Is(err, ErrMigrationRequired) {
		t.Fatalf("OpenV2(existing) = %v", err)
	}
}

func TestOpenV2MigratesPriorV2Prefix(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agentbridge-v2-prefix.db")
	db, err := openRaw(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := bootstrapV2(ctx, db, time.Now().UTC()); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO local_tasks (id, title, prompt, state, provider, created_at, updated_at) VALUES ('legacy-task', 'Imported task', 'Continue task', 'queued', 'codex', '2026-07-23T00:00:00Z', '2026-07-23T00:00:00Z')`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenV2(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var ledgerRows int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM migration_ledger").Scan(&ledgerRows); err != nil {
		t.Fatal(err)
	}
	if ledgerRows != 9 {
		t.Fatalf("migrated v2 ledger rows = %d, want 9", ledgerRows)
	}
	var counterTable int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'local_device_link_counters'").Scan(&counterTable); err != nil {
		t.Fatal(err)
	}
	if counterTable != 1 {
		t.Fatal("v2 prefix migration did not install device link counters")
	}
	var contextRows int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM local_task_contexts WHERE local_task_id = 'legacy-task'").Scan(&contextRows); err != nil {
		t.Fatal(err)
	}
	if contextRows != 1 {
		t.Fatal("v2 prefix migration did not backfill the legacy task context")
	}
}

func TestOpenV2BackfillsTaskEventCursors(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agentbridge-v2-task-cursors.db")
	db, err := openRaw(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := bootstrapV2(ctx, db, time.Now().UTC()); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	applyV2PrefixForTest(t, ctx, db, deviceCommandVersion)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO local_tasks (id, title, prompt, state, provider, created_at, updated_at)
		VALUES ('cursor-task', 'Cursor task', 'Observe', 'queued', 'codex', '2026-07-23T00:00:00Z', '2026-07-23T00:00:00Z');
		INSERT INTO local_control_events (id, resource_type, resource_id, local_task_id, revision, event_type, payload, created_at)
		VALUES ('cursor-event-1', 'task', 'cursor-task', 'cursor-task', 1, 'task_created', '{}', '2026-07-23T00:00:00Z');
		INSERT INTO local_control_events (id, resource_type, resource_id, local_task_id, revision, event_type, payload, created_at)
		VALUES ('cursor-noise', 'device', 'pi-one', NULL, 1, 'device_observed', '{}', '2026-07-23T00:00:01Z');
		INSERT INTO local_control_events (id, resource_type, resource_id, local_task_id, revision, event_type, payload, created_at)
		VALUES ('cursor-event-2', 'task', 'cursor-task', 'cursor-task', 2, 'task_updated', '{}', '2026-07-23T00:00:02Z');
	`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenV2(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var first, noise, second int64
	if err := store.db.QueryRowContext(ctx, `SELECT task_cursor FROM local_control_events WHERE id = 'cursor-event-1'`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT task_cursor FROM local_control_events WHERE id = 'cursor-noise'`).Scan(&noise); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT task_cursor FROM local_control_events WHERE id = 'cursor-event-2'`).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if first != 1 || noise != 0 || second != 2 {
		t.Fatalf("backfilled task cursors = %d, %d, %d; want 1, 0, 2", first, noise, second)
	}
}

func applyV2PrefixForTest(t *testing.T, ctx context.Context, db *sql.DB, through int) {
	t.Helper()
	for _, migration := range v2MigrationDefinitions() {
		if migration.version > through {
			break
		}
		schema, err := localControlSchemaFS.ReadFile("schema/" + migration.name)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, string(schema)); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		fingerprint, err := schemaFingerprint(ctx, tx)
		if err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO migration_ledger(version, name, checksum, structural_fingerprint, applied_at)
			VALUES (?, ?, ?, ?, ?)`, migration.version, migration.name, checksum(string(schema)), fingerprint, timestamp(time.Now().UTC())); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
}
