package sqlite

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/berkayahi/agentbridge/internal/localcontrol"
	"github.com/berkayahi/agentbridge/internal/store"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

func (s *RuntimeStore) CreateDevice(ctx context.Context, value localcontrol.Device, publicKey []byte) error {
	if s == nil || s.db == nil || strings.TrimSpace(value.ID) == "" || strings.TrimSpace(value.Name) == "" || strings.TrimSpace(value.Fingerprint) == "" || value.ConnectionEpoch == 0 || value.Revision <= 0 || value.CreatedAt.IsZero() || value.UpdatedAt.IsZero() {
		return fmt.Errorf("create local device: %w", localcontrol.ErrInvalidRequest)
	}
	if value.Kind != localcontrol.DeviceKindLocalMac && value.Kind != localcontrol.DeviceKindRaspberryPi {
		return fmt.Errorf("create local device kind: %w", localcontrol.ErrInvalidRequest)
	}
	if value.State != localcontrol.DeviceStatePaired && value.State != localcontrol.DeviceStateUnreachable && value.State != localcontrol.DeviceStateRevoked {
		return fmt.Errorf("create local device state: %w", localcontrol.ErrInvalidRequest)
	}
	if publicKey == nil {
		publicKey = []byte{}
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO local_devices (id, name, kind, fingerprint, public_key, endpoint, state, connection_epoch, revision, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, value.ID, value.Name, value.Kind, value.Fingerprint, publicKey, value.Endpoint, value.State, value.ConnectionEpoch, value.Revision, timestamp(value.CreatedAt), timestamp(value.UpdatedAt))
	if err != nil {
		return runtimeConflict("create local device", err)
	}
	return nil
}

func (s *RuntimeStore) GetDevice(ctx context.Context, id string) (localcontrol.Device, error) {
	var value localcontrol.Device
	var created, updated string
	if err := s.db.QueryRowContext(ctx, `
		SELECT id, name, kind, fingerprint, endpoint, state, connection_epoch, revision, created_at, updated_at
		FROM local_devices WHERE id = ?`, id).Scan(&value.ID, &value.Name, &value.Kind, &value.Fingerprint, &value.Endpoint, &value.State, &value.ConnectionEpoch, &value.Revision, &created, &updated); err != nil {
		return localcontrol.Device{}, runtimeNotFound("get local device", err)
	}
	var err error
	if value.CreatedAt, err = parseTimestamp(created); err != nil {
		return localcontrol.Device{}, err
	}
	if value.UpdatedAt, err = parseTimestamp(updated); err != nil {
		return localcontrol.Device{}, err
	}
	return value, nil
}

func (s *RuntimeStore) DevicePublicKey(ctx context.Context, id string) ([]byte, error) {
	var value []byte
	if err := s.db.QueryRowContext(ctx, `SELECT public_key FROM local_devices WHERE id = ?`, id).Scan(&value); err != nil {
		return nil, runtimeNotFound("get local device public key", err)
	}
	if len(value) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("local device public key: %w", localcontrol.ErrInvalidDeviceProof)
	}
	return append([]byte(nil), value...), nil
}

func (s *RuntimeStore) NextDeviceLinkSequence(ctx context.Context, id string) (uint64, uint64, error) {
	if strings.TrimSpace(id) == "" {
		return 0, 0, fmt.Errorf("reserve local device link sequence: %w", localcontrol.ErrInvalidRequest)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("begin local device link sequence: %w", err)
	}
	defer tx.Rollback()
	var found string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM local_devices WHERE id = ?`, id).Scan(&found); err != nil {
		return 0, 0, runtimeNotFound("load local device link sequence device", err)
	}
	now := timestamp(time.Now().UTC())
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO local_device_link_counters (device_id, message_id, sequence, updated_at)
		VALUES (?, 0, 0, ?)
		ON CONFLICT(device_id) DO NOTHING`, id, now); err != nil {
		return 0, 0, runtimeConflict("create local device link sequence", err)
	}
	var messageID, sequence int64
	if err := tx.QueryRowContext(ctx, `SELECT message_id, sequence FROM local_device_link_counters WHERE device_id = ?`, id).Scan(&messageID, &sequence); err != nil {
		return 0, 0, runtimeNotFound("load local device link sequence", err)
	}
	if messageID < 0 || sequence < 0 || messageID == math.MaxInt64 || sequence == math.MaxInt64 {
		return 0, 0, fmt.Errorf("reserve local device link sequence: %w", localcontrol.ErrDeviceLinkProtocol)
	}
	messageID++
	sequence++
	result, err := tx.ExecContext(ctx, `
		UPDATE local_device_link_counters SET message_id = ?, sequence = ?, updated_at = ?
		WHERE device_id = ?`, messageID, sequence, now, id)
	if err != nil {
		return 0, 0, runtimeConflict("advance local device link sequence", err)
	}
	if err := requireRuntimeChanged(result, "advance local device link sequence"); err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("commit local device link sequence: %w", err)
	}
	return uint64(messageID), uint64(sequence), nil
}

