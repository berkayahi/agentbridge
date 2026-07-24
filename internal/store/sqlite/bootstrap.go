package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

//go:embed schema/010_execution_kernel.sql
var executionKernelFS embed.FS

var ErrMigrationRequired = errors.New("sqlite: explicit v2 migration required")

func executionKernelSQL() (string, error) {
	bytes, err := executionKernelFS.ReadFile("schema/010_execution_kernel.sql")
	if err != nil {
		return "", fmt.Errorf("read execution kernel schema: %w", err)
	}
	return string(bytes), nil
}

func executionKernelChecksum() (string, error) {
	contents, err := executionKernelSQL()
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(contents))
	return hex.EncodeToString(digest[:]), nil
}

// OpenV2 opens only an empty or already-v2 database. Legacy lineages always
// fail closed until Cutover has made and verified a backup.
func OpenV2(ctx context.Context, path string) (*RuntimeStore, error) {
	release, err := AcquireDatabaseRuntimeLock(path)
	if err != nil {
		return nil, err
	}
	defer release()
	return openV2WithRuntimeLock(ctx, path)
}

// OpenV2WithRuntimeLock opens a v2 database when the caller already holds the
// cooperative runtime lock for the complete daemon lifetime. It is kept
// separate from OpenV2 so managed composition can share the same lock that
// serve owns while still using the v2 bootstrap path.
func OpenV2WithRuntimeLock(ctx context.Context, path string) (*RuntimeStore, error) {
	return openV2WithRuntimeLock(ctx, path)
}

func openV2WithRuntimeLock(ctx context.Context, path string) (*RuntimeStore, error) {

	db, err := openRaw(ctx, path)
	if err != nil {
		return nil, err
	}
	report, err := preflightDatabase(ctx, db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	switch report.Lineage {
	case LineageEmpty:
		if err := bootstrapV2(ctx, db, time.Now().UTC()); err != nil {
			_ = db.Close()
			return nil, err
		}
	case LineageV2:
		if err := ensureV2Migrations(ctx, db); err != nil {
			_ = db.Close()
			return nil, err
		}
	default:
		_ = db.Close()
		return nil, ErrMigrationRequired
	}
	if report.Lineage == LineageEmpty {
		if err := ensureV2Migrations(ctx, db); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return &RuntimeStore{db: db}, nil
}

func bootstrapV2(ctx context.Context, db *sql.DB, now time.Time) error {
	schema, err := executionKernelSQL()
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin v2 bootstrap: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("apply v2 schema: %w", err)
	}
	if err := writeMigrationLedgerTx(ctx, tx, now); err != nil {
		return err
	}
	if err := validateMigrationLedgerTx(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit v2 schema: %w", err)
	}
	return nil
}

func openRaw(ctx context.Context, path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", sqliteDSNWithImmediateTransactions(path))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		if strings.Contains(strings.ToLower(err.Error()), "file is not a database") {
			return nil, fmt.Errorf("%w: ping sqlite: %v", ErrCorruptDatabase, err)
		}
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	return db, nil
}

func isV2Table(name string) bool {
	return name == "migration_ledger"
}

func normalizeSchemaSQL(value string) string {
	tokens := strings.Fields(value)
	normalized := make([]string, 0, len(tokens))
	for index := 0; index < len(tokens); index++ {
		if index+2 < len(tokens) && strings.EqualFold(tokens[index], "IF") &&
			strings.EqualFold(tokens[index+1], "NOT") && strings.EqualFold(tokens[index+2], "EXISTS") {
			index += 2
			continue
		}
		normalized = append(normalized, tokens[index])
	}
	value = strings.Join(normalized, " ")
	value = strings.ReplaceAll(value, "( ", "(")
	value = strings.ReplaceAll(value, " )", ")")
	value = strings.ReplaceAll(value, " ,", ",")
	return value
}
