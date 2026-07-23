// Package session defines a provider-opaque runtime session binding.
package session

import (
	"errors"
	"time"
)

const maxIDBytes = 128

var (
	ErrInvalidInput     = errors.New("session: invalid input")
	ErrTaskAlreadyBound = errors.New("session: another task is active")
	ErrTaskMismatch     = errors.New("session: task does not match active binding")
)

// Session is bound to one runtime and repository for its whole lifetime. It
// can be standalone or have one active task at a time.
type Session struct {
	id           string
	runtimeID    string
	repositoryID string
	activeTaskID string
	createdAt    time.Time
}

type NewInput struct {
	ID           string
	RuntimeID    string
	RepositoryID string
	CreatedAt    time.Time
}

func New(input NewInput) (Session, error) {
	if !validID(input.ID) || !validID(input.RuntimeID) || !validID(input.RepositoryID) || input.CreatedAt.IsZero() {
		return Session{}, ErrInvalidInput
	}
	return Session{id: input.ID, runtimeID: input.RuntimeID, repositoryID: input.RepositoryID, createdAt: input.CreatedAt.UTC()}, nil
}

func (s Session) ID() string           { return s.id }
func (s Session) RuntimeID() string    { return s.runtimeID }
func (s Session) RepositoryID() string { return s.repositoryID }
func (s Session) ActiveTaskID() string { return s.activeTaskID }
func (s Session) CreatedAt() time.Time { return s.createdAt }

func (s Session) BindTask(taskID string) (Session, error) {
	if !validID(taskID) {
		return Session{}, ErrInvalidInput
	}
	if s.activeTaskID != "" && s.activeTaskID != taskID {
		return Session{}, ErrTaskAlreadyBound
	}
	s.activeTaskID = taskID
	return s, nil
}

func (s Session) ReleaseTask(taskID string) (Session, error) {
	if s.activeTaskID == "" || s.activeTaskID != taskID {
		return Session{}, ErrTaskMismatch
	}
	s.activeTaskID = ""
	return s, nil
}

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
