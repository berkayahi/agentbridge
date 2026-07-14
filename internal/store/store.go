package store

import (
	"context"
	"errors"
	"time"

	"github.com/berkayahi/agentbridge/internal/task"
)

var (
	ErrNotFound          = errors.New("store: not found")
	ErrConflict          = errors.New("store: conflict")
	ErrInvalidTransition = errors.New("store: invalid transition")
	ErrDuplicateEvent    = errors.New("store: duplicate event")
)

type ListFilter struct {
	RepoProfileID string
	States        []task.State
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
	CreateTask(context.Context, task.Task, task.Event) error
	Transition(context.Context, string, task.State, task.Event) error
	AppendEvent(context.Context, task.Event) error
	Events(context.Context, string) ([]task.Event, error)
	Task(context.Context, string) (task.Task, error)
	ListTasks(context.Context, ListFilter) ([]task.Task, error)
	NonterminalTasks(context.Context) ([]task.Task, error)
	SaveAttachment(context.Context, task.Attachment) error
	Attachments(context.Context, string) ([]task.Attachment, error)
	UpsertSession(context.Context, task.Session) error
	ResumableSessions(context.Context) ([]task.Session, error)
	UpsertApproval(context.Context, task.Approval) error
	PendingApprovals(context.Context) ([]task.Approval, error)
	UpsertAuthIncident(context.Context, task.AuthIncident) error
	OpenAuthIncident(context.Context, task.Provider) (task.AuthIncident, error)
	AcquireLease(context.Context, string, string, time.Duration) (bool, error)
	HeartbeatLease(context.Context, string, string, time.Duration) error
	ReleaseLease(context.Context, string, string) error
	ExpiredLeases(context.Context) ([]Lease, error)
}
