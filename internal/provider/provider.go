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

type ResumeRequest struct {
	TaskID  ID
	Session Session
	Input   Input
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
	Name        string
	UsedPercent float64
	ResetsAt    time.Time
}

type Usage struct {
	Provider   workmodel.Provider
	ObservedAt time.Time
	Windows    []UsageWindow
	Credits    *float64
}

type AuthStatus struct {
	Authenticated bool
	Account       string
	CheckedAt     time.Time
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
