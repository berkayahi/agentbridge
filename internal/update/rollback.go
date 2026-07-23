package update

import (
	"context"
	"errors"
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
	if value.MetadataVersion < s.value.MetadataVersion {
		return ErrRollback
	}
	s.value = value
	return nil
}

func RecordFloor(ctx context.Context, store FloorStore, metadata Metadata, now time.Time) error {
	if store == nil {
		return errors.New("update: floor store is required")
	}
	current, err := store.Load(ctx)
	if err != nil {
		return err
	}
	if metadata.Version <= current.MetadataVersion {
		return ErrRollback
	}
	return store.Save(ctx, Floor{MetadataVersion: metadata.Version, ProductVersion: metadata.Identity.ProductVersion, BuildTag: metadata.Identity.BuildTag, ArtifactDigest: metadata.Identity.ArtifactDigest, UpdatedAt: now.UTC()})
}
