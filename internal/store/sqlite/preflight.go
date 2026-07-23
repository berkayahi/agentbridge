package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"
)

var (
	ErrUnknownLineage  = errors.New("sqlite: unrecognized migration lineage")
	ErrCorruptDatabase = errors.New("sqlite: database integrity check failed")
)

type Lineage string

const (
	LineageEmpty    Lineage = "empty"
	LineagePublicV1 Lineage = "public_v1"
	LineageDonor    Lineage = "donor"
	LineageV2       Lineage = "v2"
)

type PreflightReport struct {
	Lineage               Lineage
	StructuralFingerprint string
}

// Preflight validates the file without applying either legacy or v2 changes.
func Preflight(ctx context.Context, path string) (PreflightReport, error) {
	db, err := openRaw(ctx, path)
	if err != nil {
		return PreflightReport{}, err
	}
	defer db.Close()
	return preflightDatabase(ctx, db)
}

func preflightDatabase(ctx context.Context, db *sql.DB) (PreflightReport, error) {
	if err := ensureNoActiveWriter(ctx, db); err != nil {
		return PreflightReport{}, err
	}
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return PreflightReport{}, fmt.Errorf("run integrity check: %w", err)
	}
	if integrity != "ok" {
		return PreflightReport{}, ErrCorruptDatabase
	}
	fingerprint, err := SchemaFingerprint(ctx, db)
	if err != nil {
		return PreflightReport{}, err
	}
	if exists, err := tableExists(ctx, db, "migration_ledger"); err != nil {
		return PreflightReport{}, err
	} else if exists {
		if err := validateMigrationLedger(ctx, db); err != nil {
			return PreflightReport{}, err
		}
		return PreflightReport{Lineage: LineageV2, StructuralFingerprint: fingerprint}, nil
	}
	if exists, err := tableExists(ctx, db, "schema_migrations"); err != nil {
		return PreflightReport{}, err
	} else if !exists {
		if fingerprint == emptySchemaFingerprint() {
			return PreflightReport{Lineage: LineageEmpty, StructuralFingerprint: fingerprint}, nil
		}
		return PreflightReport{}, ErrUnknownLineage
	}
	lineage, err := legacyLineage(ctx, db, fingerprint)
	if err != nil {
		return PreflightReport{}, err
	}
	return PreflightReport{Lineage: lineage, StructuralFingerprint: fingerprint}, nil
}

func ensureNoActiveWriter(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, "PRAGMA busy_timeout = 100"); err != nil {
		return fmt.Errorf("configure writer probe: %w", err)
	}
	defer func() {
		_, _ = db.ExecContext(context.Background(), fmt.Sprintf("PRAGMA busy_timeout = %d", busyTimeoutMillis))
	}()
	if _, err := db.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		if isBusy(err) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return ErrDatabaseInUse
		}
		return fmt.Errorf("probe active writer: %w", err)
	}
	if _, err := db.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		return fmt.Errorf("release writer probe: %w", err)
	}
	return nil
}

func tableExists(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", name).Scan(&count); err != nil {
		return false, fmt.Errorf("check table %s: %w", name, err)
	}
	return count == 1, nil
}

func legacyLineage(ctx context.Context, db *sql.DB, actualFingerprint string) (Lineage, error) {
	rows, err := db.QueryContext(ctx, "SELECT version, name FROM schema_migrations ORDER BY version")
	if err != nil {
		return "", fmt.Errorf("read legacy ledger: %w", err)
	}
	defer rows.Close()
	expected := legacyMigrationNames()
	versions := make([]int, 0, 6)
	for rows.Next() {
		var version int
		var name string
		if err := rows.Scan(&version, &name); err != nil {
			return "", fmt.Errorf("scan legacy ledger: %w", err)
		}
		if expected[version] != name || version < 1 || version > 6 {
			return "", ErrUnknownLineage
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate legacy ledger: %w", err)
	}
	if len(versions) == 0 || !strictlyIncreasing(versions) {
		return "", ErrUnknownLineage
	}
	if err := validateLegacyAppliedTimes(ctx, db); err != nil {
		return "", err
	}

	switch {
	case equalVersions(versions, []int{1, 2, 3}):
		if err := requireLegacyFingerprint(ctx, 3, actualFingerprint); err != nil {
			return "", err
		}
		return LineagePublicV1, nil
	case equalVersions(versions, []int{1, 2}) && attachmentChecksumExistsForPreflight(ctx, db):
		if err := requireLegacyFingerprint(ctx, 3, actualFingerprint); err != nil {
			return "", err
		}
		return LineagePublicV1, nil
	case equalVersions(versions, []int{1, 2, 3, 4, 5, 6}):
		if err := requireLegacyFingerprint(ctx, 6, actualFingerprint); err != nil {
			return "", err
		}
		return LineageDonor, nil
	default:
		return "", ErrUnknownLineage
	}

}

func validateLegacyAppliedTimes(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "SELECT applied_at FROM schema_migrations ORDER BY version")
	if err != nil {
		return fmt.Errorf("read legacy migration times: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return fmt.Errorf("scan legacy migration time: %w", err)
		}
		if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
			return ErrUnknownLineage
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate legacy migration times: %w", err)
	}
	return nil
}

