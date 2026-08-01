// Package provider defines the subscription-backed agent boundary used by the bridge.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/berkayahi/agentbridge/internal/workmodel"
)

const (
	MaxAttachments          = 16
	MaxInputBytes           = 1 << 20
	MaxAttachmentBytes      = 64 << 20
	MaxTotalAttachmentBytes = 128 << 20
)

var ErrInvalidInput = errors.New("invalid provider input")

var (
	ErrAnalysisUnavailable      = errors.New("provider read-only analysis is unavailable")
	ErrAnalysisApprovalDeclined = errors.New("provider read-only analysis approval was declined")
)

// ID is an immutable identifier. Construct it with NewID or MustID.
type ID struct{ value string }

func NewID(value string) (ID, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 256 {
		return ID{}, fmt.Errorf("%w: identifier length", ErrInvalidInput)
	}
	return ID{value: value}, nil
}

func MustID(value string) ID {
	id, err := NewID(value)
	if err != nil {
		panic(err)
	}
	return id
}

func (id ID) String() string               { return id.value }
func (id ID) Valid() bool                  { return id.value != "" }
func (id ID) MarshalJSON() ([]byte, error) { return json.Marshal(id.value) }
func (id ID) MarshalText() ([]byte, error) { return []byte(id.value), nil }

type LocalAttachment struct {
	path      string
	mediaType string
	size      int64
}

func NewLocalAttachment(path, mediaType string) (LocalAttachment, error) {
	if !filepath.IsAbs(path) {
		return LocalAttachment{}, fmt.Errorf("%w: attachment path must be absolute", ErrInvalidInput)
	}
	info, err := os.Stat(path)
	if err != nil {
		return LocalAttachment{}, fmt.Errorf("%w: attachment: %v", ErrInvalidInput, err)
	}
	if !info.Mode().IsRegular() {
		return LocalAttachment{}, fmt.Errorf("%w: attachment must be a regular file", ErrInvalidInput)
	}
	return LocalAttachment{path: filepath.Clean(path), mediaType: strings.TrimSpace(mediaType), size: info.Size()}, nil
}

func (a LocalAttachment) Path() string      { return a.path }
func (a LocalAttachment) MediaType() string { return a.mediaType }
func (a LocalAttachment) Size() int64       { return a.size }

type Input struct {
	Text        string
	Attachments []LocalAttachment
}

func (in Input) Validate() error {
	if len(in.Attachments) > MaxAttachments {
		return fmt.Errorf("%w: too many attachments", ErrInvalidInput)
	}
	if len(in.Text) > MaxInputBytes {
		return fmt.Errorf("%w: text size", ErrInvalidInput)
	}
	var attachmentBytes int64
	for _, attachment := range in.Attachments {
		if attachment.path == "" || !filepath.IsAbs(attachment.path) {
			return fmt.Errorf("%w: invalid attachment", ErrInvalidInput)
		}
		if attachment.size > MaxAttachmentBytes {
			return fmt.Errorf("%w: attachment size", ErrInvalidInput)
		}
		attachmentBytes += attachment.size
	}
	if len(in.Text) == 0 && attachmentBytes == 0 {
		return fmt.Errorf("%w: input size", ErrInvalidInput)
	}
	if attachmentBytes > MaxTotalAttachmentBytes {
		return fmt.Errorf("%w: total attachment size", ErrInvalidInput)
	}
	return nil
}

type Session struct {
	ID         ID
	TaskID     ID
	ExternalID string
	ThreadID   string
	Provider   workmodel.Provider
}

type StartRequest struct {
	TaskID           ID
	Input            Input
	WorkingDirectory string
	Model            string
	ExecutionProfile workmodel.ExecutionProfile
	// WritablePaths are absolute paths outside the worktree that this session
	// may write to — a repository's toolchain caches, typically. Empty leaves
	// the decision to the host's own provider policy.
	WritablePaths []string
}

