package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/berkayahi/agentbridge/internal/session"
)

type sessionRepository struct{ db v2Querier }

func (r sessionRepository) Create(ctx context.Context, value session.Session) error {
	if value.ActiveTaskID() == "" {
		return fmt.Errorf("create session: active task is required by the v2 schema")
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO sessions (id, runtime_id, repository_id, local_task_id, active_local_task_id, provider_session_id, provider_thread_id, status, resumable, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, '', '', 'active', 1, ?, ?)`,
		value.ID(), value.RuntimeID(), value.RepositoryID(), value.ActiveTaskID(), value.ActiveTaskID(), timestamp(value.CreatedAt()), timestamp(value.CreatedAt()))
	if err != nil {
		return v2Conflict("create session", err)
	}
	return nil
}

func (r sessionRepository) Get(ctx context.Context, id string) (session.Session, error) {
	var value session.RestoreInput
	var active sql.NullString
	var created string
	err := r.db.QueryRowContext(ctx, `SELECT id, runtime_id, repository_id, active_local_task_id, created_at FROM sessions WHERE id = ?`, id).
		Scan(&value.ID, &value.RuntimeID, &value.RepositoryID, &active, &created)
	if err != nil {
		return session.Session{}, v2NotFound("get session", err)
	}
	value.ActiveTaskID = active.String
	value.CreatedAt, err = parseTimestamp(created)
	if err != nil {
		return session.Session{}, err
	}
	return session.Restore(value)
}

func (r sessionRepository) Save(ctx context.Context, value session.Session) error {
	if value.ActiveTaskID() == "" {
		return fmt.Errorf("save session: active task is required by the v2 schema")
	}
	result, err := r.db.ExecContext(ctx, `
		UPDATE sessions SET active_local_task_id = ?, status = 'active', resumable = 1, updated_at = ? WHERE id = ?`,
		value.ActiveTaskID(), timestamp(value.CreatedAt()), value.ID())
	if err != nil {
		return v2Conflict("save session", err)
	}
	return v2Changed("save session", result)
}

func (r sessionRepository) ListResumable(ctx context.Context) ([]session.Session, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id FROM sessions WHERE resumable = 1 ORDER BY updated_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list resumable sessions: %w", err)
	}
	defer rows.Close()
	var values []session.Session
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan session id: %w", err)
		}
		value, err := r.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

var _ session.Repository = sessionRepository{}
