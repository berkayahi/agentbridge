package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/berkayahi/agentbridge/internal/store"
	"github.com/berkayahi/agentbridge/internal/task"
	_ "modernc.org/sqlite"
)

const busyTimeoutMillis = 5000

type Store struct {
	db *sql.DB
}

func Open(ctx context.Context, path string) (*Store, error) {
	dsn := sqliteDSN(path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func sqliteDSN(path string) string {
	u := &url.URL{Scheme: "file", Path: path}
	query := u.Query()
	query.Add("_pragma", "busy_timeout("+strconv.Itoa(busyTimeoutMillis)+")")
	query.Add("_pragma", "foreign_keys(ON)")
	query.Add("_pragma", "journal_mode(WAL)")
	u.RawQuery = query.Encode()
	return u.String()
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) CreateTask(ctx context.Context, value task.Task, initial task.Event) error {
	if initial.TaskID != value.ID {
		return fmt.Errorf("initial event task mismatch: %w", store.ErrConflict)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin create task: %w", err)
	}
	defer tx.Rollback()

	if err := insertTask(ctx, tx, value); err != nil {
		return translateConflict("insert task", err)
	}
	if err := insertEvent(ctx, tx, initial); err != nil {
		return translateEventError(err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit create task: %w", err)
	}
	return nil
}

func insertTask(ctx context.Context, tx *sql.Tx, value task.Task) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO tasks (
			id, repo_profile_id, title, prompt, state, provider,
			telegram_chat_id, telegram_message_id, base_sha, worktree_path,
			provider_session_id, provider_thread_id, commit_sha, push_ref,
			deployment_url, failure_reason, created_at, updated_at, started_at, finished_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		value.ID, value.RepoProfileID, value.Title, value.Prompt, value.State, value.Provider,
		value.TelegramChatID, value.TelegramMessageID, value.BaseSHA, value.WorktreePath,
		value.ProviderSessionID, value.ProviderThreadID, value.CommitSHA, value.PushRef,
		value.DeploymentURL, value.FailureReason, timestamp(value.CreatedAt), timestamp(value.UpdatedAt),
		nullableTimestamp(value.StartedAt), nullableTimestamp(value.FinishedAt),
	)
	return err
}

func (s *Store) Transition(ctx context.Context, taskID string, to task.State, event task.Event) error {
	if event.TaskID != taskID {
		return fmt.Errorf("transition event task mismatch: %w", store.ErrConflict)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return translateTransitionError("begin transition", err)
	}
	defer tx.Rollback()

	var from task.State
	if err := tx.QueryRowContext(ctx, "SELECT state FROM tasks WHERE id = ?", taskID).Scan(&from); err != nil {
		if isBusy(err) {
			return fmt.Errorf("load transition task: %w", store.ErrConflict)
		}
		return translateNotFound("load transition task", err)
	}
	if from == to {
		return fmt.Errorf("task already in %s: %w", to, store.ErrConflict)
	}
	if !task.CanTransition(from, to) {
		return fmt.Errorf("transition %s to %s: %w", from, to, store.ErrInvalidTransition)
	}
	at := timestamp(event.CreatedAt)
	result, err := tx.ExecContext(ctx, `
		UPDATE tasks SET
			state = ?,
			updated_at = ?,
			started_at = CASE
				WHEN ? = ? AND started_at IS NULL THEN ?
				WHEN ? = ? THEN NULL
				ELSE started_at
			END,
			finished_at = CASE
				WHEN ? = ? THEN NULL
				WHEN ? IN (?, ?, ?) THEN ?
				ELSE finished_at
			END
		WHERE id = ? AND state = ?`,
		to, at,
		to, task.Running, at, to, task.Queued,
		to, task.Queued, to, task.Completed, task.Failed, task.Canceled, at,
		taskID, from,
	)
	if err != nil {
		return translateTransitionError("update task state", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read transition result: %w", err)
	}
	if changed != 1 {
		return fmt.Errorf("task changed concurrently: %w", store.ErrConflict)
	}
	if err := insertEvent(ctx, tx, event); err != nil {
		return translateEventError(err)
	}
	if err := tx.Commit(); err != nil {
		return translateTransitionError("commit transition", err)
	}
	return nil
}

func (s *Store) AppendEvent(ctx context.Context, event task.Event) error {
	if err := insertEvent(ctx, s.db, event); err != nil {
		return translateEventError(err)
	}
	return nil
}

type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func insertEvent(ctx context.Context, db execer, event task.Event) error {
	var providerID any
	if event.ProviderEventID != "" {
		providerID = event.ProviderEventID
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO task_events (id, task_id, event_type, visibility, provider_event_id, redacted_payload, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		event.ID, event.TaskID, event.Type, event.Visibility, providerID, []byte(event.Payload), timestamp(event.CreatedAt),
	)
	return err
}

func (s *Store) Task(ctx context.Context, id string) (task.Task, error) {
	value, err := scanTask(s.db.QueryRowContext(ctx, taskColumns+" WHERE id = ?", id))
	if err != nil {
		return task.Task{}, translateNotFound("load task", err)
	}
	return value, nil
}

const taskColumns = `SELECT
	id, repo_profile_id, title, prompt, state, provider,
	telegram_chat_id, telegram_message_id, base_sha, worktree_path,
	provider_session_id, provider_thread_id, commit_sha, push_ref,
	deployment_url, failure_reason, created_at, updated_at, started_at, finished_at
	FROM tasks`

type scanner interface {
	Scan(...any) error
}

func scanTask(row scanner) (task.Task, error) {
	var value task.Task
	var created, updated string
	var started, finished sql.NullString
	err := row.Scan(
		&value.ID, &value.RepoProfileID, &value.Title, &value.Prompt, &value.State, &value.Provider,
		&value.TelegramChatID, &value.TelegramMessageID, &value.BaseSHA, &value.WorktreePath,
		&value.ProviderSessionID, &value.ProviderThreadID, &value.CommitSHA, &value.PushRef,
		&value.DeploymentURL, &value.FailureReason, &created, &updated, &started, &finished,
	)
	if err != nil {
		return task.Task{}, err
	}
	if value.CreatedAt, err = parseTimestamp(created); err != nil {
		return task.Task{}, err
	}
	if value.UpdatedAt, err = parseTimestamp(updated); err != nil {
		return task.Task{}, err
	}
	if value.StartedAt, err = parseNullableTimestamp(started); err != nil {
		return task.Task{}, err
	}
	if value.FinishedAt, err = parseNullableTimestamp(finished); err != nil {
		return task.Task{}, err
	}
	return value, nil
}

func (s *Store) ListTasks(ctx context.Context, filter store.ListFilter) ([]task.Task, error) {
	query := taskColumns + " WHERE 1 = 1"
	args := make([]any, 0, len(filter.States)+2)
	if filter.RepoProfileID != "" {
		query += " AND repo_profile_id = ?"
		args = append(args, filter.RepoProfileID)
	}
	if len(filter.States) > 0 {
		query += " AND state IN (" + placeholders(len(filter.States)) + ")"
		for _, state := range filter.States {
			args = append(args, state)
		}
	}
	query += " ORDER BY created_at DESC, id DESC"
	if filter.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, filter.Limit)
	}
	return s.queryTasks(ctx, query, args...)
}

func (s *Store) NonterminalTasks(ctx context.Context) ([]task.Task, error) {
	return s.queryTasks(ctx, taskColumns+" WHERE state NOT IN (?, ?) ORDER BY created_at, id", task.Completed, task.Canceled)
}

func (s *Store) queryTasks(ctx context.Context, query string, args ...any) ([]task.Task, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query tasks: %w", err)
	}
	defer rows.Close()
	var values []task.Task
	for rows.Next() {
		value, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tasks: %w", err)
	}
	return values, nil
}

func (s *Store) Events(ctx context.Context, taskID string) ([]task.Event, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, task_id, event_type, visibility, provider_event_id, redacted_payload, created_at
		FROM task_events WHERE task_id = ? ORDER BY created_at, id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	defer rows.Close()
	var events []task.Event
	for rows.Next() {
		var event task.Event
		var providerID sql.NullString
		var payload []byte
		var created string
		if err := rows.Scan(&event.ID, &event.TaskID, &event.Type, &event.Visibility, &providerID, &payload, &created); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		event.ProviderEventID = providerID.String
		event.Payload = payload
		event.CreatedAt, err = parseTimestamp(created)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate events: %w", err)
	}
	return events, nil
}

func placeholders(count int) string { return strings.TrimSuffix(strings.Repeat("?,", count), ",") }

const timestampLayout = "2006-01-02T15:04:05.000000000Z"

func timestamp(value time.Time) string { return value.UTC().Format(timestampLayout) }

func nullableTimestamp(value *time.Time) any {
	if value == nil {
		return nil
	}
	return timestamp(*value)
}

func parseTimestamp(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse timestamp: %w", err)
	}
	return parsed.UTC(), nil
}

func parseNullableTimestamp(value sql.NullString) (*time.Time, error) {
	if !value.Valid {
		return nil, nil
	}
	parsed, err := parseTimestamp(value.String)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func translateNotFound(action string, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s: %w", action, store.ErrNotFound)
	}
	return fmt.Errorf("%s: %w", action, err)
}

func translateConflict(action string, err error) error {
	if strings.Contains(err.Error(), "constraint failed") {
		return fmt.Errorf("%s: %w", action, store.ErrConflict)
	}
	return fmt.Errorf("%s: %w", action, err)
}

func translateEventError(err error) error {
	if strings.Contains(err.Error(), "UNIQUE constraint failed: task_events") {
		return fmt.Errorf("append event: %w", store.ErrDuplicateEvent)
	}
	return fmt.Errorf("append event: %w", err)
}

func translateTransitionError(action string, err error) error {
	if isBusy(err) {
		return fmt.Errorf("%s: %w", action, store.ErrConflict)
	}
	return fmt.Errorf("%s: %w", action, err)
}

func isBusy(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "database is locked") ||
		strings.Contains(message, "sqlite_busy") ||
		strings.Contains(message, "busy_snapshot")
}
