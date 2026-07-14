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
	return nil
}