func (s *RuntimeStore) ListDevices(ctx context.Context) ([]localcontrol.Device, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, kind, fingerprint, endpoint, state, connection_epoch, revision, created_at, updated_at
		FROM local_devices ORDER BY kind, name, id`)
	if err != nil {
		return nil, fmt.Errorf("list local devices: %w", err)
	}
	defer rows.Close()
	values := make([]localcontrol.Device, 0, 4)
	for rows.Next() {
		var value localcontrol.Device
		var created, updated string
		if err := rows.Scan(&value.ID, &value.Name, &value.Kind, &value.Fingerprint, &value.Endpoint, &value.State, &value.ConnectionEpoch, &value.Revision, &created, &updated); err != nil {
			return nil, fmt.Errorf("scan local device: %w", err)
		}
		var parseErr error
		if value.CreatedAt, parseErr = parseTimestamp(created); parseErr != nil {
			return nil, parseErr
		}
		if value.UpdatedAt, parseErr = parseTimestamp(updated); parseErr != nil {
			return nil, parseErr
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func (s *RuntimeStore) UpdateDevice(ctx context.Context, value localcontrol.Device, publicKey []byte) error {
	if value.ID == "" || value.Revision <= 0 || value.ConnectionEpoch == 0 || value.UpdatedAt.IsZero() {
		return fmt.Errorf("update local device: %w", localcontrol.ErrInvalidRequest)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin update local device: %w", err)
	}
	defer tx.Rollback()
	if publicKey == nil {
		if err := tx.QueryRowContext(ctx, `SELECT public_key FROM local_devices WHERE id = ?`, value.ID).Scan(&publicKey); err != nil {
			return runtimeNotFound("load local device key", err)
		}
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE local_devices SET name = ?, kind = ?, fingerprint = ?, public_key = ?, endpoint = ?, state = ?, connection_epoch = ?, revision = ?, updated_at = ?
		WHERE id = ?`, value.Name, value.Kind, value.Fingerprint, publicKey, value.Endpoint, value.State, value.ConnectionEpoch, value.Revision, timestamp(value.UpdatedAt), value.ID)
	if err != nil {
		return runtimeConflict("update local device", err)
	}
	if err := requireRuntimeChanged(result, "update local device"); err != nil {
		return fmt.Errorf("update local device: %w", store.ErrNotFound)
	}
	if value.Kind == localcontrol.DeviceKindRaspberryPi {
		assignmentState := "fenced"
		if value.State == localcontrol.DeviceStateUnreachable {
			assignmentState = "unreachable"
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE local_task_devices SET state = ?, updated_at = ?
			WHERE device_id = ? AND state IN ('assigned', 'unreachable', 'fenced')`,
			assignmentState, timestamp(value.UpdatedAt), value.ID); err != nil {
			return runtimeConflict("update local device assignments", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit local device update: %w", err)
	}
	return nil
}

// ApplyDeviceMutation commits a device lifecycle change with its challenge
// consumption, local event, and idempotency response. The service validates
// the signed proof before calling this boundary; the SQL revision/state/epoch
// predicates provide the crash and concurrency fence for the final mutation.
func (s *RuntimeStore) ApplyDeviceMutation(ctx context.Context, mutation localcontrol.AtomicDeviceMutation) error {
	value := mutation.Device
	if s == nil || s.db == nil || strings.TrimSpace(value.ID) == "" || strings.TrimSpace(value.Name) == "" || strings.TrimSpace(value.Fingerprint) == "" || value.ConnectionEpoch == 0 || value.Revision <= 0 || value.UpdatedAt.IsZero() || (mutation.Create && value.CreatedAt.IsZero()) {
		return fmt.Errorf("apply local device mutation: %w", localcontrol.ErrInvalidRequest)
	}
	if value.Kind != localcontrol.DeviceKindRaspberryPi || (value.State != localcontrol.DeviceStatePaired && value.State != localcontrol.DeviceStateUnreachable && value.State != localcontrol.DeviceStateRevoked) {
		return fmt.Errorf("apply local device mutation: %w", localcontrol.ErrInvalidRequest)
	}
	if value.Revision > math.MaxInt64 || value.ConnectionEpoch > math.MaxInt64 || mutation.ExpectedRevision < 0 || mutation.ExpectedConnectionEpoch > math.MaxInt64 {
		return fmt.Errorf("apply local device mutation: %w", localcontrol.ErrInvalidRequest)
	}
	if mutation.Create {
		if mutation.ExpectedRevision != 0 || mutation.ExpectedConnectionEpoch != 0 || len(mutation.PublicKey) != ed25519.PublicKeySize {
			return fmt.Errorf("create local device mutation: %w", localcontrol.ErrInvalidRequest)
		}
	} else if mutation.ExpectedRevision <= 0 || mutation.ExpectedConnectionEpoch == 0 || mutation.ExpectedState == "" {
		return fmt.Errorf("update local device mutation: %w", localcontrol.ErrInvalidRequest)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin local device mutation: %w", err)
	}
	defer tx.Rollback()
	if mutation.ChallengeID != "" {
		var expires string
		var used sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT expires_at, used_at FROM local_pairing_challenges WHERE id = ?`, mutation.ChallengeID).Scan(&expires, &used); err != nil {
			return runtimeNotFound("load pairing mutation challenge", err)
		}
		if used.Valid && used.String != "" {
			return fmt.Errorf("consume pairing mutation challenge: %w", localcontrol.ErrPairingUsed)
		}
		challengeExpiry, err := parseTimestamp(expires)
		if err != nil {
			return err
		}
		if !value.UpdatedAt.Before(challengeExpiry) {
			return fmt.Errorf("consume pairing mutation challenge: %w", localcontrol.ErrPairingExpired)
		}
	}
	publicKey := append([]byte(nil), mutation.PublicKey...)
	if !mutation.Create && len(publicKey) == 0 {
		if err := tx.QueryRowContext(ctx, `SELECT public_key FROM local_devices WHERE id = ?`, value.ID).Scan(&publicKey); err != nil {
			return runtimeNotFound("load local device mutation key", err)
		}
	}
	if len(publicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("local device mutation public key: %w", localcontrol.ErrInvalidDeviceProof)
	}
	if mutation.Create {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO local_devices (id, name, kind, fingerprint, public_key, endpoint, state, connection_epoch, revision, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, value.ID, value.Name, value.Kind, value.Fingerprint, publicKey, value.Endpoint, value.State, value.ConnectionEpoch, value.Revision, timestamp(value.CreatedAt), timestamp(value.UpdatedAt)); err != nil {
			return runtimeConflict("create local device mutation", err)
		}
	} else {
		if mutation.ExpectedRevision > math.MaxInt64 || mutation.ExpectedConnectionEpoch > math.MaxInt64 {
			return fmt.Errorf("update local device mutation fence: %w", localcontrol.ErrInvalidRequest)
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE local_devices SET name = ?, kind = ?, fingerprint = ?, public_key = ?, endpoint = ?, state = ?, connection_epoch = ?, revision = ?, updated_at = ?
			WHERE id = ? AND revision = ? AND connection_epoch = ? AND state = ?`, value.Name, value.Kind, value.Fingerprint, publicKey, value.Endpoint, value.State, value.ConnectionEpoch, value.Revision, timestamp(value.UpdatedAt), value.ID, mutation.ExpectedRevision, mutation.ExpectedConnectionEpoch, mutation.ExpectedState)
		if err != nil {
			return runtimeConflict("update local device mutation", err)
		}
		if err := requireRuntimeChanged(result, "update local device mutation"); err != nil {
			return fmt.Errorf("update local device mutation: %w", localcontrol.ErrStaleRevision)
		}
	}
	if value.State == localcontrol.DeviceStatePaired || value.State == localcontrol.DeviceStateRevoked || value.State == localcontrol.DeviceStateUnreachable {
		assignmentState := "fenced"
		if value.State == localcontrol.DeviceStateUnreachable {
			assignmentState = "unreachable"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE local_task_devices SET state = ?, updated_at = ? WHERE device_id = ? AND state IN ('assigned', 'unreachable', 'fenced')`, assignmentState, timestamp(value.UpdatedAt), value.ID); err != nil {
			return runtimeConflict("update local device mutation assignments", err)
		}
	}
	if mutation.ChallengeID != "" {
		result, err := tx.ExecContext(ctx, `UPDATE local_pairing_challenges SET used_at = ? WHERE id = ? AND used_at IS NULL`, timestamp(value.UpdatedAt), mutation.ChallengeID)
		if err != nil {
			return runtimeConflict("consume pairing mutation challenge", err)
		}
		if err := requireRuntimeChanged(result, "consume pairing mutation challenge"); err != nil {
			return fmt.Errorf("consume pairing mutation challenge: %w", localcontrol.ErrPairingUsed)
		}
	}
	if _, err := insertLocalEventTx(ctx, tx, mutation.Event); err != nil {
		return err
	}
	if err := saveIdempotencyTx(ctx, tx, mutation.Idempotency); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit local device mutation: %w", err)
	}
	return nil
}

