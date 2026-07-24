package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/berkayahi/agentbridge/internal/store"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

// RuntimeStore is the production adapter over the v2 execution-kernel
// database. It deliberately has no embedded LegacyStore, so legacy task,
// projection, and migration methods cannot be promoted into production
// composition by accident.
type RuntimeStore struct{ db *sql.DB }

func OpenV2Runtime(ctx context.Context, path string) (*RuntimeStore, error) {
	return OpenV2(ctx, path)
}

func OpenV2RuntimeWithRuntimeLock(ctx context.Context, path string) (*RuntimeStore, error) {
	return OpenV2WithRuntimeLock(ctx, path)
}

func (s *RuntimeStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// EnsureRepositoryBinding registers a repository profile before a task can
// reference it. The URL is kept in the canonical repository binding record;
// callers never pass a filesystem path through the task API.
func (s *RuntimeStore) EnsureRepositoryBinding(ctx context.Context, id, remoteURL string) error {
	if s == nil || s.db == nil || strings.TrimSpace(id) == "" || strings.TrimSpace(remoteURL) == "" {
		return fmt.Errorf("ensure repository binding: %w", store.ErrConflict)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO repository_bindings (id, remote_url, created_at) VALUES (?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET remote_url = excluded.remote_url`, id, remoteURL, timestamp(time.Now().UTC()))
	if err != nil {
		return fmt.Errorf("ensure repository binding: %w", err)
	}
	return nil
}

func (s *RuntimeStore) CreateTask(ctx context.Context, value workmodel.Task, initial workmodel.Event) error {
	if s == nil || s.db == nil || value.ID == "" || value.RepoProfileID == "" || !value.Provider.Valid() || strings.TrimSpace(value.Title) == "" || strings.TrimSpace(value.Prompt) == "" || value.CreatedAt.IsZero() || initial.TaskID != value.ID {
		return fmt.Errorf("create v2 task: %w", store.ErrConflict)
	}
	if !value.State.Valid() {
		value.State = workmodel.Queued
	}
	now := value.CreatedAt.UTC()
	executionID, sessionID, commandID := value.ID+"-execution", value.ID+"-session", value.ID+"-command"
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin create v2 task: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO repository_bindings (id, remote_url, created_at) VALUES (?, ?, ?) ON CONFLICT(id) DO NOTHING`, value.RepoProfileID, value.RepoProfileID, timestamp(now)); err != nil {
		return fmt.Errorf("bind v2 task repository: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO local_tasks (id, repo_profile_id, title, prompt, state, provider, active_execution_id, controller_owner, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, value.ID, value.RepoProfileID, value.Title, value.Prompt, value.State, value.Provider, executionID, workmodel.TaskControllerStandalone, timestamp(now), timestamp(now)); err != nil {
		return runtimeConflict("insert v2 local task", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO sessions (id, runtime_id, repository_id, local_task_id, active_local_task_id, provider_session_id, provider_thread_id, status, resumable, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, '', '', 'queued', 1, ?, ?)`, sessionID, value.Provider, value.RepoProfileID, value.ID, value.ID, timestamp(now), timestamp(now)); err != nil {
		return runtimeConflict("insert v2 task session", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO executions (id, local_task_id, session_id, runtime_id, repository_id, retry_of_execution_id, state, attempt, fencing_epoch, command_id, max_transient_attempts, policy_snapshot, source_state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, NULL, 'queued', 0, 1, ?, 0, ?, 'standalone', ?, ?)`, executionID, value.ID, sessionID, value.Provider, value.RepoProfileID, commandID, []byte("standalone"), timestamp(now), timestamp(now)); err != nil {
		return runtimeConflict("insert v2 task execution", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_presentations (local_task_id, telegram_chat_id, telegram_message_id) VALUES (?, ?, 0)`, value.ID, value.TelegramChatID); err != nil {
		return runtimeConflict("insert v2 task presentation", err)
	}
	if err := insertRuntimeEvent(ctx, tx, initial, value.ID, executionID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit create v2 task: %w", err)
	}
	return nil
}

func (s *RuntimeStore) Transition(ctx context.Context, taskID string, to workmodel.State, event workmodel.Event) error {
	if s == nil || s.db == nil || event.TaskID != taskID || !to.Valid() {
		return fmt.Errorf("transition v2 task: %w", store.ErrConflict)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transition v2 task: %w", err)
	}
	defer tx.Rollback()
	var from workmodel.State
	var executionID string
	if err := tx.QueryRowContext(ctx, `SELECT state, COALESCE(active_execution_id, '') FROM local_tasks WHERE id = ?`, taskID).Scan(&from, &executionID); err != nil {
		return runtimeNotFound("load v2 task transition", err)
	}
	if from == to {
		return fmt.Errorf("task already in %s: %w", to, store.ErrConflict)
	}
	if !workmodel.CanTransition(from, to) {
		return fmt.Errorf("transition %s to %s: %w", from, to, store.ErrInvalidTransition)
	}
	at := event.CreatedAt.UTC()
	if at.IsZero() {
		at = time.Now().UTC()
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE local_tasks SET state = ?, updated_at = ?, revision = revision + 1,
		started_at = CASE WHEN ? = 'running' AND started_at IS NULL THEN ? ELSE started_at END,
		finished_at = CASE WHEN ? IN ('completed', 'failed', 'canceled') THEN ? ELSE finished_at END
		WHERE id = ? AND state = ?`, to, timestamp(at), to, timestamp(at), to, timestamp(at), taskID, from)
	if err != nil {
		return runtimeConflict("update v2 task state", err)
	}
	if err := requireRuntimeChanged(result, "transition v2 task"); err != nil {
		return err
	}
	if executionID != "" {
		_, _ = tx.ExecContext(ctx, `UPDATE executions SET state = ?, updated_at = ? WHERE id = ?`, executionStateForTask(to), timestamp(at), executionID)
	}
	if err := insertRuntimeEvent(ctx, tx, event, taskID, executionID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit v2 task transition: %w", err)
	}
	return nil
}

func (s *RuntimeStore) AppendEvent(ctx context.Context, event workmodel.Event) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin append v2 event: %w", err)
	}
	defer tx.Rollback()
	var executionID string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(active_execution_id, '') FROM local_tasks WHERE id = ?`, event.TaskID).Scan(&executionID); err != nil {
		return runtimeNotFound("load v2 event task", err)
	}
	if err := insertRuntimeEvent(ctx, tx, event, event.TaskID, executionID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit append v2 event: %w", err)
	}
	return nil
}

func (s *RuntimeStore) Task(ctx context.Context, id string) (workmodel.Task, error) {
	return scanRuntimeTask(s.db.QueryRowContext(ctx, runtimeTaskColumns+` WHERE l.id = ?`, id))
}

func (s *RuntimeStore) ListTasks(ctx context.Context, filter store.ListFilter) ([]workmodel.Task, error) {
	query := runtimeTaskColumns + ` WHERE 1 = 1`
	args := make([]any, 0, len(filter.States)+2)
	if filter.RepoProfileID != "" {
		query += ` AND l.repo_profile_id = ?`
		args = append(args, filter.RepoProfileID)
	}
	if len(filter.States) > 0 {
		query += ` AND l.state IN (` + placeholders(len(filter.States)) + `)`
		for _, state := range filter.States {
			args = append(args, state)
		}
	}
	query += ` ORDER BY l.created_at DESC, l.id DESC`
	if filter.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, filter.Limit)
	}
	return s.queryRuntimeTasks(ctx, query, args...)
}

func (s *RuntimeStore) NonterminalTasks(ctx context.Context) ([]workmodel.Task, error) {
	return s.queryRuntimeTasks(ctx, runtimeTaskColumns+` WHERE l.state NOT IN ('completed', 'canceled') ORDER BY l.created_at, l.id`)
}

func (s *RuntimeStore) queryRuntimeTasks(ctx context.Context, query string, args ...any) ([]workmodel.Task, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query v2 tasks: %w", err)
	}
	defer rows.Close()
	values := make([]workmodel.Task, 0)
	for rows.Next() {
		value, err := scanRuntimeTask(rows)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

const runtimeTaskColumns = `SELECT
	l.id, l.repo_profile_id, l.title, l.prompt, l.state, l.provider, l.revision, l.controller_owner,
	COALESCE(p.telegram_chat_id, 0), COALESCE(p.telegram_message_id, 0),
	l.base_sha, l.worktree_path, l.provider_session_id, l.provider_thread_id,
	l.commit_sha, l.push_ref, l.deployment_url, l.failure_reason,
	l.created_at, l.updated_at, l.started_at, l.finished_at
	FROM local_tasks l LEFT JOIN task_presentations p ON p.local_task_id = l.id`

func scanRuntimeTask(row scanner) (workmodel.Task, error) {
	var value workmodel.Task
	var created, updated string
	var started, finished sql.NullString
	if err := row.Scan(&value.ID, &value.RepoProfileID, &value.Title, &value.Prompt, &value.State, &value.Provider,
		&value.Revision, &value.ControllerOwner, &value.TelegramChatID, &value.TelegramMessageID, &value.BaseSHA, &value.WorktreePath,
		&value.ProviderSessionID, &value.ProviderThreadID, &value.CommitSHA, &value.PushRef,
		&value.DeploymentURL, &value.FailureReason, &created, &updated, &started, &finished); err != nil {
		return workmodel.Task{}, runtimeNotFound("scan v2 task", err)
	}
	var err error
	if value.CreatedAt, err = parseTimestamp(created); err != nil {
		return workmodel.Task{}, err
	}
	if value.UpdatedAt, err = parseTimestamp(updated); err != nil {
		return workmodel.Task{}, err
	}
	if value.StartedAt, err = parseNullableTimestamp(started); err != nil {
		return workmodel.Task{}, err
	}
	if value.FinishedAt, err = parseNullableTimestamp(finished); err != nil {
		return workmodel.Task{}, err
	}
	return value, nil
}

func (s *RuntimeStore) Events(ctx context.Context, taskID string) ([]workmodel.Event, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.id, COALESCE(e.local_task_id, x.local_task_id), e.event_type, e.visibility,
		       e.provider_event_id, e.redacted_payload, e.created_at
		FROM execution_events e LEFT JOIN executions x ON x.id = e.execution_id
		WHERE COALESCE(e.local_task_id, x.local_task_id) = ? ORDER BY e.created_at, e.id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("query v2 events: %w", err)
	}
	defer rows.Close()
	values := make([]workmodel.Event, 0)
	for rows.Next() {
		var value workmodel.Event
		var providerID sql.NullString
		var payload []byte
		var created string
		if err := rows.Scan(&value.ID, &value.TaskID, &value.Type, &value.Visibility, &providerID, &payload, &created); err != nil {
			return nil, fmt.Errorf("scan v2 event: %w", err)
		}
		value.ProviderEventID, value.Payload = providerID.String, append([]byte(nil), payload...)
		value.CreatedAt, err = parseTimestamp(created)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func (s *RuntimeStore) SaveWorkspace(ctx context.Context, taskID, baseSHA, path string) error {
	return s.updateTaskProjection(ctx, "save v2 workspace", `UPDATE local_tasks SET base_sha = ?, worktree_path = ?, updated_at = ?, revision = revision + 1 WHERE id = ?`, baseSHA, path, timestamp(time.Now().UTC()), taskID)
}

func (s *RuntimeStore) SaveTelegramMessage(ctx context.Context, taskID string, messageID int64) error {
	result, err := s.db.ExecContext(ctx, `UPDATE task_presentations SET telegram_message_id = ? WHERE local_task_id = ?`, messageID, taskID)
	if err != nil {
		return fmt.Errorf("save v2 Telegram message: %w", err)
	}
	return requireRuntimeChanged(result, "save v2 Telegram message")
}

func (s *RuntimeStore) SaveProviderSession(ctx context.Context, taskID string, value workmodel.Session) error {
	if value.TaskID != taskID {
		return fmt.Errorf("save v2 provider session: %w", store.ErrConflict)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin save v2 provider session: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE local_tasks SET provider_session_id = ?, provider_thread_id = ?, updated_at = ?, revision = revision + 1 WHERE id = ?`, value.ProviderSessionID, value.ProviderThreadID, timestamp(value.UpdatedAt), taskID); err != nil {
		return runtimeConflict("save v2 provider session projection", err)
	}
	if err := upsertRuntimeSession(ctx, tx, value); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit v2 provider session: %w", err)
	}
	return nil
}

func (s *RuntimeStore) SaveDelivery(ctx context.Context, taskID, commitSHA, pushRef, deploymentURL string) error {
	return s.updateTaskProjection(ctx, "save v2 delivery", `UPDATE local_tasks SET commit_sha = ?, push_ref = ?, deployment_url = ?, updated_at = ?, revision = revision + 1 WHERE id = ?`, commitSHA, pushRef, deploymentURL, timestamp(time.Now().UTC()), taskID)
}

func (s *RuntimeStore) SaveFailure(ctx context.Context, taskID, reason string) error {
	return s.updateTaskProjection(ctx, "save v2 failure", `UPDATE local_tasks SET failure_reason = ?, updated_at = ?, revision = revision + 1 WHERE id = ?`, reason, timestamp(time.Now().UTC()), taskID)
}

func (s *RuntimeStore) RenameTask(ctx context.Context, taskID, title string) error {
	return s.updateTaskProjection(ctx, "rename v2 task", `UPDATE local_tasks SET title = ?, updated_at = ?, revision = revision + 1 WHERE id = ?`, title, timestamp(time.Now().UTC()), taskID)
}

func (s *RuntimeStore) updateTaskProjection(ctx context.Context, operation, query string, args ...any) error {
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return requireRuntimeChanged(result, operation)
}

func (s *RuntimeStore) UpsertSession(ctx context.Context, value workmodel.Session) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin upsert v2 session: %w", err)
	}
	defer tx.Rollback()
	if err := upsertRuntimeSession(ctx, tx, value); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE local_tasks SET provider_session_id = ?, provider_thread_id = ?, updated_at = ?, revision = revision + 1 WHERE id = ?`, value.ProviderSessionID, value.ProviderThreadID, timestamp(value.UpdatedAt), value.TaskID); err != nil {
		return runtimeConflict("update v2 session task", err)
	}
	return tx.Commit()
}

func upsertRuntimeSession(ctx context.Context, db execer, value workmodel.Session) error {
	var repositoryID, runtimeID string
	if err := db.(interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	}).QueryRowContext(ctx, `SELECT repo_profile_id, provider FROM local_tasks WHERE id = ?`, value.TaskID).Scan(&repositoryID, &runtimeID); err != nil {
		return runtimeNotFound("load v2 session task", err)
	}
	if runtimeID == "" {
		runtimeID = string(value.Provider)
	}
	// CreateTask installs one canonical session row and marks it as the
	// active session for the task. Provider adapters report their native
	// session/thread ID separately, so inserting another active row would
	// violate sessions_one_active_task_idx before the controller can persist
	// the native identifiers onto that canonical row.
	var activeSessionID string
	activeErr := db.(interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	}).QueryRowContext(ctx, `SELECT id FROM sessions WHERE active_local_task_id = ?`, value.TaskID).Scan(&activeSessionID)
	if activeErr == nil && activeSessionID != value.ID {
		if _, err := db.ExecContext(ctx, `
			UPDATE sessions
			SET provider_session_id = ?, provider_thread_id = ?, status = ?, resumable = ?, updated_at = ?
			WHERE id = ?`, value.ProviderSessionID, value.ProviderThreadID, value.Status, boolInt(value.Resumable), timestamp(value.UpdatedAt), activeSessionID); err != nil {
			return runtimeConflict("update active v2 session", err)
		}
		return nil
	}
	if activeErr != nil && !errors.Is(activeErr, sql.ErrNoRows) {
		return runtimeConflict("load active v2 session", activeErr)
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO sessions (id, runtime_id, repository_id, local_task_id, active_local_task_id, provider_session_id, provider_thread_id, status, resumable, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET provider_session_id = excluded.provider_session_id, provider_thread_id = excluded.provider_thread_id, status = excluded.status, resumable = excluded.resumable, updated_at = excluded.updated_at`,
		value.ID, runtimeID, repositoryID, value.TaskID, value.TaskID, value.ProviderSessionID, value.ProviderThreadID, value.Status, boolInt(value.Resumable), timestamp(value.CreatedAt), timestamp(value.UpdatedAt))
	if err != nil {
		return runtimeConflict("upsert v2 session", err)
	}
	return nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (s *RuntimeStore) ResumableSessions(ctx context.Context) ([]workmodel.Session, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, local_task_id, runtime_id, provider_session_id, provider_thread_id, status, resumable, created_at, updated_at FROM sessions WHERE resumable = 1 ORDER BY updated_at, id`)
	if err != nil {
		return nil, fmt.Errorf("query resumable v2 sessions: %w", err)
	}
	defer rows.Close()
	values := make([]workmodel.Session, 0)
	for rows.Next() {
		var value workmodel.Session
		var runtimeID string
		var resumable int
		var created, updated string
		if err := rows.Scan(&value.ID, &value.TaskID, &runtimeID, &value.ProviderSessionID, &value.ProviderThreadID, &value.Status, &resumable, &created, &updated); err != nil {
			return nil, err
		}
		value.Provider = workmodel.Provider(runtimeID)
		value.Resumable = resumable == 1
		var err error
		if value.CreatedAt, err = parseTimestamp(created); err != nil {
			return nil, err
		}
		if value.UpdatedAt, err = parseTimestamp(updated); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func (s *RuntimeStore) SaveAttachment(ctx context.Context, value workmodel.Attachment) error {
	var executionID string
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(active_execution_id, '') FROM local_tasks WHERE id = ?`, value.TaskID).Scan(&executionID); err != nil {
		return runtimeNotFound("load v2 attachment task", err)
	}
	if executionID == "" {
		return fmt.Errorf("save v2 attachment: %w", store.ErrConflict)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO attachments (id, local_task_id, execution_id, kind, name, media_type, storage_path, size_bytes, sha256, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, value.ID, value.TaskID, executionID, value.Kind, value.Name, value.MediaType, value.StoragePath, value.SizeBytes, value.SHA256, timestamp(value.CreatedAt))
	if err != nil {
		return runtimeConflict("save v2 attachment", err)
	}
	return nil
}

