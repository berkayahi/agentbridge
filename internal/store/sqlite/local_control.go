package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/berkayahi/agentbridge/internal/localcontrol"
	"github.com/berkayahi/agentbridge/internal/store"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

func (s *RuntimeStore) CreateProject(ctx context.Context, value localcontrol.Project) error {
	if s == nil || s.db == nil || strings.TrimSpace(value.ID) == "" || strings.TrimSpace(value.Name) == "" || value.Revision <= 0 || value.CreatedAt.IsZero() || value.UpdatedAt.IsZero() {
		return fmt.Errorf("create local project: %w", localcontrol.ErrInvalidRequest)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO local_projects (id, name, revision, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`, value.ID, value.Name, value.Revision, timestamp(value.CreatedAt), timestamp(value.UpdatedAt))
	if err != nil {
		return runtimeConflict("create local project", err)
	}
	return nil
}

func (s *RuntimeStore) GetProject(ctx context.Context, id string) (localcontrol.Project, error) {
	var value localcontrol.Project
	var created, updated string
	if err := s.db.QueryRowContext(ctx, `SELECT id, name, revision, created_at, updated_at FROM local_projects WHERE id = ?`, id).Scan(&value.ID, &value.Name, &value.Revision, &created, &updated); err != nil {
		return localcontrol.Project{}, runtimeNotFound("get local project", err)
	}
	var err error
	if value.CreatedAt, err = parseTimestamp(created); err != nil {
		return localcontrol.Project{}, err
	}
	if value.UpdatedAt, err = parseTimestamp(updated); err != nil {
		return localcontrol.Project{}, err
	}
	return value, nil
}

func (s *RuntimeStore) CreateRepository(ctx context.Context, value localcontrol.Repository) error {
	if s == nil || s.db == nil || strings.TrimSpace(value.ID) == "" || strings.TrimSpace(value.Remote) == "" || value.CreatedAt.IsZero() {
		return fmt.Errorf("create local repository: %w", localcontrol.ErrInvalidRequest)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO repository_bindings (id, remote_url, created_at) VALUES (?, ?, ?)`, value.ID, value.Remote, timestamp(value.CreatedAt))
	if err != nil {
		return runtimeConflict("create local repository", err)
	}
	return nil
}

func (s *RuntimeStore) GetRepository(ctx context.Context, id string) (localcontrol.Repository, error) {
	var value localcontrol.Repository
	var created string
	if err := s.db.QueryRowContext(ctx, `SELECT id, remote_url, created_at FROM repository_bindings WHERE id = ?`, id).Scan(&value.ID, &value.Remote, &created); err != nil {
		return localcontrol.Repository{}, runtimeNotFound("get local repository", err)
	}
	var err error
	if value.CreatedAt, err = parseTimestamp(created); err != nil {
		return localcontrol.Repository{}, err
	}
	return value, nil
}

func (s *RuntimeStore) CreateBoard(ctx context.Context, value localcontrol.Board) error {
	if s == nil || s.db == nil || strings.TrimSpace(value.ID) == "" || strings.TrimSpace(value.ProjectID) == "" || strings.TrimSpace(value.Name) == "" || value.Revision <= 0 || value.CreatedAt.IsZero() || value.UpdatedAt.IsZero() {
		return fmt.Errorf("create local board: %w", localcontrol.ErrInvalidRequest)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO local_boards (id, project_id, name, revision, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`, value.ID, value.ProjectID, value.Name, value.Revision, timestamp(value.CreatedAt), timestamp(value.UpdatedAt))
	if err != nil {
		return runtimeConflict("create local board", err)
	}
	return nil
}

func (s *RuntimeStore) CreateProjectAtomically(ctx context.Context, value localcontrol.Project, event localcontrol.Event, idempotency localcontrol.IdempotencyRecord) error {
	if s == nil || s.db == nil || strings.TrimSpace(value.ID) == "" || strings.TrimSpace(value.Name) == "" || value.Revision <= 0 || value.CreatedAt.IsZero() || value.UpdatedAt.IsZero() || event.ResourceType != "project" || event.ResourceID != value.ID {
		return fmt.Errorf("create local project atomically: %w", localcontrol.ErrInvalidRequest)
	}
	return s.commitLocalCreation(ctx, "project", event, idempotency, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO local_projects (id, name, revision, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`, value.ID, value.Name, value.Revision, timestamp(value.CreatedAt), timestamp(value.UpdatedAt))
		return err
	})
}

