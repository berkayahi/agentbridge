package localcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/berkayahi/agentbridge/internal/managed"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

var (
	ErrDeviceLinkUnavailable = errors.New("localcontrol: device link unavailable")
	ErrDeviceLinkRejected    = errors.New("localcontrol: device link rejected command")
)

// DeviceCommand is the transport-neutral command sent to a paired execution
// device. A concrete link may use the public device protocol, but the command
// remains high-level and carries no provider executable, filesystem path, or
// credential.
type DeviceCommand struct {
	ID               string             `json:"id"`
	Operation        string             `json:"operation"`
	TaskID           string             `json:"task_id"`
	ExecutionID      string             `json:"execution_id"`
	SessionID        string             `json:"session_id"`
	Provider         workmodel.Provider `json:"provider,omitempty"`
	RepositoryID     string             `json:"repository_id,omitempty"`
	RepositoryRemote string             `json:"repository_remote,omitempty"`
	RuntimeID        string             `json:"runtime_id,omitempty"`
	Title            string             `json:"title,omitempty"`
	Prompt           string             `json:"prompt,omitempty"`
	DeviceID         string             `json:"device_id"`
	ConnectionEpoch  uint64             `json:"connection_epoch"`
	Revision         int64              `json:"revision"`
	Payload          json.RawMessage    `json:"payload,omitempty"`
}

type DeviceReply struct {
	MessageID       uint64          `json:"message_id"`
	DeviceID        string          `json:"device_id"`
	ConnectionEpoch uint64          `json:"connection_epoch"`
	Accepted        bool            `json:"accepted"`
	Error           string          `json:"error,omitempty"`
	Payload         json.RawMessage `json:"payload,omitempty"`
}

func validateDeviceCommand(command DeviceCommand) error {
	if strings.TrimSpace(command.ID) == "" || len(command.ID) > 128 || strings.ContainsAny(command.ID, "\x00\r\n") || !validDeviceOperation(command.Operation) || !validID(command.DeviceID) || command.ConnectionEpoch == 0 || len(command.Payload) > managed.MaxPayloadBytes {
		return ErrInvalidRequest
	}
	if command.Provider != "" && !command.Provider.Valid() {
		return ErrInvalidRequest
	}
	if command.RepositoryRemote != "" {
		if err := validateRemote(command.RepositoryRemote); err != nil {
			return err
		}
	}
	for _, value := range []string{command.TaskID, command.ExecutionID, command.SessionID, command.RepositoryID, command.RuntimeID} {
		if value != "" && !validID(value) {
			return ErrInvalidRequest
		}
	}
	if len(command.Title) > 512 || len(command.Prompt) > managed.MaxPayloadBytes || strings.ContainsAny(command.Title+command.Prompt, "\x00") {
		return ErrInvalidRequest
	}
	return nil
}

func validDeviceOperation(operation string) bool {
	switch operation {
	case "start", "resume", "approve", "cancel", "verify", "commit", "observe":
		return true
	default:
		return false
	}
}

// DeviceLink is the only boundary a paired Pi needs to implement. It is
// intentionally request/response based so the caller can persist command
// idempotency and fence late acknowledgements before applying a result.
type DeviceLink interface {
	Execute(context.Context, DeviceCommand) (DeviceReply, error)
}

type LinkedRuntime struct {
	link DeviceLink
}

func NewLinkedRuntime(link DeviceLink) (*LinkedRuntime, error) {
	if link == nil {
		return nil, ErrDeviceLinkUnavailable
	}
	return &LinkedRuntime{link: link}, nil
}

// NewFencedLinkedRuntime composes the live-link fence with the typed runtime
// and closes a short-lived transport after the single controller operation.
// Durable command idempotency remains in the local store and on the device;
// the close hook only releases the WSS/socket resource.
func NewFencedLinkedRuntime(deviceID string, epoch uint64, link DeviceLink, close func() error) (DeviceRuntime, error) {
	fenced, err := NewFencedLink(deviceID, epoch, link)
	if err != nil {
		return nil, err
	}
	runtime, err := NewLinkedRuntime(fenced)
	if err != nil {
		return nil, err
	}
	if close == nil {
		return runtime, nil
	}
	return &closingDeviceRuntime{runtime: runtime, close: close}, nil
}

type closingDeviceRuntime struct {
	runtime DeviceRuntime
	close   func() error
}

func (r *closingDeviceRuntime) finish(err error) error {
	return errors.Join(err, r.close())
}

func (r *closingDeviceRuntime) Start(ctx context.Context, view TaskView, request StartRequest) error {
	return r.finish(r.runtime.Start(ctx, view, request))
}
func (r *closingDeviceRuntime) Resume(ctx context.Context, view TaskView, request ResumeRequest) error {
	return r.finish(r.runtime.Resume(ctx, view, request))
}
func (r *closingDeviceRuntime) Approve(ctx context.Context, view TaskView, approvalID, userID string, allow bool) error {
	return r.finish(r.runtime.Approve(ctx, view, approvalID, userID, allow))
}
func (r *closingDeviceRuntime) Cancel(ctx context.Context, view TaskView) error {
	return r.finish(r.runtime.Cancel(ctx, view))
}
func (r *closingDeviceRuntime) Verify(ctx context.Context, view TaskView) (VerificationReceipt, error) {
	receipt, err := r.runtime.Verify(ctx, view)
	return receipt, r.finish(err)
}
func (r *closingDeviceRuntime) Commit(ctx context.Context, view TaskView) (CommitReceipt, error) {
	receipt, err := r.runtime.Commit(ctx, view)
	return receipt, r.finish(err)
}

