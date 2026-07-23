package gitbroker

import (
	"context"
	"time"
)

type Checkpoint struct {
	ID           string
	OperationID  string
	RepositoryID string
	WorktreeID   string
	BaseSHA      string
	TreeSHA      string
	CommitSHA    string
	CreatedAt    time.Time
}

func (c Checkpoint) Validate() error {
	if !validID(c.ID) || !validID(c.OperationID) || !validID(c.RepositoryID) ||
		(c.WorktreeID != "" && !validID(c.WorktreeID)) || !validGitObjectID(c.BaseSHA) ||
		!validGitObjectID(c.TreeSHA) || !validGitObjectID(c.CommitSHA) || c.CreatedAt.IsZero() {
		return ErrInvalidOperation
	}
	return nil
}

type CheckpointStore interface {
	SaveCheckpoint(context.Context, Checkpoint) error
}

type MemoryCheckpointStore struct{ Values []Checkpoint }

func (s *MemoryCheckpointStore) SaveCheckpoint(ctx context.Context, value Checkpoint) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := value.Validate(); err != nil {
		return err
	}
	s.Values = append(s.Values, value)
	return nil
}

var _ CheckpointStore = (*MemoryCheckpointStore)(nil)
