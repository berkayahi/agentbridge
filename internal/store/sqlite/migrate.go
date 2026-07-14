package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
)

//go:embed migrations/*.sql
var migrations embed.FS

func migrate(ctx context.Context, db *sql.DB) error {
	initial, err := migrations.ReadFile("migrations/001_initial.sql")
	if err != nil {
		return fmt.Errorf("read initial migration: %w", err)
	}
	if _, err := db.ExecContext(ctx, string(initial)); err != nil {
		return fmt.Errorf("apply initial migration: %w", err)
	}
	hasChecksum, err := attachmentChecksumExists(ctx, db)
	if err != nil {
		return err
	}
	if !hasChecksum {
		checksum, err := migrations.ReadFile("migrations/002_attachment_sha256.sql")
		if err != nil {
			return fmt.Errorf("read attachment checksum migration: %w", err)
		}
		if _, err := db.ExecContext(ctx, string(checksum)); err != nil {
			return fmt.Errorf("apply attachment checksum migration: %w", err)
		}
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
