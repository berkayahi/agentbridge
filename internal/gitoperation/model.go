// Package gitoperation defines explicit, idempotent local Git intents.
package gitoperation

import (
	"errors"
	"strings"
	"time"

	"github.com/berkayahi/agentbridge/internal/gitref"
)

const maxIDBytes = 128

var (
	ErrInvalidInput      = errors.New("gitoperation: invalid input")
	ErrInvalidTransition = errors.New("gitoperation: invalid state transition")
)

type Kind string

const (
	KindPush              Kind = "push"
	KindCreatePullRequest Kind = "create_pull_request"
	KindSubmitReview      Kind = "submit_review"
	KindMerge             Kind = "merge"
	KindResolveConflict   Kind = "resolve_conflict"
)

func (k Kind) Valid() bool {
	switch k {
	case KindPush, KindCreatePullRequest, KindSubmitReview, KindMerge, KindResolveConflict:
		return true
	default:
		return false
	}
}

type State string

const (
	StateQueued    State = "queued"
	StateRunning   State = "running"
	StateSucceeded State = "succeeded"
	StateFailed    State = "failed"
	StateCanceled  State = "canceled"
)

func (s State) Terminal() bool {
	return s == StateSucceeded || s == StateFailed || s == StateCanceled
}

// Operation keeps its Git target and idempotency intent immutable. Only its
// state may advance through the explicit lifecycle below.
type Operation struct {
	id             string
	executionID    string
	kind           Kind
	targetRef      string
	expectedOldSHA string
	idempotencyKey string
	state          State
	createdAt      time.Time
}

type NewInput struct {
	ID             string
	ExecutionID    string
	Kind           Kind
	TargetRef      string
	ExpectedOldSHA string
	IdempotencyKey string
	CreatedAt      time.Time
}

func New(input NewInput) (Operation, error) {
	if !validID(input.ID) || !validID(input.ExecutionID) || !input.Kind.Valid() || !validRef(input.TargetRef) ||
		!validGitObjectID(input.ExpectedOldSHA) || !validID(input.IdempotencyKey) || input.CreatedAt.IsZero() {
		return Operation{}, ErrInvalidInput
	}
	return Operation{
		id: input.ID, executionID: input.ExecutionID, kind: input.Kind, targetRef: input.TargetRef,
		expectedOldSHA: strings.ToLower(input.ExpectedOldSHA), idempotencyKey: input.IdempotencyKey,
		state: StateQueued, createdAt: input.CreatedAt.UTC(),
	}, nil
}

func (o Operation) ID() string             { return o.id }
func (o Operation) ExecutionID() string    { return o.executionID }
func (o Operation) Kind() Kind             { return o.kind }
func (o Operation) TargetRef() string      { return o.targetRef }
func (o Operation) ExpectedOldSHA() string { return o.expectedOldSHA }
func (o Operation) IdempotencyKey() string { return o.idempotencyKey }
func (o Operation) State() State           { return o.state }
func (o Operation) CreatedAt() time.Time   { return o.createdAt }

func (o Operation) Transition(to State) (Operation, error) {
	if !canTransition(o.state, to) {
		return Operation{}, ErrInvalidTransition
	}
	o.state = to
	return o, nil
}

func canTransition(from, to State) bool {
	return (from == StateQueued && (to == StateRunning || to == StateCanceled)) ||
		(from == StateRunning && (to == StateSucceeded || to == StateFailed || to == StateCanceled))
}

func validID(value string) bool {
	if value == "" || len(value) > maxIDBytes {
		return false
	}
	for _, runeValue := range value {
		if !((runeValue >= 'a' && runeValue <= 'z') ||
			(runeValue >= 'A' && runeValue <= 'Z') ||
			(runeValue >= '0' && runeValue <= '9') ||
			runeValue == '-' || runeValue == '_') {
			return false
		}
	}
	return true
}

func validGitObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, runeValue := range value {
		if !((runeValue >= 'a' && runeValue <= 'f') || (runeValue >= 'A' && runeValue <= 'F') || (runeValue >= '0' && runeValue <= '9')) {
			return false
		}
	}
	return true
}

func validRef(value string) bool { return gitref.Valid(value) }