func (s *RuntimeStore) CreateRepositoryAtomically(ctx context.Context, value localcontrol.Repository, event localcontrol.Event, idempotency localcontrol.IdempotencyRecord) error {
	if s == nil || s.db == nil || strings.TrimSpace(value.ID) == "" || strings.TrimSpace(value.Remote) == "" || value.CreatedAt.IsZero() || event.ResourceType != "repository" || event.ResourceID != value.ID {
		return fmt.Errorf("create local repository atomically: %w", localcontrol.ErrInvalidRequest)
	}
	return s.commitLocalCreation(ctx, "repository", event, idempotency, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO repository_bindings (id, remote_url, created_at) VALUES (?, ?, ?)`, value.ID, value.Remote, timestamp(value.CreatedAt))
		return err
	})
}

func (s *RuntimeStore) CreateBoardAtomically(ctx context.Context, value localcontrol.Board, event localcontrol.Event, idempotency localcontrol.IdempotencyRecord) error {
	if s == nil || s.db == nil || strings.TrimSpace(value.ID) == "" || strings.TrimSpace(value.ProjectID) == "" || strings.TrimSpace(value.Name) == "" || value.Revision <= 0 || value.CreatedAt.IsZero() || value.UpdatedAt.IsZero() || event.ResourceType != "board" || event.ResourceID != value.ID {
		return fmt.Errorf("create local board atomically: %w", localcontrol.ErrInvalidRequest)
	}
	return s.commitLocalCreation(ctx, "board", event, idempotency, func(tx *sql.Tx) error {
		var found string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM local_projects WHERE id = ?`, value.ProjectID).Scan(&found); err != nil {
			return runtimeNotFound("validate local board project", err)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO local_boards (id, project_id, name, revision, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`, value.ID, value.ProjectID, value.Name, value.Revision, timestamp(value.CreatedAt), timestamp(value.UpdatedAt))
		return err
	})
}

func (s *RuntimeStore) commitLocalCreation(ctx context.Context, resourceType string, event localcontrol.Event, idempotency localcontrol.IdempotencyRecord, insert func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin local %s creation: %w", resourceType, err)
	}
	defer tx.Rollback()
	if err := insert(tx); err != nil {
		return runtimeConflict("insert local "+resourceType, err)
	}
	if _, err := insertLocalEventTx(ctx, tx, event); err != nil {
		return err
	}
	if err := saveIdempotencyTx(ctx, tx, idempotency); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit local %s creation: %w", resourceType, err)
	}
	return nil
}

func (s *RuntimeStore) GetBoard(ctx context.Context, id string) (localcontrol.Board, error) {
	var value localcontrol.Board
	var created, updated string
	if err := s.db.QueryRowContext(ctx, `SELECT id, project_id, name, revision, created_at, updated_at FROM local_boards WHERE id = ?`, id).Scan(&value.ID, &value.ProjectID, &value.Name, &value.Revision, &created, &updated); err != nil {
		return localcontrol.Board{}, runtimeNotFound("get local board", err)
	}
	var err error
	if value.CreatedAt, err = parseTimestamp(created); err != nil {
		return localcontrol.Board{}, err
	}
	if value.UpdatedAt, err = parseTimestamp(updated); err != nil {
		return localcontrol.Board{}, err
	}
	return value, nil
}

func (s *RuntimeStore) CreateTaskInContext(ctx context.Context, projectID, boardID, targetDeviceID string, value workmodel.Task, initial workmodel.Event, localEvent localcontrol.Event) (localcontrol.Event, error) {
	return s.createTaskInContext(ctx, projectID, boardID, targetDeviceID, value, initial, localEvent, nil)
}

// CreateTaskAtomically commits the first canonical task boundary together
// with its execution/session lineage, device assignment, audit events, and
// idempotency response. A controller retry therefore cannot observe a task
// without its durable response or allocate a second task after a crash.
func (s *RuntimeStore) CreateTaskAtomically(ctx context.Context, creation localcontrol.AtomicTaskCreation) (localcontrol.Event, error) {
	return s.createTaskInContext(ctx, creation.ProjectID, creation.BoardID, creation.TargetDeviceID, creation.Task, creation.InitialEvent, creation.LocalEvent, &creation.Idempotency)
}

func (s *RuntimeStore) createTaskInContext(ctx context.Context, projectID, boardID, targetDeviceID string, value workmodel.Task, initial workmodel.Event, localEvent localcontrol.Event, idempotency *localcontrol.IdempotencyRecord) (localcontrol.Event, error) {
	if s == nil || s.db == nil || strings.TrimSpace(projectID) == "" || strings.TrimSpace(boardID) == "" || strings.TrimSpace(targetDeviceID) == "" || value.ID == "" || value.RepoProfileID == "" || !value.Provider.Valid() || strings.TrimSpace(value.Title) == "" || strings.TrimSpace(value.Prompt) == "" || value.CreatedAt.IsZero() || initial.TaskID != value.ID {
		return localcontrol.Event{}, fmt.Errorf("create local task: %w", localcontrol.ErrInvalidRequest)
	}
	now := value.CreatedAt.UTC()
	executionID, sessionID, commandID := value.ID+"-execution", value.ID+"-session", value.ID+"-command"
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return localcontrol.Event{}, fmt.Errorf("begin local task: %w", err)
	}
	defer tx.Rollback()
	for _, check := range []struct {
		query string
		id    string
		label string
	}{
		{`SELECT id FROM local_projects WHERE id = ?`, projectID, "project"},
		{`SELECT id FROM local_boards WHERE id = ? AND project_id = ?`, boardID, "board"},
		{`SELECT id FROM repository_bindings WHERE id = ?`, value.RepoProfileID, "repository"},
		{`SELECT id FROM local_devices WHERE id = ? AND state = 'paired'`, targetDeviceID, "device"},
	} {
		var found string
		args := []any{check.id}
		if check.label == "board" {
			args = append(args, projectID)
		}
		if err := tx.QueryRowContext(ctx, check.query, args...).Scan(&found); err != nil {
			return localcontrol.Event{}, runtimeNotFound("validate local task "+check.label, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO local_tasks (id, repo_profile_id, title, prompt, state, provider, active_execution_id, controller_owner, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, value.ID, value.RepoProfileID, value.Title, value.Prompt, workmodel.Queued, value.Provider, executionID, workmodel.TaskControllerLocal, timestamp(now), timestamp(now)); err != nil {
		return localcontrol.Event{}, runtimeConflict("insert local task", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO sessions (id, runtime_id, repository_id, local_task_id, active_local_task_id, provider_session_id, provider_thread_id, status, resumable, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, '', '', 'queued', 1, ?, ?)`, sessionID, value.Provider, value.RepoProfileID, value.ID, value.ID, timestamp(now), timestamp(now)); err != nil {
		return localcontrol.Event{}, runtimeConflict("insert local task session", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO executions (id, local_task_id, session_id, runtime_id, repository_id, retry_of_execution_id, state, attempt, fencing_epoch, command_id, max_transient_attempts, policy_snapshot, source_state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, NULL, 'queued', 0, 1, ?, 0, ?, 'standalone', ?, ?)`, executionID, value.ID, sessionID, value.Provider, value.RepoProfileID, commandID, []byte(`{}`), timestamp(now), timestamp(now)); err != nil {
		return localcontrol.Event{}, runtimeConflict("insert local task execution", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_presentations (local_task_id, telegram_chat_id, telegram_message_id) VALUES (?, 0, 0)`, value.ID); err != nil {
		return localcontrol.Event{}, runtimeConflict("insert local task presentation", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO local_task_contexts (local_task_id, project_id, board_id, created_at) VALUES (?, ?, ?, ?)`, value.ID, projectID, boardID, timestamp(now)); err != nil {
		return localcontrol.Event{}, runtimeConflict("link local task context", err)
	}
	var assignmentEpoch uint64
	if err := tx.QueryRowContext(ctx, `SELECT connection_epoch FROM local_devices WHERE id = ?`, targetDeviceID).Scan(&assignmentEpoch); err != nil {
		return localcontrol.Event{}, runtimeNotFound("load local device epoch", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO local_task_devices (local_task_id, device_id, assignment_epoch, last_ack_cursor, last_observed_cursor, state, updated_at) VALUES (?, ?, ?, 0, 0, 'assigned', ?)`, value.ID, targetDeviceID, assignmentEpoch, timestamp(now)); err != nil {
		return localcontrol.Event{}, runtimeConflict("assign local task device", err)
	}
	if err := insertRuntimeEvent(ctx, tx, initial, value.ID, executionID); err != nil {
		return localcontrol.Event{}, err
	}
	stored, err := insertLocalEventTx(ctx, tx, localEvent)
	if err != nil {
		return localcontrol.Event{}, err
	}
	if idempotency != nil {
		if err := saveIdempotencyTx(ctx, tx, *idempotency); err != nil {
			return localcontrol.Event{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return localcontrol.Event{}, fmt.Errorf("commit local task: %w", err)
	}
	return stored, nil
}

func (s *RuntimeStore) TaskContext(ctx context.Context, taskID string) (string, string, error) {
	var projectID, boardID string
	if err := s.db.QueryRowContext(ctx, `SELECT project_id, board_id FROM local_task_contexts WHERE local_task_id = ?`, taskID).Scan(&projectID, &boardID); err != nil {
		return "", "", runtimeNotFound("get local task context", err)
	}
	return projectID, boardID, nil
}

func (s *RuntimeStore) UpdateTaskAtRevision(ctx context.Context, taskID string, expected int64, title, prompt string, localEvent localcontrol.Event) (localcontrol.Event, error) {
	if s == nil || s.db == nil || strings.TrimSpace(taskID) == "" || expected <= 0 || strings.TrimSpace(title) == "" || strings.TrimSpace(prompt) == "" || localEvent.TaskID != taskID {
		return localcontrol.Event{}, fmt.Errorf("update local task: %w", localcontrol.ErrInvalidRequest)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return localcontrol.Event{}, fmt.Errorf("begin local task update: %w", err)
	}
	defer tx.Rollback()
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM local_tasks WHERE id = ?`, taskID).Scan(&revision); err != nil {
		return localcontrol.Event{}, runtimeNotFound("load local task update", err)
	}
	if revision != expected {
		return localcontrol.Event{}, fmt.Errorf("local task revision %d, expected %d: %w", revision, expected, localcontrol.ErrStaleRevision)
	}
	at := localEvent.CreatedAt.UTC()
	if at.IsZero() {
		at = time.Now().UTC()
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE local_tasks SET title = ?, prompt = ?, revision = revision + 1, updated_at = ?
		WHERE id = ? AND revision = ?`, title, prompt, timestamp(at), taskID, expected)
	if err != nil {
		return localcontrol.Event{}, runtimeConflict("update local task fields", err)
	}
	if err := requireRuntimeChanged(result, "update local task fields"); err != nil {
		return localcontrol.Event{}, fmt.Errorf("update local task fields: %w", localcontrol.ErrStaleRevision)
	}
	localEvent.Revision = expected + 1
	stored, err := insertLocalEventTx(ctx, tx, localEvent)
	if err != nil {
		return localcontrol.Event{}, err
	}
	if err := tx.Commit(); err != nil {
		return localcontrol.Event{}, fmt.Errorf("commit local task update: %w", err)
	}
	return stored, nil
}

func (s *RuntimeStore) ExecutionInfo(ctx context.Context, taskID string) (localcontrol.ExecutionInfo, error) {
	var value localcontrol.ExecutionInfo
	if err := s.db.QueryRowContext(ctx, `
		SELECT e.id, e.session_id, e.runtime_id, e.repository_id, e.fencing_epoch, e.policy_snapshot
		FROM executions e JOIN local_tasks t ON t.active_execution_id = e.id
		WHERE t.id = ?`, taskID).Scan(&value.ExecutionID, &value.SessionID, &value.RuntimeID, &value.RepositoryID, &value.FencingEpoch, &value.Policy); err != nil {
		return localcontrol.ExecutionInfo{}, runtimeNotFound("get local execution", err)
	}
	return value, nil
}

func (s *RuntimeStore) TransitionAtRevision(ctx context.Context, taskID string, expected int64, to workmodel.State, event workmodel.Event, localEvent localcontrol.Event) (localcontrol.Event, error) {
	if s == nil || s.db == nil || event.TaskID != taskID || expected <= 0 || !to.Valid() {
		return localcontrol.Event{}, fmt.Errorf("transition local task: %w", localcontrol.ErrInvalidRequest)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return localcontrol.Event{}, fmt.Errorf("begin local transition: %w", err)
	}
	defer tx.Rollback()
	var from workmodel.State
	var executionID string
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT state, COALESCE(active_execution_id, ''), revision FROM local_tasks WHERE id = ?`, taskID).Scan(&from, &executionID, &revision); err != nil {
		return localcontrol.Event{}, runtimeNotFound("load local transition", err)
	}
	if revision != expected {
		return localcontrol.Event{}, fmt.Errorf("local task revision %d, expected %d: %w", revision, expected, localcontrol.ErrStaleRevision)
	}
	if !workmodel.CanTransition(from, to) {
		return localcontrol.Event{}, fmt.Errorf("transition %s to %s: %w", from, to, store.ErrInvalidTransition)
	}
	at := event.CreatedAt.UTC()
	if at.IsZero() {
		at = time.Now().UTC()
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE local_tasks SET state = ?, updated_at = ?, revision = revision + 1,
		started_at = CASE WHEN ? = 'running' AND started_at IS NULL THEN ? ELSE started_at END,
		finished_at = CASE WHEN ? IN ('completed', 'failed', 'canceled') THEN ? ELSE finished_at END
		WHERE id = ? AND state = ? AND revision = ?`, to, timestamp(at), to, timestamp(at), to, timestamp(at), taskID, from, expected)
	if err != nil {
		return localcontrol.Event{}, runtimeConflict("update local task", err)
	}
	if err := requireRuntimeChanged(result, "update local task"); err != nil {
		return localcontrol.Event{}, fmt.Errorf("%w: %v", localcontrol.ErrStaleRevision, err)
	}
	if executionID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE executions SET state = ?, updated_at = ? WHERE id = ?`, executionStateForTask(to), timestamp(at), executionID); err != nil {
			return localcontrol.Event{}, runtimeConflict("update local execution", err)
		}
	}
	if err := insertRuntimeEvent(ctx, tx, event, taskID, executionID); err != nil {
		return localcontrol.Event{}, err
	}
	localEvent.Revision = expected + 1
	stored, err := insertLocalEventTx(ctx, tx, localEvent)
	if err != nil {
		return localcontrol.Event{}, err
	}
	if err := tx.Commit(); err != nil {
		return localcontrol.Event{}, fmt.Errorf("commit local transition: %w", err)
	}
	return stored, nil
}

func (s *RuntimeStore) AppendLocalEvent(ctx context.Context, value localcontrol.Event) (localcontrol.Event, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return localcontrol.Event{}, fmt.Errorf("begin append local event: %w", err)
	}
	defer tx.Rollback()
	stored, err := insertLocalEventTx(ctx, tx, value)
	if err != nil {
		return localcontrol.Event{}, err
	}
	if err := tx.Commit(); err != nil {
		return localcontrol.Event{}, fmt.Errorf("commit append local event: %w", err)
	}
	return stored, nil
}

func insertLocalEventTx(ctx context.Context, db *sql.Tx, value localcontrol.Event) (localcontrol.Event, error) {
	if strings.TrimSpace(value.ID) == "" || strings.TrimSpace(value.ResourceType) == "" || strings.TrimSpace(value.ResourceID) == "" || value.Revision <= 0 || strings.TrimSpace(value.Type) == "" || value.CreatedAt.IsZero() {
		return localcontrol.Event{}, fmt.Errorf("insert local event: %w", localcontrol.ErrInvalidRequest)
	}
	payload := append([]byte(nil), value.Payload...)
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	taskCursor := int64(0)
	if value.TaskID != "" {
		if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(task_cursor), 0) + 1 FROM local_control_events WHERE local_task_id = ?`, value.TaskID).Scan(&taskCursor); err != nil {
			return localcontrol.Event{}, fmt.Errorf("read local task event cursor: %w", err)
		}
		if taskCursor <= 0 {
			return localcontrol.Event{}, fmt.Errorf("local task event cursor overflow: %w", localcontrol.ErrInvalidRequest)
		}
	}
	result, err := db.ExecContext(ctx, `INSERT INTO local_control_events (id, resource_type, resource_id, local_task_id, task_cursor, revision, event_type, payload, created_at) VALUES (?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?)`, value.ID, value.ResourceType, value.ResourceID, value.TaskID, taskCursor, value.Revision, value.Type, payload, timestamp(value.CreatedAt))
	if err != nil {
		return localcontrol.Event{}, runtimeConflict("insert local event", err)
	}
	cursor, err := result.LastInsertId()
	if err != nil {
		return localcontrol.Event{}, fmt.Errorf("read local event cursor: %w", err)
	}
	value.Cursor = uint64(cursor)
	value.TaskCursor = uint64(taskCursor)
	value.Payload = payload
	if value.TaskID != "" {
		if _, err := db.ExecContext(ctx, `
			UPDATE local_task_devices SET last_ack_cursor = CASE WHEN last_ack_cursor < ? THEN ? ELSE last_ack_cursor END, updated_at = ?
			WHERE local_task_id = ? AND state IN ('assigned', 'unreachable')`, value.Cursor, value.Cursor, timestamp(value.CreatedAt), value.TaskID); err != nil {
			return localcontrol.Event{}, runtimeConflict("advance local device event cursor", err)
		}
	}
	return value, nil
}

func (s *RuntimeStore) ListLocalEvents(ctx context.Context, taskID string, after uint64, limit int) ([]localcontrol.Event, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT cursor, task_cursor, id, resource_type, resource_id, COALESCE(local_task_id, ''), revision, event_type, payload, created_at
		FROM local_control_events WHERE local_task_id = ? AND cursor > ? ORDER BY cursor LIMIT ?`, taskID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("list local events: %w", err)
	}
	defer rows.Close()
	values := make([]localcontrol.Event, 0, limit)
	for rows.Next() {
		var value localcontrol.Event
		var cursor, taskCursor int64
		var created string
		if err := rows.Scan(&cursor, &taskCursor, &value.ID, &value.ResourceType, &value.ResourceID, &value.TaskID, &value.Revision, &value.Type, &value.Payload, &created); err != nil {
			return nil, fmt.Errorf("scan local event: %w", err)
		}
		if cursor < 0 || taskCursor < 0 {
			return nil, fmt.Errorf("scan local event: negative cursor")
		}
		value.Cursor = uint64(cursor)
		value.TaskCursor = uint64(taskCursor)
		value.Payload = append([]byte(nil), value.Payload...)
		var parseErr error
		if value.CreatedAt, parseErr = parseTimestamp(created); parseErr != nil {
			return nil, parseErr
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func (s *RuntimeStore) LoadIdempotency(ctx context.Context, key string) (localcontrol.IdempotencyRecord, error) {
	var value localcontrol.IdempotencyRecord
	var created string
	if err := s.db.QueryRowContext(ctx, `SELECT idempotency_key, operation, request_hash, response_payload, created_at FROM local_command_idempotency WHERE idempotency_key = ?`, key).Scan(&value.Key, &value.Operation, &value.RequestHash, &value.ResponseBytes, &created); err != nil {
		return localcontrol.IdempotencyRecord{}, runtimeNotFound("load local idempotency", err)
	}
	var err error
	if value.CreatedAt, err = parseTimestamp(created); err != nil {
		return localcontrol.IdempotencyRecord{}, err
	}
	value.ResponseBytes = append([]byte(nil), value.ResponseBytes...)
	return value, nil
}

func (s *RuntimeStore) SaveIdempotency(ctx context.Context, value localcontrol.IdempotencyRecord) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin save local idempotency: %w", err)
	}
	defer tx.Rollback()
	if err := saveIdempotencyTx(ctx, tx, value); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit save local idempotency: %w", err)
	}
	return nil
}

func saveIdempotencyTx(ctx context.Context, tx *sql.Tx, value localcontrol.IdempotencyRecord) error {
	if value.Key == "" || value.Operation == "" || value.RequestHash == "" || len(value.ResponseBytes) == 0 || value.CreatedAt.IsZero() {
		return fmt.Errorf("save local idempotency: %w", localcontrol.ErrInvalidRequest)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO local_command_idempotency (idempotency_key, operation, request_hash, response_payload, created_at) VALUES (?, ?, ?, ?, ?)`, value.Key, value.Operation, value.RequestHash, value.ResponseBytes, timestamp(value.CreatedAt))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique constraint") {
			var existing localcontrol.IdempotencyRecord
			var created string
			loadErr := tx.QueryRowContext(ctx, `SELECT idempotency_key, operation, request_hash, response_payload, created_at FROM local_command_idempotency WHERE idempotency_key = ?`, value.Key).Scan(&existing.Key, &existing.Operation, &existing.RequestHash, &existing.ResponseBytes, &created)
			if loadErr == nil && existing.Operation == value.Operation && existing.RequestHash == value.RequestHash {
				if bytes.Equal(existing.ResponseBytes, value.ResponseBytes) {
					return nil
				}
				// A disconnected remote action may first persist a queued
				// response and later replace it with the durable completion
				// after reconnect. The request hash remains immutable; only
				// that same request's response is allowed to advance.
				if _, updateErr := tx.ExecContext(ctx, `
					UPDATE local_command_idempotency
					SET response_payload = ?, created_at = ?
					WHERE idempotency_key = ? AND operation = ? AND request_hash = ?`, value.ResponseBytes, timestamp(value.CreatedAt), value.Key, value.Operation, value.RequestHash); updateErr != nil {
					return runtimeConflict("update local idempotency", updateErr)
				}
				return nil
			}
			return fmt.Errorf("save local idempotency: %w", store.ErrConflict)
		}
		return runtimeConflict("save local idempotency", err)
	}
	return nil
}