func (s *RuntimeStore) Attachments(ctx context.Context, taskID string) ([]workmodel.Attachment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, local_task_id, kind, name, media_type, storage_path, size_bytes, sha256, created_at FROM attachments WHERE local_task_id = ? ORDER BY created_at, id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("query v2 attachments: %w", err)
	}
	defer rows.Close()
	values := make([]workmodel.Attachment, 0)
	for rows.Next() {
		var value workmodel.Attachment
		var created string
		if err := rows.Scan(&value.ID, &value.TaskID, &value.Kind, &value.Name, &value.MediaType, &value.StoragePath, &value.SizeBytes, &value.SHA256, &created); err != nil {
			return nil, err
		}
		var err error
		value.CreatedAt, err = parseTimestamp(created)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func (s *RuntimeStore) UpsertApproval(ctx context.Context, value workmodel.Approval) error {
	var executionID string
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(active_execution_id, '') FROM local_tasks WHERE id = ?`, value.TaskID).Scan(&executionID); err != nil {
		return runtimeNotFound("load v2 approval task", err)
	}
	if executionID == "" {
		return fmt.Errorf("upsert v2 approval: %w", store.ErrConflict)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO approvals (id, local_task_id, execution_id, kind, status, request_payload, decision_payload, requested_at, expires_at, resolved_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET status = excluded.status, decision_payload = excluded.decision_payload, expires_at = excluded.expires_at, resolved_at = excluded.resolved_at`, value.ID, value.TaskID, executionID, value.Kind, value.Status, []byte(value.RequestPayload), nullableBytes(value.DecisionPayload), timestamp(value.RequestedAt), nullableTimestamp(value.ExpiresAt), nullableTimestamp(value.ResolvedAt))
	if err != nil {
		return runtimeConflict("upsert v2 approval", err)
	}
	return nil
}

