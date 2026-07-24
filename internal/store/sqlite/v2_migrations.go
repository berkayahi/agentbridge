package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"fmt"
	"time"
)

//go:embed schema/011_local_control.sql schema/012_device_routing.sql schema/013_device_link_sequences.sql schema/014_local_control_backfill.sql schema/015_device_command_queue.sql schema/016_task_event_cursors.sql schema/017_remote_observation_cursors.sql schema/018_controller_ownership.sql
var localControlSchemaFS embed.FS

const (
	localControlVersion    = 11
	localControlName       = "011_local_control.sql"
	deviceRoutingVersion   = 12
	deviceRoutingName      = "012_device_routing.sql"
	deviceLinkVersion      = 13
	deviceLinkName         = "013_device_link_sequences.sql"
	localBackfillVersion   = 14
	localBackfillName      = "014_local_control_backfill.sql"
	deviceCommandVersion   = 15
	deviceCommandName      = "015_device_command_queue.sql"
	taskCursorVersion      = 16
	taskCursorName         = "016_task_event_cursors.sql"
	remoteCursorVersion    = 17
	remoteCursorName       = "017_remote_observation_cursors.sql"
	controllerOwnerVersion = 18
	controllerOwnerName    = "018_controller_ownership.sql"
)

type v2Migration struct {
	version int
	name    string
}

func v2MigrationDefinitions() []v2Migration {
	return []v2Migration{
		{version: localControlVersion, name: localControlName},
		{version: deviceRoutingVersion, name: deviceRoutingName},
		{version: deviceLinkVersion, name: deviceLinkName},
		{version: localBackfillVersion, name: localBackfillName},
		{version: deviceCommandVersion, name: deviceCommandName},
		{version: taskCursorVersion, name: taskCursorName},
		{version: remoteCursorVersion, name: remoteCursorName},
		{version: controllerOwnerVersion, name: controllerOwnerName},
	}
}

func localControlSchema() (string, error) {
	contents, err := localControlSchemaFS.ReadFile("schema/011_local_control.sql")
	if err != nil {
		return "", fmt.Errorf("read local control schema: %w", err)
	}
	return string(contents), nil
}

func localControlChecksum() (string, error) {
	contents, err := localControlSchema()
	if err != nil {
		return "", err
	}
	return checksum(contents), nil
}

func deviceRoutingSchema() (string, error) {
	contents, err := localControlSchemaFS.ReadFile("schema/012_device_routing.sql")
	if err != nil {
		return "", fmt.Errorf("read device routing schema: %w", err)
	}
	return string(contents), nil
}

func deviceRoutingChecksum() (string, error) {
	contents, err := deviceRoutingSchema()
	if err != nil {
		return "", err
	}
	return checksum(contents), nil
}

func deviceLinkSchema() (string, error) {
	contents, err := localControlSchemaFS.ReadFile("schema/013_device_link_sequences.sql")
	if err != nil {
		return "", fmt.Errorf("read device link schema: %w", err)
	}
	return string(contents), nil
}

func deviceLinkChecksum() (string, error) {
	contents, err := deviceLinkSchema()
	if err != nil {
		return "", err
	}
	return checksum(contents), nil
}

func localBackfillSchema() (string, error) {
	contents, err := localControlSchemaFS.ReadFile("schema/014_local_control_backfill.sql")
	if err != nil {
		return "", fmt.Errorf("read local control backfill schema: %w", err)
	}
	return string(contents), nil
}

func localBackfillChecksum() (string, error) {
	contents, err := localBackfillSchema()
	if err != nil {
		return "", err
	}
	return checksum(contents), nil
}

func deviceCommandSchema() (string, error) {
	contents, err := localControlSchemaFS.ReadFile("schema/015_device_command_queue.sql")
	if err != nil {
		return "", fmt.Errorf("read device command queue schema: %w", err)
	}
	return string(contents), nil
}

func deviceCommandChecksum() (string, error) {
	contents, err := deviceCommandSchema()
	if err != nil {
		return "", err
	}
	return checksum(contents), nil
}

func taskCursorSchema() (string, error) {
	contents, err := localControlSchemaFS.ReadFile("schema/016_task_event_cursors.sql")
	if err != nil {
		return "", fmt.Errorf("read task event cursor schema: %w", err)
	}
	return string(contents), nil
}

func taskCursorChecksum() (string, error) {
	contents, err := taskCursorSchema()
	if err != nil {
		return "", err
	}
	return checksum(contents), nil
}

func remoteCursorSchema() (string, error) {
	contents, err := localControlSchemaFS.ReadFile("schema/017_remote_observation_cursors.sql")
	if err != nil {
		return "", fmt.Errorf("read remote observation cursor schema: %w", err)
	}
	return string(contents), nil
}

func remoteCursorChecksum() (string, error) {
	contents, err := remoteCursorSchema()
	if err != nil {
		return "", err
	}
	return checksum(contents), nil
}

func controllerOwnerSchema() (string, error) {
	contents, err := localControlSchemaFS.ReadFile("schema/018_controller_ownership.sql")
	if err != nil {
		return "", fmt.Errorf("read controller ownership schema: %w", err)
	}
	return string(contents), nil
}

func controllerOwnerChecksum() (string, error) {
	contents, err := controllerOwnerSchema()
	if err != nil {
		return "", err
	}
	return checksum(contents), nil
}

func ensureV2Migrations(ctx context.Context, db *sql.DB) error {
	if err := validateMigrationLedger(ctx, db); err != nil {
		return err
	}
	var current int
	if err := db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM migration_ledger").Scan(&current); err != nil {
		return fmt.Errorf("read latest v2 migration: %w", err)
	}
	for _, migration := range v2MigrationDefinitions() {
		if migration.version <= current {
			continue
		}
		schema, err := localControlSchemaFS.ReadFile("schema/" + migration.name)
		if err != nil {
			return fmt.Errorf("read v2 migration %s: %w", migration.name, err)
		}
		migrationChecksum := checksum(string(schema))
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin v2 migration %s: %w", migration.name, err)
		}
		if _, err := tx.ExecContext(ctx, string(schema)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply v2 migration %s: %w", migration.name, err)
		}
		fingerprint, err := schemaFingerprint(ctx, tx)
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("fingerprint v2 migration %s: %w", migration.name, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO migration_ledger(version, name, checksum, structural_fingerprint, applied_at)
			VALUES (?, ?, ?, ?, ?)`, migration.version, migration.name, migrationChecksum, fingerprint, timestamp(time.Now().UTC())); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record v2 migration %s: %w", migration.name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit v2 migration %s: %w", migration.name, err)
		}
		current = migration.version
	}
	return validateMigrationLedger(ctx, db)
}

func checksum(contents string) string {
	digest := sha256.Sum256([]byte(contents))
	return hex.EncodeToString(digest[:])
}
