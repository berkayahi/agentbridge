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

// APIVersion is the local control contract version. It increases when a client
// would need to change: a host compares it before trusting this API, so an
// engine that is too old fails a version gate instead of failing mid-flight.
const APIVersion = 1

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
	ErrRepositoryNotConfigured      = errors.New("localcontrol: repository remote is not configured")
	ErrRepositoryAmbiguous          = errors.New("localcontrol: repository remote maps to multiple configured profiles")
	ErrDeliveryNotEnabled           = errors.New("localcontrol: repository delivery is not enabled")
)

// RepositoryProfile is a configured, executable repository binding. The
// executor resolves work against these ids, so the authority must never hand
// out any other id: a task bound to an unresolvable repository cannot start.
type RepositoryProfile struct {
	ID      string `json:"id"`
	Remote  string `json:"remote,omitempty"`
	BaseRef string `json:"base_ref,omitempty"`
	// CheckoutPath is the control checkout this profile resolves to. It is
	// reported because a client that cannot locate a repository on disk cannot
	// show anything the repository itself holds — the hive's own memory files
	// being the case that prompted this. It is a loopback-only API on the
	// keeper's own machine describing the keeper's own paths, and it grants no
	// authority: work is still resolved against the configured id.
	CheckoutPath string `json:"checkout_path,omitempty"`
}

// RepositoryCatalog reports the configured repository profiles. It is the one
// source of truth shared by the authority and the executor; the authority uses
// it to resolve registrations and the executor uses it to prepare worktrees.
type RepositoryCatalog interface {
	RepositoryProfiles(ctx context.Context) ([]RepositoryProfile, error)
}

// ProviderInfo describes a runtime this host can actually dispatch to. Available
// is reported rather than filtered so a client can explain why a runtime cannot
// be chosen instead of silently omitting it.
type ProviderInfo struct {
	ID                  string `json:"id"`
	DefaultModel        string `json:"default_model,omitempty"`
	DefaultApprovalMode string `json:"default_approval_mode,omitempty"`
	// Models is retained as the selectable-value-only compatibility view.
	// ModelAliases identifies values whose provider resolves them dynamically.
	Models        []string               `json:"models,omitempty"`
	ModelAliases  []string               `json:"model_aliases,omitempty"`
	ModelProfiles []ProviderModel        `json:"model_profiles,omitempty"`
	ApprovalModes []ProviderApprovalMode `json:"approval_modes,omitempty"`
	Available     bool                   `json:"available"`
}

type ProviderModel struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name,omitempty"`
	Description string `json:"description,omitempty"`
	// Aliases are additional selectable values with this model's capabilities.
	Aliases                   []string                  `json:"aliases,omitempty"`
	DefaultReasoningEffort    string                    `json:"default_reasoning_effort,omitempty"`
	SupportedReasoningEfforts []ProviderReasoningEffort `json:"supported_reasoning_efforts,omitempty"`
	SupportedApprovalModes    []string                  `json:"supported_approval_modes,omitempty"`
}

type ProviderReasoningEffort struct {
	ID          string `json:"id"`
	Description string `json:"description,omitempty"`
	// Kind is "reasoning" for ordinary depth controls and "orchestration" for
	// provider values that also enable automatic delegation.
	Kind string `json:"kind"`
}

type ProviderApprovalMode struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name,omitempty"`
	Description string `json:"description,omitempty"`
}

// ProviderCatalog reports the configured provider runtimes and their default
// models, so a client never has to hardcode a provider list or guess a model.
type ProviderCatalog interface {
	ProviderProfiles(ctx context.Context) ([]ProviderInfo, error)
}

// UsageWindowView is how much of one allowance is gone, and when it renews.
type UsageWindowView struct {
	Name        string    `json:"name"`
	UsedPercent float64   `json:"used_percent"`
	ResetsAt    time.Time `json:"resets_at,omitempty"`
}

