// Package intent defines durable, fenced side-effect intent.
package intent

import (
	"errors"
	"strings"
	"time"
)

var (
	ErrInvalidInput    = errors.New("intent: invalid input")
	ErrAlreadyClaimed  = errors.New("intent: already claimed")
	ErrStaleClaim      = errors.New("intent: stale claimant")
	ErrNotPending      = errors.New("intent: not pending")
	ErrExpired         = errors.New("intent: expired")
	ErrAlreadyComplete = errors.New("intent: already completed")
)

type State string

const (
	StatePending              State = "pending"
	StateClaimed              State = "claimed"
	StateCompleted            State = "completed"
	StateFailed               State = "failed"
	StateReconciliationNeeded State = "reconciliation_required"
	StateCanceled             State = "canceled"
)

func (s State) Valid() bool {
	return s == StatePending || s == StateClaimed || s == StateCompleted || s == StateFailed || s == StateReconciliationNeeded || s == StateCanceled
}

type Intent struct {
	ID             string
	ExecutionID    string
	Kind           string
	RuntimeID      string
	TargetTaskID   string
	ResultTaskID   string
	PayloadRef     string
	State          State
	ClaimOwner     string
	SafeProgress   string
	SafeResult     string
	CreatedAt      time.Time
	ExpiresAt      time.Time
	ClaimedAt      *time.Time
	LeaseExpiresAt *time.Time
	CompletedAt    *time.Time
}

type NewInput struct {
	ID, ExecutionID, Kind, RuntimeID, TargetTaskID, PayloadRef string
	CreatedAt, ExpiresAt                                       time.Time
}

func New(input NewInput) (Intent, error) {
	if !validID(input.ID) || !validID(input.Kind) || !validID(input.RuntimeID) || input.ExecutionID != "" && !validID(input.ExecutionID) ||
		input.TargetTaskID != "" && !validID(input.TargetTaskID) ||
		input.CreatedAt.IsZero() || input.ExpiresAt.IsZero() || !input.ExpiresAt.After(input.CreatedAt) {
		return Intent{}, ErrInvalidInput
	}
	return Intent{ID: input.ID, ExecutionID: input.ExecutionID, Kind: input.Kind, RuntimeID: input.RuntimeID, TargetTaskID: input.TargetTaskID, PayloadRef: input.PayloadRef, State: StatePending, CreatedAt: input.CreatedAt.UTC(), ExpiresAt: input.ExpiresAt.UTC()}, nil
}

func Restore(value Intent) (Intent, error) {
	if !validID(value.ID) || !validID(value.Kind) || !validID(value.RuntimeID) || value.ExecutionID != "" && !validID(value.ExecutionID) ||
		value.TargetTaskID != "" && !validID(value.TargetTaskID) || !value.State.Valid() || value.CreatedAt.IsZero() || value.ExpiresAt.IsZero() {
		return Intent{}, ErrInvalidInput
	}
	return value, nil
}

func (i Intent) Claim(owner string, now time.Time, lease time.Duration) (Intent, error) {
	if strings.TrimSpace(owner) == "" || now.IsZero() || lease <= 0 {
		return Intent{}, ErrInvalidInput
	}
	if !now.Before(i.ExpiresAt) {
		return Intent{}, ErrExpired
	}
	if i.State != StatePending && !(i.State == StateClaimed && i.LeaseExpiresAt != nil && !now.Before(*i.LeaseExpiresAt)) {
		return Intent{}, ErrAlreadyClaimed
	}
	claimed := now.UTC()
	expires := claimed.Add(lease)
	i.State, i.ClaimOwner, i.ClaimedAt, i.LeaseExpiresAt = StateClaimed, owner, &claimed, &expires
	return i, nil
}

func (i Intent) Complete(owner, result string, now time.Time) (Intent, error) {
	if i.State != StateClaimed {
		if i.State == StateCompleted {
			return Intent{}, ErrAlreadyComplete
		}
		return Intent{}, ErrStaleClaim
	}
	if i.ClaimOwner != owner || i.LeaseExpiresAt == nil || !now.Before(*i.LeaseExpiresAt) {
		return Intent{}, ErrStaleClaim
	}
	completed := now.UTC()
	i.State, i.SafeResult, i.CompletedAt = StateCompleted, result, &completed
	return i, nil
}

func (i Intent) Reconcile(owner, progress string, now time.Time) (Intent, error) {
	if i.State != StateClaimed || i.ClaimOwner != owner || i.LeaseExpiresAt == nil || !now.Before(*i.LeaseExpiresAt) {
		return Intent{}, ErrStaleClaim
	}
	i.State, i.SafeProgress = StateReconciliationNeeded, progress
	return i, nil
}

func validID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}
