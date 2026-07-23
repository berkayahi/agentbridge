package store

import (
	"context"
	"errors"
	"time"

	"github.com/berkayahi/agentbridge/internal/events"
	"github.com/berkayahi/agentbridge/internal/execution"
	"github.com/berkayahi/agentbridge/internal/gitoperation"
	"github.com/berkayahi/agentbridge/internal/intent"
	"github.com/berkayahi/agentbridge/internal/localtask"
	"github.com/berkayahi/agentbridge/internal/repository"
	"github.com/berkayahi/agentbridge/internal/session"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

var (
	ErrNotFound          = errors.New("store: not found")
	ErrConflict          = errors.New("store: conflict")
	ErrInvalidTransition = errors.New("store: invalid transition")
	ErrDuplicateEvent    = errors.New("store: duplicate event")
)

type ListFilter struct {
	RepoProfileID string
	States        []workmodel.State
	Limit         int
}

type Lease struct {
	RepoProfileID string
	OwnerID       string
	AcquiredAt    time.Time
	HeartbeatAt   time.Time
	ExpiresAt     time.Time
}

type Store interface {
	CreateTask(context.Context, workmodel.Task, workmodel.Event) error
	Transition(context.Context, string, workmodel.State, workmodel.Event) error
	AppendEvent(context.Context, workmodel.Event) error
	Events(context.Context, string) ([]workmodel.Event, error)
	Task(context.Context, string) (workmodel.Task, error)
	ListTasks(context.Context, ListFilter) ([]workmodel.Task, error)
	NonterminalTasks(context.Context) ([]workmodel.Task, error)
	SaveWorkspace(context.Context, string, string, string) error
	SaveTelegramMessage(context.Context, string, int64) error
	SaveProviderSession(context.Context, string, workmodel.Session) error
	SaveDelivery(context.Context, string, string, string, string) error
	SaveFailure(context.Context, string, string) error
	SaveAttachment(context.Context, workmodel.Attachment) error
	Attachments(context.Context, string) ([]workmodel.Attachment, error)
	UpsertSession(context.Context, workmodel.Session) error
	ResumableSessions(context.Context) ([]workmodel.Session, error)
	UpsertApproval(context.Context, workmodel.Approval) error
	PendingApprovals(context.Context) ([]workmodel.Approval, error)
	UpsertAuthIncident(context.Context, workmodel.AuthIncident) error
	OpenAuthIncident(context.Context, workmodel.Provider) (workmodel.AuthIncident, error)
	AcquireLease(context.Context, string, string, time.Duration) (bool, error)
	HeartbeatLease(context.Context, string, string, time.Duration) error
	ReleaseLease(context.Context, string, string) error
	ExpiredLeases(context.Context) ([]Lease, error)
}

// Repositories groups the narrow 2.0 repositories that share one transaction.
// It is deliberately separate from the legacy Store interface so the old
// daemon can remain source-compatible until the atomic v2 activation.
type Repositories struct {
	LocalTasks    localtask.Repository
	Executions    execution.Repository
	Sessions      session.Repository
	Repositories  repository.Repository
	GitOperations gitoperation.Repository
	Events        events.Repository
	Intents       intent.Repository
}

// UnitOfWork is the transaction boundary for v2 state plus durable events.
type UnitOfWork interface {
	Within(context.Context, func(Repositories) error) error
}