func (s *RuntimeStore) PendingApprovals(ctx context.Context) ([]workmodel.Approval, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, local_task_id, kind, status, request_payload, decision_payload, requested_at, expires_at, resolved_at FROM approvals WHERE status = ? ORDER BY requested_at, id`, workmodel.ApprovalPending)
	if err != nil {
		return nil, fmt.Errorf("query pending v2 approvals: %w", err)
	}
	defer rows.Close()
	values := make([]workmodel.Approval, 0)
	for rows.Next() {
		var value workmodel.Approval
		var request, decision []byte
		var requested string
		var expires, resolved sql.NullString
		if err := rows.Scan(&value.ID, &value.TaskID, &value.Kind, &value.Status, &request, &decision, &requested, &expires, &resolved); err != nil {
			return nil, err
		}
		value.RequestPayload, value.DecisionPayload = request, decision
		var err error
		if value.RequestedAt, err = parseTimestamp(requested); err != nil {
			return nil, err
		}
		if value.ExpiresAt, err = parseNullableTimestamp(expires); err != nil {
			return nil, err
		}
		if value.ResolvedAt, err = parseNullableTimestamp(resolved); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func (s *RuntimeStore) GetApproval(ctx context.Context, id string) (workmodel.Approval, error) {
	var value workmodel.Approval
	var request, decision []byte
	var requested string
	var expires, resolved sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id, local_task_id, kind, status, request_payload, decision_payload, requested_at, expires_at, resolved_at FROM approvals WHERE id = ?`, id).
		Scan(&value.ID, &value.TaskID, &value.Kind, &value.Status, &request, &decision, &requested, &expires, &resolved)
	if err != nil {
		return workmodel.Approval{}, runtimeNotFound("get v2 approval", err)
	}
	value.RequestPayload, value.DecisionPayload = append([]byte(nil), request...), append([]byte(nil), decision...)
	var parseErr error
	if value.RequestedAt, parseErr = parseTimestamp(requested); parseErr != nil {
		return workmodel.Approval{}, parseErr
	}
	if value.ExpiresAt, parseErr = parseNullableTimestamp(expires); parseErr != nil {
		return workmodel.Approval{}, parseErr
	}
	if value.ResolvedAt, parseErr = parseNullableTimestamp(resolved); parseErr != nil {
		return workmodel.Approval{}, parseErr
	}
	return value, nil
}