// MarkDeviceUnreachable is an optimistic, transactional reachability update.
// A late transport error must not overwrite a concurrent key rotation or
// revocation, so the revision and paired state are part of the update fence.
func (s *RuntimeStore) MarkDeviceUnreachable(ctx context.Context, id string) error {
	if s == nil || s.db == nil || strings.TrimSpace(id) == "" {
		return fmt.Errorf("mark local device unreachable: %w", localcontrol.ErrInvalidRequest)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin mark local device unreachable: %w", err)
	}
	defer tx.Rollback()
	var state localcontrol.DeviceState
	var kind localcontrol.DeviceKind
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT kind, state, revision FROM local_devices WHERE id = ?`, id).Scan(&kind, &state, &revision); err != nil {
		return runtimeNotFound("load local device reachability", err)
	}
	if kind != localcontrol.DeviceKindRaspberryPi || state != localcontrol.DeviceStatePaired {
		return nil
	}
	if revision <= 0 || revision == math.MaxInt64 {
		return fmt.Errorf("mark local device unreachable: %w", localcontrol.ErrInvalidRequest)
	}
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `
		UPDATE local_devices SET state = ?, revision = ?, updated_at = ?
		WHERE id = ? AND state = ? AND revision = ?`, localcontrol.DeviceStateUnreachable, revision+1, timestamp(now), id, localcontrol.DeviceStatePaired, revision)
	if err != nil {
		return runtimeConflict("update local device reachability", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read local device reachability update: %w", err)
	}
	if changed == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE local_task_devices SET state = 'unreachable', updated_at = ?
		WHERE device_id = ? AND state = 'assigned'`, timestamp(now), id); err != nil {
		return runtimeConflict("mark local device assignments unreachable", err)
	}
	payload, _ := json.Marshal(map[string]string{"state": string(localcontrol.DeviceStateUnreachable), "reason": "transport_unavailable"})
	if _, err := insertLocalEventTx(ctx, tx, localcontrol.Event{
		ID: fmt.Sprintf("device-unreachable-%s-%d", id, now.UnixNano()), ResourceType: "device", ResourceID: id,
		Revision: revision + 1, Type: "device_state_changed", Payload: payload, CreatedAt: now,
	}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit local device reachability: %w", err)
	}
	return nil
}

