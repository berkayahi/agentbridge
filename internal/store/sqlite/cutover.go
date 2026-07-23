package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/berkayahi/agentbridge/internal/gitref"
	moderncsqlite "modernc.org/sqlite"
)

// CutoverResult identifies the independently restorable evidence produced
// before the irreversible 1.x-to-v2 transformation.
type CutoverResult struct {
	BackupPath   string
	ManifestPath string
}

type backupManifest struct {
	SourceHash            string           `json:"source_hash"`
	BackupHash            string           `json:"backup_hash"`
	StructuralFingerprint string           `json:"structural_fingerprint"`
	RowCounts             map[string]int64 `json:"row_counts"`
	ToolVersion           string           `json:"tool_version"`
	CreatedAt             time.Time        `json:"created_at"`
}

var cutoverFailureHook = func(string) error { return nil }

// Cutover performs the one-way v2 transform only after preflight and a
// verified local backup. Ordinary Store.Open never invokes this function.
func Cutover(ctx context.Context, path, toolVersion string) (CutoverResult, error) {
	release, err := acquireMigrationLock(path)
	if err != nil {
		return CutoverResult{}, err
	}
	defer release()

	db, err := openRaw(ctx, path)
	if err != nil {
		return CutoverResult{}, err
	}
	defer db.Close()
	report, err := preflightDatabase(ctx, db)
	if err != nil {
		return CutoverResult{}, err
	}
	if report.Lineage != LineagePublicV1 && report.Lineage != LineageDonor {
		return CutoverResult{}, ErrMigrationRequired
	}

	now := time.Now().UTC()
	backupPath, manifestPath, sourceDataVersion, err := createVerifiedBackup(ctx, db, path, report.StructuralFingerprint, toolVersion, now)
	if err != nil {
		return CutoverResult{}, err
	}
	if err := cutoverFailureHook("after-backup"); err != nil {
		return CutoverResult{}, err
	}
	if err := transformLegacy(ctx, db, now, report.Lineage, sourceDataVersion); err != nil {
		return CutoverResult{}, err
	}
	return CutoverResult{BackupPath: backupPath, ManifestPath: manifestPath}, nil
}

func acquireMigrationLock(path string) (func(), error) {
	return AcquireDatabaseRuntimeLock(path)
}

func createVerifiedBackup(ctx context.Context, db *sql.DB, path, fingerprint, toolVersion string, now time.Time) (string, string, int64, error) {
	backupPath := path + ".pre-v2-" + now.Format("20060102T150405.000000000Z") + ".db"
	if _, err := db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return "", "", 0, fmt.Errorf("checkpoint database before backup: %w", err)
	}
	sourceDataVersion, err := databaseDataVersion(ctx, db)
	if err != nil {
		return "", "", 0, err
	}
	sourceRows, err := rowCounts(ctx, db)
	if err != nil {
		return "", "", 0, err
	}
	sourceHash, err := fileHash(path)
	if err != nil {
		return "", "", 0, err
	}
	if err := createSecureBackupPlaceholder(backupPath); err != nil {
		return "", "", 0, err
	}
	if err := createSQLiteBackup(ctx, db, backupPath); err != nil {
		return "", "", 0, err
	}
	if err := os.Chmod(backupPath, 0o600); err != nil {
		return "", "", 0, fmt.Errorf("secure migration backup: %w", err)
	}
	if err := verifyBackup(ctx, db, backupPath, fingerprint, sourceRows); err != nil {
		return "", "", 0, err
	}
	currentDataVersion, err := databaseDataVersion(ctx, db)
	if err != nil {
		return "", "", 0, err
	}
	currentHash, err := fileHash(path)
	if err != nil {
		return "", "", 0, err
	}
	currentRows, err := rowCounts(ctx, db)
	if err != nil {
		return "", "", 0, err
	}
	if currentDataVersion != sourceDataVersion || currentHash != sourceHash || !sameRowCounts(sourceRows, currentRows) {
		return "", "", 0, ErrDatabaseInUse
	}
	backupHash, err := fileHash(backupPath)
	if err != nil {
		return "", "", 0, err
	}
	manifestPath := backupPath + ".manifest.json"
	manifest := backupManifest{SourceHash: sourceHash, BackupHash: backupHash, StructuralFingerprint: fingerprint, RowCounts: sourceRows, ToolVersion: toolVersion, CreatedAt: now}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return "", "", 0, fmt.Errorf("encode backup manifest: %w", err)
	}
	if err := os.WriteFile(manifestPath, append(encoded, '\n'), 0o600); err != nil {
		return "", "", 0, fmt.Errorf("write backup manifest: %w", err)
	}
	return backupPath, manifestPath, sourceDataVersion, nil
}

