// Package usage records provider quota windows without provider secrets.
package usage

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

var ErrInvalid = errors.New("usage: invalid observation")

type Status string

const (
	StatusAvailable Status = "available"
	StatusUnknown   Status = "unknown"
	StatusStale     Status = "stale"
)

type Window struct {
	Provider      string
	DeviceID      string
	Runtime       string
	AccountSafeID string
	ObservedAt    time.Time
	ResetAt       *time.Time
	Credits       *int64
	SchemaVersion uint32
	Status        Status
}

func (w Window) Validate(now time.Time) error {
	if strings.TrimSpace(w.Provider) == "" || strings.TrimSpace(w.DeviceID) == "" || strings.TrimSpace(w.Runtime) == "" || strings.TrimSpace(w.AccountSafeID) == "" || w.ObservedAt.IsZero() || w.SchemaVersion == 0 {
		return ErrInvalid
	}
	if w.Status != StatusAvailable && w.Status != StatusUnknown && w.Status != StatusStale {
		return ErrInvalid
	}
	if w.ResetAt != nil && w.ResetAt.Before(w.ObservedAt) {
		return ErrInvalid
	}
	if now.Before(w.ObservedAt.Add(-5 * time.Minute)) {
		return ErrInvalid
	}
	if w.Credits != nil && *w.Credits < 0 {
		return ErrInvalid
	}
	return nil
}

type Store interface {
	Save(context.Context, Window) error
	Latest(context.Context, string, string) (Window, error)
}

type MemoryStore struct {
	mu     sync.Mutex
	values map[string]Window
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{values: make(map[string]Window)} }

func (s *MemoryStore) Save(ctx context.Context, value Window) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := value.Validate(time.Now().UTC()); err != nil {
		return err
	}
	s.mu.Lock()
	s.values[value.Provider+"\x00"+value.DeviceID] = value
	s.mu.Unlock()
	return nil
}

func (s *MemoryStore) Latest(ctx context.Context, provider, device string) (Window, error) {
	if err := ctx.Err(); err != nil {
		return Window{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[provider+"\x00"+device]
	if !ok {
		return Window{}, ErrInvalid
	}
	return value, nil
}

type Service struct {
	store Store
	now   func() time.Time
}

func NewService(store Store, now func() time.Time) (*Service, error) {
	if store == nil {
		return nil, ErrInvalid
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{store: store, now: now}, nil
}

func (s *Service) Observe(ctx context.Context, value Window) error {
	if s == nil || s.store == nil {
		return ErrInvalid
	}
	if err := value.Validate(s.now().UTC()); err != nil {
		return err
	}
	return s.store.Save(ctx, value)
}

func (s *Service) Latest(ctx context.Context, provider, device string) (Window, error) {
	if s == nil || s.store == nil || strings.TrimSpace(provider) == "" || strings.TrimSpace(device) == "" {
		return Window{}, ErrInvalid
	}
	return s.store.Latest(ctx, provider, device)
}