func (s *RuntimeStore) UpsertAuthIncident(ctx context.Context, value workmodel.AuthIncident) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO auth_incidents (id, execution_id, provider, status, redacted_detail, detected_at, resolved_at)
		VALUES (?, NULL, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET status = excluded.status, redacted_detail = excluded.redacted_detail, resolved_at = excluded.resolved_at`, value.ID, value.Provider, value.Status, []byte(value.Detail), timestamp(value.DetectedAt), nullableTimestamp(value.ResolvedAt))
	if err != nil {
		return runtimeConflict("upsert v2 auth incident", err)
	}
	return nil
}

func (s *RuntimeStore) OpenAuthIncident(ctx context.Context, provider workmodel.Provider) (workmodel.AuthIncident, error) {
	var value workmodel.AuthIncident
	var detail []byte
	var detected string
	var resolved sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id, provider, status, redacted_detail, detected_at, resolved_at FROM auth_incidents WHERE provider = ? AND status = 'open' ORDER BY detected_at DESC, id DESC LIMIT 1`, provider).Scan(&value.ID, &value.Provider, &value.Status, &detail, &detected, &resolved)
	if errors.Is(err, sql.ErrNoRows) {
		return workmodel.AuthIncident{}, store.ErrNotFound
	}
	if err != nil {
		return workmodel.AuthIncident{}, fmt.Errorf("load open v2 auth incident: %w", err)
	}
	value.Detail = append([]byte(nil), detail...)
	var parseErr error
	if value.DetectedAt, parseErr = parseTimestamp(detected); parseErr != nil {
		return workmodel.AuthIncident{}, parseErr
	}
	if value.ResolvedAt, parseErr = parseNullableTimestamp(resolved); parseErr != nil {
		return workmodel.AuthIncident{}, parseErr
	}
	return value, nil
}