func createSecureBackupPlaceholder(path string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("reserve migration backup: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close migration backup placeholder: %w", err)
	}
	return nil
}

type rowQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func databaseDataVersion(ctx context.Context, db rowQueryer) (int64, error) {
	var version int64
	if err := db.QueryRowContext(ctx, "PRAGMA data_version").Scan(&version); err != nil {
		return 0, fmt.Errorf("read sqlite data version: %w", err)
	}
	return version, nil
}

func createSQLiteBackup(ctx context.Context, db *sql.DB, backupPath string) error {
	connection, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire sqlite backup connection: %w", err)
	}
	defer connection.Close()
	if err := connection.Raw(func(driverConn any) error {
		backuper, ok := driverConn.(interface {
			NewBackup(string) (*moderncsqlite.Backup, error)
		})
		if !ok {
			return fmt.Errorf("sqlite driver does not support online backup")
		}
		backup, err := backuper.NewBackup(backupPath)
		if err != nil {
			return fmt.Errorf("initialize sqlite backup: %w", err)
		}
		var stepErr error
		for {
			more, err := backup.Step(-1)
			if err != nil {
				stepErr = err
				break
			}
			if !more {
				break
			}
		}
		finishErr := backup.Finish()
		if stepErr != nil {
			return fmt.Errorf("copy sqlite backup: %w", stepErr)
		}
		if finishErr != nil {
			return fmt.Errorf("finish sqlite backup: %w", finishErr)
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}

func verifyBackup(ctx context.Context, source *sql.DB, backupPath, wantFingerprint string, wantRows map[string]int64) error {
	backup, err := openRaw(ctx, backupPath)
	if err != nil {
		return fmt.Errorf("open migration backup: %w", err)
	}
	defer backup.Close()
	for _, db := range []*sql.DB{source, backup} {
		var integrity string
		if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
			return fmt.Errorf("verify migration backup integrity")
		}
	}
	gotFingerprint, err := SchemaFingerprint(ctx, backup)
	if err != nil {
		return err
	}
	if gotFingerprint != wantFingerprint {
		return fmt.Errorf("verify migration backup fingerprint")
	}
	var sourcePages, backupPages int
	if err := source.QueryRowContext(ctx, "PRAGMA page_count").Scan(&sourcePages); err != nil {
		return err
	}
	if err := backup.QueryRowContext(ctx, "PRAGMA page_count").Scan(&backupPages); err != nil {
		return err
	}
	if sourcePages != backupPages {
		return fmt.Errorf("verify migration backup page count")
	}
	gotRows, err := rowCounts(ctx, backup)
	if err != nil {
		return err
	}
	if !sameRowCounts(wantRows, gotRows) {
		return fmt.Errorf("verify migration backup row counts")
	}
	return nil
}

func rowCounts(ctx context.Context, db *sql.DB) (map[string]int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT name FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
		ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list migration backup tables: %w", err)
	}
	names := make([]string, 0)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan migration backup table: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close migration backup tables: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate migration backup tables: %w", err)
	}
	counts := make(map[string]int64, len(names))
	for _, name := range names {
		quoted := `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
		var count int64
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quoted).Scan(&count); err != nil {
			return nil, fmt.Errorf("count migration backup table %s: %w", name, err)
		}
		counts[name] = count
	}
	return counts, nil
}

func sameRowCounts(want, got map[string]int64) bool {
	if len(want) != len(got) {
		return false
	}
	for name, count := range want {
		if got[name] != count {
			return false
		}
	}
	return true
}

func fileHash(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("hash migration file: %w", err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("hash migration file: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func transformLegacy(ctx context.Context, db *sql.DB, now time.Time, lineage Lineage, sourceDataVersion int64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin v2 cutover: %w", err)
	}
	defer tx.Rollback()
	currentDataVersion, err := databaseDataVersion(ctx, tx)
	if err != nil {
		return err
	}
	if currentDataVersion != sourceDataVersion {
		return ErrDatabaseInUse
	}
	for _, table := range tablesForLineage(lineage) {
		if _, err := tx.ExecContext(ctx, "ALTER TABLE "+table+" RENAME TO _cutover_source_"+table); err != nil {
			return fmt.Errorf("rename legacy table %s: %w", table, err)
		}
	}
	if err := cutoverFailureHook("after-rename"); err != nil {
		return err
	}
	schema, err := executionKernelSQL()
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("create v2 cutover schema: %w", err)
	}
	if err := cutoverFailureHook("after-schema"); err != nil {
		return err
	}
	if err := mapLegacyRows(ctx, tx, lineage); err != nil {
		return err
	}
	if err := validateCutoverMapping(ctx, tx, lineage); err != nil {
		return err
	}
	if err := cutoverFailureHook("after-map"); err != nil {
		return err
	}
	if err := dropCutoverSources(ctx, tx, lineage); err != nil {
		return err
	}
	if err := cutoverFailureHook("after-drop"); err != nil {
		return err
	}
	if err := writeMigrationLedgerTx(ctx, tx, now); err != nil {
		return err
	}
	if err := cutoverFailureHook("after-ledger"); err != nil {
		return err
	}
	if err := validateMigrationLedgerTx(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit v2 cutover: %w", err)
	}
	return nil
}

func validateCutoverMapping(ctx context.Context, tx *sql.Tx, lineage Lineage) error {
	checks := []struct {
		name   string
		source string
		target string
	}{
		{name: "local tasks", source: "_cutover_source_tasks", target: "local_tasks"},
		{name: "task presentations", source: "_cutover_source_tasks", target: "task_presentations"},
		{name: "executions", source: "_cutover_source_tasks", target: "executions"},
		{name: "events", source: "_cutover_source_task_events", target: "execution_events"},
		{name: "approvals", source: "_cutover_source_approvals", target: "approvals"},
		{name: "attachments", source: "_cutover_source_attachments", target: "attachments"},
		{name: "auth incidents", source: "_cutover_source_auth_incidents", target: "auth_incidents"},
	}
	for _, check := range checks {
		if err := requireMatchingCounts(ctx, tx, check.name, check.source, check.target); err != nil {
			return err
		}
	}
	var sourceSessions, tasksWithoutSessions, targetSessions int64
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM _cutover_source_sessions").Scan(&sourceSessions); err != nil {
		return fmt.Errorf("count source sessions: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM _cutover_source_tasks t
		WHERE NOT EXISTS (SELECT 1 FROM _cutover_source_sessions s WHERE s.task_id = t.id)`).Scan(&tasksWithoutSessions); err != nil {
		return fmt.Errorf("count synthetic sessions: %w", err)
	}
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions").Scan(&targetSessions); err != nil {
		return fmt.Errorf("count mapped sessions: %w", err)
	}
	if targetSessions != sourceSessions+tasksWithoutSessions {
		return fmt.Errorf("validate sessions: source=%d synthetic=%d target=%d", sourceSessions, tasksWithoutSessions, targetSessions)
	}
	if err := requireMatchingCounts(ctx, tx, "repository bindings", "_cutover_source_tasks", "repository_bindings", "repo_profile_id"); err != nil {
		return err
	}
	if lineage == LineagePublicV1 || lineage == LineageDonor {
		sourceCheckpoints, sourceOperations, err := countLegacyGitEvidence(ctx, tx)
		if err != nil {
			return err
		}
		var targetCheckpoints, targetOperations int64
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM git_checkpoints").Scan(&targetCheckpoints); err != nil {
			return fmt.Errorf("count mapped Git checkpoints: %w", err)
		}
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM git_operations").Scan(&targetOperations); err != nil {
			return fmt.Errorf("count mapped Git operations: %w", err)
		}
		if sourceCheckpoints != targetCheckpoints || sourceOperations != targetOperations {
			return fmt.Errorf("validate Git evidence: checkpoints source=%d target=%d operations source=%d target=%d", sourceCheckpoints, targetCheckpoints, sourceOperations, targetOperations)
		}
	}
	if lineage == LineageDonor {
		if err := requireMatchingCounts(ctx, tx, "intent evidence", "_cutover_source_operator_actions", "intent_evidence"); err != nil {
			return err
		}
		if err := requireMatchingCounts(ctx, tx, "retry evidence", "_cutover_source_task_retries", "retry_evidence"); err != nil {
			return err
		}
	}
	return nil
}

