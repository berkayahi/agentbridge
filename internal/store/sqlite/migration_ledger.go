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
	executionChecksum, err := executionKernelChecksum()
	if err != nil {
		return err
	}
	localChecksum, err := localControlChecksum()
	if err != nil {
		return err
	}
	deviceChecksum, err := deviceRoutingChecksum()
	if err != nil {
		return err
	}
	linkChecksum, err := deviceLinkChecksum()
	if err != nil {
		return err
	}
	backfillChecksum, err := localBackfillChecksum()
	if err != nil {
		return err
	}
	deviceCommandSchemaChecksum, err := deviceCommandChecksum()
	if err != nil {
		return err
	}
	taskCursorSchemaChecksum, err := taskCursorChecksum()
	if err != nil {
		return err
	}
	remoteCursorSchemaChecksum, err := remoteCursorChecksum()
	if err != nil {
		return err
	}
	controllerOwnerSchemaChecksum, err := controllerOwnerChecksum()
	if err != nil {
		return err
	}
	expected := map[int]struct {
		name     string
		checksum string
	}{
		executionKernelVersion: {name: executionKernelName, checksum: executionChecksum},
		localControlVersion:    {name: localControlName, checksum: localChecksum},
		deviceRoutingVersion:   {name: deviceRoutingName, checksum: deviceChecksum},
		deviceLinkVersion:      {name: deviceLinkName, checksum: linkChecksum},
		localBackfillVersion:   {name: localBackfillName, checksum: backfillChecksum},
		deviceCommandVersion:   {name: deviceCommandName, checksum: deviceCommandSchemaChecksum},
		taskCursorVersion:      {name: taskCursorName, checksum: taskCursorSchemaChecksum},
		remoteCursorVersion:    {name: remoteCursorName, checksum: remoteCursorSchemaChecksum},
		controllerOwnerVersion: {name: controllerOwnerName, checksum: controllerOwnerSchemaChecksum},
	}
	rows, err := db.QueryContext(ctx, `SELECT version, name, checksum, structural_fingerprint, applied_at FROM migration_ledger ORDER BY version`)
	if err != nil {
		return fmt.Errorf("read migration ledger: %w", err)
	}
	defer rows.Close()
	versions := make([]int, 0, len(expected))
	var latestFingerprint string
	for rows.Next() {
		var version int
		var name, gotChecksum, fingerprint, appliedAt string
		if err := rows.Scan(&version, &name, &gotChecksum, &fingerprint, &appliedAt); err != nil {
			return fmt.Errorf("scan migration ledger: %w", err)
		}
		want, ok := expected[version]
		if !ok || name != want.name || gotChecksum != want.checksum || fingerprint == "" || appliedAt == "" {
			return ErrUnknownLineage
		}
		if _, err := time.Parse(time.RFC3339Nano, appliedAt); err != nil {
			return ErrUnknownLineage
		}
		versions = append(versions, version)
		latestFingerprint = fingerprint
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate migration ledger: %w", err)
	}
	if len(versions) == 0 || versions[0] != executionKernelVersion || !strictlyIncreasing(versions) || !contiguousMigrationVersions(versions) {
		return ErrUnknownLineage
	}
	current, err := schemaFingerprint(ctx, db)
	if err != nil {
		return err
	}
	if current != latestFingerprint {
		return ErrUnknownLineage
	}
	return nil
}

func contiguousMigrationVersions(versions []int) bool {
	if len(versions) == 0 || versions[0] != executionKernelVersion {
		return false
	}
	for index := 1; index < len(versions); index++ {
		if versions[index] != versions[index-1]+1 {
			return false
		}
	}
	return versions[len(versions)-1] <= controllerOwnerVersion
}
