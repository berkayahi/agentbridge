// Package localcontrol defines the transport-neutral local Kovan authority.
// The package owns the command contract; HTTP, Desktop, Telegram, and future
// projections are clients of this contract and never open SQLite directly.
package localcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/berkayahi/agentbridge/internal/deviceidentity"
	"github.com/berkayahi/agentbridge/internal/kernel"
	"github.com/berkayahi/agentbridge/internal/runtime"
	"github.com/berkayahi/agentbridge/internal/store"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

var (
	ErrInvalidRequest               = errors.New("localcontrol: invalid request")
	ErrUnknownProvider              = errors.New("localcontrol: unknown provider")
	ErrStaleRevision                = errors.New("localcontrol: stale revision")
	ErrIdempotencyConflict          = errors.New("localcontrol: idempotency key conflict")
	ErrNotConfigured                = errors.New("localcontrol: operation is not configured")
	ErrTaskOwnedByAnotherController = errors.New("localcontrol: task is owned by another controller")
	ErrApprovalNotPending           = errors.New("localcontrol: approval is not pending")
	ErrVerificationRequired         = errors.New("localcontrol: verification is required")
	ErrCommitRequired               = errors.New("localcontrol: commit is required")
)

type Project struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Revision  int64     `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Repository struct {
	ID        string    `json:"id"`
	Remote    string    `json:"remote,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type Board struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	Name      string    `json:"name"`
	Revision  int64     `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TaskView deliberately omits filesystem paths and presentation identifiers.
// Those values are resolved inside AgentBridge from the approved repository
// binding and are never selected by a local client.
type TaskView struct {
	ID               string             `json:"id"`
	ProjectID        string             `json:"project_id"`
	BoardID          string             `json:"board_id"`
	RepositoryID     string             `json:"repository_id"`
	RepositoryRemote string             `json:"repository_remote,omitempty"`
	TargetDeviceID   string             `json:"target_device_id"`
	TargetEpoch      uint64             `json:"target_epoch"`
	Title            string             `json:"title"`
	Prompt           string             `json:"prompt"`
	Provider         workmodel.Provider `json:"provider"`
	State            workmodel.State    `json:"state"`
	Revision         int64              `json:"revision"`
	ExecutionID      string             `json:"execution_id,omitempty"`
	SessionID        string             `json:"session_id,omitempty"`
	RuntimeID        string             `json:"runtime_id,omitempty"`
	CommitSHA        string             `json:"commit_sha,omitempty"`
	PushRef          string             `json:"push_ref,omitempty"`
	CreatedAt        time.Time          `json:"created_at"`
	UpdatedAt        time.Time          `json:"updated_at"`
}

type Event struct {
	Cursor uint64 `json:"cursor"`
	// TaskCursor is contiguous for one task. Cursor remains the global local
	// event cursor used by the authenticated API's after_cursor query.
	TaskCursor   uint64          `json:"task_cursor,omitempty"`
	ID           string          `json:"id"`
	ResourceType string          `json:"resource_type"`
	ResourceID   string          `json:"resource_id"`
	TaskID       string          `json:"task_id,omitempty"`
	Revision     int64           `json:"revision"`
	Type         string          `json:"type"`
	Payload      json.RawMessage `json:"payload"`
	CreatedAt    time.Time       `json:"created_at"`
}

type ExecutionInfo struct {
	ExecutionID  string
	SessionID    string
	RuntimeID    string
	RepositoryID string
	FencingEpoch uint64
	Policy       []byte
}

type IdempotencyRecord struct {
	Key           string
	Operation     string
	RequestHash   string
	ResponseBytes json.RawMessage
	CreatedAt     time.Time
}

type VerificationReceipt struct {
	ID         string    `json:"id"`
	Passed     bool      `json:"passed"`
	Summary    string    `json:"summary,omitempty"`
	ObservedAt time.Time `json:"observed_at"`
}

type CommitReceipt struct {
	ID         string    `json:"id"`
	CommitSHA  string    `json:"commit_sha"`
	RemoteRef  string    `json:"remote_ref"`
	ObservedAt time.Time `json:"observed_at"`
}

type CreateProjectRequest struct {
	Name           string `json:"name"`
	IdempotencyKey string `json:"idempotency_key"`
}

type RegisterRepositoryRequest struct {
	Remote         string `json:"remote"`
	IdempotencyKey string `json:"idempotency_key"`
}

type CreateBoardRequest struct {
	ProjectID      string `json:"project_id"`
	Name           string `json:"name"`
	IdempotencyKey string `json:"idempotency_key"`
}

type CreateTaskRequest struct {
	ProjectID      string             `json:"project_id"`
	BoardID        string             `json:"board_id"`
	RepositoryID   string             `json:"repository_id"`
	TargetDeviceID string             `json:"target_device_id,omitempty"`
	Provider       workmodel.Provider `json:"provider"`
	Title          string             `json:"title"`
	Prompt         string             `json:"prompt"`
	IdempotencyKey string             `json:"idempotency_key"`
}

type UpdateTaskRequest struct {
	TaskID         string `json:"task_id"`
	Revision       int64  `json:"revision"`
	Title          string `json:"title"`
	Prompt         string `json:"prompt"`
	IdempotencyKey string `json:"idempotency_key"`
}

type StartRequest struct {
	TaskID         string `json:"task_id"`
	Revision       int64  `json:"revision"`
	Input          string `json:"input,omitempty"`
	Model          string `json:"model,omitempty"`
	PolicySnapshot []byte `json:"policy_snapshot,omitempty"`
	IdempotencyKey string `json:"idempotency_key"`
}

type ResumeRequest struct {
	TaskID         string `json:"task_id"`
	Revision       int64  `json:"revision"`
	Input          string `json:"input,omitempty"`
	IdempotencyKey string `json:"idempotency_key"`
}

type ObserveRequest struct {
	TaskID      string
	AfterCursor uint64
	Limit       int
}

type ApproveRequest struct {
	TaskID         string `json:"task_id"`
	ApprovalID     string `json:"approval_id"`
	Revision       int64  `json:"revision"`
	UserID         string `json:"user_id"`
	Allow          bool   `json:"allow"`
	IdempotencyKey string `json:"idempotency_key"`
}

// LocalAuthorityUserID is the stable approval principal for the authenticated
// local API and controller-to-device link. Provider adapters may translate it
// to their configured native identity before resolving an approval.
const LocalAuthorityUserID = "local-authority"

type CancelRequest struct {
	TaskID         string `json:"task_id"`
	Revision       int64  `json:"revision"`
	IdempotencyKey string `json:"idempotency_key"`
}

type VerifyRequest struct {
	TaskID         string `json:"task_id"`
	Revision       int64  `json:"revision"`
	IdempotencyKey string `json:"idempotency_key"`
}

type CommitRequest struct {
	TaskID         string `json:"task_id"`
	Revision       int64  `json:"revision"`
	IdempotencyKey string `json:"idempotency_key"`
}

type ProjectResponse struct {
	Project Project `json:"project"`
}
type RepositoryResponse struct {
	Repository Repository `json:"repository"`
}
type BoardResponse struct {
	Board Board `json:"board"`
}
type TaskResponse struct {
	Task TaskView `json:"task"`
}
type ApprovalView struct {
	ID             string          `json:"id"`
	TaskID         string          `json:"task_id"`
	Kind           string          `json:"kind"`
	Status         string          `json:"status"`
	RequestPayload json.RawMessage `json:"request_payload,omitempty"`
	RequestedAt    time.Time       `json:"requested_at"`
	ExpiresAt      *time.Time      `json:"expires_at,omitempty"`
}
type ApprovalsResponse struct {
	Approvals []ApprovalView `json:"approvals"`
}
type ActionResponse struct {
	Task   TaskView `json:"task"`
	Event  Event    `json:"event"`
	Queued bool     `json:"queued,omitempty"`
}
type ObserveResponse struct {
	Task       TaskView `json:"task"`
	Events     []Event  `json:"events"`
	NextCursor uint64   `json:"next_cursor,omitempty"`
}

// DeviceEvent is provider/runtime evidence observed on a paired execution
// device. Its cursor is scoped to the device response; the controller assigns
// the durable local-control cursor when it ingests the event.
type DeviceEvent struct {
	Cursor    uint64          `json:"cursor"`
	ID        string          `json:"id"`
	TaskID    string          `json:"task_id"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// DeviceObservation is the read-only, typed observation boundary from a
