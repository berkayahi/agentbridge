package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrations embed.FS

type migration struct {
	version int
	name    string
	sql     string
}

func migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			applied_at TEXT NOT NULL
		)`); err != nil {
		return fmt.Errorf("create schema migrations: %w", err)
	}

	available, err := readMigrations()
	if err != nil {
		return err
	}
	applied, err := appliedMigrations(ctx, db)
	if err != nil {
		return err
	}
	for _, next := range available {
		if applied[next.version] {
			continue
		}
		if next.version == 3 && next.name == "003_attachment_sha256.sql" {
			hasChecksum, err := attachmentChecksumExists(ctx, db)
			if err != nil {
				return err
			}
			if hasChecksum {
				if err := adoptAttachmentChecksumMigration(ctx, db, next); err != nil {
					return err
				}
				continue
			}
		}
		if err := applyMigration(ctx, db, next); err != nil {
			return err
		}
	}
	return nil
}

func readMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations directory: %w", err)
	}
	available := make([]migration, 0, len(entries))
	versions := make(map[int]string, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		separator := strings.IndexByte(entry.Name(), '_')
		if separator <= 0 {
			return nil, fmt.Errorf("invalid migration filename %q", entry.Name())
		}
		version, err := strconv.Atoi(entry.Name()[:separator])
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("invalid migration version in %q", entry.Name())
		}
		if previous, exists := versions[version]; exists {
			return nil, fmt.Errorf("duplicate migration version %d in %q and %q", version, previous, entry.Name())
		}
		body, err := migrations.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", entry.Name(), err)
		}
		versions[version] = entry.Name()
		available = append(available, migration{version: version, name: entry.Name(), sql: string(body)})
	}
	sort.Slice(available, func(i, j int) bool { return available[i].version < available[j].version })
	return available, nil
}

func appliedMigrations(ctx context.Context, db *sql.DB) (map[int]bool, error) {
	rows, err := db.QueryContext(ctx, "SELECT version FROM schema_migrations ORDER BY version")
	if err != nil {
		return nil, fmt.Errorf("query schema migrations: %w", err)
	}
	defer rows.Close()
	applied := make(map[int]bool)
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("scan schema migration: %w", err)
		}
		applied[version] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schema migrations: %w", err)
	}
	return applied, nil
}

func applyMigration(ctx context.Context, db *sql.DB, next migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %s: %w", next.name, err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, next.sql); err != nil {
		return fmt.Errorf("apply migration %s: %w", next.name, err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)",
		next.version, next.name, timestamp(time.Now()),
	); err != nil {
		return fmt.Errorf("record migration %s: %w", next.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %s: %w", next.name, err)
	}
	return nil
}

func adoptAttachmentChecksumMigration(ctx context.Context, db *sql.DB, next migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration adoption %s: %w", next.name, err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		CREATE UNIQUE INDEX IF NOT EXISTS attachments_task_sha256_idx
		ON attachments(task_id, sha256) WHERE sha256 <> ''`); err != nil {
		return fmt.Errorf("repair adopted migration %s: %w", next.name, err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)",
		next.version, next.name, timestamp(time.Now()),
	); err != nil {
		return fmt.Errorf("record adopted migration %s: %w", next.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit adopted migration %s: %w", next.name, err)
	}
	return nil
}

func attachmentChecksumExists(ctx context.Context, db *sql.DB) (bool, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info(attachments)")
	if err != nil {
		return false, fmt.Errorf("inspect attachment schema: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notnull, &defaultValue, &pk); err != nil {
			return false, fmt.Errorf("scan attachment schema: %w", err)
		}
		if name == "sha256" {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("inspect attachment schema: %w", err)
	}
	return false, nil
}
