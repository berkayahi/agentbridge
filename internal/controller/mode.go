// Package controller owns runtime authority and mode activation.
package controller

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
)

type Mode string

const (
	ModeStandalone Mode = "standalone"
	ModeManaged    Mode = "managed"
)

var (
	ErrInvalidMode     = errors.New("controller: invalid mode")
	ErrActiveExecution = errors.New("controller: active execution prevents mode change")
)

type ModeState struct {
	Version           int    `json:"version"`
	Mode              Mode   `json:"mode"`
	ActiveExecutionID string `json:"active_execution_id,omitempty"`
}

func (s ModeState) Validate() error {
	if s.Version != 1 || !s.Mode.Valid() {
		return ErrInvalidMode
	}
	if strings.ContainsAny(s.ActiveExecutionID, "\x00\r\n") {
		return ErrInvalidMode
	}
	return nil
}

func (m Mode) Valid() bool { return m == ModeStandalone || m == ModeManaged }

type ModeStore interface {
	Load(context.Context) (ModeState, error)
	Save(context.Context, ModeState) error
}

type FileModeStore struct {
	path string
	mu   sync.Mutex
}

func NewFileModeStore(path string) (*FileModeStore, error) {
	if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) {
		return nil, ErrInvalidMode
	}
	return &FileModeStore{path: filepath.Clean(path)}, nil
}

func (s *FileModeStore) Load(ctx context.Context) (ModeState, error) {
	if err := ctx.Err(); err != nil {
		return ModeState{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

func (s *FileModeStore) load() (ModeState, error) {
	info, err := os.Lstat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return ModeState{Version: 1, Mode: ModeStandalone}, nil
	}
	if err != nil {
		return ModeState{}, fmt.Errorf("inspect mode state: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return ModeState{}, ErrInvalidMode
	}
	file, err := os.Open(s.path)
	if err != nil {
		return ModeState{}, fmt.Errorf("open mode state: %w", err)
	}
	defer file.Close()
	var state ModeState
	if err := json.NewDecoder(io.LimitReader(file, 4096)).Decode(&state); err != nil {
		return ModeState{}, fmt.Errorf("decode mode state: %w", err)
	}
	if err := state.Validate(); err != nil {
		return ModeState{}, err
	}
	return state, nil
}

func (s *FileModeStore) Save(ctx context.Context, state ModeState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if state.Version == 0 {
		state.Version = 1
	}
	if err := state.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	directory := filepath.Dir(s.path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create mode directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return fmt.Errorf("protect mode directory: %w", err)
	}
	value, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode mode state: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".mode-*")
	if err != nil {
		return fmt.Errorf("create mode state: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(value, '\n')); err != nil {
		temporary.Close()
		return fmt.Errorf("write mode state: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync mode state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, s.path); err != nil {
		return fmt.Errorf("install mode state: %w", err)
	}
	return nil
}

func Activate(ctx context.Context, store ModeStore, desired Mode) (ModeState, error) {
	if store == nil || !desired.Valid() {
		return ModeState{}, ErrInvalidMode
	}
	state, err := store.Load(ctx)
	if err != nil {
		return ModeState{}, err
	}
	if state.Mode != desired && state.ActiveExecutionID != "" {
		return state, ErrActiveExecution
	}
	state.Version = 1
	state.Mode = desired
	if err := store.Save(ctx, state); err != nil {
		return ModeState{}, err
	}
	return state, nil
}

func SetActiveExecution(ctx context.Context, store ModeStore, executionID string) (ModeState, error) {
	if store == nil || strings.TrimSpace(executionID) == "" || strings.ContainsAny(executionID, "\x00\r\n") {
		return ModeState{}, ErrInvalidMode
	}
	state, err := store.Load(ctx)
	if err != nil {
		return ModeState{}, err
	}
	state.ActiveExecutionID = executionID
	if err := store.Save(ctx, state); err != nil {
		return ModeState{}, err
	}
	return state, nil
}

func ClearActiveExecution(ctx context.Context, store ModeStore, executionID string) (ModeState, error) {
	if store == nil {
		return ModeState{}, ErrInvalidMode
	}
	state, err := store.Load(ctx)
	if err != nil {
		return ModeState{}, err
	}
	if executionID != "" && state.ActiveExecutionID != executionID {
		return state, nil
	}
	state.ActiveExecutionID = ""
	if err := store.Save(ctx, state); err != nil {
		return ModeState{}, err
	}
	return state, nil
}