func attachmentChecksumExistsForPreflight(ctx context.Context, db *sql.DB) bool {
	exists, err := attachmentChecksumExists(ctx, db)
	return err == nil && exists
}

func strictlyIncreasing(values []int) bool {
	for index := 1; index < len(values); index++ {
		if values[index] <= values[index-1] {
			return false
		}
	}
	return true
}

func equalVersions(got, want []int) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

func legacyMigrationNames() map[int]string {
	return map[int]string{
		1: "001_initial.sql",
		2: "002_normalize_timestamps.sql",
		3: "003_attachment_sha256.sql",
		4: "004_operator_actions.sql",
		5: "005_action_claim_lease.sql",
		6: "006_task_retries.sql",
	}
}

func requireLegacyFingerprint(ctx context.Context, version int, actual string) error {
	expected, err := legacySchemaFingerprint(ctx, version)
	if err != nil {
		return err
	}
	if expected != actual {
		return ErrUnknownLineage
	}
	return nil
}

func emptySchemaFingerprint() string {
	return "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
}

func legacySchemaFingerprint(ctx context.Context, version int) (string, error) {
	dsn := fmt.Sprintf("file:agentbridge-legacy-fingerprint-%d?mode=memory&cache=private", time.Now().UnixNano())
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return "", fmt.Errorf("open legacy fingerprint database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	defer db.Close()
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE schema_migrations (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			applied_at TEXT NOT NULL
		)`); err != nil {
		return "", fmt.Errorf("create legacy fingerprint ledger: %w", err)
	}
	definitions, err := legacyMigrationDefinitions()
	if err != nil {
		return "", err
	}
	if err := validateLegacyMigrationSourceChecksums(definitions, version); err != nil {
		return "", err
	}
	for _, migration := range definitions {
		if migration.version > version {
			break
		}
		if _, err := db.ExecContext(ctx, migration.sql); err != nil {
			return "", fmt.Errorf("apply legacy fingerprint migration %s: %w", migration.name, err)
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO schema_migrations(version, name, applied_at) VALUES (?, ?, ?)", migration.version, migration.name, timestamp(time.Unix(0, 0).UTC())); err != nil {
			return "", fmt.Errorf("record legacy fingerprint migration %s: %w", migration.name, err)
		}
	}
	return SchemaFingerprint(ctx, db)
}

func legacyMigrationDefinitions() ([]migration, error) {
	available, err := readMigrations()
	if err != nil {
		return nil, err
	}
	legacy := make([]migration, 0, len(available)+3)
	for _, migration := range available {
		if migration.version <= 3 {
			legacy = append(legacy, migration)
		}
	}
	legacy = append(legacy,
		migration{version: 4, name: "004_operator_actions.sql", sql: donorOperatorActionsMigration},
		migration{version: 5, name: "005_action_claim_lease.sql", sql: donorActionClaimLeaseMigration},
		migration{version: 6, name: "006_task_retries.sql", sql: donorTaskRetriesMigration},
	)
	sort.Slice(legacy, func(i, j int) bool { return legacy[i].version < legacy[j].version })
	return legacy, nil
}

func migrationChecksum(contents string) string {
	digest := sha256.Sum256([]byte(contents))
	return hex.EncodeToString(digest[:])
}

var legacyMigrationSourceChecksums = map[string]string{
	"001_initial.sql":              "d610f1a63f6b52644e0271b7216749939d2587ace22a60f8deaa05fc69c39a34",
	"002_normalize_timestamps.sql": "a6a22ef018d4a3784f0071ee1e58ff9690e894ed9c6c2c0b879329457f9b508e",
	"003_attachment_sha256.sql":    "de6c5c1e62452a9d5c02f5c47a31004dd1ec1c36fa2808bf4c13a20e4500be01",
	"004_operator_actions.sql":     "845da6285196d58ecb516e25a3088090ee2b8d27622ecdf8228bbf903a657379",
	"005_action_claim_lease.sql":   "4c57a0d3f4cc135df9e01a44f33d27bde7d37205407a2a1d7dde75288bd5d0c2",
	"006_task_retries.sql":         "d20df46f2113a605df3b640db303dd7654787bcd92addf7826a29214b83ef8ad",
}

func validateLegacyMigrationSourceChecksums(definitions []migration, version int) error {
	for _, definition := range definitions {
		if definition.version > version {
			break
		}
		expected, ok := legacyMigrationSourceChecksums[definition.name]
		if !ok || migrationChecksum(definition.sql) != expected {
			return ErrUnknownLineage
		}
	}
	return nil
}

const donorOperatorActionsMigration = `CREATE TABLE operator_actions (
    id TEXT PRIMARY KEY CHECK (length(id) BETWEEN 1 AND 16),
    chat_id INTEGER NOT NULL,
    user_id INTEGER NOT NULL,
    kind TEXT NOT NULL CHECK (length(kind) BETWEEN 1 AND 16),
    provider TEXT NOT NULL DEFAULT '' CHECK (provider IN ('', 'codex', 'claude')),
    target_task_id TEXT NOT NULL DEFAULT '' CHECK (length(target_task_id) <= 128),
    result_task_id TEXT NOT NULL DEFAULT '' CHECK (length(result_task_id) <= 128),
    payload_ref TEXT NOT NULL DEFAULT '' CHECK (length(payload_ref) <= 64),
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    claimed_at TEXT,
    completed_at TEXT,
    safe_result TEXT NOT NULL DEFAULT '' CHECK (length(safe_result) <= 64),
    CHECK (expires_at > created_at),
    CHECK (completed_at IS NULL OR claimed_at IS NOT NULL)
);

CREATE INDEX operator_actions_expiry_idx
    ON operator_actions(expires_at) WHERE claimed_at IS NULL;
CREATE INDEX operator_actions_completion_idx
    ON operator_actions(completed_at) WHERE completed_at IS NOT NULL;
CREATE UNIQUE INDEX operator_actions_result_task_idx
    ON operator_actions(result_task_id) WHERE result_task_id <> '';
CREATE UNIQUE INDEX operator_actions_retry_source_idx
    ON operator_actions(target_task_id) WHERE kind = 'retry';`

const donorActionClaimLeaseMigration = `ALTER TABLE operator_actions
    ADD COLUMN claim_owner TEXT NOT NULL DEFAULT '' CHECK (length(claim_owner) <= 64);

ALTER TABLE operator_actions
    ADD COLUMN lease_expires_at TEXT;

ALTER TABLE operator_actions
    ADD COLUMN safe_progress TEXT NOT NULL DEFAULT '' CHECK (length(safe_progress) <= 64);

CREATE INDEX operator_actions_claim_lease_idx
    ON operator_actions(lease_expires_at, claimed_at)
    WHERE claimed_at IS NOT NULL AND completed_at IS NULL;`

const donorTaskRetriesMigration = `CREATE TABLE task_retries (
    task_id TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    failure_class TEXT NOT NULL CHECK (failure_class IN ('unknown','auth','rate_limited','transient_transport','permanent','canceled')),
    attempt INTEGER NOT NULL CHECK (attempt >= 0),
    next_attempt_at TEXT NOT NULL,
    last_checkpoint_at TEXT NOT NULL,
    safe_summary TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('waiting','claimed','executing','completed','manual')),
    claim_owner TEXT NOT NULL DEFAULT '',
    claimed_at TEXT,
    lease_expires_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE INDEX task_retries_due_idx ON task_retries(status, next_attempt_at, task_id);
CREATE INDEX task_retries_lease_idx ON task_retries(status, lease_expires_at);`
