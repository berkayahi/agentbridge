package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/berkayahi/agentbridge/internal/intent"
)

type intentRepository struct{ db v2Querier }

func (r intentRepository) Create(ctx context.Context, value intent.Intent) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO intent_evidence (id, execution_id, kind, runtime_id, target_task_id, result_task_id, payload_ref, state, claim_owner, safe_progress, safe_result, created_at, expires_at, claimed_at, lease_expires_at, completed_at)
		VALUES (?, ?, ?, ?, ?, '', ?, ?, '', '', '', ?, ?, NULL, NULL, NULL)`,
		value.ID, nullableString(value.ExecutionID), value.Kind, value.RuntimeID, value.TargetTaskID, value.PayloadRef, value.State, timestamp(value.CreatedAt), timestamp(value.ExpiresAt))
	if err != nil {
		return v2Conflict("create intent", err)
	}
	return nil
}

func (r intentRepository) Get(ctx context.Context, id string) (intent.Intent, error) {
	return scanIntent(r.db.QueryRowContext(ctx, intentQuery+" WHERE id = ?", id))
}

func (r intentRepository) Claim(ctx context.Context, id, owner string, now time.Time, lease time.Duration) (intent.Intent, error) {
	if lease <= 0 {
		return intent.Intent{}, intent.ErrInvalidInput
	}
	claimed := now.UTC()
	leaseExpires := claimed.Add(lease)
	tx, err := beginImmediate(ctx, r.db)
	if err != nil {
		return intent.Intent{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE intent_evidence SET state = 'claimed', claim_owner = ?, claimed_at = ?, lease_expires_at = ? WHERE id = ? AND expires_at > ? AND (state = 'pending' OR (state = 'claimed' AND lease_expires_at <= ?))`, owner, timestamp(claimed), timestamp(leaseExpires), id, timestamp(claimed), timestamp(claimed))
	if err != nil {
		return intent.Intent{}, v2Conflict("claim intent", err)
	}
	if err := v2Changed("claim intent", result); err != nil {
		return intent.Intent{}, intent.ErrAlreadyClaimed
	}
	value, err := scanIntent(tx.QueryRowContext(ctx, intentQuery+" WHERE id = ?", id))
	if err != nil {
		return intent.Intent{}, err
	}
	if err := tx.Commit(); err != nil {
		return intent.Intent{}, fmt.Errorf("commit intent claim: %w", err)
	}
	return value, nil
}

func (r intentRepository) Complete(ctx context.Context, id, owner, result string, now time.Time) (intent.Intent, error) {
	return r.finish(ctx, id, owner, result, now, intent.StateCompleted, "")
}

func (r intentRepository) Reconcile(ctx context.Context, id, owner, progress string, now time.Time) (intent.Intent, error) {
	return r.finish(ctx, id, owner, "", now, intent.StateReconciliationNeeded, progress)
}

func (r intentRepository) finish(ctx context.Context, id, owner, result string, now time.Time, state intent.State, progress string) (intent.Intent, error) {
	completed := now.UTC()
	tx, err := beginImmediate(ctx, r.db)
	if err != nil {
		return intent.Intent{}, err
	}
	defer tx.Rollback()
	resultRow, err := tx.ExecContext(ctx, `UPDATE intent_evidence SET state = ?, safe_result = ?, safe_progress = ?, completed_at = ? WHERE id = ? AND state = 'claimed' AND claim_owner = ? AND lease_expires_at > ?`, state, result, progress, timestamp(completed), id, owner, timestamp(completed))
	if err != nil {
		return intent.Intent{}, v2Conflict("finish intent", err)
	}
	if err := v2Changed("finish intent", resultRow); err != nil {
		return intent.Intent{}, intent.ErrStaleClaim
	}
	value, err := scanIntent(tx.QueryRowContext(ctx, intentQuery+" WHERE id = ?", id))
	if err != nil {
		return intent.Intent{}, err
	}
	if err := tx.Commit(); err != nil {
		return intent.Intent{}, fmt.Errorf("commit intent result: %w", err)
	}
	return value, nil
}

const intentQuery = `SELECT id, COALESCE(execution_id, ''), kind, runtime_id, target_task_id, result_task_id, payload_ref, state, claim_owner, safe_progress, safe_result, created_at, expires_at, claimed_at, lease_expires_at, completed_at FROM intent_evidence`

func scanIntent(row scanner) (intent.Intent, error) {
	var value intent.Intent
	var created, expires string
	var claimed, lease, completed sql.NullString
	if err := row.Scan(&value.ID, &value.ExecutionID, &value.Kind, &value.RuntimeID, &value.TargetTaskID, &value.ResultTaskID, &value.PayloadRef, &value.State, &value.ClaimOwner, &value.SafeProgress, &value.SafeResult, &created, &expires, &claimed, &lease, &completed); err != nil {
		return intent.Intent{}, v2NotFound("get intent", err)
	}
	var err error
	value.CreatedAt, err = parseTimestamp(created)
	if err != nil {
		return intent.Intent{}, err
	}
	value.ExpiresAt, err = parseTimestamp(expires)
	if err != nil {
		return intent.Intent{}, err
	}
	value.ClaimedAt, err = parseNullableTimestamp(claimed)
	if err != nil {
		return intent.Intent{}, err
	}
	value.LeaseExpiresAt, err = parseNullableTimestamp(lease)
	if err != nil {
		return intent.Intent{}, err
	}
	value.CompletedAt, err = parseNullableTimestamp(completed)
	if err != nil {
		return intent.Intent{}, err
	}
	return intent.Restore(value)
}

func beginImmediate(ctx context.Context, db v2Querier) (*sql.Tx, error) {
	// SQLite's configured immediate transaction mode makes BeginTx acquire the
	// write reservation before the compare-and-set update.
	if database, ok := db.(*sql.DB); ok {
		return database.BeginTx(ctx, nil)
	}
	return nil, fmt.Errorf("intent transaction requires a database connection")
}

var _ intent.Repository = intentRepository{}