// ProviderUsageView is what one runtime reports about the keeper's own
// subscription. Absence is reported rather than filled in: "not reported" and
// "nothing used" are different facts and only one of them is good news.
type ProviderUsageView struct {
	Provider   string            `json:"provider"`
	Reported   bool              `json:"reported"`
	Reason     string            `json:"reason,omitempty"`
	ObservedAt time.Time         `json:"observed_at,omitempty"`
	Windows    []UsageWindowView `json:"windows,omitempty"`
	// Authenticated is a pointer so "the runtime did not say" stays distinct
	// from "it said no".
	Authenticated *bool  `json:"authenticated,omitempty"`
	Account       string `json:"account,omitempty"`
}

type UsageResponse struct {
	Providers []ProviderUsageView `json:"providers"`
	// CachedFor tells a client how stale this may be, so it can decide for
	// itself whether to ask again rather than guessing at the cadence.
	CachedForSeconds int `json:"cached_for_seconds"`
}

// ConfigureRepositoryRequest adds a repository to the host's configuration.
// Unlike RegisterRepositoryRequest, which resolves a remote against an already
// configured profile, this one creates the profile.
type ConfigureRepositoryRequest struct {
	ID             string   `json:"id"`
	CheckoutPath   string   `json:"checkout_path"`
	Remote         string   `json:"remote,omitempty"`
	BaseRef        string   `json:"base_ref"`
	Verification   []string `json:"verification,omitempty"`
	Delivery       bool     `json:"delivery"`
	IdempotencyKey string   `json:"idempotency_key"`
}

type ConfigureRepositoryResponse struct {
	Repository RepositoryProfile `json:"repository"`
	// AppliesAfterRestart is always true today and says so rather than letting a
	// keeper watch for a repository that cannot appear: the engine reads its
	// configuration at startup, and restarting it pauses every bee in flight.
	AppliesAfterRestart bool `json:"applies_after_restart"`
}

// RepositoryConfigurator writes a repository into the host's configuration. The
// implementation lives where the configuration path is known; the authority only
// knows that something can do it.
type RepositoryConfigurator interface {
	ConfigureRepository(ctx context.Context, request ConfigureRepositoryRequest) (RepositoryProfile, error)
}

// UsageSource reports subscription usage and authentication for one runtime.
// Asking is not free — one adapter answers from an in-memory cache fed by a
// statusline hook, another makes two live RPC calls against a running session —
// which is why this is its own endpoint with its own cache rather than a field
// on the provider catalog that the surface polls every 2.5 seconds.
type UsageSource interface {
	ProviderUsage(ctx context.Context) ([]ProviderUsageView, error)
}

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
	ID               string                     `json:"id"`
	ProjectID        string                     `json:"project_id"`
	BoardID          string                     `json:"board_id"`
	RepositoryID     string                     `json:"repository_id"`
	RepositoryRemote string                     `json:"repository_remote,omitempty"`
	TargetDeviceID   string                     `json:"target_device_id"`
	TargetEpoch      uint64                     `json:"target_epoch"`
	Title            string                     `json:"title"`
	Prompt           string                     `json:"prompt"`
	Provider         workmodel.Provider         `json:"provider"`
	Model            string                     `json:"model,omitempty"`
	ExecutionProfile workmodel.ExecutionProfile `json:"execution_profile,omitempty"`
	State            workmodel.State            `json:"state"`
	Revision         int64                      `json:"revision"`
	ExecutionID      string                     `json:"execution_id,omitempty"`
	SessionID        string                     `json:"session_id,omitempty"`
	RuntimeID        string                     `json:"runtime_id,omitempty"`
	CommitSHA        string                     `json:"commit_sha,omitempty"`
	PushRef          string                     `json:"push_ref,omitempty"`
	FailureReason    string                     `json:"failure_reason,omitempty"`
	CreatedAt        time.Time                  `json:"created_at"`
	UpdatedAt        time.Time                  `json:"updated_at"`
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
	ProjectID        string                     `json:"project_id"`
	BoardID          string                     `json:"board_id"`
	RepositoryID     string                     `json:"repository_id"`
	TargetDeviceID   string                     `json:"target_device_id,omitempty"`
	Provider         workmodel.Provider         `json:"provider"`
	Model            string                     `json:"model,omitempty"`
	ExecutionProfile workmodel.ExecutionProfile `json:"execution_profile,omitempty"`
	Title            string                     `json:"title"`
	Prompt           string                     `json:"prompt"`
	IdempotencyKey   string                     `json:"idempotency_key"`
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

