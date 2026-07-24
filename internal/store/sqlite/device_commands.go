package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/berkayahi/agentbridge/internal/localcontrol"
)

func (s *RuntimeStore) EnqueueDeviceCommand(ctx context.Context, value localcontrol.DeviceCommandRecord) error {
	if s == nil || s.db == nil || !validDeviceCommandRecord(value) {
		return fmt.Errorf("enqueue device command: %w", localcontrol.ErrInvalidRequest)
	}
	var existing localcontrol.DeviceCommandRecord
	var created, updated string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, task_id, device_id, assignment_epoch, operation, request_hash, request_payload,
			revision, state, attempts, last_error, created_at, updated_at
		FROM local_device_commands WHERE id = ?`, value.ID).Scan(
		&existing.ID, &existing.TaskID, &existing.DeviceID, &existing.AssignmentEpoch, &existing.Operation,
		&existing.RequestHash, &existing.RequestPayload, &existing.Revision, &existing.State, &existing.Attempts,
		&existing.LastError, &created, &updated,
	)
	if err == nil {
		if existing.TaskID != value.TaskID || existing.DeviceID != value.DeviceID || existing.AssignmentEpoch != value.AssignmentEpoch || existing.Operation != value.Operation || existing.RequestHash != value.RequestHash {
			return fmt.Errorf("enqueue device command %q: %w", value.ID, localcontrol.ErrIdempotencyConflict)
		}
		return nil
	}
	if !isNoRows(err) {
		return runtimeConflict("load device command", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO local_device_commands (
			id, task_id, device_id, assignment_epoch, operation, request_hash, request_payload,
			revision, state, attempts, last_error, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, '', ?, ?)`,
		value.ID, value.TaskID, value.DeviceID, value.AssignmentEpoch, value.Operation, value.RequestHash,
		[]byte(value.RequestPayload), value.Revision, value.State, timestamp(value.CreatedAt), timestamp(value.UpdatedAt))
	if err != nil {
		return runtimeConflict("insert device command", err)
	}
	return nil
}

