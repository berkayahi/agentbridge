// Package localtask defines transport-neutral work requested on one device.
package localtask

import (
	"errors"
	"strings"
	"time"
)

const maxIDBytes = 128

var (
	ErrInvalidInput      = errors.New("localtask: invalid input")
	ErrActiveExecution   = errors.New("localtask: execution already active")
	ErrExecutionMismatch = errors.New("localtask: execution does not match active execution")
)

// LocalTask is a durable request that can have a history of executions.
// Its active execution is a single-valued invariant; past executions live in
// the execution repository rather than in this record.
type LocalTask struct {
	id                string
	title             string
	prompt            string
	activeExecutionID string
	createdAt         time.Time
}

// NewInput contains the immutable values required to create a local task.
type NewInput struct {
	ID        string
	Title     string
	Prompt    string
	CreatedAt time.Time
}

func New(input NewInput) (LocalTask, error) {
	if !validID(input.ID) || strings.TrimSpace(input.Title) == "" || strings.TrimSpace(input.Prompt) == "" || input.CreatedAt.IsZero() {
		return LocalTask{}, ErrInvalidInput
	}
	return LocalTask{
		id:        input.ID,
		title:     input.Title,
		prompt:    input.Prompt,
		createdAt: input.CreatedAt.UTC(),
	}, nil
}

func (t LocalTask) ID() string           { return t.id }
func (t LocalTask) Title() string        { return t.title }
func (t LocalTask) Prompt() string       { return t.prompt }
func (t LocalTask) CreatedAt() time.Time { return t.createdAt }

// ActiveExecutionID is empty when no execution currently owns this task.
func (t LocalTask) ActiveExecutionID() string { return t.activeExecutionID }

// BeginExecution returns a copy with one active execution. Callers must first
// finish the existing execution before assigning another one.
func (t LocalTask) BeginExecution(executionID string) (LocalTask, error) {
	if !validID(executionID) {
		return LocalTask{}, ErrInvalidInput
	}
	if t.activeExecutionID != "" {
		return LocalTask{}, ErrActiveExecution
	}
	t.activeExecutionID = executionID
	return t, nil
}

// FinishExecution clears only the matching active execution.
func (t LocalTask) FinishExecution(executionID string) (LocalTask, error) {
	if t.activeExecutionID == "" || t.activeExecutionID != executionID {
		return LocalTask{}, ErrExecutionMismatch
	}
	t.activeExecutionID = ""
	return t, nil
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