// AnalysisExecutionPolicy is the explicit provider policy for repository
// understanding. The workspace is disposable and the only writable root;
// all other capabilities are denied.
type AnalysisExecutionPolicy struct {
	WorkspacePath           string
	WritablePaths           []string
	NetworkAccess           bool
	ApprovalAllowed         bool
	DeliveryAllowed         bool
	HostEnvironmentAllowed  bool
	ProductionDataAllowed   bool
	CredentialsAllowed      bool
	DestructiveActionsAllow bool
	RequireOSIsolation      bool
}

// AnalysisPolicyResult is returned by ValidateAnalysisExecutionPolicy so
// callers can expose a truthful unavailable state without running a provider.
type AnalysisPolicyResult struct {
	Allowed             bool
	WorkspaceOnly       bool
	NetworkAccess       bool
	ApprovalAllowed     bool
	DeliveryAllowed     bool
	HostEnvironment     bool
	ProductionData      bool
	CredentialsAllowed  bool
	DestructiveActions  bool
	OSIsolationRequired bool
	Reason              string
}

func (p AnalysisExecutionPolicy) Validate() AnalysisPolicyResult {
	result := AnalysisPolicyResult{
		WorkspaceOnly:       filepath.IsAbs(p.WorkspacePath) && len(p.WritablePaths) == 0,
		NetworkAccess:       p.NetworkAccess,
		ApprovalAllowed:     p.ApprovalAllowed,
		DeliveryAllowed:     p.DeliveryAllowed,
		HostEnvironment:     p.HostEnvironmentAllowed,
		ProductionData:      p.ProductionDataAllowed,
		CredentialsAllowed:  p.CredentialsAllowed,
		DestructiveActions:  p.DestructiveActionsAllow,
		OSIsolationRequired: p.RequireOSIsolation,
	}
	switch {
	case !result.WorkspaceOnly:
		result.Reason = "analysis requires an absolute disposable workspace and no external writable paths"
	case p.NetworkAccess:
		result.Reason = "analysis network access is denied"
	case p.ApprovalAllowed:
		result.Reason = "analysis approvals are always declined"
	case p.DeliveryAllowed:
		result.Reason = "analysis cannot deliver or commit"
	case p.HostEnvironmentAllowed:
		result.Reason = "analysis cannot inherit the host environment"
	case p.ProductionDataAllowed:
		result.Reason = "analysis cannot access production data"
	case p.CredentialsAllowed:
		result.Reason = "analysis cannot use credentials"
	case p.DestructiveActionsAllow:
		result.Reason = "analysis cannot perform destructive actions"
	case !p.RequireOSIsolation:
		result.Reason = "analysis requires OS isolation"
	default:
		result.Allowed = true
	}
	return result
}

func NewReadOnlyAnalysisPolicy(workspacePath string) AnalysisExecutionPolicy {
	return AnalysisExecutionPolicy{
		WorkspacePath: workspacePath, RequireOSIsolation: true,
	}
}

// AnalysisRequest is passed only to providers that explicitly implement the
// non-persistent, read-only analysis capability.
type AnalysisRequest struct {
	TaskID           ID
	Input            Input
	WorkingDirectory string
	Model            string
	Policy           AnalysisExecutionPolicy
}

type AnalysisResult struct {
	ProviderID workmodel.Provider
	Model      string
	Output     []byte
}

// AnalysisIsolationAttestation is a host-issued capability boundary. Merely
// sending a provider a workspaceWrite policy is not an attestation: a provider
// protocol may restrict writes while still allowing reads of the host. The
// analysis capability is unavailable unless a trusted launcher can attest all
// of these independently enforced properties.
type AnalysisIsolationAttestation struct {
	Mechanism                    string
	FilesystemReadsWorkspaceOnly bool
	HostEnvironmentExcluded      bool
	NetworkDenied                bool
	ProductionDataDenied         bool
	DestructiveActionsDenied     bool
}