func requireMatchingCounts(ctx context.Context, tx *sql.Tx, name, source, target string, distinctColumn ...string) error {
	sourceExpression := "COUNT(*)"
	if len(distinctColumn) == 1 {
		sourceExpression = "COUNT(DISTINCT " + distinctColumn[0] + ")"
	}
	var sourceCount, targetCount int64
	if err := tx.QueryRowContext(ctx, "SELECT "+sourceExpression+" FROM "+source).Scan(&sourceCount); err != nil {
		return fmt.Errorf("count source %s: %w", name, err)
	}
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+target).Scan(&targetCount); err != nil {
		return fmt.Errorf("count mapped %s: %w", name, err)
	}
	if sourceCount != targetCount {
		return fmt.Errorf("validate %s: source=%d target=%d", name, sourceCount, targetCount)
	}
	return nil
}

// repository_leases are 1.x process-coordination state, not execution
// evidence; they are intentionally dropped with the aggregate tables.
var legacyTables = []string{"task_events", "attachments", "approvals", "sessions", "repository_leases", "auth_incidents", "tasks", "schema_migrations"}

var donorTables = []string{"operator_actions", "task_retries"}

func tablesForLineage(lineage Lineage) []string {
	tables := append([]string(nil), legacyTables...)
	if lineage == LineageDonor {
		tables = append(tables, donorTables...)
	}
	return tables
}