func (s *RuntimeStore) GetDeviceCommand(ctx context.Context, id string) (localcontrol.DeviceCommandRecord, error) {
	if s == nil || s.db == nil || strings.TrimSpace(id) == "" {
		return localcontrol.DeviceCommandRecord{}, fmt.Errorf("get device command: %w", localcontrol.ErrInvalidRequest)
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT id, task_id, device_id, assignment_epoch, operation, request_hash, request_payload,
			revision, state, attempts, last_error, created_at, updated_at
		FROM local_device_commands WHERE id = ?`, id)
	value, err := scanDeviceCommand(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return localcontrol.DeviceCommandRecord{}, runtimeNotFound("get device command", sql.ErrNoRows)
		}
		return localcontrol.DeviceCommandRecord{}, err
	}
	return value, nil
}

func (s *RuntimeStore) ClaimDeviceCommand(ctx context.Context, id string, now time.Time) (bool, error) {
	if s == nil || s.db == nil || strings.TrimSpace(id) == "" || now.IsZero() {
		return false, fmt.Errorf("claim device command: %w", localcontrol.ErrInvalidRequest)
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE local_device_commands
		SET state = 'in_flight', attempts = attempts + 1, updated_at = ?
		WHERE id = ? AND state IN ('pending', 'in_flight')`, timestamp(now), id)
	if err != nil {
		return false, runtimeConflict("claim device command", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read claimed device command: %w", err)
	}
	return changed == 1, nil
}

func (s *RuntimeStore) ResetDeviceCommand(ctx context.Context, id, reason string, now time.Time) error {
	return s.updateDeviceCommand(ctx, id, localcontrol.DeviceCommandPending, reason, now, "reset")
}

func (s *RuntimeStore) CompleteDeviceCommand(ctx context.Context, id string, now time.Time) error {
	if s == nil || s.db == nil || strings.TrimSpace(id) == "" || now.IsZero() {
		return fmt.Errorf("complete device command: %w", localcontrol.ErrInvalidRequest)
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE local_device_commands
		SET state = 'completed', last_error = '', updated_at = ?
		WHERE id = ? AND state IN ('pending', 'in_flight', 'completed')`, timestamp(now), id)
	if err != nil {
		return runtimeConflict("complete device command", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read completed device command: %w", err)
	}
	if changed == 1 {
		return nil
	}
	return runtimeNotFound("complete device command", sql.ErrNoRows)
}

func (s *RuntimeStore) FailDeviceCommand(ctx context.Context, id, reason string, now time.Time) error {
	return s.updateDeviceCommand(ctx, id, localcontrol.DeviceCommandFailed, reason, now, "fail")
}

func (s *RuntimeStore) updateDeviceCommand(ctx context.Context, id string, state localcontrol.DeviceCommandState, reason string, now time.Time, action string) error {
	if s == nil || s.db == nil || strings.TrimSpace(id) == "" || now.IsZero() {
		return fmt.Errorf("%s device command: %w", action, localcontrol.ErrInvalidRequest)
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE local_device_commands SET state = ?, last_error = ?, updated_at = ?
		WHERE id = ? AND state IN ('pending', 'in_flight', 'failed')`,
		state, boundedCommandError(reason), timestamp(now), id)
	if err != nil {
		return runtimeConflict(action+" device command", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read %s device command: %w", action, err)
	}
	if changed != 1 {
		return runtimeNotFound(action+" device command", sql.ErrNoRows)
	}
	return nil
}

func (s *RuntimeStore) ListPendingDeviceCommands(ctx context.Context, deviceID string, limit int) ([]localcontrol.DeviceCommandRecord, error) {
	if s == nil || s.db == nil || strings.TrimSpace(deviceID) == "" {
		return nil, fmt.Errorf("list device commands: %w", localcontrol.ErrInvalidRequest)
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, task_id, device_id, assignment_epoch, operation, request_hash, request_payload,
			revision, state, attempts, last_error, created_at, updated_at
		FROM local_device_commands
		WHERE device_id = ? AND state IN ('pending', 'in_flight')
		ORDER BY created_at, id LIMIT ?`, deviceID, limit)
	if err != nil {
		return nil, fmt.Errorf("list device commands: %w", err)
	}
	defer rows.Close()
	values := make([]localcontrol.DeviceCommandRecord, 0, limit)
	for rows.Next() {
		value, err := scanDeviceCommand(rows)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

type deviceCommandScanner interface {
	Scan(...any) error
}

func scanDeviceCommand(row deviceCommandScanner) (localcontrol.DeviceCommandRecord, error) {
	var value localcontrol.DeviceCommandRecord
	var created, updated string
	if err := row.Scan(&value.ID, &value.TaskID, &value.DeviceID, &value.AssignmentEpoch, &value.Operation, &value.RequestHash, &value.RequestPayload, &value.Revision, &value.State, &value.Attempts, &value.LastError, &created, &updated); err != nil {
		return localcontrol.DeviceCommandRecord{}, fmt.Errorf("scan device command: %w", err)
	}
	var err error
	if value.CreatedAt, err = parseTimestamp(created); err != nil {
		return localcontrol.DeviceCommandRecord{}, err
	}
	if value.UpdatedAt, err = parseTimestamp(updated); err != nil {
		return localcontrol.DeviceCommandRecord{}, err
	}
	value.RequestPayload = append([]byte(nil), value.RequestPayload...)
	return value, nil
}

func validDeviceCommandRecord(value localcontrol.DeviceCommandRecord) bool {
	if strings.TrimSpace(value.ID) == "" || strings.TrimSpace(value.TaskID) == "" || strings.TrimSpace(value.DeviceID) == "" || value.AssignmentEpoch == 0 || value.Revision <= 0 || strings.TrimSpace(value.Operation) == "" || strings.TrimSpace(value.RequestHash) == "" || len(value.RequestPayload) == 0 || len(value.RequestPayload) > 1<<20 || value.CreatedAt.IsZero() || value.UpdatedAt.IsZero() {
		return false
	}
	switch value.State {
	case localcontrol.DeviceCommandPending, localcontrol.DeviceCommandInFlight, localcontrol.DeviceCommandCompleted, localcontrol.DeviceCommandFailed:
	default:
		return false
	}
	switch value.Operation {
	case "start", "resume", "approve", "cancel", "verify", "commit":
		return true
	default:
		return false
	}
}

func boundedCommandError(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > 512 {
		return value[:512]
	}
	return value
}

func isNoRows(err error) bool { return err == sql.ErrNoRows }

var _ localcontrol.DeviceAuthority = (*RuntimeStore)(nil)
var _ interface {
	EnqueueDeviceCommand(context.Context, localcontrol.DeviceCommandRecord) error
	GetDeviceCommand(context.Context, string) (localcontrol.DeviceCommandRecord, error)
	ClaimDeviceCommand(context.Context, string, time.Time) (bool, error)
	ResetDeviceCommand(context.Context, string, string, time.Time) error
	CompleteDeviceCommand(context.Context, string, time.Time) error
	FailDeviceCommand(context.Context, string, string, time.Time) error
	ListPendingDeviceCommands(context.Context, string, int) ([]localcontrol.DeviceCommandRecord, error)
} = (*RuntimeStore)(nil)
