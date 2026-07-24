package localcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/berkayahi/agentbridge/internal/store"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

const deviceCommandQueuedEvent = "device_command_queued"

func (s *Service) targetAvailability(ctx context.Context, view TaskView) (bool, error) {
	device, err := s.store.GetDevice(ctx, view.TargetDeviceID)
	if err != nil {
		return false, err
	}
	if device.ConnectionEpoch != view.TargetEpoch {
		return false, fmt.Errorf("task target epoch %d, device epoch %d: %w", view.TargetEpoch, device.ConnectionEpoch, ErrDeviceFence)
	}
	switch device.State {
	case DeviceStatePaired:
		return true, nil
	case DeviceStateUnreachable:
		if view.TargetDeviceID != LocalDeviceID {
			return false, nil
		}
		return false, ErrDeviceUnreachable
	case DeviceStateRevoked:
		return false, ErrDeviceRevoked
	default:
		return false, ErrDeviceNotPaired
	}
}

func (s *Service) enqueueDeviceCommand(ctx context.Context, view TaskView, operation, id string, hashPayload, requestPayload any, claim bool) (DeviceCommandRecord, bool, error) {
	if view.TargetDeviceID == LocalDeviceID {
		return DeviceCommandRecord{}, false, nil
	}
	if err := validateIdempotencyKey(id); err != nil {
		return DeviceCommandRecord{}, true, err
	}
	encoded, err := json.Marshal(requestPayload)
	if err != nil {
		return DeviceCommandRecord{}, true, err
	}
	now := s.clock().UTC()
	record := DeviceCommandRecord{
		ID: id, TaskID: view.ID, DeviceID: view.TargetDeviceID, AssignmentEpoch: view.TargetEpoch,
		Operation: operation, RequestHash: deviceCommandHash(operation, hashPayload), RequestPayload: encoded,
		Revision: view.Revision, State: DeviceCommandPending, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.store.EnqueueDeviceCommand(ctx, record); err != nil {
		return DeviceCommandRecord{}, true, err
	}
	if stored, err := s.store.GetDeviceCommand(ctx, id); err == nil {
		record = stored
	} else if !errors.Is(err, store.ErrNotFound) {
		return DeviceCommandRecord{}, true, err
	}
	if record.State == DeviceCommandFailed {
		if err := s.store.ResetDeviceCommand(ctx, id, "retry requested", now); err != nil {
			return DeviceCommandRecord{}, true, err
		}
		record.State = DeviceCommandPending
	}
	if claim && record.State != DeviceCommandCompleted {
		claimed, err := s.store.ClaimDeviceCommand(ctx, id, now)
		if err != nil {
			return DeviceCommandRecord{}, true, err
		}
		if claimed {
			record.State = DeviceCommandInFlight
			record.Attempts++
		}
	}
	return record, true, nil
}

func (s *Service) queueUnavailable(ctx context.Context, view TaskView, operation, id string, hashPayload, requestPayload any, reason error) (Event, error) {
	record, remote, err := s.enqueueDeviceCommand(ctx, view, operation, id, hashPayload, requestPayload, false)
	if err != nil {
		return Event{}, err
	}
	if !remote {
		return Event{}, ErrNotConfigured
	}
	if record.State != DeviceCommandCompleted {
		if record.State == DeviceCommandFailed {
			if err := s.store.ResetDeviceCommand(ctx, id, safeError(reason), s.clock().UTC()); err != nil {
				return Event{}, err
			}
		} else if err := s.store.ResetDeviceCommand(ctx, id, safeError(reason), s.clock().UTC()); err != nil {
			return Event{}, err
		}
	}
	return s.appendQueuedDeviceEvent(ctx, view, operation, safeError(reason))
}

func (s *Service) deferDeviceCommand(ctx context.Context, record DeviceCommandRecord, view TaskView, reason error) (Event, error) {
	if record.ID == "" || view.TargetDeviceID == LocalDeviceID {
		return Event{}, ErrInvalidRequest
	}
	if err := s.store.ResetDeviceCommand(ctx, record.ID, safeError(reason), s.clock().UTC()); err != nil {
		return Event{}, err
	}
	return s.appendQueuedDeviceEvent(ctx, view, record.Operation, safeError(reason))
}

func (s *Service) completeDeviceCommand(ctx context.Context, record DeviceCommandRecord) error {
	if record.ID == "" || record.DeviceID == LocalDeviceID {
		return nil
	}
	return s.store.CompleteDeviceCommand(ctx, record.ID, s.clock().UTC())
}

// completePendingDeviceCommands closes commands whose native effect is
// already represented by a durable task/checkpoint projection. This is
// needed for commit recovery: a process can die after RecordCheckpoint but
// before the command row is marked completed. Replaying the commit must not
// leave that old in-flight command visible forever.
func (s *Service) completePendingDeviceCommands(ctx context.Context, view TaskView, operation string) error {
	if view.TargetDeviceID == LocalDeviceID {
		return nil
	}
	pending, err := s.store.ListPendingDeviceCommands(ctx, view.TargetDeviceID, 200)
	if err != nil {
		return err
	}
	now := s.clock().UTC()
	for _, record := range pending {
		if record.TaskID != view.ID || record.AssignmentEpoch != view.TargetEpoch || record.Operation != operation {
			continue
		}
		if err := s.store.CompleteDeviceCommand(ctx, record.ID, now); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	return nil
}

func (s *Service) failDeviceCommand(ctx context.Context, record DeviceCommandRecord, err error) error {
	if record.ID == "" || record.DeviceID == LocalDeviceID {
		return nil
	}
	return s.store.FailDeviceCommand(ctx, record.ID, safeError(err), s.clock().UTC())
}

func (s *Service) appendQueuedDeviceEvent(ctx context.Context, view TaskView, operation, reason string) (Event, error) {
	event := localEvent(s.newID("event"), "task", view.ID, view.ID, view.Revision, deviceCommandQueuedEvent, map[string]any{
		"device_id": view.TargetDeviceID, "operation": operation, "reason": reason,
	}, s.clock().UTC())
	return s.store.AppendLocalEvent(ctx, event)
}

func isDeviceUnavailable(err error) bool {
	return errors.Is(err, ErrDeviceUnreachable) || errors.Is(err, ErrDeviceLinkUnavailable)
}

func deviceCommandHash(operation string, payload any) string {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return requestHash(operation, payload)
	}
	var normalized any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		return requestHash(operation, payload)
	}
	removeDeviceCommandRevision(normalized)
	return requestHash(operation, normalized)
}