func mapLegacyRows(ctx context.Context, tx *sql.Tx, lineage Lineage) error {
	statements := []string{
		`INSERT INTO repository_bindings(id, remote_url, created_at)
			 SELECT DISTINCT repo_profile_id, 'legacy:' || repo_profile_id, MIN(created_at) FROM _cutover_source_tasks GROUP BY repo_profile_id`,
		`INSERT INTO local_tasks(id, repo_profile_id, title, prompt, state, provider, active_execution_id, base_sha, worktree_path, commit_sha, push_ref, deployment_url, failure_reason, started_at, finished_at, created_at, updated_at)
			 SELECT id, repo_profile_id, title, prompt, state, provider,
			 CASE WHEN state IN ('completed','failed','canceled') THEN NULL ELSE 'legacy-execution-' || id END,
			 base_sha, worktree_path, commit_sha, push_ref, deployment_url, failure_reason, started_at, finished_at, created_at, updated_at FROM _cutover_source_tasks`,
		`INSERT INTO task_presentations(local_task_id, telegram_chat_id, telegram_message_id)
			 SELECT id, telegram_chat_id, telegram_message_id FROM _cutover_source_tasks`,
		`INSERT INTO sessions(id, runtime_id, repository_id, local_task_id, active_local_task_id, provider_session_id, provider_thread_id, status, resumable, created_at, updated_at)
			 SELECT s.id, s.provider, t.repo_profile_id, t.id,
			 CASE WHEN t.state IN ('completed','failed','canceled') THEN NULL WHEN s.id = (SELECT s2.id FROM _cutover_source_sessions s2 WHERE s2.task_id = t.id ORDER BY (s2.provider_session_id = t.provider_session_id) DESC, s2.updated_at DESC, s2.id DESC LIMIT 1) THEN s.task_id ELSE NULL END,
			 s.provider_session_id, s.provider_thread_id, s.status, s.resumable, s.created_at, s.updated_at
			 FROM _cutover_source_sessions s JOIN _cutover_source_tasks t ON t.id = s.task_id`,
		`INSERT INTO sessions(id, runtime_id, repository_id, local_task_id, active_local_task_id, provider_session_id, provider_thread_id, status, resumable, created_at, updated_at)
			 SELECT 'legacy-session-' || t.id, t.provider, t.repo_profile_id, t.id,
			 CASE WHEN t.state IN ('completed','failed','canceled') THEN NULL ELSE t.id END,
			 t.provider_session_id, t.provider_thread_id, CASE WHEN t.provider_session_id = '' THEN 'unknown' ELSE 'active' END,
			 CASE WHEN t.provider_session_id = '' THEN 0 ELSE 1 END, t.created_at, t.updated_at
			 FROM _cutover_source_tasks t
			 WHERE NOT EXISTS (SELECT 1 FROM _cutover_source_sessions s WHERE s.task_id = t.id)`,
		`INSERT INTO executions(id, local_task_id, session_id, runtime_id, repository_id, retry_of_execution_id, state, attempt, fencing_epoch, command_id, max_transient_attempts, policy_snapshot, source_state, started_at, finished_at, created_at, updated_at)
			 SELECT 'legacy-execution-' || t.id, t.id,
			 COALESCE((SELECT s.id FROM _cutover_source_sessions s WHERE s.task_id = t.id ORDER BY (s.provider_session_id = t.provider_session_id) DESC, s.updated_at DESC, s.id DESC LIMIT 1), 'legacy-session-' || t.id),
			 t.provider, t.repo_profile_id, NULL,
			 CASE WHEN t.state IN ('queued','completed','failed','canceled','awaiting_approval','awaiting_auth','running') THEN t.state WHEN t.state = 'preparing' THEN 'accepted' ELSE 'running' END,
			 0, 1, 'legacy-command-' || t.id, 0, '{}', t.state, t.started_at, t.finished_at, t.created_at, t.updated_at FROM _cutover_source_tasks t`,
		`INSERT INTO execution_events(id, local_task_id, execution_id, event_type, visibility, provider_event_id, redacted_payload, created_at)
			 SELECT id, task_id, 'legacy-execution-' || task_id, event_type, visibility, provider_event_id, redacted_payload, created_at FROM _cutover_source_task_events`,
		`INSERT INTO approvals(id, local_task_id, execution_id, kind, status, request_payload, decision_payload, requested_at, expires_at, resolved_at)
			 SELECT id, task_id, 'legacy-execution-' || task_id, kind, status, request_payload, decision_payload, requested_at, expires_at, resolved_at FROM _cutover_source_approvals`,
		`INSERT INTO attachments(id, local_task_id, execution_id, kind, name, media_type, storage_path, size_bytes, sha256, created_at)
			 SELECT id, task_id, 'legacy-execution-' || task_id, kind, name, media_type, storage_path, size_bytes, sha256, created_at FROM _cutover_source_attachments`,
		`INSERT INTO auth_incidents(id, local_task_id, execution_id, provider, status, redacted_detail, detected_at, resolved_at)
			 SELECT id, task_id, CASE WHEN task_id IS NULL THEN NULL ELSE 'legacy-execution-' || task_id END, provider, status, redacted_detail, detected_at, resolved_at FROM _cutover_source_auth_incidents`,
	}
	if lineage == LineageDonor {
		statements = append(statements,
			`INSERT INTO intent_evidence(id, execution_id, kind, runtime_id, target_task_id, result_task_id, payload_ref, state, claim_owner, safe_progress, safe_result, created_at, expires_at, claimed_at, lease_expires_at, completed_at)
				SELECT oa.id, CASE WHEN oa.target_task_id = '' THEN NULL ELSE 'legacy-execution-' || oa.target_task_id END,
				oa.kind, oa.provider, oa.target_task_id, oa.result_task_id, oa.payload_ref,
				CASE WHEN oa.completed_at IS NOT NULL THEN 'completed' WHEN oa.claimed_at IS NOT NULL THEN 'claimed' ELSE 'pending' END,
				oa.claim_owner, oa.safe_progress, oa.safe_result, oa.created_at, oa.expires_at, oa.claimed_at, oa.lease_expires_at, oa.completed_at
				FROM _cutover_source_operator_actions oa`,
			`INSERT INTO retry_evidence(id, execution_id, task_id, failure_class, attempt, next_attempt_at, last_checkpoint_at, safe_summary, status, claim_owner, claimed_at, lease_expires_at, created_at, updated_at)
				SELECT 'retry-' || tr.task_id, 'legacy-execution-' || tr.task_id, tr.task_id, tr.failure_class, tr.attempt,
				tr.next_attempt_at, tr.last_checkpoint_at, tr.safe_summary, tr.status, tr.claim_owner, tr.claimed_at, tr.lease_expires_at, tr.created_at, tr.updated_at
				FROM _cutover_source_task_retries tr`,
		)
	}
	for index, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("map legacy records statement %d: %w", index, err)
		}
	}
	if lineage == LineagePublicV1 || lineage == LineageDonor {
		if err := mapGitEvidence(ctx, tx); err != nil {
			return err
		}
	}
	return nil
}

