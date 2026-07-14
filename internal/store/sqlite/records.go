package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/berkayahi/agentbridge/internal/task"
)

func (s *Store) SaveAttachment(ctx context.Context, value task.Attachment) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO attachments (id, task_id, kind, name, media_type, storage_path, size_bytes, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		value.ID, value.TaskID, value.Kind, value.Name, value.MediaType, value.StoragePath, value.SizeBytes, timestamp(value.CreatedAt),
	)
	if err != nil {
		return fmt.Errorf("save attachment: %w", err)
	}
	return nil
}

func (s *Store) Attachments(ctx context.Context, taskID string) ([]task.Attachment, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, task_id, kind, name, media_type, storage_path, size_bytes, created_at
		FROM attachments WHERE task_id = ? ORDER BY created_at, id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("query attachments: %w", err)
	}
	defer rows.Close()
	var values []task.Attachment
	for rows.Next() {
		var value task.Attachment
		var created string
		if err := rows.Scan(&value.ID, &value.TaskID, &value.Kind, &value.Name, &value.MediaType, &value.StoragePath, &value.SizeBytes, &created); err != nil {
			return nil, fmt.Errorf("scan attachment: %w", err)
		}
		value.CreatedAt, err = parseTimestamp(created)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func (s *Store) UpsertSession(ctx context.Context, value task.Session) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (
			id, task_id, provider, provider_session_id, provider_thread_id,
			status, resumable, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			provider_session_id = excluded.provider_session_id,
			provider_thread_id = excluded.provider_thread_id,
			status = excluded.status,
			resumable = excluded.resumable,
			updated_at = excluded.updated_at`,
		value.ID, value.TaskID, value.Provider, value.ProviderSessionID, value.ProviderThreadID,
		value.Status, value.Resumable, timestamp(value.CreatedAt), timestamp(value.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("upsert session: %w", err)
	}
	return nil
}

func (s *Store) ResumableSessions(ctx context.Context) ([]task.Session, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, task_id, provider, provider_session_id, provider_thread_id,
		       status, resumable, created_at, updated_at
		FROM sessions WHERE resumable = 1 ORDER BY updated_at, id`)
	if err != nil {
		return nil, fmt.Errorf("query resumable sessions: %w", err)
	}
	defer rows.Close()
	var values []task.Session
	for rows.Next() {
		value, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func scanSession(row scanner) (task.Session, error) {
	var value task.Session
	var created, updated string
	if err := row.Scan(
		&value.ID, &value.TaskID, &value.Provider, &value.ProviderSessionID, &value.ProviderThreadID,
		&value.Status, &value.Resumable, &created, &updated,
	); err != nil {
		return task.Session{}, fmt.Errorf("scan session: %w", err)
	}
	var err error
	if value.CreatedAt, err = parseTimestamp(created); err != nil {
		return task.Session{}, err
	}
	if value.UpdatedAt, err = parseTimestamp(updated); err != nil {
		return task.Session{}, err
	}
	return value, nil
}

func (s *Store) UpsertApproval(ctx context.Context, value task.Approval) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO approvals (
			id, task_id, kind, status, request_payload, decision_payload,
			requested_at, expires_at, resolved_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			status = excluded.status,
			decision_payload = excluded.decision_payload,
			expires_at = excluded.expires_at,
			resolved_at = excluded.resolved_at`,
		value.ID, value.TaskID, value.Kind, value.Status, []byte(value.RequestPayload), nullableBytes(value.DecisionPayload),
		timestamp(value.RequestedAt), nullableTimestamp(value.ExpiresAt), nullableTimestamp(value.ResolvedAt),
	)
	if err != nil {
		return fmt.Errorf("upsert approval: %w", err)
	}
	return nil
}

func (s *Store) PendingApprovals(ctx context.Context) ([]task.Approval, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, task_id, kind, status, request_payload, decision_payload,
		       requested_at, expires_at, resolved_at
		FROM approvals WHERE status = ? ORDER BY requested_at, id`, task.ApprovalPending)
	if err != nil {
		return nil, fmt.Errorf("query pending approvals: %w", err)
	}
	defer rows.Close()
	var values []task.Approval
	for rows.Next() {
		value, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func scanApproval(row scanner) (task.Approval, error) {
	var value task.Approval
	var request []byte
	var decision []byte
	var requested string
	var expires, resolved sql.NullString
	if err := row.Scan(
		&value.ID, &value.TaskID, &value.Kind, &value.Status, &request, &decision,
		&requested, &expires, &resolved,
	); err != nil {
		return task.Approval{}, fmt.Errorf("scan approval: %w", err)
	}
	value.RequestPayload = request
	value.DecisionPayload = decision
	var err error
	if value.RequestedAt, err = parseTimestamp(requested); err != nil {
		return task.Approval{}, err
	}
	if value.ExpiresAt, err = parseNullableTimestamp(expires); err != nil {
		return task.Approval{}, err
	}
	if value.ResolvedAt, err = parseNullableTimestamp(resolved); err != nil {
		return task.Approval{}, err
	}
	return value, nil
}

func nullableBytes(value []byte) any {
	if value == nil {
		return nil
	}
	return value
}
