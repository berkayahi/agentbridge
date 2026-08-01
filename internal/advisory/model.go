// Package advisory defines a provider-neutral, read-only structured execution
// boundary. It has no repository, product, workflow, or approval concepts.
package advisory

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

const (
	ContractVersion       = "advisory-session-v1"
	MaxPromptBytes        = 64 << 10
	MaxSchemaBytes        = 128 << 10
	MaxOutputBytes        = 256 << 10
	MaxContextItems       = 64
	MaxContextKeyBytes    = 256
	MaxContextValueBytes  = 64 << 10
	MaxContextTotalBytes  = 512 << 10
	MaxSchemaVersionBytes = 128
)

var (
	ErrInvalidRequest       = errors.New("advisory: invalid request")
	ErrNotConfigured        = errors.New("advisory: no eligible provider")
	ErrPolicyViolation      = errors.New("advisory: policy violation")
	ErrStructuredOutput     = errors.New("advisory: structured output validation failed")
	ErrProviderIdentity     = errors.New("advisory: provider identity mismatch")
	ErrProviderOutputBounds = errors.New("advisory: provider output exceeds bounds")
)

type ContextBundle struct {
	Items []ContextItem `json:"items"`
}

type ContextItem struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type WebResearchPolicy struct {
	Enabled    bool `json:"enabled"`
	MaxSources int  `json:"max_sources,omitempty"`
	MaxBytes   int  `json:"max_bytes,omitempty"`
}

type SessionRequest struct {
	ProviderID     string            `json:"provider_id,omitempty"`
	ModelID        string            `json:"model_id,omitempty"`
	Prompt         string            `json:"prompt"`
	Context        ContextBundle     `json:"context"`
	OutputSchema   json.RawMessage   `json:"output_schema"`
	SchemaVersion  string            `json:"schema_version"`
	WebResearch    WebResearchPolicy `json:"web_research,omitempty"`
	IdempotencyKey string            `json:"idempotency_key"`
}

// ExecutionPolicy is fixed by the boundary. Providers receive it as an
// attestation input and cannot widen it through caller-controlled fields.
type ExecutionPolicy struct {
	ReadOnly           bool `json:"read_only"`
	RepositoryWrites   bool `json:"repository_writes"`
	BranchMutation     bool `json:"branch_mutation"`
	WorktreeMutation   bool `json:"worktree_mutation"`
	GitIntegration     bool `json:"git_integration"`
	SecretValueAccess  bool `json:"secret_value_access"`
	DecisionMutation   bool `json:"decision_mutation"`
	HumanApproval      bool `json:"human_approval"`
	WebResearchAllowed bool `json:"web_research_allowed"`
}

type ExecutionRequest struct {
	ContractVersion    string            `json:"contract_version"`
	ExecutionSessionID string            `json:"execution_session_id"`
	ProviderID         string            `json:"provider_id"`
	ModelID            string            `json:"model_id"`
	Prompt             string            `json:"prompt"`
	Context            ContextBundle     `json:"context"`
	OutputSchema       json.RawMessage   `json:"output_schema"`
	SchemaVersion      string            `json:"schema_version"`
	Policy             ExecutionPolicy   `json:"policy"`
	WebResearch        WebResearchPolicy `json:"web_research"`
}

type ExecutionResult struct {
	ProviderID string
	ModelID    string
	Output     []byte
}

type ProviderCapability struct {
	ID                string `json:"id"`
	AdvisorySessions  bool   `json:"advisory_sessions"`
	ReadOnly          bool   `json:"read_only"`
	StructuredOutput  bool   `json:"structured_output"`
	WebResearch       bool   `json:"web_research"`
	RepositoryWrites  bool   `json:"repository_writes"`
	BranchMutation    bool   `json:"branch_mutation"`
	WorktreeMutation  bool   `json:"worktree_mutation"`
	GitIntegration    bool   `json:"git_integration"`
	SecretValueAccess bool   `json:"secret_value_access"`
	DecisionMutation  bool   `json:"decision_mutation"`
	HumanApproval     bool   `json:"human_approval"`
}

type ProviderProfile struct {
	ID         string             `json:"id"`
	ModelID    string             `json:"model_id,omitempty"`
	Available  bool               `json:"available"`
	Capability ProviderCapability `json:"capability"`
}

type ProviderCatalog interface {
	ProviderProfiles(context.Context) ([]ProviderProfile, error)
}

type Provider interface {
	Capability() ProviderCapability
	Execute(context.Context, ExecutionRequest) (ExecutionResult, error)
}

type ExecutionReceipt struct {
	ReceiptID          string    `json:"receipt_id"`
	ExecutionSessionID string    `json:"execution_session_id"`
	ProviderID         string    `json:"provider_id"`
	ModelID            string    `json:"model_id"`
	ContextDigest      string    `json:"context_digest"`
	PromptDigest       string    `json:"prompt_digest"`
	OutputDigest       string    `json:"output_digest"`
	SchemaVersion      string    `json:"schema_version"`
	ContractVersion    string    `json:"contract_version"`
	StartedAt          time.Time `json:"started_at"`
	CompletedAt        time.Time `json:"completed_at"`
	Status             string    `json:"status"`
}

type SessionResponse struct {
	ContractVersion string           `json:"contract_version"`
	Output          json.RawMessage  `json:"output"`
	Receipt         ExecutionReceipt `json:"receipt"`
}