func mapGitEvidence(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, repo_profile_id, base_sha, commit_sha, push_ref, updated_at
		FROM _cutover_source_tasks ORDER BY id`)
	if err != nil {
		return fmt.Errorf("read legacy Git evidence: %w", err)
	}
	type gitEvidence struct {
		taskID, repositoryID, baseSHA, commitSHA, pushRef, updatedAt string
	}
	evidence := make([]gitEvidence, 0)
	for rows.Next() {
		var value gitEvidence
		if err := rows.Scan(&value.taskID, &value.repositoryID, &value.baseSHA, &value.commitSHA, &value.pushRef, &value.updatedAt); err != nil {
			rows.Close()
			return fmt.Errorf("scan legacy Git evidence: %w", err)
		}
		evidence = append(evidence, value)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close legacy Git evidence: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate legacy Git evidence: %w", err)
	}
	for _, value := range evidence {
		if !validGitObjectID(value.commitSHA) || !gitref.Valid(value.pushRef) || value.commitSHA == "" || value.pushRef == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO git_checkpoints(id, execution_id, repository_id, commit_sha, remote_ref, created_at)
			VALUES (?, ?, ?, ?, ?, ?)`, "legacy-checkpoint-"+value.taskID, "legacy-execution-"+value.taskID, value.repositoryID, value.commitSHA, value.pushRef, value.updatedAt); err != nil {
			return fmt.Errorf("map Git checkpoint for task %s: %w", value.taskID, err)
		}
		if !validGitObjectID(value.baseSHA) {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO git_operations(id, execution_id, kind, target_ref, expected_old_sha, idempotency_key, state, created_at)
			VALUES (?, ?, 'push', ?, ?, ?, 'succeeded', ?)`, "legacy-push-"+value.taskID, "legacy-execution-"+value.taskID, value.pushRef, value.baseSHA, "legacy-push-"+value.taskID, value.updatedAt); err != nil {
			return fmt.Errorf("map Git push for task %s: %w", value.taskID, err)
		}
	}
	return nil
}

func countLegacyGitEvidence(ctx context.Context, tx *sql.Tx) (int64, int64, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT base_sha, commit_sha, push_ref
		FROM _cutover_source_tasks`)
	if err != nil {
		return 0, 0, fmt.Errorf("read legacy Git evidence for validation: %w", err)
	}
	defer rows.Close()
	var checkpoints, operations int64
	for rows.Next() {
		var baseSHA, commitSHA, pushRef string
		if err := rows.Scan(&baseSHA, &commitSHA, &pushRef); err != nil {
			return 0, 0, fmt.Errorf("scan legacy Git evidence for validation: %w", err)
		}
		if !validGitObjectID(commitSHA) || !gitref.Valid(pushRef) || commitSHA == "" || pushRef == "" {
			continue
		}
		checkpoints++
		if validGitObjectID(baseSHA) {
			operations++
		}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("iterate legacy Git evidence for validation: %w", err)
	}
	return checkpoints, operations, nil
}

func validGitObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, runeValue := range value {
		if !((runeValue >= 'a' && runeValue <= 'f') || (runeValue >= 'A' && runeValue <= 'F') || (runeValue >= '0' && runeValue <= '9')) {
			return false
		}
	}
	return true
}

func dropCutoverSources(ctx context.Context, tx *sql.Tx, lineage Lineage) error {
	for _, table := range tablesForLineage(lineage) {
		if _, err := tx.ExecContext(ctx, "DROP TABLE IF EXISTS _cutover_source_"+table); err != nil {
			return fmt.Errorf("drop cutover source %s: %w", table, err)
		}
	}
	return nil
}

func migrationBackupDirectory(path string) string { return filepath.Dir(path) }