func (s *RuntimeStore) AcquireLease(ctx context.Context, repoProfileID, ownerID string, ttl time.Duration) (bool, error) {
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `INSERT INTO repository_leases (repo_profile_id, owner_id, acquired_at, heartbeat_at, expires_at) VALUES (?, ?, ?, ?, ?) ON CONFLICT(repo_profile_id) DO UPDATE SET owner_id = excluded.owner_id, acquired_at = excluded.acquired_at, heartbeat_at = excluded.heartbeat_at, expires_at = excluded.expires_at WHERE repository_leases.expires_at <= excluded.acquired_at OR repository_leases.owner_id = excluded.owner_id`, repoProfileID, ownerID, timestamp(now), timestamp(now), timestamp(now.Add(ttl)))
	if err != nil {
		return false, fmt.Errorf("acquire v2 lease: %w", err)
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

func (s *RuntimeStore) HeartbeatLease(ctx context.Context, repoProfileID, ownerID string, ttl time.Duration) error {
	result, err := s.db.ExecContext(ctx, `UPDATE repository_leases SET heartbeat_at = ?, expires_at = ? WHERE repo_profile_id = ? AND owner_id = ?`, timestamp(time.Now().UTC()), timestamp(time.Now().UTC().Add(ttl)), repoProfileID, ownerID)
	if err != nil {
		return fmt.Errorf("heartbeat v2 lease: %w", err)
	}
	return requireRuntimeChanged(result, "heartbeat v2 lease")
}

func (s *RuntimeStore) ReleaseLease(ctx context.Context, repoProfileID, ownerID string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM repository_leases WHERE repo_profile_id = ? AND owner_id = ?`, repoProfileID, ownerID)
	if err != nil {
		return fmt.Errorf("release v2 lease: %w", err)
	}
	return requireRuntimeChanged(result, "release v2 lease")
}

func (s *RuntimeStore) ExpiredLeases(ctx context.Context) ([]store.Lease, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT repo_profile_id, owner_id, acquired_at, heartbeat_at, expires_at FROM repository_leases WHERE expires_at <= ? ORDER BY expires_at, repo_profile_id`, timestamp(time.Now().UTC()))
	if err != nil {
		return nil, fmt.Errorf("query expired v2 leases: %w", err)
	}
	defer rows.Close()
	values := make([]store.Lease, 0)
	for rows.Next() {
		var value store.Lease
		var acquired, heartbeat, expires string
		if err := rows.Scan(&value.RepoProfileID, &value.OwnerID, &acquired, &heartbeat, &expires); err != nil {
			return nil, err
		}
		var err error
		if value.AcquiredAt, err = parseTimestamp(acquired); err != nil {
			return nil, err
		}
		if value.HeartbeatAt, err = parseTimestamp(heartbeat); err != nil {
			return nil, err
		}
		if value.ExpiresAt, err = parseTimestamp(expires); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func insertRuntimeEvent(ctx context.Context, db execer, value workmodel.Event, taskID, executionID string) error {
	if executionID == "" {
		return fmt.Errorf("append v2 event: %w", store.ErrConflict)
	}
	var providerID any
	if value.ProviderEventID != "" {
		providerID = value.ProviderEventID
	}
	_, err := db.ExecContext(ctx, `INSERT INTO execution_events (id, local_task_id, execution_id, event_type, visibility, provider_event_id, redacted_payload, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, value.ID, taskID, executionID, value.Type, value.Visibility, providerID, []byte(value.Payload), timestamp(value.CreatedAt))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique constraint") {
			return fmt.Errorf("append v2 event: %w", store.ErrDuplicateEvent)
		}
		return runtimeConflict("append v2 event", err)
	}
	return nil
}

func executionStateForTask(state workmodel.State) string {
	switch state {
	case workmodel.Queued:
		return "queued"
	case workmodel.Preparing:
		return "accepted"
	case workmodel.Running, workmodel.Verifying, workmodel.Committing, workmodel.Pushing:
		return "running"
	case workmodel.AwaitingApproval:
		return "awaiting_approval"
	case workmodel.AwaitingAuth:
		return "awaiting_auth"
	case workmodel.Canceled:
		return "canceled"
	case workmodel.Completed:
		return "completed"
	case workmodel.Failed:
		return "failed"
	default:
		return "running"
	}
}

func requireRuntimeChanged(result sql.Result, action string) error {
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s rows affected: %w", action, err)
	}
	if changed != 1 {
		return fmt.Errorf("%s: %w", action, store.ErrConflict)
	}
	return nil
}

func runtimeConflict(action string, err error) error {
	if strings.Contains(strings.ToLower(err.Error()), "constraint failed") || strings.Contains(strings.ToLower(err.Error()), "unique constraint") {
		return fmt.Errorf("%s: %w", action, store.ErrConflict)
	}
	return fmt.Errorf("%s: %w", action, err)
}

func runtimeNotFound(action string, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s: %w", action, store.ErrNotFound)
	}
	return fmt.Errorf("%s: %w", action, err)
}

var _ store.RuntimeStore = (*RuntimeStore)(nil)
