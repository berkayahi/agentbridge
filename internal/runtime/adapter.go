package runtime

import (
	"context"
	"errors"
	"time"

	"github.com/berkayahi/agentbridge/internal/kernel"
)

var (
	ErrUnknownRuntime = errors.New("runtime: unknown runtime")
	ErrUnsupported    = errors.New("runtime: unsupported operation")
	ErrInvalidSession = errors.New("runtime: invalid session")
)

type Session struct {
	ID         string
	TaskID     string
	ExternalID string
	ThreadID   string
	RuntimeID  string
	Native     any
}

type StartRequest struct {
	TaskID, ExecutionID, WorkingDirectory, Model string
	Input                                        kernel.Input
}
type ResumeRequest struct {
	TaskID, ExecutionID string
	Session             Session
	Input               kernel.Input
}
type ApprovalDecision struct {
	RequestID, TaskID, ExecutionID, UserID string
	Allow                                  bool
}
type Usage struct {
	RuntimeID string
	Observed  time.Time
	Windows   []UsageWindow
}
type UsageWindow struct {
	Name        string
	UsedPercent float64
	ResetsAt    time.Time
}
type AuthStatus struct {
	Authenticated bool
	Account       string
	CheckedAt     time.Time
}

type Adapter interface {
	ID() string
	Detect(context.Context) (Installation, error)
	Capabilities(context.Context) (Capabilities, error)
	Start(context.Context, StartRequest, kernel.EventSink) (Session, error)
	Resume(context.Context, ResumeRequest, kernel.EventSink) (Session, error)
	Steer(context.Context, Session, kernel.Input) error
	Interrupt(context.Context, Session) error
	Close(context.Context, Session) error
	Fork(context.Context, StartRequest, kernel.EventSink) (Session, error)
	ResolveApproval(context.Context, ApprovalDecision) error
	Usage(context.Context) (Usage, error)
	AuthStatus(context.Context) (AuthStatus, error)
}