func (r *closingDeviceRuntime) Observe(ctx context.Context, view TaskView, after uint64) (DeviceObservation, error) {
	observer, ok := r.runtime.(DeviceObserver)
	if !ok {
		return DeviceObservation{}, r.finish(ErrNotConfigured)
	}
	value, err := observer.Observe(ctx, view, after)
	return value, r.finish(err)
}

func (r *LinkedRuntime) Start(ctx context.Context, view TaskView, request StartRequest) error {
	return r.command(ctx, view, "start", request.IdempotencyKey, request)
}

func (r *LinkedRuntime) Resume(ctx context.Context, view TaskView, request ResumeRequest) error {
	return r.command(ctx, view, "resume", request.IdempotencyKey, request)
}

func (r *LinkedRuntime) Approve(ctx context.Context, view TaskView, approvalID, userID string, allow bool) error {
	payload := struct {
		ApprovalID string `json:"approval_id"`
		UserID     string `json:"user_id"`
		Allow      bool   `json:"allow"`
	}{approvalID, userID, allow}
	return r.command(ctx, view, "approve", "approve:"+approvalID, payload)
}

func (r *LinkedRuntime) Cancel(ctx context.Context, view TaskView) error {
	return r.command(ctx, view, "cancel", "cancel:"+view.ID, struct{}{})
}

func (r *LinkedRuntime) Verify(ctx context.Context, view TaskView) (VerificationReceipt, error) {
	var receipt VerificationReceipt
	if err := r.commandResult(ctx, view, "verify", fmt.Sprintf("verify:%s:%d", view.ID, view.Revision), struct{}{}, &receipt); err != nil {
		return VerificationReceipt{}, err
	}
	return receipt, nil
}

func (r *LinkedRuntime) Commit(ctx context.Context, view TaskView) (CommitReceipt, error) {
	var receipt CommitReceipt
	if err := r.commandResult(ctx, view, "commit", fmt.Sprintf("commit:%s:%d", view.ID, view.Revision), struct{}{}, &receipt); err != nil {
		return CommitReceipt{}, err
	}
	return receipt, nil
}

func (r *LinkedRuntime) Observe(ctx context.Context, view TaskView, after uint64) (DeviceObservation, error) {
	var observation DeviceObservation
	payload := struct {
		AfterCursor uint64 `json:"after_cursor"`
	}{AfterCursor: after}
	// Observation is read-only and intentionally does not enter the durable
	// command queue. Use a fresh command id so the device result cache cannot
	// turn a later poll into a stale replay of an earlier snapshot.
	if err := r.commandResult(ctx, view, "observe", defaultID("observe"), payload, &observation); err != nil {
		return DeviceObservation{}, err
	}
	return observation, nil
}

func (r *LinkedRuntime) command(ctx context.Context, view TaskView, operation, id string, payload any) error {
	return r.commandResult(ctx, view, operation, id, payload, nil)
}

func (r *LinkedRuntime) commandResult(ctx context.Context, view TaskView, operation, id string, payload any, destination any) error {
	if r == nil || r.link == nil {
		return ErrDeviceLinkUnavailable
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	command := DeviceCommand{
		ID: id, Operation: operation, TaskID: view.ID, ExecutionID: view.ExecutionID,
		SessionID: view.SessionID, Provider: view.Provider, RepositoryID: view.RepositoryID,
		RepositoryRemote: view.RepositoryRemote,
		RuntimeID:        view.RuntimeID, Title: view.Title, Prompt: view.Prompt, DeviceID: view.TargetDeviceID,
		ConnectionEpoch: view.TargetEpoch, Revision: view.Revision, Payload: encoded,
	}
	reply, err := r.link.Execute(ctx, command)
	if err != nil {
		return err
	}
	if reply.DeviceID != command.DeviceID || reply.ConnectionEpoch != command.ConnectionEpoch {
		return fmt.Errorf("device link reply epoch/device mismatch: %w", ErrDeviceFence)
	}
	if !reply.Accepted {
		if message := strings.TrimSpace(reply.Error); message != "" {
			return fmt.Errorf("%s: %w", message, ErrDeviceLinkRejected)
		}
		return ErrDeviceLinkRejected
	}
	if destination != nil && len(reply.Payload) > 0 {
		if err := json.Unmarshal(reply.Payload, destination); err != nil {
			return fmt.Errorf("decode device link %s reply: %w", operation, err)
		}
	}
	return nil
}

var _ DeviceRuntime = (*LinkedRuntime)(nil)
var _ DeviceObserver = (*LinkedRuntime)(nil)
