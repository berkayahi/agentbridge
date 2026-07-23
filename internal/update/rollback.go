package update

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

type Floor struct {
	MetadataVersion uint64
	ProductVersion  string
	BuildTag        string
	ArtifactDigest  string
	UpdatedAt       time.Time
}

func (f Floor) Validate() error {
	if f.MetadataVersion == 0 {
		return nil
	}
	if !semverCore.MatchString(f.ProductVersion) || strings.TrimSpace(f.BuildTag) == "" || !hexString(f.ArtifactDigest, 64) || f.UpdatedAt.IsZero() {
		return ErrInvalidMetadata
	}
	return nil
}

type FloorStore interface {
	Load(context.Context) (Floor, error)
	Save(context.Context, Floor) error
}

type MemoryFloorStore struct {
	mu    sync.Mutex
	value Floor
}

func (s *MemoryFloorStore) Load(ctx context.Context) (Floor, error) {
	if err := ctx.Err(); err != nil {
		return Floor{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.value, nil
}

func (s *MemoryFloorStore) Save(ctx context.Context, value Floor) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := value.Validate(); err != nil {
		return err
	}
	if value.MetadataVersion < s.value.MetadataVersion || value.MetadataVersion == s.value.MetadataVersion && value != s.value {
		return ErrRollback
	}
	s.value = value
	return nil
}

func RecordFloor(ctx context.Context, store FloorStore, metadata Metadata, now time.Time) error {
	if store == nil {
		return errors.New("update: floor store is required")
	}
	if err := metadata.Identity.Validate(); err != nil {
		return err
	}
	current, err := store.Load(ctx)
	if err != nil {
		return err
	}
	if metadata.Version <= current.MetadataVersion {
		return ErrRollback
	}
	if current.ProductVersion != "" {
		comparison := compareCoreVersions(metadata.Identity.ProductVersion, current.ProductVersion)
		if comparison < 0 || comparison == 0 && current.ArtifactDigest != metadata.Identity.ArtifactDigest {
			return ErrRollback
		}
	}
	return store.Save(ctx, Floor{MetadataVersion: metadata.Version, ProductVersion: metadata.Identity.ProductVersion, BuildTag: metadata.Identity.BuildTag, ArtifactDigest: metadata.Identity.ArtifactDigest, UpdatedAt: now.UTC()})
}

type FileFloorStore struct {
	path string
	mu   sync.Mutex
}

func NewFileFloorStore(path string) (*FileFloorStore, error) {
	if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) {
		return nil, ErrInvalidMetadata
	}
	return &FileFloorStore{path: filepath.Clean(path)}, nil
}

func (s *FileFloorStore) Load(ctx context.Context) (Floor, error) {
	if err := ctx.Err(); err != nil {
		return Floor{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *FileFloorStore) Save(ctx context.Context, value Floor) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := value.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.loadLocked()
	if err != nil {
		return err
	}
	if value.MetadataVersion < current.MetadataVersion || value.MetadataVersion == current.MetadataVersion && value != current {
		return ErrRollback
	}
	directory := filepath.Dir(s.path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".update-floor-*")
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

func (s *FileFloorStore) loadLocked() (Floor, error) {
	info, err := os.Lstat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return Floor{}, nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return Floor{}, ErrInvalidMetadata
	}
	file, err := os.Open(s.path)
	if err != nil {
		return Floor{}, err
	}
	defer file.Close()
	var value Floor
	if err := json.NewDecoder(io.LimitReader(file, 16*1024)).Decode(&value); err != nil {
		return Floor{}, err
	}
	if err := value.Validate(); err != nil {
		return Floor{}, err
	}
	return value, nil
}

func compareCoreVersions(left, right string) int {
	var leftParts, rightParts [3]int
	_, _ = fmt.Sscanf(left, "%d.%d.%d", &leftParts[0], &leftParts[1], &leftParts[2])
	_, _ = fmt.Sscanf(right, "%d.%d.%d", &rightParts[0], &rightParts[1], &rightParts[2])
	for i := range leftParts {
		if leftParts[i] < rightParts[i] {
			return -1
		}
		if leftParts[i] > rightParts[i] {
			return 1
		}
	}
	return 0
}
