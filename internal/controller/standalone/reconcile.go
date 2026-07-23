package standalone

import (
	"context"
	"errors"
	"fmt"

	"github.com/berkayahi/agentbridge/internal/workmodel"
)

// Reconcile converts durable restart evidence into safe queue decisions. It
// never guesses through commit/push boundaries or adopts an unknown process.
func (a *App) Reconcile(ctx context.Context) error {
	leases, err := a.deps.Store.ExpiredLeases(ctx)
	if err != nil {
		return fmt.Errorf("load expired repository leases: %w", err)
	}
	for _, lease := range leases {
		_ = a.deps.Store.ReleaseLease(ctx, lease.RepoProfileID, lease.OwnerID)
	}
	sessions, err := a.deps.Store.ResumableSessions(ctx)
	if err != nil {
		return fmt.Errorf("load resumable sessions: %w", err)
	}
	byTask := make(map[string]workmodel.Session, len(sessions))
	for _, session := range sessions {
		byTask[session.TaskID] = session
	}
	values, err := a.deps.Store.NonterminalTasks(ctx)
	if err != nil {
		return fmt.Errorf("load nonterminal tasks: %w", err)
	}
	for _, value := range values {
		switch value.State {
		case workmodel.Queued:
			if err := a.enqueue(queuedTask{id: value.ID}); err != nil {
				return err
			}
		case workmodel.Preparing:
			if value.WorktreePath == "" && value.BaseSHA == "" {
				if err := a.enqueue(queuedTask{id: value.ID}); err != nil {
					return err
				}
				continue
			}
			a.pause(value, "partial workspace preparation requires manual review")
		case workmodel.Running:
			inspection, inspectErr := a.deps.Workspace.Inspect(ctx, value)
			session, hasSession := byTask[value.ID]
			sessionMatches := hasSession && session.Provider == value.Provider && session.ProviderSessionID == value.ProviderSessionID && session.ProviderSessionID != ""
			if inspectErr != nil || !inspection.Exists || !inspection.BaseMatches || inspection.ProcessRunning || !sessionMatches {
				a.pause(value, "restart invariants changed; manual review required")
				continue
			}
			if err := a.enqueue(queuedTask{id: value.ID, resume: true}); err != nil {
				return err
			}
		case workmodel.Verifying:
			inspection, inspectErr := a.deps.Workspace.Inspect(ctx, value)
			if inspectErr != nil || !inspection.Exists || !inspection.BaseMatches || inspection.ProcessRunning {
				a.pause(value, "verification workspace changed; manual review required")
				continue
			}
			if err := a.enqueue(queuedTask{id: value.ID}); err != nil {
				return err
			}
		case workmodel.AwaitingApproval, workmodel.Committing, workmodel.Pushing:
			a.pause(value, "interrupted operation cannot be resumed automatically")
		case workmodel.AwaitingAuth, workmodel.Failed, workmodel.Paused:
			// Operator/auth recovery owns these states.
		}
	}
	return nil
}

// ValidateResume implements the authentication recovery safety gate.
func (a *App) ValidateResume(ctx context.Context, value workmodel.Task) error {
	if value.State != workmodel.AwaitingAuth {
		return errors.New("app: task is not awaiting authentication")
	}
	return a.validateResumeEvidence(ctx, value)
}

func (a *App) validateResumeEvidence(ctx context.Context, value workmodel.Task) error {
	if value.WorktreePath == "" || value.BaseSHA == "" || value.ProviderSessionID == "" {
		return errors.New("app: incomplete resumable task")
	}
	inspection, err := a.deps.Workspace.Inspect(ctx, value)
	if err != nil {
		return err
	}
	if !inspection.Exists || !inspection.BaseMatches || inspection.ProcessRunning {
		return errors.New("app: workspace cannot be resumed safely")
	}
	sessions, err := a.deps.Store.ResumableSessions(ctx)
	if err != nil {
		return err
	}
	for _, session := range sessions {
		if session.TaskID == value.ID && session.Provider == value.Provider && session.ProviderSessionID == value.ProviderSessionID {
			return nil
		}
	}
	return errors.New("app: resumable provider session not found")
}

// ResumeTask schedules provider resume after auth.Service durably transitions
// AwaitingAuth back to Running. The goroutine is owned by App shutdown.
func (a *App) ResumeTask(ctx context.Context, value workmodel.Task) error {
	if value.State != workmodel.Running {
		return errors.New("app: recovered task is not running")
	}
	if err := a.validateResumeEvidence(ctx, value); err != nil {
		return err
	}
	a.mu.Lock()
	if a.closed || !a.started {
		a.mu.Unlock()
		return ErrClosed
	}
	a.mu.Unlock()
	return a.enqueue(queuedTask{id: value.ID, resume: true})
}