func (s *RuntimeStore) CreatePairingChallenge(ctx context.Context, value localcontrol.PairingChallenge) error {
	if value.ID == "" || value.DeviceID == "" || value.Nonce == "" || value.TrustSetDigest == "" || value.ExpiresAt.IsZero() || value.CreatedAt.IsZero() {
		return fmt.Errorf("create pairing challenge: %w", localcontrol.ErrInvalidRequest)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO local_pairing_challenges (id, device_id, browser_fingerprint, nonce, trust_set_digest, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, value.ID, value.DeviceID, value.BrowserFingerprint, value.Nonce, value.TrustSetDigest, timestamp(value.ExpiresAt), timestamp(value.CreatedAt))
	if err != nil {
		return runtimeConflict("create pairing challenge", err)
	}
	return nil
}

func (s *RuntimeStore) GetPairingChallenge(ctx context.Context, id string) (localcontrol.PairingChallenge, error) {
	var value localcontrol.PairingChallenge
	var created, expires string
	var used string
	if err := s.db.QueryRowContext(ctx, `
		SELECT id, device_id, browser_fingerprint, nonce, trust_set_digest, expires_at, created_at, COALESCE(used_at, '')
		FROM local_pairing_challenges WHERE id = ?`, id).Scan(&value.ID, &value.DeviceID, &value.BrowserFingerprint, &value.Nonce, &value.TrustSetDigest, &expires, &created, &used); err != nil {
		return localcontrol.PairingChallenge{}, runtimeNotFound("get pairing challenge", err)
	}
	var err error
	if value.ExpiresAt, err = parseTimestamp(expires); err != nil {
		return localcontrol.PairingChallenge{}, err
	}
	if value.CreatedAt, err = parseTimestamp(created); err != nil {
		return localcontrol.PairingChallenge{}, err
	}
	_ = used
	return value, nil
}

func (s *RuntimeStore) ConsumePairingChallenge(ctx context.Context, id string, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE local_pairing_challenges SET used_at = ? WHERE id = ? AND used_at IS NULL`, timestamp(now), id)
	if err != nil {
		return runtimeConflict("consume pairing challenge", err)
	}
	if err := requireRuntimeChanged(result, "consume pairing challenge"); err != nil {
		return fmt.Errorf("consume pairing challenge: %w", localcontrol.ErrPairingUsed)
	}
	return nil
}

func (s *RuntimeStore) TaskDevice(ctx context.Context, taskID string) (localcontrol.DeviceAssignment, error) {
	var value localcontrol.DeviceAssignment
	var updated string
	var cursor, observedCursor int64
	if err := s.db.QueryRowContext(ctx, `
		SELECT local_task_id, device_id, assignment_epoch, last_ack_cursor, last_observed_cursor, state, updated_at
		FROM local_task_devices WHERE local_task_id = ?`, taskID).Scan(&value.TaskID, &value.DeviceID, &value.AssignmentEpoch, &cursor, &observedCursor, &value.State, &updated); err != nil {
		return localcontrol.DeviceAssignment{}, runtimeNotFound("get local task device", err)
	}
	if cursor < 0 || observedCursor < 0 {
		return localcontrol.DeviceAssignment{}, fmt.Errorf("get local task device: negative cursor")
	}
	value.LastAckCursor = uint64(cursor)
	value.LastObservedCursor = uint64(observedCursor)
	var err error
	if value.UpdatedAt, err = parseTimestamp(updated); err != nil {
		return localcontrol.DeviceAssignment{}, err
	}
	return value, nil
}

func (s *RuntimeStore) AdvanceTaskDeviceObservationCursor(ctx context.Context, taskID, deviceID string, epoch, cursor uint64) error {
	if strings.TrimSpace(taskID) == "" || strings.TrimSpace(deviceID) == "" || epoch == 0 {
		return fmt.Errorf("advance remote observation cursor: %w", localcontrol.ErrInvalidRequest)
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE local_task_devices
		SET last_observed_cursor = ?, updated_at = ?
		WHERE local_task_id = ? AND device_id = ? AND assignment_epoch = ?
		  AND state = 'assigned' AND last_observed_cursor < ?`, cursor, timestamp(time.Now().UTC()), taskID, deviceID, epoch, cursor)
	if err != nil {
		return runtimeConflict("advance remote observation cursor", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read remote observation cursor update: %w", err)
	}
	if changed > 0 {
		return nil
	}
	assignment, err := s.TaskDevice(ctx, taskID)
	if err != nil {
		return err
	}
	if assignment.DeviceID != deviceID || assignment.AssignmentEpoch != epoch || assignment.State != "assigned" {
		return fmt.Errorf("advance remote observation cursor: %w", localcontrol.ErrDeviceFence)
	}
	if assignment.LastObservedCursor >= cursor {
		return nil
	}
	return fmt.Errorf("advance remote observation cursor: %w", localcontrol.ErrDeviceFence)
}

// ApplyDeviceObservation is the controller's durable remote-observation
// boundary. The assignment fence, task revision, event idempotency, approval
// projection, and remote cursor advance commit together so an observation
// from a rotated or concurrently reassigned device cannot leave partial
// evidence in the local authority.
func (s *RuntimeStore) ApplyDeviceObservation(ctx context.Context, taskID, deviceID string, epoch uint64, taskRevision int64, cursor uint64, events []localcontrol.Event, approvals []workmodel.Approval) error {
	if s == nil || s.db == nil || strings.TrimSpace(taskID) == "" || strings.TrimSpace(deviceID) == "" || epoch == 0 || epoch > math.MaxInt64 || taskRevision <= 0 || cursor > math.MaxInt64 {
		return fmt.Errorf("apply remote observation: %w", localcontrol.ErrInvalidRequest)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin remote observation: %w", err)
	}
	defer tx.Rollback()
	var assignedDevice, assignmentState string
	var assignmentEpoch, lastObserved int64
	if err := tx.QueryRowContext(ctx, `
		SELECT device_id, assignment_epoch, last_observed_cursor, state
		FROM local_task_devices WHERE local_task_id = ?`, taskID).
		Scan(&assignedDevice, &assignmentEpoch, &lastObserved, &assignmentState); err != nil {
		return runtimeNotFound("load remote observation assignment", err)
	}
	if lastObserved < 0 || assignedDevice != deviceID || assignmentEpoch != int64(epoch) || assignmentState != "assigned" {
		return fmt.Errorf("apply remote observation assignment: %w", localcontrol.ErrDeviceFence)
	}
	var currentRevision int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM local_tasks WHERE id = ?`, taskID).Scan(&currentRevision); err != nil {
		return runtimeNotFound("load remote observation task", err)
	}
	if currentRevision != taskRevision {
		return fmt.Errorf("apply remote observation task revision: %w", localcontrol.ErrDeviceFence)
	}

	durableRemoteCursor := uint64(lastObserved)
	var previousRemoteCursor uint64
	for _, event := range events {
		if event.TaskID != taskID || event.Revision != taskRevision {
			return fmt.Errorf("apply remote observation event fence: %w", localcontrol.ErrDeviceFence)
		}
		remoteCursor, cursorErr := remoteObservationEventCursor(event)
		if cursorErr != nil {
			return cursorErr
		}
		if remoteCursor > cursor || (previousRemoteCursor > 0 && remoteCursor <= previousRemoteCursor) {
			return fmt.Errorf("remote observation event cursor order: %w", localcontrol.ErrInvalidRequest)
		}
		if remoteCursor > durableRemoteCursor {
			if remoteCursor != durableRemoteCursor+1 {
				return fmt.Errorf("remote observation event cursor gap: %w", localcontrol.ErrInvalidRequest)
			}
			durableRemoteCursor = remoteCursor
		}
		previousRemoteCursor = remoteCursor
		existing, loadErr := loadLocalEventByIDTx(ctx, tx, event.ID)
		switch {
		case loadErr == nil:
			if !samePersistedRemoteObservationEvent(existing, event) {
				return fmt.Errorf("remote observation event %q changed on replay: %w", event.ID, localcontrol.ErrIdempotencyConflict)
			}
		case errors.Is(loadErr, sql.ErrNoRows):
			if remoteCursor <= uint64(lastObserved) {
				return fmt.Errorf("remote observation event %q is behind the durable cursor: %w", event.ID, localcontrol.ErrIdempotencyConflict)
			}
			if _, err := insertLocalEventTx(ctx, tx, event); err != nil {
				return err
			}
		default:
			return fmt.Errorf("load remote observation event %q: %w", event.ID, loadErr)
		}
	}
	if cursor > uint64(lastObserved) && durableRemoteCursor != cursor {
		return fmt.Errorf("remote observation cursor gap: %w", localcontrol.ErrInvalidRequest)
	}

	var executionID string
	if len(approvals) > 0 {
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(active_execution_id, '') FROM local_tasks WHERE id = ?`, taskID).Scan(&executionID); err != nil {
			return runtimeNotFound("load remote observation execution", err)
		}
		if executionID == "" {
			return fmt.Errorf("apply remote observation approval: %w", store.ErrConflict)
		}
	}
	for _, approval := range approvals {
		if approval.ID == "" || approval.TaskID != taskID || approval.Kind == "" || approval.Status != workmodel.ApprovalPending || approval.RequestedAt.IsZero() {
			return fmt.Errorf("apply remote observation approval: %w", localcontrol.ErrInvalidRequest)
		}
		var existingTask, existingKind, existingStatus string
		var existingPayload []byte
		loadErr := tx.QueryRowContext(ctx, `
			SELECT local_task_id, kind, status, request_payload
			FROM approvals WHERE id = ?`, approval.ID).
			Scan(&existingTask, &existingKind, &existingStatus, &existingPayload)
		switch {
		case loadErr == nil:
			if existingTask != taskID || existingKind != approval.Kind {
				return fmt.Errorf("remote observation approval %q identity: %w", approval.ID, localcontrol.ErrInvalidRequest)
			}
			if existingStatus != string(workmodel.ApprovalPending) {
				continue
			}
			if len(existingPayload) > 0 && len(approval.RequestPayload) > 0 && !bytes.Equal(existingPayload, approval.RequestPayload) {
				return fmt.Errorf("remote observation approval %q changed on replay: %w", approval.ID, localcontrol.ErrIdempotencyConflict)
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE approvals SET status = ?, decision_payload = NULL, expires_at = ?, resolved_at = NULL
				WHERE id = ?`, approval.Status, nullableTimestamp(approval.ExpiresAt), approval.ID); err != nil {
				return runtimeConflict("update remote observation approval", err)
			}
		case errors.Is(loadErr, sql.ErrNoRows):
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO approvals (id, local_task_id, execution_id, kind, status, request_payload, decision_payload, requested_at, expires_at, resolved_at)
				VALUES (?, ?, ?, ?, ?, ?, NULL, ?, ?, NULL)`, approval.ID, taskID, executionID, approval.Kind, approval.Status, []byte(approval.RequestPayload), timestamp(approval.RequestedAt), nullableTimestamp(approval.ExpiresAt)); err != nil {
				return runtimeConflict("insert remote observation approval", err)
			}
		default:
			return fmt.Errorf("load remote observation approval %q: %w", approval.ID, loadErr)
		}
	}
	if cursor > uint64(lastObserved) {
		result, err := tx.ExecContext(ctx, `
			UPDATE local_task_devices SET last_observed_cursor = ?, updated_at = ?
			WHERE local_task_id = ? AND device_id = ? AND assignment_epoch = ?
			  AND state = 'assigned' AND last_observed_cursor < ?`, cursor, timestamp(time.Now().UTC()), taskID, deviceID, epoch, cursor)
		if err != nil {
			return runtimeConflict("advance remote observation cursor", err)
		}
		if changed, err := result.RowsAffected(); err != nil {
			return fmt.Errorf("read remote observation cursor update: %w", err)
		} else if changed != 1 {
			return fmt.Errorf("advance remote observation cursor: %w", localcontrol.ErrDeviceFence)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit remote observation: %w", err)
	}
	return nil
}

