// Package operations contains supported, schema-aware maintenance commands.
package operations

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/berkayahi/agentbridge/internal/controller"
	"github.com/berkayahi/agentbridge/internal/deviceidentity"
	"github.com/berkayahi/agentbridge/internal/managed"
	"github.com/berkayahi/agentbridge/internal/store/sqlite"
	_ "modernc.org/sqlite"
)

type BackupOptions struct {
	Database         string
	Output           string
	IdentityPath     string
	RecordPath       string
	ModePath         string
	ManagedStatePath string
	Now              func() time.Time
}

type BackupManifest struct {
	SourceSHA256           string           `json:"source_sha256"`
	BackupSHA256           string           `json:"backup_sha256"`
	SchemaFingerprint      string           `json:"schema_fingerprint"`
	ToolVersion            string           `json:"tool_version"`
	CreatedAt              time.Time        `json:"created_at"`
	Counts                 map[string]int64 `json:"counts"`
	Mode                   string           `json:"mode"`
	OrganizationID         string           `json:"organization_id,omitempty"`
	DeviceID               string           `json:"device_id,omitempty"`
	DeviceFingerprint      string           `json:"device_fingerprint,omitempty"`
	HighestControllerEpoch uint64           `json:"highest_controller_epoch,omitempty"`
	ManagedCursor          *managed.Cursor  `json:"managed_cursor,omitempty"`
	ReEnrollmentRequired   bool             `json:"re_enrollment_required,omitempty"`
}

type BackupResult struct {
	Database string
	Manifest string
}

func Backup(ctx context.Context, options BackupOptions) (BackupResult, error) {
	if strings.TrimSpace(options.Database) == "" || strings.TrimSpace(options.Output) == "" {
		return BackupResult{}, errors.New("backup requires database and output")
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	if _, err := os.Stat(options.Database); err != nil {
		return BackupResult{}, fmt.Errorf("inspect database: %w", err)
	}
	if err := os.MkdirAll(options.Output, 0o700); err != nil {
		return BackupResult{}, fmt.Errorf("create backup directory: %w", err)
	}
	release, err := sqlite.AcquireDatabaseRuntimeLock(options.Database)
	if err != nil {
		return BackupResult{}, err
	}
	defer release()
	source, err := openDatabase(ctx, options.Database)
	if err != nil {
		return BackupResult{}, err
	}
	defer source.Close()
	if err := verifyV2(ctx, source); err != nil {
		return BackupResult{}, err
	}
	stamp := now().UTC().Format("20060102T150405.000000000Z")
	temporary := filepath.Join(options.Output, ".agentbridge-"+stamp+".tmp.db")
	destination := filepath.Join(options.Output, "agentbridge-"+stamp+".db")
	if err := snapshot(ctx, source, temporary); err != nil {
		return BackupResult{}, err
	}
	defer os.Remove(temporary)
	if err := os.Chmod(temporary, 0o600); err != nil {
		return BackupResult{}, fmt.Errorf("secure temporary backup: %w", err)
	}
	backup, err := openDatabase(ctx, temporary)
	if err != nil {
		return BackupResult{}, err
	}
	if err := verifyV2(ctx, backup); err != nil {
		backup.Close()
		return BackupResult{}, fmt.Errorf("verify backup: %w", err)
	}
	manifest := BackupManifest{ToolVersion: "agentbridge-2.0", CreatedAt: now().UTC()}
	if err := collectManagedFacts(ctx, options, &manifest); err != nil {
		backup.Close()
		return BackupResult{}, err
	}
	manifest.SourceSHA256, err = fileSHA256(options.Database)
	if err != nil {
		backup.Close()
		return BackupResult{}, err
	}
	manifest.BackupSHA256, err = fileSHA256(temporary)
	if err != nil {
		backup.Close()
		return BackupResult{}, err
	}
	manifest.SchemaFingerprint, err = schemaFingerprint(ctx, backup)
	if err != nil {
		backup.Close()
		return BackupResult{}, err
	}
	manifest.Counts, err = counts(ctx, backup)
	if err != nil {
		backup.Close()
		return BackupResult{}, err
	}
	if err := backup.Close(); err != nil {
		return BackupResult{}, fmt.Errorf("close backup: %w", err)
	}
	if err := os.Rename(temporary, destination); err != nil {
		return BackupResult{}, fmt.Errorf("activate backup: %w", err)
	}
	if err := os.Chmod(destination, 0o600); err != nil {
		return BackupResult{}, fmt.Errorf("secure backup: %w", err)
	}
	manifestPath := destination + ".manifest.json"
	if err := writeJSON(manifestPath, manifest); err != nil {
		return BackupResult{}, err
	}
	return BackupResult{Database: destination, Manifest: manifestPath}, nil
}

func collectManagedFacts(ctx context.Context, options BackupOptions, manifest *BackupManifest) error {
	manifest.Mode = string(controller.ModeStandalone)
	if options.ModePath != "" {
		modeStore, err := controller.NewFileModeStore(options.ModePath)
		if err != nil {
			return err
		}
		state, err := modeStore.Load(ctx)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("read mode state: %w", err)
			}
		} else {
			manifest.Mode = string(state.Mode)
		}
	}
	if options.RecordPath != "" {
		record, err := deviceidentity.LoadRecord(options.RecordPath)
		if err == nil {
			if options.IdentityPath != "" {
				key, keyErr := deviceidentity.Load(options.IdentityPath)
				if keyErr != nil || key.Fingerprint() != record.Fingerprint {
					return errors.New("enrollment record does not match the device key")
				}
			}
			manifest.OrganizationID = record.OrganizationID
			manifest.DeviceID = record.DeviceID
			manifest.DeviceFingerprint = record.Fingerprint
			manifest.HighestControllerEpoch = record.HighestControllerEpoch
			manifest.ReEnrollmentRequired = record.Revoked || record.Quarantined
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("read enrollment facts: %w", err)
		} else if manifest.Mode == string(controller.ModeManaged) {
			manifest.ReEnrollmentRequired = true
		}
	}
	if options.ManagedStatePath != "" {
		state, err := managed.NewFileStateStore(options.ManagedStatePath)
		if err != nil {
			return err
		}
		cursor, err := state.Load(ctx)
		if err == nil {
			manifest.ManagedCursor = &cursor
			if cursor.ControllerEpoch > manifest.HighestControllerEpoch {
				manifest.HighestControllerEpoch = cursor.ControllerEpoch
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("read managed cursor: %w", err)
		}
	}
	return nil
}

