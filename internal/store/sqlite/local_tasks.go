package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/berkayahi/agentbridge/internal/localtask"
)

type localTaskRepository struct{ db v2Querier }

func (r localTaskRepository) Create(ctx context.Context, value localtask.LocalTask) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO local_tasks (id, title, prompt, active_execution_id, base_sha, worktree_path, commit_sha, push_ref, deployment_url, failure_reason, created_at, updated_at)
		VALUES (?, ?, ?, ?, '', '', '', '', '', '', ?, ?)`,
		value.ID(), value.Title(), value.Prompt(), nullableString(value.ActiveExecutionID()), timestamp(value.CreatedAt()), timestamp(value.CreatedAt()))
	if err != nil {
		return v2Conflict("create local task", err)
	}
	return nil
}

func (r localTaskRepository) Get(ctx context.Context, id string) (localtask.LocalTask, error) {
	var value localtask.RestoreInput
	var active sql.NullString
	var created, updated string
	err := r.db.QueryRowContext(ctx, `SELECT id, title, prompt, active_execution_id, created_at, updated_at FROM local_tasks WHERE id = ?`, id).
		Scan(&value.ID, &value.Title, &value.Prompt, &active, &created, &updated)
	if err != nil {
		return localtask.LocalTask{}, v2NotFound("get local task", err)
	}
	value.ActiveExecutionID = active.String
	value.CreatedAt, err = parseTimestamp(created)
	if err != nil {
		return localtask.LocalTask{}, err
	}
	return localtask.Restore(value)
}

func (r localTaskRepository) Save(ctx context.Context, value localtask.LocalTask) error {
	result, err := r.db.ExecContext(ctx, `
		UPDATE local_tasks SET title = ?, prompt = ?, active_execution_id = ?, updated_at = ? WHERE id = ?`,
		value.Title(), value.Prompt(), nullableString(value.ActiveExecutionID()), timestamp(time.Now().UTC()), value.ID())
	if err != nil {
		return v2Conflict("save local task", err)
	}
	if err := v2Changed("save local task", result); err != nil {
		return err
	}
	return nil
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

var _ localtask.Repository = localTaskRepository{}

var _ = fmt.Sprintf