// paired device back to the controller. Approvals are task-scoped provider
// records, not presentation-generated placeholders.
type DeviceObservation struct {
	Cursor    uint64         `json:"cursor"`
	Events    []DeviceEvent  `json:"events"`
	Approvals []ApprovalView `json:"approvals"`
}

type DeviceObserver interface {
	Observe(context.Context, TaskView, uint64) (DeviceObservation, error)
}
type VerifyResponse struct {
	Task    TaskView            `json:"task"`
	Receipt VerificationReceipt `json:"receipt"`
	Event   Event               `json:"event"`
	Queued  bool                `json:"queued,omitempty"`
}
type CommitResponse struct {
	Task    TaskView      `json:"task"`
	Receipt CommitReceipt `json:"receipt"`
	Event   Event         `json:"event"`
	Queued  bool          `json:"queued,omitempty"`
}

// AuthorityStore is implemented by sqlite.RuntimeStore. The interface lives
// at the controller boundary so tests can use a deterministic fake without
// opening a second datastore or teaching a client SQLite details.
type AuthorityStore interface {
	store.RuntimeStore
	DeviceAuthority
	EnsureRepositoryBinding(context.Context, string, string) error
	CreateProject(context.Context, Project) error
	GetProject(context.Context, string) (Project, error)
	CreateRepository(context.Context, Repository) error
	GetRepository(context.Context, string) (Repository, error)
	CreateBoard(context.Context, Board) error
	GetBoard(context.Context, string) (Board, error)
	CreateTaskInContext(context.Context, string, string, string, workmodel.Task, workmodel.Event, Event) (Event, error)
	UpdateTaskAtRevision(context.Context, string, int64, string, string, Event) (Event, error)
	TaskContext(context.Context, string) (string, string, error)
	ExecutionInfo(context.Context, string) (ExecutionInfo, error)
	TransitionAtRevision(context.Context, string, int64, workmodel.State, workmodel.Event, Event) (Event, error)
	AppendLocalEvent(context.Context, Event) (Event, error)
	ListLocalEvents(context.Context, string, uint64, int) ([]Event, error)
	GetApproval(context.Context, string) (workmodel.Approval, error)
	LoadIdempotency(context.Context, string) (IdempotencyRecord, error)
	SaveIdempotency(context.Context, IdempotencyRecord) error
	RecordCheckpoint(context.Context, string, CommitReceipt) error
	LoadCheckpoint(context.Context, string) (CommitReceipt, error)
}

