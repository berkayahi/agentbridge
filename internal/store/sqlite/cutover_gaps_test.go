package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestCutoverPreservesLegacyEvidenceAndDelivery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence.db")
	legacy, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	baseSHA := strings.Repeat("a", 40)
	commitSHA := strings.Repeat("b", 40)
	if _, err := legacy.db.Exec(`
		INSERT INTO tasks (
			id, repo_profile_id, title, prompt, state, provider,
			telegram_chat_id, telegram_message_id, base_sha, worktree_path,
			provider_session_id, provider_thread_id, commit_sha, push_ref,
			deployment_url, failure_reason, created_at, updated_at, started_at, finished_at
		) VALUES ('task-1', 'repo-1', 'Legacy task', 'Repair this', 'completed', 'codex',
			99, 100, ?, '/srv/worktree', 'provider-session-1', 'thread-1', ?,
			'refs/heads/main', 'https://deploy.invalid/task-1', 'verification failed',
			'2026-07-01T00:00:00.000000000Z', '2026-07-01T01:00:00.000000000Z',
			'2026-07-01T00:10:00.000000000Z', '2026-07-01T01:00:00.000000000Z')`, baseSHA, commitSHA); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.db.Exec(`
		INSERT INTO sessions (
			id, task_id, provider, provider_session_id, provider_thread_id,
			status, resumable, created_at, updated_at
		) VALUES ('session-1', 'task-1', 'codex', 'provider-session-1', 'thread-1',
			'closed', 1, '2026-07-01T00:00:00.000000000Z', '2026-07-01T01:00:00.000000000Z');
		INSERT INTO task_events (id, task_id, event_type, visibility, provider_event_id, redacted_payload, created_at)
		VALUES ('event-1', 'task-1', 'commit_created', 'user', 'provider-event-1', '{"sha":"commit"}', '2026-07-01T01:00:00.000000000Z');
		INSERT INTO approvals (id, task_id, kind, status, request_payload, decision_payload, requested_at, expires_at, resolved_at)
		VALUES ('approval-1', 'task-1', 'command', 'approved', '{"command":"go test"}', '{"approved":true}', '2026-07-01T00:20:00.000000000Z', '2026-07-01T00:30:00.000000000Z', '2026-07-01T00:21:00.000000000Z');
		INSERT INTO attachments (id, task_id, kind, name, media_type, storage_path, size_bytes, sha256, created_at)
		VALUES ('attachment-1', 'task-1', 'log', 'result.txt', 'text/plain', '/srv/attachments/result.txt', 7, 'attachment-sha', '2026-07-01T00:22:00.000000000Z');
		INSERT INTO auth_incidents (id, task_id, provider, status, redacted_detail, detected_at, resolved_at)
		VALUES ('auth-1', 'task-1', 'codex', 'resolved', '{"detail":"reauth required"}', '2026-07-01T00:23:00.000000000Z', '2026-07-01T00:24:00.000000000Z')`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Cutover(context.Background(), path, "test-build"); err != nil {
		t.Fatal(err)
	}
	check, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()

	var taskTitle, prompt, base, worktree, commit, pushRef, deployment, failure string
	if err := check.QueryRow(`SELECT title, prompt, base_sha, worktree_path, commit_sha, push_ref, deployment_url, failure_reason FROM local_tasks WHERE id = 'task-1'`).Scan(&taskTitle, &prompt, &base, &worktree, &commit, &pushRef, &deployment, &failure); err != nil {
		t.Fatal(err)
	}
	if taskTitle != "Legacy task" || prompt != "Repair this" || base != baseSHA || worktree != "/srv/worktree" || commit != commitSHA || pushRef != "refs/heads/main" || deployment != "https://deploy.invalid/task-1" || failure != "verification failed" {
		t.Fatalf("local task evidence = %q, %q, %q, %q, %q, %q, %q, %q", taskTitle, prompt, base, worktree, commit, pushRef, deployment, failure)
	}

	var sessionID, runtimeID, historicalTaskID, providerSessionID, providerThreadID, sessionStatus string
	var resumable int
	if err := check.QueryRow(`SELECT id, runtime_id, local_task_id, provider_session_id, provider_thread_id, status, resumable FROM sessions WHERE id = 'session-1'`).Scan(&sessionID, &runtimeID, &historicalTaskID, &providerSessionID, &providerThreadID, &sessionStatus, &resumable); err != nil {
		t.Fatal(err)
	}
	if sessionID != "session-1" || runtimeID != "codex" || historicalTaskID != "task-1" || providerSessionID != "provider-session-1" || providerThreadID != "thread-1" || sessionStatus != "closed" || resumable != 1 {
		t.Fatalf("session evidence = %q, %q, %q, %q, %q, %q, %d", sessionID, runtimeID, historicalTaskID, providerSessionID, providerThreadID, sessionStatus, resumable)
	}
	var sourceState string
	var startedAt, finishedAt sql.NullString
	if err := check.QueryRow(`SELECT source_state, started_at, finished_at FROM executions WHERE id = 'legacy-execution-task-1'`).Scan(&sourceState, &startedAt, &finishedAt); err != nil {
		t.Fatal(err)
	}
	if sourceState != "completed" || !startedAt.Valid || startedAt.String != "2026-07-01T00:10:00.000000000Z" || !finishedAt.Valid || finishedAt.String != "2026-07-01T01:00:00.000000000Z" {
		t.Fatalf("execution lifecycle evidence = %q, %v, %v", sourceState, startedAt, finishedAt)
	}

	var eventPayload []byte
	if err := check.QueryRow(`SELECT redacted_payload FROM execution_events WHERE id = 'event-1'`).Scan(&eventPayload); err != nil {
		t.Fatal(err)
	}
	if string(eventPayload) != `{"sha":"commit"}` {
		t.Fatalf("event payload = %q", eventPayload)
	}
	var approvalPayload, decisionPayload, attachmentSHA, incidentDetail string
	if err := check.QueryRow(`SELECT request_payload, decision_payload FROM approvals WHERE id = 'approval-1'`).Scan(&approvalPayload, &decisionPayload); err != nil {
		t.Fatal(err)
	}
	if err := check.QueryRow(`SELECT sha256 FROM attachments WHERE id = 'attachment-1'`).Scan(&attachmentSHA); err != nil {
		t.Fatal(err)
	}
	if err := check.QueryRow(`SELECT redacted_detail FROM auth_incidents WHERE id = 'auth-1'`).Scan(&incidentDetail); err != nil {
		t.Fatal(err)
	}
	if approvalPayload != `{"command":"go test"}` || decisionPayload != `{"approved":true}` || attachmentSHA != "attachment-sha" || incidentDetail != `{"detail":"reauth required"}` {
		t.Fatalf("related evidence = %q, %q, %q, %q", approvalPayload, decisionPayload, attachmentSHA, incidentDetail)
	}

	var checkpointSHA, checkpointRef, operationState string
	if err := check.QueryRow(`SELECT commit_sha, remote_ref FROM git_checkpoints WHERE execution_id = 'legacy-execution-task-1'`).Scan(&checkpointSHA, &checkpointRef); err != nil {
		t.Fatal(err)
	}
	if err := check.QueryRow(`SELECT state FROM git_operations WHERE execution_id = 'legacy-execution-task-1'`).Scan(&operationState); err != nil {
		t.Fatal(err)
	}
	if checkpointSHA != commitSHA || checkpointRef != "refs/heads/main" || operationState != "succeeded" {
		t.Fatalf("git evidence = %q, %q, %q", checkpointSHA, checkpointRef, operationState)
	}

	var legacyColumnCount int
	if err := check.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('local_tasks') WHERE name IN ('telegram_chat_id', 'telegram_message_id')`).Scan(&legacyColumnCount); err != nil {
		t.Fatal(err)
	}
	if legacyColumnCount != 0 {
		t.Fatal("presentation identifiers were copied into the v2 task record")
	}
}

func TestCutoverPreservesDonorIntentAndRetryEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "donor.db")
	seedDonorMigrationDatabase(t, path)

	if _, err := Cutover(context.Background(), path, "test-build"); err != nil {
		t.Fatal(err)
	}
	check, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()

	var intentKind, intentProvider, intentProgress, intentResult string
	if err := check.QueryRow(`SELECT kind, runtime_id, safe_progress, safe_result FROM intent_evidence WHERE id = 'action-1'`).Scan(&intentKind, &intentProvider, &intentProgress, &intentResult); err != nil {
		t.Fatal(err)
	}
	if intentKind != "retry" || intentProvider != "codex" || intentProgress != "provider_started" || intentResult != "retry_queued" {
		t.Fatalf("intent evidence = %q, %q, %q, %q", intentKind, intentProvider, intentProgress, intentResult)
	}

	var failureClass, retryStatus, summary string
	var attempt int
	if err := check.QueryRow(`SELECT failure_class, status, attempt, safe_summary FROM retry_evidence WHERE id = 'retry-task-1'`).Scan(&failureClass, &retryStatus, &attempt, &summary); err != nil {
		t.Fatal(err)
	}
	if failureClass != "transient_transport" || retryStatus != "waiting" || attempt != 2 || summary != "retrying after transport interruption" {
		t.Fatalf("retry evidence = %q, %q, %d, %q", failureClass, retryStatus, attempt, summary)
	}
}

func TestCutoverFailureLeavesActiveDatabaseUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollback.db")
	legacy, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.db.Exec(`
		INSERT INTO tasks (id, repo_profile_id, title, prompt, state, provider, telegram_chat_id, telegram_message_id, base_sha, worktree_path, created_at, updated_at)
		VALUES ('task-rollback', 'repo', 'Rollback', 'test', 'queued', 'codex', 1, 1, '', '', '2026-07-01T00:00:00.000000000Z', '2026-07-01T00:00:00.000000000Z')`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	originalHash, err := fileHash(path)
	if err != nil {
		t.Fatal(err)
	}

	oldHook := cutoverFailureHook
	cutoverFailureHook = func(stage string) error {
		if stage == "after-map" {
			return errors.New("injected cutover failure")
		}
		return nil
	}
	t.Cleanup(func() { cutoverFailureHook = oldHook })
	if _, err := Cutover(context.Background(), path, "test-build"); err == nil || !strings.Contains(err.Error(), "injected cutover failure") {
		t.Fatalf("Cutover() error = %v, want injected failure", err)
	}
	currentHash, err := fileHash(path)
	if err != nil {
		t.Fatal(err)
	}
	if currentHash != originalHash {
		t.Fatalf("active database hash = %q, want original %q", currentHash, originalHash)
	}

	check, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var legacyTables, v2Ledgers int
	if err := check.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'tasks'").Scan(&legacyTables); err != nil {
		t.Fatal(err)
	}
	if err := check.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'migration_ledger'").Scan(&v2Ledgers); err != nil {
		t.Fatal(err)
	}
	if legacyTables != 1 || v2Ledgers != 0 {
		t.Fatalf("post-failure tables = legacy %d, v2 %d", legacyTables, v2Ledgers)
	}
}

func TestCutoverRejectsDatabaseChangedAfterBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "changed-after-backup.db")
	legacy, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.db.Exec(`
		INSERT INTO tasks (id, repo_profile_id, title, prompt, state, provider,
			telegram_chat_id, telegram_message_id, base_sha, worktree_path, created_at, updated_at)
		VALUES ('changed-task', 'repo', 'Before', 'backup', 'queued', 'codex', 1, 1, '', '',
			'2026-07-01T00:00:00.000000000Z', '2026-07-01T00:00:00.000000000Z')`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	oldHook := cutoverFailureHook
	cutoverFailureHook = func(stage string) error {
		if stage != "after-backup" {
			return nil
		}
		writer, err := sql.Open("sqlite", sqliteDSN(path))
		if err != nil {
			return err
		}
		defer writer.Close()
		_, err = writer.Exec("UPDATE tasks SET title = 'After' WHERE id = 'changed-task'")
		return err
	}
	t.Cleanup(func() { cutoverFailureHook = oldHook })

	if _, err := Cutover(context.Background(), path, "test-build"); !errors.Is(err, ErrDatabaseInUse) {
		t.Fatalf("Cutover() error = %v, want ErrDatabaseInUse", err)
	}
	check, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var title string
	if err := check.QueryRow("SELECT title FROM tasks WHERE id = 'changed-task'").Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "After" {
		t.Fatalf("task title = %q, want external change preserved without cutover", title)
	}
}

func TestCutoverFailureAtEveryStageLeavesLegacyDatabase(t *testing.T) {
	for _, stage := range []string{"after-backup", "after-rename", "after-schema", "after-map", "after-drop", "after-ledger"} {
		t.Run(stage, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "failure.db")
			legacy, err := Open(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			taskID := "failure-" + strings.ReplaceAll(stage, "after-", "")
			if _, err := legacy.db.Exec(`
				INSERT INTO tasks (id, repo_profile_id, title, prompt, state, provider,
					telegram_chat_id, telegram_message_id, base_sha, worktree_path, created_at, updated_at)
				VALUES (?, 'repo', 'Failure', 'cutover', 'queued', 'codex', 1, 1, '', '',
					'2026-07-01T00:00:00.000000000Z', '2026-07-01T00:00:00.000000000Z')`, taskID); err != nil {
				t.Fatal(err)
			}
			if err := legacy.Close(); err != nil {
				t.Fatal(err)
			}

			oldHook := cutoverFailureHook
			cutoverFailureHook = func(gotStage string) error {
				if gotStage == stage {
					return errors.New("injected cutover stage failure")
				}
				return nil
			}
			result, err := Cutover(context.Background(), path, "test-build")
			cutoverFailureHook = oldHook
			if err == nil || !strings.Contains(err.Error(), "injected cutover stage failure") {
				t.Fatalf("Cutover() result=%#v error=%v, want injected failure", result, err)
			}

			check, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer check.Close()
			var legacyTables, v2Ledgers int
			if err := check.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'tasks'").Scan(&legacyTables); err != nil {
				t.Fatal(err)
			}
			if err := check.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'migration_ledger'").Scan(&v2Ledgers); err != nil {
				t.Fatal(err)
			}
			if legacyTables != 1 || v2Ledgers != 0 {
				t.Fatalf("post-failure tables = legacy %d, v2 %d", legacyTables, v2Ledgers)
			}
		})
	}
}

func seedDonorMigrationDatabase(t *testing.T, path string) {
	t.Helper()
	fixture, err := os.ReadFile(filepath.Join("testdata", "migration", "donor_v1.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, fixture, 0o600); err != nil {
		t.Fatal(err)
	}
	legacy, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`
		INSERT INTO tasks (id, repo_profile_id, title, prompt, state, provider, telegram_chat_id, telegram_message_id, base_sha, worktree_path, created_at, updated_at)
		VALUES ('task-1', 'repo', 'Donor', 'retry', 'failed', 'codex', 1, 1, '', '', '2026-07-01T00:00:00.000000000Z', '2026-07-01T00:00:00.000000000Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`
		INSERT INTO operator_actions (
			id, chat_id, user_id, kind, provider, target_task_id, result_task_id, payload_ref,
			created_at, expires_at, claimed_at, completed_at, safe_result, claim_owner, lease_expires_at, safe_progress
		) VALUES ('action-1', 11, 22, 'retry', 'codex', 'task-1', '', 'menu-1',
			'2026-07-01T00:01:00.000000000Z', '2026-07-01T01:01:00.000000000Z',
			'2026-07-01T00:02:00.000000000Z', '2026-07-01T00:03:00.000000000Z', 'retry_queued', 'owner',
			'2026-07-01T00:04:00.000000000Z', 'provider_started');
		INSERT INTO task_retries (
			task_id, failure_class, attempt, next_attempt_at, last_checkpoint_at, safe_summary, status,
			claim_owner, claimed_at, lease_expires_at, created_at, updated_at
		) VALUES ('task-1', 'transient_transport', 2, '2026-07-01T00:05:00.000000000Z',
			'2026-07-01T00:00:00.000000000Z', 'retrying after transport interruption', 'waiting',
			'', NULL, NULL, '2026-07-01T00:00:00.000000000Z', '2026-07-01T00:05:00.000000000Z');
		`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCutoverBackupManifestContainsRowCounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.db")
	legacy, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := Cutover(context.Background(), path, "test-build")
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(result.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		SourceHash string         `json:"source_hash"`
		BackupHash string         `json:"backup_hash"`
		RowCounts  map[string]int `json:"row_counts"`
	}
	if err := json.Unmarshal(contents, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.SourceHash == "" || manifest.BackupHash == "" || manifest.RowCounts["tasks"] != 0 || manifest.RowCounts["schema_migrations"] != 3 {
		t.Fatalf("manifest = %#v", manifest)
	}
}

func TestCutoverHandlesFreelistPagesBeforeBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "freelist.db")
	legacy, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.db.Exec(`
		INSERT INTO tasks (id, repo_profile_id, title, prompt, state, provider,
			telegram_chat_id, telegram_message_id, base_sha, worktree_path, created_at, updated_at)
		VALUES ('freelist-task', 'repo', 'Freelist', 'backup', 'queued', 'codex', 1, 1, '', '',
			'2026-07-01T00:00:00.000000000Z', '2026-07-01T00:00:00.000000000Z')`); err != nil {
		t.Fatal(err)
	}
	tx, err := legacy.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 256; index++ {
		if _, err := tx.Exec(`
			INSERT INTO attachments (id, task_id, kind, name, media_type, storage_path, size_bytes, sha256, created_at)
			VALUES (?, 'freelist-task', 'log', ?, 'text/plain', ?, ?, ?, '2026-07-01T00:00:00.000000000Z')`,
			fmt.Sprintf("attachment-%03d", index), strings.Repeat("n", 80), strings.Repeat("p", 80), 4096, fmt.Sprintf("sha-%03d", index)); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.db.Exec("DELETE FROM attachments WHERE task_id = 'freelist-task'"); err != nil {
		t.Fatal(err)
	}
	var freelist int
	if err := legacy.db.QueryRow("PRAGMA freelist_count").Scan(&freelist); err != nil {
		t.Fatal(err)
	}
	if freelist == 0 {
		t.Fatal("fixture did not create freelist pages")
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Cutover(context.Background(), path, "test-build"); err != nil {
		t.Fatalf("Cutover() with freelist pages = %v", err)
	}
}

func TestCutoverRejectsActiveSQLiteWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "active-writer.db")
	legacy, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	writer, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	tx, err := writer.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("UPDATE tasks SET title = title WHERE id = 'missing'"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("INSERT INTO tasks (id, repo_profile_id, title, prompt, state, provider, telegram_chat_id, telegram_message_id, base_sha, worktree_path, created_at, updated_at) VALUES ('writer-task', 'repo', 'Writer', 'hold', 'queued', 'codex', 1, 1, '', '', '2026-07-01T00:00:00.000000000Z', '2026-07-01T00:00:00.000000000Z')"); err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if _, err := Cutover(ctx, path, "test-build"); !errors.Is(err, ErrDatabaseInUse) {
		t.Fatalf("Cutover() error = %v, want ErrDatabaseInUse", err)
	}
}
