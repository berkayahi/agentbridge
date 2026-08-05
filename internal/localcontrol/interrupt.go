package localcontrol

import (
	"context"
	"fmt"

	"github.com/berkayahi/agentbridge/internal/store"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

// Interrupter stops a runtime's active provider turn without canceling the
// durable task or deleting its resumable session.
type Interrupter interface {
	Interrupt(context.Context, TaskView) error
}

// Interrupt pauses a task after its live provider turn acknowledges the stop.
// The command lock remains held across both operations: a provider-completed
// observation racing the interrupt cannot advance the task to verification
// before the paused state is durable.
func (s *Service) Interrupt(ctx context.Context, request InterruptRequest) (ActionResponse, error) {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()

	payload := struct {
		TaskID   string `json:"task_id"`
		Revision int64  `json:"revision"`
	}{request.TaskID, request.Revision}
	var cached ActionResponse
	if done, err := s.replayAction(ctx, request.IdempotencyKey, "interrupt", payload, &cached); done || err != nil {
		return cached, err
	}
	if !validID(request.TaskID) || request.Revision <= 0 {
		return ActionResponse{}, ErrInvalidRequest
	}
	view, err := s.taskView(ctx, request.TaskID)
	if err != nil {
		return ActionResponse{}, err
	}
	if err := checkRevision(view, request.Revision); err != nil {
		return ActionResponse{}, err
	}
	if !interruptibleState(view.State) {
		return ActionResponse{}, fmt.Errorf("interrupt task in %s: %w", view.State, store.ErrConflict)
	}
	interrupter, ok := s.executor.(Interrupter)
	if !ok {
		return ActionResponse{}, fmt.Errorf("runtime cannot be interrupted: %w", ErrNotConfigured)
	}
	if err := s.ensureTaskTarget(ctx, view); err != nil {
		return ActionResponse{}, err
	}
	available, err := s.targetAvailability(ctx, view)
	if err != nil {
		return ActionResponse{}, err
	}
	if !available {
		return ActionResponse{}, ErrDeviceUnreachable
	}
	if err := interrupter.Interrupt(ctx, view); err != nil {
		return ActionResponse{}, err
	}
	next, event, err := s.transition(ctx, view, workmodel.Paused, "interrupted", map[string]any{
		"from":    string(view.State),
		"message": "The keeper interrupted the active turn. The same session can be resumed.",
	})
	if err != nil {
		return ActionResponse{}, err
	}
	response := ActionResponse{Task: next, Event: event}
	if err := s.rememberAction(ctx, request.IdempotencyKey, "interrupt", payload, response); err != nil {
		return ActionResponse{}, err
	}
	return response, nil
}

func interruptibleState(state workmodel.State) bool {
	switch state {
	case workmodel.Running, workmodel.AwaitingApproval, workmodel.AwaitingAuth:
		return true
	}
	return false
}
