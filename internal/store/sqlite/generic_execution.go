package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/berkayahi/agentbridge/internal/executioncontract"
)

func (s *RuntimeStore) CreateExecution(ctx context.Context, request executioncontract.ExecutionRequest, now time.Time) (executioncontract.ExecutionRecord, error) {
	if s == nil || s.db == nil {
		return executioncontract.ExecutionRecord{}, executioncontract.ErrInvalidRequest
	}
	if err := executioncontract.ValidateRequest(request); err != nil {
		return executioncontract.ExecutionRecord{}, err
	}
	digest, err := request.Digest()
	if err != nil {
		return executioncontract.ExecutionRecord{}, err
	}
	body, err := json.Marshal(request)
	if err != nil {
		return executioncontract.ExecutionRecord{}, fmt.Errorf("marshal execution request: %w", err)
	}
	now = executionNow(now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return executioncontract.ExecutionRecord{}, fmt.Errorf("begin generic execution: %w", err)
	}
	defer tx.Rollback()

	var existingID, existingDigest string
	err = tx.QueryRowContext(ctx, `SELECT execution_id, request_digest FROM generic_executions WHERE idempotency_key = ?`, request.IdempotencyKey).Scan(&existingID, &existingDigest)
	switch {
	case err == nil:
		if existingDigest != digest {
			return executioncontract.ExecutionRecord{}, executioncontract.ErrIdempotencyConflict
		}
		if err := tx.Commit(); err != nil {
			return executioncontract.ExecutionRecord{}, fmt.Errorf("commit generic execution replay: %w", err)
		}
		return s.GetExecution(ctx, existingID)
	case !errors.Is(err, sql.ErrNoRows):
		return executioncontract.ExecutionRecord{}, fmt.Errorf("read generic execution replay: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO generic_executions (
			execution_id, idempotency_key, request_digest, request_json, state,
			revision, recovery_count, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, 1, 0, ?, ?)`,
		request.ExecutionID, request.IdempotencyKey, digest, string(body),
		executioncontract.StateAccepted, timestamp(now), timestamp(now))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return executioncontract.ExecutionRecord{}, executioncontract.ErrConflict
		}
		return executioncontract.ExecutionRecord{}, fmt.Errorf("insert generic execution: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return executioncontract.ExecutionRecord{}, fmt.Errorf("commit generic execution: %w", err)
	}
	return s.GetExecution(ctx, request.ExecutionID)
}

func (s *RuntimeStore) GetExecution(ctx context.Context, executionID string) (executioncontract.ExecutionRecord, error) {
	if s == nil || s.db == nil || strings.TrimSpace(executionID) == "" {
		return executioncontract.ExecutionRecord{}, executioncontract.ErrInvalidRequest
	}
	return scanExecution(s.db.QueryRowContext(ctx, `
		SELECT request_json, result_json, state, revision, recovery_count, created_at, updated_at
		FROM generic_executions WHERE execution_id = ?`, executionID))
}

func (s *RuntimeStore) SaveExecutionResult(ctx context.Context, executionID string, expectedRevision int64, result executioncontract.ExecutionResult, now time.Time) (executioncontract.ExecutionRecord, error) {
	if s == nil || s.db == nil || expectedRevision < 1 {
		return executioncontract.ExecutionRecord{}, executioncontract.ErrInvalidRequest
	}
	record, err := s.GetExecution(ctx, executionID)
	if err != nil {
		return executioncontract.ExecutionRecord{}, err
	}
	if err := executioncontract.ValidateResult(record.Request, result); err != nil {
		return executioncontract.ExecutionRecord{}, err
	}
	if record.State != executioncontract.StateAccepted && record.State != executioncontract.StateRunning {
		return executioncontract.ExecutionRecord{}, executioncontract.ErrInvalidState
	}
	body, err := json.Marshal(result)
	if err != nil {
		return executioncontract.ExecutionRecord{}, fmt.Errorf("marshal execution result: %w", err)
	}
	now = executionNow(now)
	updated, err := s.db.ExecContext(ctx, `
		UPDATE generic_executions
		SET result_json = ?, state = ?, revision = revision + 1, updated_at = ?
		WHERE execution_id = ? AND revision = ? AND state IN ('accepted', 'running')`,
		string(body), result.State, timestamp(now), executionID, expectedRevision)
	if err != nil {
		return executioncontract.ExecutionRecord{}, fmt.Errorf("save execution result: %w", err)
	}
	changed, err := updated.RowsAffected()
	if err != nil {
		return executioncontract.ExecutionRecord{}, fmt.Errorf("read execution result update: %w", err)
	}
	if changed != 1 {
		return executioncontract.ExecutionRecord{}, executioncontract.ErrConflict
	}
	return s.GetExecution(ctx, executionID)
}

func (s *RuntimeStore) RecoverExecutions(ctx context.Context, now time.Time) ([]executioncontract.ExecutionRecord, error) {
	if s == nil || s.db == nil {
		return nil, executioncontract.ErrInvalidRequest
	}
	now = executionNow(now)
	if _, err := s.db.ExecContext(ctx, `
		UPDATE generic_executions
		SET state = 'accepted', recovery_count = recovery_count + 1,
			revision = revision + 1, updated_at = ?
		WHERE state IN ('accepted', 'running')`, timestamp(now)); err != nil {
		return nil, fmt.Errorf("recover generic executions: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT request_json, result_json, state, revision, recovery_count, created_at, updated_at
		FROM generic_executions WHERE state IN ('accepted', 'running') ORDER BY created_at, execution_id`)
	if err != nil {
		return nil, fmt.Errorf("list recoverable executions: %w", err)
	}
	defer rows.Close()
	var records []executioncontract.ExecutionRecord
	for rows.Next() {
		record, err := scanExecutionRow(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recoverable executions: %w", err)
	}
	return records, nil
}

func (s *RuntimeStore) AcquireResourceLease(ctx context.Context, request executioncontract.ResourceLeaseRequest, now time.Time) (executioncontract.ResourceLease, error) {
	if s == nil || s.db == nil {
		return executioncontract.ResourceLease{}, executioncontract.ErrInvalidLease
	}
	if err := executioncontract.ValidateLeaseRequest(request); err != nil {
		return executioncontract.ResourceLease{}, err
	}
	now = executionNow(now)
	expires := now.Add(time.Duration(request.TTLSeconds) * time.Second)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return executioncontract.ResourceLease{}, fmt.Errorf("begin resource lease: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM resource_leases WHERE expires_at <= ?`, timestamp(now)); err != nil {
		return executioncontract.ResourceLease{}, fmt.Errorf("clear expired resource leases: %w", err)
	}

	var existing executioncontract.ResourceLease
	_, err = scanLease(tx.QueryRowContext(ctx, `
		SELECT lease_id, resource_key, owner_execution_id, mode, acquired_at, heartbeat_at, expires_at, revision
		FROM resource_leases WHERE lease_id = ?`, request.LeaseID), &existing)
	if err == nil {
		if existing.ResourceKey != request.ResourceKey || existing.OwnerExecutionID != request.OwnerExecutionID || existing.Mode != request.Mode {
			return executioncontract.ResourceLease{}, executioncontract.ErrConflict
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE resource_leases SET heartbeat_at = ?, expires_at = ?, revision = revision + 1
			WHERE lease_id = ?`, timestamp(now), timestamp(expires), request.LeaseID); err != nil {
			return executioncontract.ResourceLease{}, fmt.Errorf("renew resource lease: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return executioncontract.ResourceLease{}, fmt.Errorf("commit resource lease replay: %w", err)
		}
		return s.loadLease(ctx, request.LeaseID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return executioncontract.ResourceLease{}, err
	}

	var conflicts int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM resource_leases
		WHERE resource_key = ? AND expires_at > ?
			AND (mode = 'exclusive' OR ? = 'exclusive')
			AND owner_execution_id <> ?`,
		request.ResourceKey, timestamp(now), request.Mode, request.OwnerExecutionID).Scan(&conflicts); err != nil {
		return executioncontract.ResourceLease{}, fmt.Errorf("check resource lease conflict: %w", err)
	}
	if conflicts > 0 {
		return executioncontract.ResourceLease{}, executioncontract.ErrLeaseHeld
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO resource_leases (
			lease_id, resource_key, owner_execution_id, mode,
			acquired_at, heartbeat_at, expires_at, revision
		) VALUES (?, ?, ?, ?, ?, ?, ?, 1)`,
		request.LeaseID, request.ResourceKey, request.OwnerExecutionID, request.Mode,
		timestamp(now), timestamp(now), timestamp(expires))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "foreign key") {
			return executioncontract.ResourceLease{}, executioncontract.ErrNotFound
		}
		return executioncontract.ResourceLease{}, fmt.Errorf("insert resource lease: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return executioncontract.ResourceLease{}, fmt.Errorf("commit resource lease: %w", err)
	}
	return s.loadLease(ctx, request.LeaseID)
}

func (s *RuntimeStore) HeartbeatResourceLease(ctx context.Context, leaseID, ownerExecutionID string, ttlSeconds int, now time.Time) (executioncontract.ResourceLease, error) {
	request := executioncontract.ResourceLeaseRequest{
		LeaseID: leaseID, OwnerExecutionID: ownerExecutionID, TTLSeconds: ttlSeconds,
		ResourceKey: "heartbeat", Mode: executioncontract.LeaseShared,
	}
	if err := executioncontract.ValidateLeaseRequest(request); err != nil {
		return executioncontract.ResourceLease{}, err
	}
	now = executionNow(now)
	expires := now.Add(time.Duration(ttlSeconds) * time.Second)
	updated, err := s.db.ExecContext(ctx, `
		UPDATE resource_leases SET heartbeat_at = ?, expires_at = ?, revision = revision + 1
		WHERE lease_id = ? AND owner_execution_id = ? AND expires_at > ?`,
		timestamp(now), timestamp(expires), leaseID, ownerExecutionID, timestamp(now))
	if err != nil {
		return executioncontract.ResourceLease{}, fmt.Errorf("heartbeat resource lease: %w", err)
	}
	changed, err := updated.RowsAffected()
	if err != nil {
		return executioncontract.ResourceLease{}, fmt.Errorf("read resource lease heartbeat: %w", err)
	}
	if changed != 1 {
		return executioncontract.ResourceLease{}, executioncontract.ErrNotFound
	}
	return s.loadLease(ctx, leaseID)
}

func (s *RuntimeStore) ReleaseResourceLease(ctx context.Context, leaseID, ownerExecutionID string, _ time.Time) error {
	if s == nil || s.db == nil || strings.TrimSpace(leaseID) == "" || strings.TrimSpace(ownerExecutionID) == "" {
		return executioncontract.ErrInvalidLease
	}
	updated, err := s.db.ExecContext(ctx, `DELETE FROM resource_leases WHERE lease_id = ? AND owner_execution_id = ?`, leaseID, ownerExecutionID)
	if err != nil {
		return fmt.Errorf("release resource lease: %w", err)
	}
	changed, err := updated.RowsAffected()
	if err != nil {
		return fmt.Errorf("read resource lease release: %w", err)
	}
	if changed != 1 {
		return executioncontract.ErrNotFound
	}
	return nil
}

func (s *RuntimeStore) ExpiredResourceLeases(ctx context.Context, now time.Time) ([]executioncontract.ResourceLease, error) {
	if s == nil || s.db == nil {
		return nil, executioncontract.ErrInvalidLease
	}
	now = executionNow(now)
	rows, err := s.db.QueryContext(ctx, `
		SELECT lease_id, resource_key, owner_execution_id, mode, acquired_at, heartbeat_at, expires_at, revision
		FROM resource_leases WHERE expires_at <= ? ORDER BY expires_at, lease_id`, timestamp(now))
	if err != nil {
		return nil, fmt.Errorf("list expired resource leases: %w", err)
	}
	defer rows.Close()
	var leases []executioncontract.ResourceLease
	for rows.Next() {
		var lease executioncontract.ResourceLease
		if _, err := scanLease(rows, &lease); err != nil {
			return nil, err
		}
		leases = append(leases, lease)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate expired resource leases: %w", err)
	}
	return leases, nil
}

func (s *RuntimeStore) loadLease(ctx context.Context, leaseID string) (executioncontract.ResourceLease, error) {
	lease, err := scanLease(s.db.QueryRowContext(ctx, `
		SELECT lease_id, resource_key, owner_execution_id, mode, acquired_at, heartbeat_at, expires_at, revision
		FROM resource_leases WHERE lease_id = ?`, leaseID), nil)
	if errors.Is(err, sql.ErrNoRows) {
		return executioncontract.ResourceLease{}, executioncontract.ErrNotFound
	}
	return lease, err
}

type rowScanner interface {
	Scan(...any) error
}

func scanExecution(scanner rowScanner) (executioncontract.ExecutionRecord, error) {
	return scanExecutionRow(scanner)
}

func scanExecutionRow(scanner rowScanner) (executioncontract.ExecutionRecord, error) {
	var requestBody, state, created, updated string
	var resultBody sql.NullString
	var revision int64
	var recoveryCount int
	if err := scanner.Scan(&requestBody, &resultBody, &state, &revision, &recoveryCount, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return executioncontract.ExecutionRecord{}, executioncontract.ErrNotFound
		}
		return executioncontract.ExecutionRecord{}, fmt.Errorf("scan generic execution: %w", err)
	}
	var record executioncontract.ExecutionRecord
	if err := json.Unmarshal([]byte(requestBody), &record.Request); err != nil {
		return executioncontract.ExecutionRecord{}, fmt.Errorf("decode generic execution request: %w", err)
	}
	if resultBody.Valid && resultBody.String != "" {
		var result executioncontract.ExecutionResult
		if err := json.Unmarshal([]byte(resultBody.String), &result); err != nil {
			return executioncontract.ExecutionRecord{}, fmt.Errorf("decode generic execution result: %w", err)
		}
		record.Result = &result
	}
	createdAt, err := parseTimestamp(created)
	if err != nil {
		return executioncontract.ExecutionRecord{}, err
	}
	updatedAt, err := parseTimestamp(updated)
	if err != nil {
		return executioncontract.ExecutionRecord{}, err
	}
	record.State = executioncontract.ExecutionState(state)
	record.Revision = revision
	record.RecoveryCount = recoveryCount
	record.CreatedAt = createdAt
	record.UpdatedAt = updatedAt
	return record, nil
}

func scanLease(scanner rowScanner, target *executioncontract.ResourceLease) (executioncontract.ResourceLease, error) {
	var lease executioncontract.ResourceLease
	var acquired, heartbeat, expires string
	if err := scanner.Scan(&lease.LeaseID, &lease.ResourceKey, &lease.OwnerExecutionID, &lease.Mode, &acquired, &heartbeat, &expires, &lease.Revision); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return executioncontract.ResourceLease{}, sql.ErrNoRows
		}
		return executioncontract.ResourceLease{}, fmt.Errorf("scan resource lease: %w", err)
	}
	var err error
	if lease.AcquiredAt, err = parseTimestamp(acquired); err != nil {
		return executioncontract.ResourceLease{}, err
	}
	if lease.HeartbeatAt, err = parseTimestamp(heartbeat); err != nil {
		return executioncontract.ResourceLease{}, err
	}
	if lease.ExpiresAt, err = parseTimestamp(expires); err != nil {
		return executioncontract.ResourceLease{}, err
	}
	if target != nil {
		*target = lease
	}
	return lease, nil
}

func executionNow(value time.Time) time.Time {
	if value.IsZero() {
		return time.Now().UTC()
	}
	return value.UTC()
}
