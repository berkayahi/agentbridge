// Package usage records provider quota windows without provider secrets.
package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	ErrInvalid          = errors.New("usage: invalid observation")
	ErrStaleObservation = errors.New("usage: stale observation")
)

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

type usageState struct {
	Version int               `json:"version"`
	Windows map[string]Window `json:"windows"`
}

type FileStore struct {
	path string
	mu   sync.Mutex
}

func NewFileStore(path string) (*FileStore, error) {
	if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) {
		return nil, ErrInvalid
	}
	return &FileStore{path: filepath.Clean(path)}, nil
}

func (s *FileStore) Save(ctx context.Context, value Window) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := value.Validate(time.Now().UTC()); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.loadLocked()
	if err != nil {
		return err
	}
	key := value.Provider + "\x00" + value.DeviceID
	if existing, ok := state.Windows[key]; ok && value.ObservedAt.Before(existing.ObservedAt) {
		return ErrStaleObservation
	}
	state.Windows[key] = value
	return s.saveLocked(state)
}

func (s *FileStore) Latest(ctx context.Context, provider, device string) (Window, error) {
	if err := ctx.Err(); err != nil {
		return Window{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.loadLocked()
	if err != nil {
		return Window{}, err
	}
	value, ok := state.Windows[provider+"\x00"+device]
	if !ok {
		return Window{}, ErrInvalid
	}
	return value, nil
}

func (s *FileStore) loadLocked() (usageState, error) {
	info, err := os.Lstat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return usageState{Version: 1, Windows: make(map[string]Window)}, nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return usageState{}, ErrInvalid
	}
	file, err := os.Open(s.path)
	if err != nil {
		return usageState{}, fmt.Errorf("open usage state: %w", err)
	}
	defer file.Close()
	var state usageState
	if err := json.NewDecoder(io.LimitReader(file, 4<<20)).Decode(&state); err != nil {
		return usageState{}, fmt.Errorf("decode usage state: %w", err)
	}
	if state.Version != 1 || state.Windows == nil {
		return usageState{}, ErrInvalid
	}
	return state, nil
}

func (s *FileStore) saveLocked(state usageState) error {
	if state.Version == 0 {
		state.Version = 1
	}
	if state.Windows == nil {
		return ErrInvalid
	}
	directory := filepath.Dir(s.path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return err
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".usage-state-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(encoded, '\n')); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, s.path)
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