type RuntimeCatalog interface {
	Get(string) (runtime.Adapter, error)
}

type CommandController interface {
	Start(context.Context, kernel.StartExecution) error
	Cancel(context.Context, kernel.CancelExecution) error
}

type Executor interface {
	Start(context.Context, TaskView, StartRequest) error
	Resume(context.Context, TaskView, ResumeRequest) error
	Approve(context.Context, TaskView, string, string, bool) error
	Cancel(context.Context, TaskView) error
}

type Verifier interface {
	Verify(context.Context, TaskView) (VerificationReceipt, error)
}

type Committer interface {
	Commit(context.Context, TaskView) (CommitReceipt, error)
}

// DeviceRuntime is the typed execution boundary for a paired device. A
// remote implementation may use the device protocol or another authenticated
// transport, but it receives TaskView and receipts rather than paths or raw
// provider commands.
type DeviceRuntime interface {
	Executor
	Verifier
	Committer
}

// RemoteDeviceFactory creates a short-lived authenticated runtime for the
// task's currently fenced target. The factory is evaluated after the local
// controller validates device state and assignment epoch, so a newly paired
// or reconnected device cannot silently inherit an old task link.
type RemoteDeviceFactory func(context.Context, TaskView) (DeviceRuntime, error)

type Config struct {
	Store               AuthorityStore
	Identity            deviceidentity.Key
	Runtimes            RuntimeCatalog
	Controller          CommandController
	Executor            Executor
	Verifier            Verifier
	Committer           Committer
	RemoteDevices       map[string]DeviceRuntime
	RemoteDeviceFactory RemoteDeviceFactory
	Clock               func() time.Time
	NewID               func(string) string
}
