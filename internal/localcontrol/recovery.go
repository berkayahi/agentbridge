package localcontrol

import (
	"context"

	"github.com/berkayahi/agentbridge/internal/store"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

// strandedStates are the states in which a locally executed task depended on a
// live provider session. That session dies with the daemon, so a task in any of
// these is no longer being worked on by anybody.
//
// Verifying, committing and pushing are deliberately excluded. By then the bee
// is already home: her worktree, her verification receipt and any commit
// checkpoint are durable, and the keeper's tap is still valid. Pausing her would
// throw away a finished flight and force the work to be done twice.
var strandedStates = []workmodel.State{
	workmodel.Preparing, workmodel.Running,
	workmodel.AwaitingApproval, workmodel.AwaitingAuth,
}

// RecoverLocalTasks pauses locally executed tasks that a restart stranded.
//
// Their provider session lived in the previous process, so leaving them
// `running` would report work that nobody is doing — the hive would be lying
// about a bee still being in the air. Pausing is recoverable: the keeper can
// resume, and the worktree, the durable events and any verification receipt are
// all still there.
//
// Paired devices are deliberately untouched: their evidence returns through
// observation and replay, and a Pi keeps working while the controller is down.
func (s *Service) RecoverLocalTasks(ctx context.Context) error {
	if s == nil || s.store == nil {
		return ErrNotConfigured
	}
	authority, err := s.listing()
	if err != nil {
		// An authority that cannot list cannot recover. That is a composition
		// problem, not a task problem.
		return err
	}
	tasks, err := authority.ListTasks(ctx, store.ListFilter{
		ControllerOwner: workmodel.TaskControllerLocal,
		States:          strandedStates,
		TargetDeviceID:  LocalDeviceID,
		Limit:           1000,
	})
	if err != nil {
		return err
	}
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	for _, task := range tasks {
		view, err := s.taskView(ctx, task.ID)
		if err != nil {
			continue
		}
		if !workmodel.CanTransition(view.State, workmodel.Paused) {
			continue
		}
		// Recorded before anything else so the flight log explains the gap
		// rather than showing a bee that simply stopped.
		_, _, _ = s.transition(ctx, view, workmodel.Paused, "local_session_lost", map[string]any{
			"message": "the hive restarted while she was working; her provider session did not survive",
			"from":    string(view.State),
		})
	}
	return nil
}
