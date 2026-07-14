// Package provider defines the subscription-backed agent boundary used by the bridge.
package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/berkayahi/agentbridge/internal/task"
)

const (
	MaxAttachments = 16
	MaxInputBytes  = 1 << 20
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

func (id ID) String() string { return id.value }
func (id ID) Valid() bool    { return id.value != "" }

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
	total := len(in.Text)
	for _, attachment := range in.Attachments {
		if attachment.path == "" || !filepath.IsAbs(attachment.path) {
			return fmt.Errorf("%w: invalid attachment", ErrInvalidInput)
		}
		total += int(attachment.size)
	}
	if total == 0 || total > MaxInputBytes {
		return fmt.Errorf("%w: input size", ErrInvalidInput)
	}
	return nil
}

type Session struct {
	ID         ID
	TaskID     ID
	ExternalID string
	ThreadID   string
	Provider   task.Provider
}

type StartRequest struct {
	TaskID           ID
	Input            Input
	WorkingDirectory string
	Model            string
}

type ResumeRequest struct {
	TaskID  ID
	Session Session
	Input   Input
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
	Provider   task.Provider
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
	Name() task.Provider
	Start(context.Context, StartRequest) (Session, <-chan Event, error)
	Resume(context.Context, ResumeRequest) (Session, <-chan Event, error)
	Steer(context.Context, Session, Input) error
	Interrupt(context.Context, Session) error
	ResolveApproval(context.Context, ApprovalDecision) error
	Usage(context.Context) (Usage, error)
	AuthStatus(context.Context) (AuthStatus, error)
}