// IntegrateRepositoryRequest is an explicit Git authority gesture. Unlike task
// delivery, it may target a repository's protected main branch because the
// local keeper selected both exact refs and fenced both remote tips.
type IntegrateRepositoryRequest struct {
	RepositoryID      string `json:"repository_id"`
	SourceRef         string `json:"source_ref"`
	TargetRef         string `json:"target_ref"`
	ExpectedSourceSHA string `json:"expected_source_sha"`
	ExpectedTargetSHA string `json:"expected_target_sha,omitempty"`
	Message           string `json:"message"`
	UpdateSource      bool   `json:"update_source"`
	IdempotencyKey    string `json:"idempotency_key"`
}

type IntegrationReceipt struct {
	ID                string    `json:"id"`
	RepositoryID      string    `json:"repository_id"`
	SourceRef         string    `json:"source_ref"`
	TargetRef         string    `json:"target_ref"`
	SourceSHA         string    `json:"source_sha"`
	PreviousTargetSHA string    `json:"previous_target_sha"`
	MergeSHA          string    `json:"merge_sha"`
	SourceUpdated     bool      `json:"source_updated"`
	Verification      string    `json:"verification"`
	ObservedAt        time.Time `json:"observed_at"`
}

type IntegrationResponse struct {
	Receipt IntegrationReceipt `json:"receipt"`
}

type ProjectResponse struct {
	Project Project `json:"project"`
}
type RepositoryResponse struct {
	Repository Repository `json:"repository"`
}
type RepositoriesResponse struct {
	Repositories []RepositoryProfile `json:"repositories"`
}
type ProvidersResponse struct {
	Providers []ProviderInfo `json:"providers"`
}
type ProjectsResponse struct {
	Projects []Project `json:"projects"`
}
type BoardsResponse struct {
	Boards []Board `json:"boards"`
}
type TasksResponse struct {
	Tasks []TaskView `json:"tasks"`
}

// HiveResponse is the whole local event log from a cursor. One feed keeps a
// client's cost constant in the number of tasks and is the only way non-task
// events are readable.
type HiveResponse struct {
	Events     []Event `json:"events"`
	NextCursor uint64  `json:"next_cursor,omitempty"`
}

// TaskFilter scopes a task listing. An empty filter lists this controller's
// tasks, newest first.
type TaskFilter struct {
	ProjectID      string
	BoardID        string
	RepositoryID   string
	TargetDeviceID string
	States         []workmodel.State
	Limit          int
}

// LocalListingAuthority reads the hive rather than one task. It is an optional
// store capability: without it a client can only see what it created itself.
type LocalListingAuthority interface {
	ListProjects(ctx context.Context) ([]Project, error)
	ListBoards(ctx context.Context, projectID string) ([]Board, error)
	ListTasks(ctx context.Context, filter store.ListFilter) ([]workmodel.Task, error)
	ListLocalEventsSince(ctx context.Context, after uint64, limit int) ([]Event, error)
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

// RepositoryIntegrator owns explicit, fenced integration into a selected
// branch. The interface is generic Git authority and contains no Kovan product
// concepts such as objectives, tasks, boards, or Queen.
type RepositoryIntegrator interface {
	Integrate(context.Context, IntegrateRepositoryRequest) (IntegrationReceipt, error)
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
	Repositories        RepositoryCatalog
	Providers           ProviderCatalog
	Controller          CommandController
	Executor            Executor
	Verifier            Verifier
	Committer           Committer
	Integrator          RepositoryIntegrator
	RemoteDevices       map[string]DeviceRuntime
	RemoteDeviceFactory RemoteDeviceFactory
	Clock               func() time.Time
	NewID               func(string) string
}