func (a AnalysisIsolationAttestation) Valid() bool {
	return strings.TrimSpace(a.Mechanism) != "" &&
		a.FilesystemReadsWorkspaceOnly && a.HostEnvironmentExcluded && a.NetworkDenied &&
		a.ProductionDataDenied && a.DestructiveActionsDenied
}

// SafeAnalysisProvider is intentionally separate from Provider.Start. A
// normal task provider may persist sessions and permit delivery; implementing
// this interface is an explicit promise that analysis has neither behavior.
type SafeAnalysisProvider interface {
	AnalysisIsolationAttestation() AnalysisIsolationAttestation
	AnalyzeReadOnly(context.Context, AnalysisRequest) (AnalysisResult, error)
}

type ResumeRequest struct {
	TaskID           ID
	Session          Session
	Input            Input
	WorkingDirectory string
	// Model is the model this session was dispatched with. A resume must carry
	// it or the session silently continues on the provider's default, which is
	// not what the operator chose.
	Model            string
	ExecutionProfile workmodel.ExecutionProfile
	WritablePaths    []string
}

type ReasoningEffortKind string

const (
	ReasoningEffortStandard      ReasoningEffortKind = "reasoning"
	ReasoningEffortOrchestration ReasoningEffortKind = "orchestration"
)

type ReasoningEffort struct {
	ID          string
	Description string
	Kind        ReasoningEffortKind
}

type Model struct {
	ID                     string
	DisplayName            string
	Description            string
	Aliases                []string
	DefaultReasoningEffort string
	ReasoningEfforts       []ReasoningEffort
	ApprovalModes          []string
}

type ApprovalMode struct {
	ID          string
	Description string
}

type ExecutionCatalog struct {
	DefaultModel        string
	Models              []Model
	ModelAliases        []string
	DefaultApprovalMode string
	ApprovalModes       []ApprovalMode
}

// ExecutionCatalogProvider is optional because not every provider protocol
// exposes a live model catalog.
type ExecutionCatalogProvider interface {
	ExecutionCatalog(context.Context) (ExecutionCatalog, error)
}

type ApprovalDecision struct {
	RequestID ID
	TaskID    ID
	UserID    string
	Allow     bool
	DecidedAt time.Time
}

type UsageWindow struct {
	Name        string    `json:"name"`
	UsedPercent float64   `json:"used_percent"`
	ResetsAt    time.Time `json:"resets_at"`
}

// TokenUsage is what one turn actually cost. A subscription window says how much
// of an allowance is gone; this says which bee spent it, which is the question a
// keeper asks when a window is nearly closed.
type TokenUsage struct {
	Input           int64 `json:"input"`
	CachedInput     int64 `json:"cached_input"`
	Output          int64 `json:"output"`
	ReasoningOutput int64 `json:"reasoning_output"`
	Total           int64 `json:"total"`
}

type Usage struct {
	Provider   workmodel.Provider `json:"provider"`
	ObservedAt time.Time          `json:"observed_at"`
	Windows    []UsageWindow      `json:"windows,omitempty"`
	Credits    *float64           `json:"credits,omitempty"`
	// Tokens is present when the provider reported the cost of a specific turn
	// rather than the state of an account-wide allowance.
	Tokens *TokenUsage `json:"tokens,omitempty"`
	// TurnID ties a token report to the turn that spent them, so a cost can be
	// attributed to one bee instead of to the session as a whole.
	TurnID string `json:"turn_id,omitempty"`
}

type AuthStatus struct {
	Authenticated bool      `json:"authenticated"`
	Account       string    `json:"account"`
	CheckedAt     time.Time `json:"checked_at"`
}

type Provider interface {
	Name() workmodel.Provider
	Start(context.Context, StartRequest) (Session, <-chan Event, error)
	Resume(context.Context, ResumeRequest) (Session, <-chan Event, error)
	Steer(context.Context, Session, Input) error
	Interrupt(context.Context, Session) error
	ResolveApproval(context.Context, ApprovalDecision) error
	Usage(context.Context) (Usage, error)
	AuthStatus(context.Context) (AuthStatus, error)
}