func loadLocalEventByIDTx(ctx context.Context, tx *sql.Tx, id string) (localcontrol.Event, error) {
	var value localcontrol.Event
	var created string
	if err := tx.QueryRowContext(ctx, `
		SELECT id, resource_type, resource_id, COALESCE(local_task_id, ''), revision, event_type, payload, created_at
		FROM local_control_events WHERE id = ?`, id).
		Scan(&value.ID, &value.ResourceType, &value.ResourceID, &value.TaskID, &value.Revision, &value.Type, &value.Payload, &created); err != nil {
		return localcontrol.Event{}, err
	}
	var err error
	if value.CreatedAt, err = parseTimestamp(created); err != nil {
		return localcontrol.Event{}, err
	}
	value.Payload = append([]byte(nil), value.Payload...)
	return value, nil
}

func samePersistedLocalEvent(a, b localcontrol.Event) bool {
	return a.ID == b.ID && a.ResourceType == b.ResourceType && a.ResourceID == b.ResourceID &&
		a.TaskID == b.TaskID && a.Revision == b.Revision && a.Type == b.Type &&
		a.CreatedAt.Equal(b.CreatedAt) && bytes.Equal(a.Payload, b.Payload)
}

// Remote observation events are immutable by remote cursor and event ID, but
// their local projection may be replayed after the task revision advances
// during a reconnect or device reassignment. Keep the original local event
// revision instead of treating that expected projection change as a payload
// conflict.
func samePersistedRemoteObservationEvent(a, b localcontrol.Event) bool {
	return a.ID == b.ID && a.ResourceType == b.ResourceType && a.ResourceID == b.ResourceID &&
		a.TaskID == b.TaskID && a.Type == b.Type && a.CreatedAt.Equal(b.CreatedAt) &&
		bytes.Equal(a.Payload, b.Payload)
}