func (s *RuntimeStore) RecordCheckpoint(ctx context.Context, taskID string, receipt localcontrol.CommitReceipt) error {
	info, err := s.ExecutionInfo(ctx, taskID)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO git_checkpoints (id, execution_id, repository_id, commit_sha, remote_ref, created_at) VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`, receipt.ID, info.ExecutionID, info.RepositoryID, receipt.CommitSHA, receipt.RemoteRef, timestamp(receipt.ObservedAt))
	if err != nil {
		return runtimeConflict("record local checkpoint", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("record local checkpoint rows affected: %w", err)
	}
	if changed == 0 {
		var existing localcontrol.CommitReceipt
		var observed string
		if err := s.db.QueryRowContext(ctx, `SELECT commit_sha, remote_ref, created_at FROM git_checkpoints WHERE id = ?`, receipt.ID).Scan(&existing.CommitSHA, &existing.RemoteRef, &observed); err != nil {
			return runtimeConflict("load existing local checkpoint", err)
		}
		existing.ID = receipt.ID
		existing.ObservedAt, err = parseTimestamp(observed)
		if err != nil {
			return err
		}
		if existing.CommitSHA != receipt.CommitSHA || existing.RemoteRef != receipt.RemoteRef {
			return fmt.Errorf("record local checkpoint: %w", store.ErrConflict)
		}
	}
	return nil
}

func (s *RuntimeStore) LoadCheckpoint(ctx context.Context, taskID string) (localcontrol.CommitReceipt, error) {
	var receipt localcontrol.CommitReceipt
	var observed string
	err := s.db.QueryRowContext(ctx, `
		SELECT g.id, g.commit_sha, g.remote_ref, g.created_at
		FROM git_checkpoints g JOIN executions e ON e.id = g.execution_id
		WHERE e.local_task_id = ? ORDER BY g.created_at DESC, g.id DESC LIMIT 1`, taskID).Scan(&receipt.ID, &receipt.CommitSHA, &receipt.RemoteRef, &observed)
	if err != nil {
		return localcontrol.CommitReceipt{}, runtimeNotFound("load local checkpoint", err)
	}
	var parseErr error
	if receipt.ObservedAt, parseErr = parseTimestamp(observed); parseErr != nil {
		return localcontrol.CommitReceipt{}, parseErr
	}
	return receipt, nil
}

var _ localcontrol.AuthorityStore = (*RuntimeStore)(nil)