func snapshot(ctx context.Context, db *sql.DB, destination string) error {
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return errors.New("backup destination already exists")
		}
		return err
	}
	query := "VACUUM INTO '" + strings.ReplaceAll(destination, "'", "''") + "'"
	if _, err := db.ExecContext(ctx, query); err != nil {
		return fmt.Errorf("create SQLite snapshot: %w", err)
	}
	return nil
}

func openDatabase(ctx context.Context, path string) (*sql.DB, error) {
	u := &url.URL{Scheme: "file", Path: path}
	query := u.Query()
	query.Set("_pragma", "busy_timeout(10000)")
	u.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("open SQLite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping SQLite database: %w", err)
	}
	return db, nil
}

func verifyV2(ctx context.Context, db *sql.DB) error {
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return fmt.Errorf("SQLite integrity check: %w", err)
	}
	if integrity != "ok" {
		return fmt.Errorf("SQLite integrity check failed: %s", integrity)
	}
	for _, table := range []string{"migration_ledger", "local_tasks", "executions", "execution_events", "spool_metadata"} {
		var count int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = ?", table).Scan(&count); err != nil {
			return fmt.Errorf("inspect v2 table %s: %w", table, err)
		}
		if count != 1 {
			return fmt.Errorf("database is not an AgentBridge 2.0 database: missing %s", table)
		}
	}
	return nil
}

func schemaFingerprint(ctx context.Context, db *sql.DB) (string, error) {
	rows, err := db.QueryContext(ctx, "SELECT name, COALESCE(sql, '') FROM sqlite_schema WHERE type IN ('table', 'index', 'trigger', 'view') ORDER BY type, name")
	if err != nil {
		return "", fmt.Errorf("read schema fingerprint: %w", err)
	}
	defer rows.Close()
	hash := sha256.New()
	for rows.Next() {
		var name, definition string
		if err := rows.Scan(&name, &definition); err != nil {
			return "", err
		}
		io.WriteString(hash, name+"\x00"+definition+"\n")
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func counts(ctx context.Context, db *sql.DB) (map[string]int64, error) {
	result := make(map[string]int64)
	for _, table := range []string{"local_tasks", "executions", "sessions", "execution_events", "attachments", "spool_metadata"} {
		var count int64
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			return nil, fmt.Errorf("count %s: %w", table, err)
		}
		result[table] = count
	}
	return result, nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		file.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}

func findBackup(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return path, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return "", err
	}
	var candidates []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".db") && strings.HasPrefix(entry.Name(), "agentbridge-") {
			candidates = append(candidates, filepath.Join(path, entry.Name()))
		}
	}
	sort.Strings(candidates)
	if len(candidates) == 0 {
		return "", errors.New("backup database not found")
	}
	return candidates[len(candidates)-1], nil
}
