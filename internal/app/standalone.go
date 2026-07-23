package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/berkayahi/agentbridge/internal/provider"
	"github.com/berkayahi/agentbridge/internal/store"
	"github.com/berkayahi/agentbridge/internal/task"
)

// CreateTaskRequest is the transport-neutral input for standalone task creation.
type CreateTaskRequest struct {
	Provider task.Provider
	Prompt   string
}

// ApprovalDecisionRequest is the transport-neutral input for a provider-native
// approval decision.
type ApprovalDecisionRequest struct {
	TaskID     string
	ApprovalID string
	UserID     string
	Allow      bool
}

// CreateTask creates and schedules standalone work without requiring a
// Telegram update. Transport-specific projection remains an adapter concern.
func (a *App) CreateTask(ctx context.Context, request CreateTaskRequest) (string, error) {
	if err := a.requireStarted(); err != nil {
		return "", err
	}
	if !request.Provider.Valid() {
		return "", ErrUnknownProvider
	}
	if strings.TrimSpace(request.Prompt) == "" {
		return "", errors.New("app: task prompt is required")
	}
	value, err := a.createTaskRecord(ctx, request.Provider, request.Prompt, 0, false)
	if err != nil {
		return "", err
	}
	if err := a.enqueue(queuedTask{id: value.ID}); err != nil {
		return "", err
	}
	return value.ID, nil
}

// ContinueTask resumes an existing provider session without transport-owned
// chat or callback identifiers.
func (a *App) ContinueTask(ctx context.Context, id, input string) error {
	if err := a.requireStarted(); err != nil {
		return err
	}
	value, err := a.deps.Store.Task(ctx, id)
	if err != nil {
		return err
	}
	if value.ProviderSessionID == "" {
		return errors.New("app: task has no resumable provider session")
	}
	if !task.CanTransition(value.State, task.Running) {
		return errors.New("app: task is not resumable in its current state")
	}
	if !a.transition(ctx, &value, task.Running, "queued session continuation") {
		return errors.New("app: could not resume task")
	}
	return a.enqueue(queuedTask{id: id, resume: true, input: input})
}

// CancelTask durably cancels standalone work before interrupting its provider.
func (a *App) CancelTask(ctx context.Context, id string) error {
	if err := a.requireStarted(); err != nil {
		return err
	}
	return a.cancelTask(ctx, id)
}

// DecideApproval durably records a standalone approval decision before
// releasing the provider.
func (a *App) DecideApproval(ctx context.Context, request ApprovalDecisionRequest) error {
	if err := a.requireStarted(); err != nil {
		return err
	}
	if request.TaskID == "" || request.ApprovalID == "" || request.UserID == "" {
		return errors.New("app: incomplete approval decision")
	}
	pending, err := a.deps.Store.PendingApprovals(ctx)
	if err != nil {
		return err
	}
	var record task.Approval
	for _, value := range pending {
		if value.ID == request.ApprovalID && value.TaskID == request.TaskID {
			record = value
			break
		}
	}
	if record.ID == "" {
		return store.ErrNotFound
	}
	if a.deps.Approvals != nil && a.deps.Approvals.Owns(request.TaskID, request.ApprovalID) {
		if err := a.deps.Approvals.HandleDecision(
			ctx,
			request.TaskID,
			request.ApprovalID,
			request.UserID,
			request.Allow,
		); err != nil {
			return err
		}
		return nil
	}
	value, err := a.deps.Store.Task(ctx, request.TaskID)
	if err != nil {
		return err
	}
	a.mu.Lock()
	active, ok := a.active[value.ID]
	a.mu.Unlock()
	if !ok {
		return errors.New("app: provider session is not active")
	}
	requestID, err := provider.NewID(record.ID)
	if err != nil {
		return err
	}
	taskID, err := provider.NewID(value.ID)
	if err != nil {
		return err
	}
	decision := provider.ApprovalDecision{
		RequestID: requestID,
		TaskID:    taskID,
		UserID:    request.UserID,
		Allow:     request.Allow,
		DecidedAt: a.deps.Clock().UTC(),
	}
	status := task.ApprovalRejected
	if request.Allow {
		status = task.ApprovalApproved
	}
	if err := a.finishApproval(ctx, &record, status, request.Allow, ""); err != nil {
		return err
	}
	if err := a.appendApprovalDecisionEvent(ctx, value.ID, request.Allow); err != nil {
		record.Status = task.ApprovalPending
		record.ResolvedAt = nil
		record.DecisionPayload = nil
		return errors.Join(err, a.deps.Store.UpsertApproval(ctx, record))
	}
	if request.Allow {
		if !a.transition(ctx, &value, task.Running, "approval granted") {
			return store.ErrInvalidTransition
		}
	} else {
		a.fail(value, fmt.Errorf("operator %s rejected approval", request.UserID))
	}
	if err := active.provider.ResolveApproval(ctx, decision); err != nil {
		compensationErr := a.finishApproval(ctx, &record, task.ApprovalRejected, false, "provider_release_failed")
		a.fail(value, err)
		return errors.Join(err, compensationErr)
	}
	return nil
}

func (a *App) appendApprovalDecisionEvent(ctx context.Context, id string, allow bool) error {
	event := a.event(id, task.EventApprovalResolved, task.VisibilityUser, map[string]any{"approved": allow})
	if err := a.deps.Store.AppendEvent(ctx, event); err != nil {
		return err
	}
	_ = a.publish(ctx, event)
	return nil
}

func (a *App) requireStarted() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return ErrClosed
	}
	if !a.started {
		return ErrNotStarted
	}
	return nil
}
