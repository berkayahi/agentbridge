// Package execution defines one immutable assignment of local work.
package execution

import (
	"errors"
	"time"
)

const maxIDBytes = 128

var (
	ErrInvalidInput       = errors.New("execution: invalid input")
	ErrInvalidTransition  = errors.New("execution: invalid state transition")
	ErrUnsafeRetry        = errors.New("execution: transient retry is unsafe")
	ErrNotTerminalFailure = errors.New("execution: retry successor requires terminal failure")
)

// Execution is one task assignment to exactly one session, runtime, repository
// binding, and immutable policy snapshot.
type Execution struct {
	id                   string
	localTaskID          string
	sessionID            string
	runtimeID            string
	repositoryID         string
	retryOfExecutionID   string
	state                State
	attempt              uint32
	fencingEpoch         uint64
	commandID            string
	maxTransientAttempts uint32
	policySnapshot       []byte
	createdAt            time.Time
	updatedAt            time.Time
}

// NewInput supplies the immutable assignment boundary for a new execution.
type NewInput struct {
	ID                   string
	LocalTaskID          string
	SessionID            string
	RuntimeID            string
	RepositoryID         string
	CommandID            string
	FencingEpoch         uint64
	MaxTransientAttempts uint32
	PolicySnapshot       []byte
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

func New(input NewInput) (Execution, error) {
	if !validID(input.ID) || !validID(input.LocalTaskID) || !validID(input.SessionID) ||
		!validID(input.RuntimeID) || !validID(input.RepositoryID) || !validID(input.CommandID) || input.FencingEpoch == 0 || len(input.PolicySnapshot) == 0 ||
		input.CreatedAt.IsZero() || input.UpdatedAt.IsZero() || input.UpdatedAt.Before(input.CreatedAt) {
		return Execution{}, ErrInvalidInput
	}
	return Execution{
		id:                   input.ID,
		localTaskID:          input.LocalTaskID,
		sessionID:            input.SessionID,
		runtimeID:            input.RuntimeID,
		repositoryID:         input.RepositoryID,
		state:                StateQueued,
		fencingEpoch:         input.FencingEpoch,
		commandID:            input.CommandID,
		maxTransientAttempts: input.MaxTransientAttempts,
		policySnapshot:       append([]byte(nil), input.PolicySnapshot...),
		createdAt:            input.CreatedAt.UTC(),
		updatedAt:            input.UpdatedAt.UTC(),
	}, nil
}

func (e Execution) ID() string                   { return e.id }
func (e Execution) LocalTaskID() string          { return e.localTaskID }
func (e Execution) SessionID() string            { return e.sessionID }
func (e Execution) RuntimeID() string            { return e.runtimeID }
func (e Execution) RepositoryID() string         { return e.repositoryID }
func (e Execution) RetryOfExecutionID() string   { return e.retryOfExecutionID }
func (e Execution) State() State                 { return e.state }
func (e Execution) Attempt() uint32              { return e.attempt }
func (e Execution) FencingEpoch() uint64         { return e.fencingEpoch }
func (e Execution) CommandID() string            { return e.commandID }
func (e Execution) MaxTransientAttempts() uint32 { return e.maxTransientAttempts }
func (e Execution) CreatedAt() time.Time         { return e.createdAt }
func (e Execution) UpdatedAt() time.Time         { return e.updatedAt }

// PolicySnapshot returns a defensive copy of the pinned policy bytes.
func (e Execution) PolicySnapshot() []byte { return append([]byte(nil), e.policySnapshot...) }

// RetryBasis identifies the durable evidence classification that permits a
// bounded retry. Later command-inbox work records the referenced evidence.
type RetryBasis string

const (
	RetryBasisNoUncertainExternalEffect    RetryBasis = "no_uncertain_external_effect"
	RetryBasisProviderIdempotentReconciled RetryBasis = "provider_idempotent_reconciled"
)

func (b RetryBasis) Valid() bool {
	return b == RetryBasisNoUncertainExternalEffect || b == RetryBasisProviderIdempotentReconciled
}

// TransientRetryCommand is a fresh, evidence-bound command for one in-place
// retry. Its identity and fence replace the prior command on success.
type TransientRetryCommand struct {
	ID           string
	EvidenceID   string
	Basis        RetryBasis
	FencingEpoch uint64
	IssuedAt     time.Time
}

// TransientRetry keeps a bounded retry within the same execution only when it
// is safe and uses a newer fence. Explicit workflow retries use Successor.
func (e Execution) TransientRetry(command TransientRetryCommand) (Execution, error) {
	if e.state != StateRunning || !validID(command.ID) || command.ID == e.commandID || !validID(command.EvidenceID) ||
		!command.Basis.Valid() || command.FencingEpoch <= e.fencingEpoch || command.IssuedAt.IsZero() || !command.IssuedAt.After(e.updatedAt) ||
		e.attempt >= e.maxTransientAttempts {
		return Execution{}, ErrUnsafeRetry
	}
	e.attempt++
	e.fencingEpoch = command.FencingEpoch
	e.commandID = command.ID
	e.updatedAt = command.IssuedAt.UTC()
	return e, nil
}

// Successor creates a fresh queued execution after a terminal failure, keeping
// the retry lineage explicit instead of mutating a completed attempt history.
func (e Execution) Successor(id, commandID string, fencingEpoch uint64, createdAt time.Time) (Execution, error) {
	if e.state != StateFailed {
		return Execution{}, ErrNotTerminalFailure
	}
	if !validID(id) || !validID(commandID) || commandID == e.commandID || fencingEpoch <= e.fencingEpoch || createdAt.IsZero() || createdAt.Before(e.updatedAt) {
		return Execution{}, ErrInvalidInput
	}
	return Execution{
		id:                   id,
		localTaskID:          e.localTaskID,
		sessionID:            e.sessionID,
		runtimeID:            e.runtimeID,
		repositoryID:         e.repositoryID,
		retryOfExecutionID:   e.id,
		state:                StateQueued,
		fencingEpoch:         fencingEpoch,
		commandID:            commandID,
		maxTransientAttempts: e.maxTransientAttempts,
		policySnapshot:       append([]byte(nil), e.policySnapshot...),
		createdAt:            createdAt.UTC(),
		updatedAt:            createdAt.UTC(),
	}, nil
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
