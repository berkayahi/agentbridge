package localcontrol

import (
	"context"
	"fmt"
	"strings"

	"github.com/berkayahi/agentbridge/internal/store"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

// SteerRequest carries one more instruction to a bee already in flight.
type SteerRequest struct {
	TaskID         string `json:"task_id"`
	Revision       int64  `json:"revision"`
	Input          string `json:"input"`
	IdempotencyKey string `json:"idempotency_key"`
}

// Steerer is an executor that can reach a live provider session. Runtimes that
// cannot are refused rather than silently accepting an instruction that would
// never arrive.
type Steerer interface {
	Steer(ctx context.Context, view TaskView, request SteerRequest) error
}

const maxSteerInputBytes = 8 << 10

// Steer sends the keeper's instruction to a flying bee.
//
// The instruction is recorded as a durable event before the response returns,
// because the flight log has to explain why she changed course. Steering does
// not transition the task: she is still the same bee on the same task, now
// better informed.
func (s *Service) Steer(ctx context.Context, request SteerRequest) (ActionResponse, error) {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()

	input := strings.TrimSpace(request.Input)
	payload := struct {
		TaskID   string `json:"task_id"`
		Revision int64  `json:"revision"`
		Input    string `json:"input"`
	}{request.TaskID, request.Revision, input}
	var cached ActionResponse
	if done, err := s.replayAction(ctx, request.IdempotencyKey, "steer", payload, &cached); done || err != nil {
		return cached, err
	}
	if !validID(request.TaskID) || request.Revision <= 0 || input == "" || len(input) > maxSteerInputBytes {
		return ActionResponse{}, ErrInvalidRequest
	}
	view, err := s.taskView(ctx, request.TaskID)
	if err != nil {
		return ActionResponse{}, err
	}
	if err := checkRevision(view, request.Revision); err != nil {
		return ActionResponse{}, err
	}
	if !steerableState(view.State) {
		return ActionResponse{}, fmt.Errorf("task in state %s is not in flight: %w", view.State, store.ErrConflict)
	}
	steerer, ok := s.executor.(Steerer)
	if !ok {
		return ActionResponse{}, fmt.Errorf("runtime cannot be steered: %w", ErrNotConfigured)
	}
	if err := s.ensureTaskTarget(ctx, view); err != nil {
		return ActionResponse{}, err
	}
	request.Input = input
	if err := steerer.Steer(ctx, view, request); err != nil {
		return ActionResponse{}, err
	}
	now := s.clock().UTC()
	event, err := s.store.AppendLocalEvent(ctx, localEvent(s.newID("event"), "task", view.ID, view.ID, view.Revision, "steered",
		map[string]any{"input": input}, now))
	if err != nil {
		return ActionResponse{}, err
	}
	response := ActionResponse{Task: view, Event: event}
	if err := s.rememberAction(ctx, request.IdempotencyKey, "steer", payload, response); err != nil {
		return ActionResponse{}, err
	}
	return response, nil
}

// steerableState reports the states in which a provider session is live enough
// to receive an instruction.
func steerableState(state workmodel.State) bool {
	switch state {
	case workmodel.Running, workmodel.AwaitingApproval, workmodel.AwaitingAuth:
		return true
	}
	return false
}
