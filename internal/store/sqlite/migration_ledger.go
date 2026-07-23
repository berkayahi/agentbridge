package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const (
	executionKernelVersion = 10
	executionKernelName    = "010_execution_kernel.sql"
)

func writeMigrationLedger(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, fingerprint string, now time.Time) error {
	checksum, err := executionKernelChecksum()
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO migration_ledger(version, name, checksum, structural_fingerprint, applied_at)
		VALUES (?, ?, ?, ?, ?)`, executionKernelVersion, executionKernelName, checksum, fingerprint, timestamp(now)); err != nil {
		return fmt.Errorf("write migration ledger: %w", err)
	}
	return nil
}

func writeMigrationLedgerTx(ctx context.Context, tx *sql.Tx, now time.Time) error {
	checksum, err := executionKernelChecksum()
	if err != nil {
		return err
	}
	fingerprint, err := schemaFingerprint(ctx, tx)
	if err != nil {
		return fmt.Errorf("fingerprint v2 migration: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO migration_ledger(version, name, checksum, structural_fingerprint, applied_at)
		VALUES (?, ?, ?, ?, ?)`, executionKernelVersion, executionKernelName, checksum, fingerprint, timestamp(now)); err != nil {
		return fmt.Errorf("write migration ledger: %w", err)
	}
	return nil
}

func validateMigrationLedger(ctx context.Context, db *sql.DB) error {
	return validateMigrationLedgerQueryer(ctx, db)
}

func validateMigrationLedgerTx(ctx context.Context, tx *sql.Tx) error {
	return validateMigrationLedgerQueryer(ctx, tx)
}

type migrationLedgerQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func validateMigrationLedgerQueryer(ctx context.Context, db migrationLedgerQueryer) error {
	checksum, err := executionKernelChecksum()
	if err != nil {
		return err
	}
	var rows int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM migration_ledger").Scan(&rows); err != nil {
		return fmt.Errorf("count migration ledger: %w", err)
	}
	if rows != 1 {
		return ErrUnknownLineage
	}
	var version int
	var name, gotChecksum, fingerprint, appliedAt string
	if err := db.QueryRowContext(ctx, `SELECT version, name, checksum, structural_fingerprint, applied_at FROM migration_ledger`).Scan(&version, &name, &gotChecksum, &fingerprint, &appliedAt); err != nil {
		return fmt.Errorf("read migration ledger: %w", err)
	}
	if version != executionKernelVersion || name != executionKernelName || gotChecksum != checksum || fingerprint == "" || appliedAt == "" {
		return ErrUnknownLineage
	}
	if _, err := time.Parse(time.RFC3339Nano, appliedAt); err != nil {
		return ErrUnknownLineage
	}
	current, err := schemaFingerprint(ctx, db)
	if err != nil {
		return err
	}
	if current != fingerprint {
		return ErrUnknownLineage
	}
	return nil
}
