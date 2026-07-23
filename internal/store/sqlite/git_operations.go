package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/berkayahi/agentbridge/internal/gitoperation"
)

type gitOperationRepository struct{ db v2Querier }

func (r gitOperationRepository) Create(ctx context.Context, value gitoperation.Operation) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO git_operations (id, execution_id, kind, target_ref, expected_old_sha, idempotency_key, state, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, value.ID(), value.ExecutionID(), value.Kind(), value.TargetRef(), value.ExpectedOldSHA(), value.IdempotencyKey(), value.State(), timestamp(value.CreatedAt()))
	if err != nil {
		return v2Conflict("create Git operation", err)
	}
	return nil
}

func (r gitOperationRepository) Get(ctx context.Context, id string) (gitoperation.Operation, error) {
	var input gitoperation.RestoreInput
	var created string
	err := r.db.QueryRowContext(ctx, `SELECT id, execution_id, kind, target_ref, expected_old_sha, idempotency_key, state, created_at FROM git_operations WHERE id = ?`, id).
		Scan(&input.ID, &input.ExecutionID, &input.Kind, &input.TargetRef, &input.ExpectedOldSHA, &input.IdempotencyKey, &input.State, &created)
	if err != nil {
		return gitoperation.Operation{}, v2NotFound("get Git operation", err)
	}
	input.CreatedAt, err = parseTimestamp(created)
	if err != nil {
		return gitoperation.Operation{}, err
	}
	return gitoperation.Restore(input)
}

func (r gitOperationRepository) Save(ctx context.Context, value gitoperation.Operation) error {
	result, err := r.db.ExecContext(ctx, `UPDATE git_operations SET state = ? WHERE id = ?`, value.State(), value.ID())
	if err != nil {
		return v2Conflict("save Git operation", err)
	}
	return v2Changed("save Git operation", result)
}

func (r gitOperationRepository) GetByIdempotencyKey(ctx context.Context, key string) (gitoperation.Operation, error) {
	var id string
	if err := r.db.QueryRowContext(ctx, `SELECT id FROM git_operations WHERE idempotency_key = ?`, key).Scan(&id); err != nil {
		return gitoperation.Operation{}, v2NotFound("get Git operation by idempotency key", err)
	}
	return r.Get(ctx, id)
}

var _ gitoperation.Repository = gitOperationRepository{}

var _ = sql.ErrNoRows
var _ = fmt.Sprintf
