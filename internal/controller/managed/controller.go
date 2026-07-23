package managed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/berkayahi/agentbridge/internal/kernel"
	managedprotocol "github.com/berkayahi/agentbridge/internal/managed"
)

var ErrInvalidManagedCommand = errors.New("managed controller: invalid command")

type executor interface {
	Start(context.Context, kernel.StartExecution) error
	Resume(context.Context, kernel.ResumeExecution) error
	Steer(context.Context, kernel.SteerExecution) error
	Cancel(context.Context, kernel.CancelExecution) error
	Close(context.Context, kernel.CloseExecution) error
	Fork(context.Context, kernel.ForkExecution) error
}

type Controller struct{ executor executor }

func New(k *kernel.Kernel) *Controller { return NewWithExecutor(k) }

func NewWithExecutor(value executor) *Controller { return &Controller{executor: value} }

func (c *Controller) Start(ctx context.Context, command kernel.StartExecution) error {
	return c.executor.Start(ctx, command)
}
func (c *Controller) Resume(ctx context.Context, command kernel.ResumeExecution) error {
	return c.executor.Resume(ctx, command)
}
func (c *Controller) Steer(ctx context.Context, command kernel.SteerExecution) error {
	return c.executor.Steer(ctx, command)
}
func (c *Controller) Cancel(ctx context.Context, command kernel.CancelExecution) error {
	return c.executor.Cancel(ctx, command)
}
func (c *Controller) Close(ctx context.Context, command kernel.CloseExecution) error {
	return c.executor.Close(ctx, command)
}
func (c *Controller) Fork(ctx context.Context, command kernel.ForkExecution) error {
	return c.executor.Fork(ctx, command)
}

// Dispatcher returns the only canonical managed command entry point. The
// connection has already verified the platform signature and durably admitted
// the frame before this handler can invoke the kernel.
func (c *Controller) Dispatcher() managedprotocol.Dispatcher {
	return managedprotocol.Dispatcher{Handlers: map[string]managedprotocol.CommandHandler{
		"command": c.Dispatch,
	}}
}

func (c *Controller) Dispatch(ctx context.Context, frame managedprotocol.Frame) error {
	if c == nil || c.executor == nil || frame.PayloadType != "command" || strings.TrimSpace(frame.CommandID) == "" || strings.TrimSpace(frame.ExecutionID) == "" || len(frame.Payload) == 0 {
		return ErrInvalidManagedCommand
	}
	var payload commandPayload
	decoder := json.NewDecoder(bytes.NewReader(frame.Payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil || strings.TrimSpace(payload.Kind) == "" {
		return ErrInvalidManagedCommand
	}
	if payload.CommandID != "" && payload.CommandID != frame.CommandID {
		return fmt.Errorf("%w: command identity mismatch", ErrInvalidManagedCommand)
	}
	if payload.ExecutionID != "" && payload.ExecutionID != frame.ExecutionID {
		return fmt.Errorf("%w: execution identity mismatch", ErrInvalidManagedCommand)
	}
	if payload.SessionID != "" && payload.SessionID != frame.SessionID {
		return fmt.Errorf("%w: session identity mismatch", ErrInvalidManagedCommand)
	}
	if strings.TrimSpace(payload.RuntimeID) == "" || strings.TrimSpace(payload.TaskID) == "" {
		return ErrInvalidManagedCommand
	}
	switch strings.ToLower(payload.Kind) {
	case "start":
		sessionID := payload.SessionID
		if sessionID == "" {
			sessionID = frame.SessionID
		}
		if sessionID == "" || len(payload.PolicySnapshot) == 0 || strings.TrimSpace(payload.Input) == "" || payload.FencingEpoch == 0 {
			return ErrInvalidManagedCommand
		}
		expiresAt := payload.ExpiresAt
		if expiresAt.IsZero() {
			expiresAt = frame.ExpiresAt
		}
		return c.Start(ctx, kernel.StartExecution{
			CommandID: frame.CommandID, ExecutionID: frame.ExecutionID, TaskID: payload.TaskID,
			SessionID: sessionID, RepositoryID: payload.RepositoryID, RuntimeID: payload.RuntimeID,
			Model: payload.Model, PolicySnapshot: append([]byte(nil), payload.PolicySnapshot...),
			FencingEpoch: payload.FencingEpoch, Input: kernel.Input{Text: payload.Input}, ExpiresAt: expiresAt,
		})
	case "resume":
		if strings.TrimSpace(payload.Input) == "" {
			return ErrInvalidManagedCommand
		}
		return c.Resume(ctx, kernel.ResumeExecution{CommandID: frame.CommandID, ExecutionID: frame.ExecutionID, TaskID: payload.TaskID, RuntimeID: payload.RuntimeID, Input: kernel.Input{Text: payload.Input}})
	case "steer":
		if strings.TrimSpace(payload.Input) == "" {
			return ErrInvalidManagedCommand
		}
		return c.Steer(ctx, kernel.SteerExecution{CommandID: frame.CommandID, ExecutionID: frame.ExecutionID, TaskID: payload.TaskID, RuntimeID: payload.RuntimeID, Input: kernel.Input{Text: payload.Input}})
	case "cancel", "interrupt":
		return c.Cancel(ctx, kernel.CancelExecution{CommandID: frame.CommandID, ExecutionID: frame.ExecutionID, TaskID: payload.TaskID, RuntimeID: payload.RuntimeID})
	case "close":
		return c.Close(ctx, kernel.CloseExecution{CommandID: frame.CommandID, ExecutionID: frame.ExecutionID, TaskID: payload.TaskID, RuntimeID: payload.RuntimeID})
	case "fork":
		if strings.TrimSpace(payload.SuccessorTaskID) == "" || strings.TrimSpace(payload.Input) == "" {
			return ErrInvalidManagedCommand
		}
		return c.Fork(ctx, kernel.ForkExecution{CommandID: frame.CommandID, ExecutionID: frame.ExecutionID, TaskID: payload.TaskID, RuntimeID: payload.RuntimeID, SuccessorTaskID: payload.SuccessorTaskID, Input: kernel.Input{Text: payload.Input}})
	default:
		return fmt.Errorf("%w: unsupported kind %q", ErrInvalidManagedCommand, payload.Kind)
	}
}

type commandPayload struct {
	Kind            string    `json:"kind"`
	CommandID       string    `json:"command_id,omitempty"`
	ExecutionID     string    `json:"execution_id,omitempty"`
	SessionID       string    `json:"session_id,omitempty"`
	TaskID          string    `json:"task_id"`
	RuntimeID       string    `json:"runtime_id"`
	RepositoryID    string    `json:"repository_id,omitempty"`
	Model           string    `json:"model,omitempty"`
	Input           string    `json:"input,omitempty"`
	PolicySnapshot  []byte    `json:"policy_snapshot,omitempty"`
	FencingEpoch    uint64    `json:"fencing_epoch,omitempty"`
	SuccessorTaskID string    `json:"successor_task_id,omitempty"`
	ExpiresAt       time.Time `json:"expires_at,omitempty"`
}
