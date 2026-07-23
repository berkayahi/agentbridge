package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/berkayahi/agentbridge/internal/execution"
)

type executionRepository struct{ db v2Querier }

func (r executionRepository) Create(ctx context.Context, value execution.Execution) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO executions (id, local_task_id, session_id, runtime_id, repository_id, retry_of_execution_id, state, attempt, fencing_epoch, command_id, max_transient_attempts, policy_snapshot, source_state, started_at, finished_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', NULL, NULL, ?, ?)`,
		value.ID(), value.LocalTaskID(), value.SessionID(), value.RuntimeID(), value.RepositoryID(), nullableString(value.RetryOfExecutionID()), value.State(), value.Attempt(), value.FencingEpoch(), value.CommandID(), value.MaxTransientAttempts(), value.PolicySnapshot(), timestamp(value.CreatedAt()), timestamp(value.UpdatedAt()))
	if err != nil {
		return v2Conflict("create execution", err)
	}
	return nil
}

func (r executionRepository) Get(ctx context.Context, id string) (execution.Execution, error) {
	var value execution.RestoreInput
	var retry sql.NullString
	var created, updated string
	err := r.db.QueryRowContext(ctx, `
		SELECT id, local_task_id, session_id, runtime_id, repository_id, retry_of_execution_id, state, attempt, fencing_epoch, command_id, max_transient_attempts, policy_snapshot, created_at, updated_at
		FROM executions WHERE id = ?`, id).Scan(
		&value.ID, &value.LocalTaskID, &value.SessionID, &value.RuntimeID, &value.RepositoryID, &retry, &value.State,
		&value.Attempt, &value.FencingEpoch, &value.CommandID, &value.MaxTransientAttempts, &value.PolicySnapshot, &created, &updated)
	if err != nil {
		return execution.Execution{}, v2NotFound("get execution", err)
	}
	value.RetryOfExecutionID = retry.String
	value.CreatedAt, err = parseTimestamp(created)
	if err != nil {
		return execution.Execution{}, err
	}
	value.UpdatedAt, err = parseTimestamp(updated)
	if err != nil {
		return execution.Execution{}, err
	}
	return execution.Restore(value)
}

func (r executionRepository) Save(ctx context.Context, value execution.Execution) error {
	result, err := r.db.ExecContext(ctx, `
		UPDATE executions SET state = ?, attempt = ?, fencing_epoch = ?, command_id = ?, updated_at = ? WHERE id = ?`,
		value.State(), value.Attempt(), value.FencingEpoch(), value.CommandID(), timestamp(value.UpdatedAt()), value.ID())
	if err != nil {
		return v2Conflict("save execution", err)
	}
	return v2Changed("save execution", result)
}

func (r executionRepository) ListByTask(ctx context.Context, taskID string) ([]execution.Execution, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id FROM executions WHERE local_task_id = ? ORDER BY created_at, id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("list executions: %w", err)
	}
	defer rows.Close()
	var values []execution.Execution
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan execution id: %w", err)
		}
		value, err := r.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate executions: %w", err)
	}
	return values, nil
}

var _ execution.Repository = executionRepository{}