func removeDeviceCommandRevision(value any) {
	switch current := value.(type) {
	case map[string]any:
		delete(current, "revision")
		for _, nested := range current {
			removeDeviceCommandRevision(nested)
		}
	case []any:
		for _, nested := range current {
			removeDeviceCommandRevision(nested)
		}
	}
}

// ReplayDeviceCommands is an explicit reconnect/recovery boundary. It is
// intentionally caller-driven: the Desktop or a supervisor can invoke it
// after a paired device becomes reachable without adding an unbounded
// background goroutine to the controller.
func (s *Service) ReplayDeviceCommands(ctx context.Context, request ReplayDeviceCommandsRequest) (ReplayDeviceCommandsResponse, error) {
	// Reconnect is an explicit recovery boundary, but it is still reachable
	// through concurrent Desktop/API calls. Serialize recovery passes so two
	// callers cannot both re-drive the same in-flight command while the local
	// authority is waiting for the remote result. A process restart can still
	// retry durable in-flight work through the next recovery pass.
	s.replayMu.Lock()
	defer s.replayMu.Unlock()

	deviceID := strings.TrimSpace(request.DeviceID)
	if !validID(deviceID) {
		return ReplayDeviceCommandsResponse{}, ErrInvalidRequest
	}
	device, err := s.store.GetDevice(ctx, deviceID)
	if err != nil {
		return ReplayDeviceCommandsResponse{}, err
	}
	if device.State == DeviceStateRevoked {
		return ReplayDeviceCommandsResponse{}, ErrDeviceRevoked
	}
	limit := request.Limit
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	pending, err := s.store.ListPendingDeviceCommands(ctx, deviceID, limit)
	if err != nil {
		return ReplayDeviceCommandsResponse{}, err
	}
	response := ReplayDeviceCommandsResponse{DeviceID: deviceID}
	if device.State != DeviceStatePaired {
		response.Pending = pending
		return response, nil
	}
	for _, record := range pending {
		view, err := s.taskView(ctx, record.TaskID)
		if err != nil {
			_ = s.failDeviceCommand(ctx, record, err)
			continue
		}
		if view.TargetDeviceID != deviceID || view.TargetEpoch != record.AssignmentEpoch {
			_ = s.failDeviceCommand(ctx, record, ErrDeviceFence)
			continue
		}
		if err := s.replayDeviceCommand(ctx, record, view); err != nil {
			if isDeviceUnavailable(err) {
				continue
			}
			_ = s.failDeviceCommand(ctx, record, err)
			continue
		}
		stored, err := s.store.GetDeviceCommand(ctx, record.ID)
		if err == nil && stored.State == DeviceCommandCompleted {
			response.Replayed++
		}
	}
	response.Pending, err = s.store.ListPendingDeviceCommands(ctx, deviceID, limit)
	if err != nil {
		return ReplayDeviceCommandsResponse{}, err
	}
	return response, nil
}