func remoteObservationEventCursor(event localcontrol.Event) (uint64, error) {
	var payload struct {
		RemoteCursor uint64 `json:"remote_cursor"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.RemoteCursor == 0 {
		return 0, fmt.Errorf("remote observation event cursor: %w", localcontrol.ErrInvalidRequest)
	}
	return payload.RemoteCursor, nil
}

func (s *RuntimeStore) AssignTaskDevice(ctx context.Context, taskID string, expectedRevision int64, deviceID string, epoch uint64, event localcontrol.Event) (localcontrol.DeviceAssignment, localcontrol.Event, error) {
	if taskID == "" || deviceID == "" || expectedRevision <= 0 || epoch == 0 {
		return localcontrol.DeviceAssignment{}, localcontrol.Event{}, fmt.Errorf("assign local task device: %w", localcontrol.ErrInvalidRequest)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return localcontrol.DeviceAssignment{}, localcontrol.Event{}, fmt.Errorf("begin local device assignment: %w", err)
	}
	defer tx.Rollback()
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM local_tasks WHERE id = ?`, taskID).Scan(&revision); err != nil {
		return localcontrol.DeviceAssignment{}, localcontrol.Event{}, runtimeNotFound("load local device assignment task", err)
	}
	if revision != expectedRevision {
		return localcontrol.DeviceAssignment{}, localcontrol.Event{}, fmt.Errorf("local task revision %d, expected %d: %w", revision, expectedRevision, localcontrol.ErrStaleRevision)
	}
	var state localcontrol.DeviceState
	var currentEpoch uint64
	if err := tx.QueryRowContext(ctx, `SELECT state, connection_epoch FROM local_devices WHERE id = ?`, deviceID).Scan(&state, &currentEpoch); err != nil {
		return localcontrol.DeviceAssignment{}, localcontrol.Event{}, runtimeNotFound("load assignment device", err)
	}
	if state != localcontrol.DeviceStatePaired {
		return localcontrol.DeviceAssignment{}, localcontrol.Event{}, fmt.Errorf("assign device: %w", deviceStateError(state))
	}
	if currentEpoch != epoch {
		return localcontrol.DeviceAssignment{}, localcontrol.Event{}, fmt.Errorf("assign device epoch %d, expected %d: %w", currentEpoch, epoch, localcontrol.ErrDeviceFence)
	}
	at := event.CreatedAt.UTC()
	if at.IsZero() {
		at = time.Now().UTC()
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE local_tasks SET revision = revision + 1, updated_at = ?
		WHERE id = ? AND revision = ?`, timestamp(at), taskID, expectedRevision)
	if err != nil {
		return localcontrol.DeviceAssignment{}, localcontrol.Event{}, runtimeConflict("advance local task device revision", err)
	}
	if err := requireRuntimeChanged(result, "advance local task device revision"); err != nil {
		return localcontrol.DeviceAssignment{}, localcontrol.Event{}, fmt.Errorf("advance local task device revision: %w", localcontrol.ErrStaleRevision)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE local_task_devices SET device_id = ?, assignment_epoch = ?, last_observed_cursor = 0, state = 'assigned', updated_at = ?
		WHERE local_task_id = ?`, deviceID, epoch, timestamp(at), taskID); err != nil {
		return localcontrol.DeviceAssignment{}, localcontrol.Event{}, runtimeConflict("save local task device", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE local_device_commands SET device_id = ?, assignment_epoch = ?, updated_at = ?
		WHERE task_id = ? AND state IN ('pending', 'in_flight')`, deviceID, epoch, timestamp(at), taskID); err != nil {
		return localcontrol.DeviceAssignment{}, localcontrol.Event{}, runtimeConflict("rebind local device commands", err)
	}
	storedEvent, err := insertLocalEventTx(ctx, tx, event)
	if err != nil {
		return localcontrol.DeviceAssignment{}, localcontrol.Event{}, err
	}
	event = storedEvent
	if err := tx.Commit(); err != nil {
		return localcontrol.DeviceAssignment{}, localcontrol.Event{}, fmt.Errorf("commit local device assignment: %w", err)
	}
	assignment, err := s.TaskDevice(ctx, taskID)
	if err != nil {
		return localcontrol.DeviceAssignment{}, localcontrol.Event{}, err
	}
	return assignment, event, nil
}

func deviceStateError(state localcontrol.DeviceState) error {
	switch state {
	case localcontrol.DeviceStateUnreachable:
		return localcontrol.ErrDeviceUnreachable
	case localcontrol.DeviceStateRevoked:
		return localcontrol.ErrDeviceRevoked
	default:
		return localcontrol.ErrDeviceNotPaired
	}
}

var _ localcontrol.DeviceAuthority = (*RuntimeStore)(nil)
