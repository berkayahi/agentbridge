package workmodel

import (
	"encoding/json"
	"strings"
	"time"
)

const DefaultTitleRunes = 80

type Provider string

const (
	CodexSubscription  Provider = "codex"
	ClaudeSubscription Provider = "claude"
)

func (p Provider) Valid() bool {
	return p == CodexSubscription || p == ClaudeSubscription
}

type Task struct {
	ID                string
	RepoProfileID     string
	Title             string
	Prompt            string
	State             State
	Provider          Provider
	TelegramChatID    int64
	TelegramMessageID int64
	BaseSHA           string
	WorktreePath      string
	ProviderSessionID string
	ProviderThreadID  string
	CommitSHA         string
	PushRef           string
	DeploymentURL     string
	FailureReason     string
	Revision          int64
	CreatedAt         time.Time
	UpdatedAt         time.Time
	StartedAt         *time.Time
	FinishedAt        *time.Time
}

type Attachment struct {
	ID          string
	TaskID      string
	Kind        string
	Name        string
	MediaType   string
	StoragePath string
	SizeBytes   int64
	SHA256      string
	CreatedAt   time.Time
}

type Session struct {
	ID                string
	TaskID            string
	Provider          Provider
	ProviderSessionID string
	ProviderThreadID  string
	Status            string
	Resumable         bool
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type ApprovalStatus string

const (
	ApprovalPending  ApprovalStatus = "pending"
	ApprovalApproved ApprovalStatus = "approved"
	ApprovalRejected ApprovalStatus = "rejected"
	ApprovalExpired  ApprovalStatus = "expired"
)

type Approval struct {
	ID              string
	TaskID          string
	Kind            string
	Status          ApprovalStatus
	RequestPayload  json.RawMessage
	DecisionPayload json.RawMessage
	RequestedAt     time.Time
	ExpiresAt       *time.Time
	ResolvedAt      *time.Time
}

// AuthIncident is the durable, secret-free authentication incident record.
type AuthIncident struct {
	ID         string
	Provider   Provider
	Status     string
	Detail     json.RawMessage
	DetectedAt time.Time
	ResolvedAt *time.Time
}

// Title normalizes whitespace and deterministically truncates text by Unicode runes.
func Title(text string, maxRunes int) string {
	if maxRunes <= 0 {
		maxRunes = DefaultTitleRunes
	}
	normalized := strings.Join(strings.Fields(text), " ")
	runes := []rune(normalized)
	if len(runes) <= maxRunes {
		return normalized
	}
	if maxRunes == 1 {
		return "…"
	}
	return string(runes[:maxRunes-1]) + "…"
}

// Elapsed returns the task's duration using now as the clock for unfinished tasks.
func (t Task) Elapsed(now time.Time) time.Duration {
	start := t.CreatedAt
	if t.StartedAt != nil {
		start = *t.StartedAt
	}
	end := now
	if t.FinishedAt != nil {
		end = *t.FinishedAt
	}
	if end.Before(start) {
		return 0
	}
	return end.Sub(start)
}
