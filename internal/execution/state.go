package execution

import (
	"errors"
	"time"
)

// State is the durable lifecycle of one device execution.
type State string

const (
	StateQueued           State = "queued"
	StateAccepted         State = "accepted"
	StateRunning          State = "running"
	StateAwaitingApproval State = "awaiting_approval"
	StateAwaitingAuth     State = "awaiting_auth"
	StateCancelRequested  State = "cancel_requested"
	StateCanceled         State = "canceled"
	StateCompleted        State = "completed"
	StateFailed           State = "failed"
)

// ParseState accepts only durable, explicitly modeled execution states.
func ParseState(value string) (State, error) {
	state := State(value)
	if !state.Valid() {
		return "", errors.New("execution: unknown state")
	}
	return state, nil
}

func (s State) Valid() bool {
	switch s {
	case StateQueued, StateAccepted, StateRunning, StateAwaitingApproval, StateAwaitingAuth,
		StateCancelRequested, StateCanceled, StateCompleted, StateFailed:
		return true
	default:
		return false
	}
}

func (s State) Terminal() bool {
	return s == StateCanceled || s == StateCompleted || s == StateFailed
}

// CanTransition is deliberately encoded here rather than exposed as mutable
// policy so callers cannot change execution semantics at runtime.
func CanTransition(from, to State) bool {
	switch from {
	case StateQueued:
		return to == StateAccepted || to == StateCancelRequested || to == StateCanceled || to == StateFailed
	case StateAccepted:
		return to == StateRunning || to == StateCancelRequested || to == StateCanceled || to == StateFailed
	case StateRunning:
		return to == StateAwaitingApproval || to == StateAwaitingAuth || to == StateCancelRequested || to == StateCompleted || to == StateFailed
	case StateAwaitingApproval, StateAwaitingAuth:
		return to == StateRunning || to == StateCancelRequested || to == StateCanceled || to == StateFailed
	case StateCancelRequested:
		return to == StateCanceled || to == StateCompleted || to == StateFailed
	default:
		return false
	}
}

// Transition returns a new execution state while preserving assignment and
// intent fields. Terminal states cannot return to running.
func (e Execution) Transition(to State, updatedAt time.Time) (Execution, error) {
	if !to.Valid() || updatedAt.IsZero() || updatedAt.Before(e.updatedAt) || !CanTransition(e.state, to) {
		return Execution{}, ErrInvalidTransition
	}
	e.state = to
	e.updatedAt = updatedAt.UTC()
	return e, nil
}
