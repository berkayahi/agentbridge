// Package managed contains the device-side managed controller boundary.
package managed

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

var (
	ErrReplay               = errors.New("managed: duplicate or replayed frame")
	ErrStaleEpoch           = errors.New("managed: stale protocol epoch")
	ErrInvalidFrame         = errors.New("managed: invalid frame")
	ErrOrganizationMismatch = errors.New("managed: organization mismatch")
)

type Cursor struct {
	MessageID       uint64
	Sequence        uint64
	ConnectionEpoch uint64
	ControllerEpoch uint64
}

type CursorStore interface {
	Load(context.Context) (Cursor, error)
	Save(context.Context, Cursor) error
}

type MemoryCursorStore struct {
	mu     sync.Mutex
	cursor Cursor
}

func (s *MemoryCursorStore) Load(ctx context.Context) (Cursor, error) {
	if err := ctx.Err(); err != nil {
		return Cursor{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursor, nil
}

func (s *MemoryCursorStore) Save(ctx context.Context, value Cursor) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.cursor = value
	s.mu.Unlock()
	return nil
}

type Frame struct {
	OrganizationID  string
	DeviceID        string
	ConnectionEpoch uint64
	ControllerEpoch uint64
	MessageID       uint64
	Sequence        uint64
	CommandID       string
	PayloadType     string
	Payload         []byte
	Signature       []byte
	SigningKeyID    string
	ExpiresAt       time.Time
}

type ReplayGuard struct {
	store        CursorStore
	organization string
	device       string
	mu           sync.Mutex
}

func NewReplayGuard(store CursorStore, organization, device string) (*ReplayGuard, error) {
	if store == nil || strings.TrimSpace(organization) == "" || strings.TrimSpace(device) == "" {
		return nil, ErrInvalidFrame
	}
	return &ReplayGuard{store: store, organization: organization, device: device}, nil
}

// Accept persists the cursor before the caller dispatches the payload. A
// restart therefore cannot accept a frame whose side effect was already
// admitted by this device.
func (g *ReplayGuard) Accept(ctx context.Context, frame Frame, now time.Time) error {
	if g == nil || frame.OrganizationID != g.organization || frame.DeviceID != g.device || frame.ConnectionEpoch == 0 || frame.ControllerEpoch == 0 || frame.MessageID == 0 || frame.Sequence == 0 || strings.TrimSpace(frame.PayloadType) == "" || frame.ExpiresAt.IsZero() || !now.Before(frame.ExpiresAt) {
		return ErrInvalidFrame
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	current, err := g.store.Load(ctx)
	if err != nil {
		return err
	}
	if frame.ControllerEpoch < current.ControllerEpoch || frame.ConnectionEpoch < current.ConnectionEpoch {
		return ErrStaleEpoch
	}
	if frame.ConnectionEpoch == current.ConnectionEpoch && (frame.MessageID <= current.MessageID || frame.Sequence <= current.Sequence) {
		return ErrReplay
	}
	return g.store.Save(ctx, Cursor{MessageID: frame.MessageID, Sequence: frame.Sequence, ConnectionEpoch: frame.ConnectionEpoch, ControllerEpoch: frame.ControllerEpoch})
}