func (s *Service) replayDeviceCommand(ctx context.Context, record DeviceCommandRecord, view TaskView) error {
	requestHash := ""
	if original, err := s.store.LoadIdempotency(ctx, record.ID); err == nil {
		if original.Operation != record.Operation {
			return ErrIdempotencyConflict
		}
		requestHash = original.RequestHash
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	replayCtx := withDeviceReplay(ctx, requestHash)
	if record.Operation == "cancel" && view.State == workmodel.Canceled {
		if s.executor == nil {
			return ErrNotConfigured
		}
		if err := s.executor.Cancel(ctx, view); err != nil {
			return err
		}
		events, err := s.store.ListLocalEvents(ctx, view.ID, 0, 200)
		if err != nil {
			return err
		}
		var canceled Event
		for index := len(events) - 1; index >= 0; index-- {
			if events[index].Type == "canceled" {
				canceled = events[index]
				break
			}
		}
		if canceled.ID == "" {
			return fmt.Errorf("replay canceled device command: %w", store.ErrConflict)
		}
		if err := s.rememberAction(replayCtx, record.ID, "cancel", nil, ActionResponse{Task: view, Event: canceled}); err != nil {
			return err
		}
		return s.completeDeviceCommand(ctx, record)
	}
	switch record.Operation {
	case "start":
		var request StartRequest
		if err := json.Unmarshal(record.RequestPayload, &request); err != nil {
			return err
		}
		request.TaskID, request.Revision, request.IdempotencyKey = view.ID, view.Revision, record.ID
		_, err := s.Start(replayCtx, request)
		return err
	case "resume":
		var request ResumeRequest
		if err := json.Unmarshal(record.RequestPayload, &request); err != nil {
			return err
		}
		request.TaskID, request.Revision, request.IdempotencyKey = view.ID, view.Revision, record.ID
		_, err := s.Resume(replayCtx, request)
		return err
	case "approve":
		var request ApproveRequest
		if err := json.Unmarshal(record.RequestPayload, &request); err != nil {
			return err
		}
		request.TaskID, request.Revision, request.IdempotencyKey = view.ID, view.Revision, record.ID
		_, err := s.Approve(replayCtx, request)
		return err
	case "cancel":
		var request CancelRequest
		if err := json.Unmarshal(record.RequestPayload, &request); err != nil {
			return err
		}
		request.TaskID, request.Revision, request.IdempotencyKey = view.ID, view.Revision, record.ID
		_, err := s.Cancel(replayCtx, request)
		return err
	case "verify":
		var request VerifyRequest
		if err := json.Unmarshal(record.RequestPayload, &request); err != nil {
			return err
		}
		request.TaskID, request.Revision, request.IdempotencyKey = view.ID, view.Revision, record.ID
		_, err := s.Verify(replayCtx, request)
		return err
	case "commit":
		var request CommitRequest
		if err := json.Unmarshal(record.RequestPayload, &request); err != nil {
			return err
		}
		request.TaskID, request.Revision, request.IdempotencyKey = view.ID, view.Revision, record.ID
		_, err := s.Commit(replayCtx, request)
		return err
	default:
		return ErrInvalidRequest
	}
}
