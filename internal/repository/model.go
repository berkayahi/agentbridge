// Package repository defines opaque, locally approved repository bindings.
package repository

import (
	"errors"
	"strings"
	"time"

	"github.com/berkayahi/agentbridge/internal/gitref"
)

const maxIDBytes = 128

var ErrInvalidInput = errors.New("repository: invalid input")

// Binding identifies a locally approved repository without exposing a local
// filesystem path to transport or presentation layers.
type Binding struct {
	id        string
	remoteURL string
	createdAt time.Time
}

type BindingInput struct {
	ID        string
	RemoteURL string
	CreatedAt time.Time
}

func NewBinding(input BindingInput) (Binding, error) {
	if !validID(input.ID) || strings.TrimSpace(input.RemoteURL) == "" || input.CreatedAt.IsZero() {
		return Binding{}, ErrInvalidInput
	}
	return Binding{id: input.ID, remoteURL: input.RemoteURL, createdAt: input.CreatedAt.UTC()}, nil
}

func (b Binding) ID() string           { return b.id }
func (b Binding) RemoteURL() string    { return b.remoteURL }
func (b Binding) CreatedAt() time.Time { return b.createdAt }

// Checkpoint is exact Git evidence suitable for later cross-device handoff.
type Checkpoint struct {
	id           string
	repositoryID string
	commitSHA    string
	remoteRef    string
	createdAt    time.Time
}

type CheckpointInput struct {
	ID           string
	RepositoryID string
	CommitSHA    string
	RemoteRef    string
	CreatedAt    time.Time
}

func NewCheckpoint(input CheckpointInput) (Checkpoint, error) {
	if !validID(input.ID) || !validID(input.RepositoryID) || !validGitObjectID(input.CommitSHA) || !validRemoteRef(input.RemoteRef) || input.CreatedAt.IsZero() {
		return Checkpoint{}, ErrInvalidInput
	}
	return Checkpoint{
		id: input.ID, repositoryID: input.RepositoryID, commitSHA: strings.ToLower(input.CommitSHA),
		remoteRef: input.RemoteRef, createdAt: input.CreatedAt.UTC(),
	}, nil
}

func (c Checkpoint) ID() string           { return c.id }
func (c Checkpoint) RepositoryID() string { return c.repositoryID }
func (c Checkpoint) CommitSHA() string    { return c.commitSHA }
func (c Checkpoint) RemoteRef() string    { return c.remoteRef }
func (c Checkpoint) CreatedAt() time.Time { return c.createdAt }

func validID(value string) bool {
	if value == "" || len(value) > maxIDBytes {
		return false
	}
	for _, runeValue := range value {
		if !((runeValue >= 'a' && runeValue <= 'z') ||
			(runeValue >= 'A' && runeValue <= 'Z') ||
			(runeValue >= '0' && runeValue <= '9') ||
			runeValue == '-' || runeValue == '_') {
			return false
		}
	}
	return true
}

func validGitObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, runeValue := range value {
		if !((runeValue >= 'a' && runeValue <= 'f') || (runeValue >= 'A' && runeValue <= 'F') || (runeValue >= '0' && runeValue <= '9')) {
			return false
		}
	}
	return true
}

func validRemoteRef(value string) bool { return gitref.Valid(value) }
