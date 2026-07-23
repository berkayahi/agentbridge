package managed

import (
	"bytes"
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
	ErrInvalidState = errors.New("managed: invalid persistent state")
	ErrInboxFull    = errors.New("managed: persistent inbox is full")
)

const maxInboxEntries = 4096

// InboxStore records a command before its handler can produce a side effect.
type InboxStore interface {
	Persist(context.Context, Frame) (bool, error)
}

// AtomicAcceptStore combines inbox admission and cursor advancement. A
// single atomic file replacement prevents an inbox entry without its cursor
// (or the reverse) after a process crash.
type AtomicAcceptStore interface {
	CursorStore
	InboxStore
	Accept(context.Context, Frame, Cursor) (bool, error)
}

type fileState struct {
	Version int              `json:"version"`
	Cursor  Cursor           `json:"cursor"`
	Inbox   map[string]Frame `json:"inbox"`
	Trust   TrustSet         `json:"trust"`
}

type FileStateStore struct {
	path string
	mu   sync.Mutex
}

func NewFileStateStore(path string) (*FileStateStore, error) {
	if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) {
		return nil, ErrInvalidState
	}
	return &FileStateStore{path: filepath.Clean(path)}, nil
}

func (s *FileStateStore) Load(ctx context.Context) (Cursor, error) {
	if err := ctx.Err(); err != nil {
		return Cursor{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.loadLocked()
	if err != nil {
		return Cursor{}, err
	}
	return state.Cursor, nil
}

func (s *FileStateStore) Save(ctx context.Context, cursor Cursor) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.loadLocked()
	if err != nil {
		return err
	}
	state.Cursor = cursor
	return s.saveLocked(state)
}

func (s *FileStateStore) LoadTrust(ctx context.Context) (TrustSet, error) {
	if err := ctx.Err(); err != nil {
		return TrustSet{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.loadLocked()
	if err != nil {
		return TrustSet{}, err
	}
	return cloneTrust(state.Trust), nil
}

func (s *FileStateStore) SaveTrust(ctx context.Context, trust TrustSet) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := trust.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.loadLocked()
	if err != nil {
		return err
	}
	state.Trust = cloneTrust(trust)
	return s.saveLocked(state)
}

func (s *FileStateStore) Persist(ctx context.Context, frame Frame) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.loadLocked()
	if err != nil {
		return false, err
	}
	duplicate, err := admitFrame(&state, frame)
	if err != nil || duplicate {
		return duplicate, err
	}
	return false, s.saveLocked(state)
}

func (s *FileStateStore) Accept(ctx context.Context, frame Frame, cursor Cursor) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.loadLocked()
	if err != nil {
		return false, err
	}
	duplicate, err := admitFrame(&state, frame)
	if err != nil || duplicate {
		return duplicate, err
	}
	state.Cursor = cursor
	return false, s.saveLocked(state)
}

func (s *FileStateStore) loadLocked() (fileState, error) {
	info, err := os.Lstat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return fileState{Version: 1, Inbox: make(map[string]Frame)}, nil
	}
	if err != nil {
		return fileState{}, fmt.Errorf("inspect managed state: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return fileState{}, ErrInvalidState
	}
	file, err := os.Open(s.path)
	if err != nil {
		return fileState{}, fmt.Errorf("open managed state: %w", err)
	}
	defer file.Close()
	var state fileState
	if err := json.NewDecoder(io.LimitReader(file, 64<<20)).Decode(&state); err != nil {
		return fileState{}, fmt.Errorf("decode managed state: %w", err)
	}
	if state.Version != 1 || state.Inbox == nil || len(state.Inbox) > maxInboxEntries {
		return fileState{}, ErrInvalidState
	}
	pruneExpired(&state, time.Now().UTC())
	return state, nil
}

func (s *FileStateStore) saveLocked(state fileState) error {
	if state.Version == 0 {
		state.Version = 1
	}
	if state.Inbox == nil || len(state.Inbox) > maxInboxEntries {
		return ErrInboxFull
	}
	directory := filepath.Dir(s.path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create managed state directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return fmt.Errorf("protect managed state directory: %w", err)
	}
	value, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode managed state: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".managed-state-*")
	if err != nil {
		return fmt.Errorf("create managed state: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(value, '\n')); err != nil {
		temporary.Close()
		return fmt.Errorf("write managed state: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync managed state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, s.path); err != nil {
		return fmt.Errorf("install managed state: %w", err)
	}
	return nil
}

func admitFrame(state *fileState, frame Frame) (bool, error) {
	key := fmt.Sprintf("%d", frame.MessageID)
	if existing, ok := state.Inbox[key]; ok {
		left, _ := existing.CanonicalSigningBytes()
		right, _ := frame.CanonicalSigningBytes()
		if bytes.Equal(left, right) && bytes.Equal(existing.Signature, frame.Signature) {
			return true, nil
		}
		return false, ErrReplay
	}
	if len(state.Inbox) >= maxInboxEntries {
		return false, ErrInboxFull
	}
	state.Inbox[key] = frame
	return false, nil
}

func pruneExpired(state *fileState, now time.Time) {
	for key, frame := range state.Inbox {
		if !frame.ExpiresAt.IsZero() && !now.Before(frame.ExpiresAt) {
			delete(state.Inbox, key)
		}
	}
}

func cloneTrust(value TrustSet) TrustSet {
	return TrustSet{Active: cloneKeys(value.Active), Next: cloneKeys(value.Next), HighestEpoch: value.HighestEpoch, Revoked: value.Revoked}
}
