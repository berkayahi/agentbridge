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
	ContractVersion               = "advisory-session-v1"
	CapabilityContractVersion     = "provider-capabilities-v1"
	StructuredOutputSchemaVersion = "json-schema-subset-v1"
	StructuredOutputMode          = "strict-json-schema"
	MaxPromptBytes                = 64 << 10
	MaxSchemaBytes                = 128 << 10
	MaxOutputBytes                = 256 << 10
	MaxContextItems               = 64
	MaxContextKeyBytes            = 256
	MaxContextValueBytes          = 64 << 10
	MaxContextTotalBytes          = 512 << 10
	MaxSchemaVersionBytes         = 128
)

var (
	ErrInvalidRequest       = errors.New("advisory: invalid request")
	ErrNotConfigured        = errors.New("advisory: no eligible provider")
	ErrPolicyViolation      = errors.New("advisory: policy violation")
	ErrStructuredOutput     = errors.New("advisory: structured output validation failed")
	ErrProviderIdentity     = errors.New("advisory: provider identity mismatch")
	ErrProviderOutputBounds = errors.New("advisory: provider output exceeds bounds")
	ErrProviderExecution    = errors.New("advisory: provider execution failed")
	ErrReceiptIntegrity     = errors.New("advisory: receipt integrity validation failed")
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
	ReadOnly              bool `json:"read_only"`
	RepositoryWrites      bool `json:"repository_writes"`
	BranchMutation        bool `json:"branch_mutation"`
	WorktreeMutation      bool `json:"worktree_mutation"`
	GitIntegration        bool `json:"git_integration"`
	SecretValueAccess     bool `json:"secret_value_access"`
	ExternalStateMutation bool `json:"external_state_mutation"`
	HumanApproval         bool `json:"human_approval"`
	WebResearchAllowed    bool `json:"web_research_allowed"`
}

type ExecutionRequest struct {
	ContractVersion    string            `json:"contract_version"`
	ExecutionSessionID string            `json:"execution_session_id"`
	ProviderID         string            `json:"provider_id"`
	ModelID            string            `json:"model_id"`
	Prompt             string            `json:"prompt"`
	Context            ContextBundle     `json:"context"`
	OutputSchema       json.RawMessage   `json:"output_schema"`
	SchemaDigest       string            `json:"schema_digest"`
	SchemaVersion      string            `json:"schema_version"`
	Policy             ExecutionPolicy   `json:"policy"`
	PolicyDigest       string            `json:"policy_digest"`
	WebResearch        WebResearchPolicy `json:"web_research"`
}

type ExecutionResult struct {
	ProviderID string
	ModelID    string
	Output     []byte
	// Stdout and Stderr are optional provider-side diagnostics. They are
	// inspected by the same advisory pipeline and are never copied into a
	// receipt or returned to a caller.
	Stdout []byte
	Stderr []byte
}

type StructuredOutputCapability struct {
	Supported     bool   `json:"supported"`
	Mode          string `json:"mode,omitempty"`
	SchemaVersion string `json:"schema_version,omitempty"`
}

type WebResearchCapability struct {
	Available  bool `json:"available"`
	Enforced   bool `json:"enforced"`
	MaxSources int  `json:"max_sources,omitempty"`
	MaxBytes   int  `json:"max_bytes,omitempty"`
}

// EffectivePolicy is the provider's truthful, generic policy after all
// launchers and protocol adapters have applied their restrictions. It is
// deliberately not a product or workflow policy.
type EffectivePolicy struct {
	FilesystemRead        bool `json:"filesystem_read"`
	RepositoryRead        bool `json:"repository_read"`
	RepositoryWrite       bool `json:"repository_write"`
	GitMutation           bool `json:"git_mutation"`
	NetworkAccess         bool `json:"network_access"`
	SecretValueAccess     bool `json:"secret_value_access"`
	ExternalStateMutation bool `json:"external_state_mutation"`
	ApprovalRequests      bool `json:"approval_requests"`
}

type ProviderProtocol struct {
	ProtocolVersion string `json:"protocol_version"`
	SchemaVersion   string `json:"schema_version"`
}

// ProviderCapability is the one wire contract exchanged between AgentBridge
// producers and advisory consumers. A consumer must reject an unsupported
// contract instead of inferring capabilities from provider names or silently
// accepting a legacy shape.
type ProviderCapability struct {
	ContractVersion   string                     `json:"contract_version"`
	ProviderID        string                     `json:"provider_id"`
	Eligible          bool                       `json:"eligible"`
	ReasonCode        string                     `json:"reason_code,omitempty"`
	Reason            string                     `json:"reason,omitempty"`
	AdvisoryExecution bool                       `json:"advisory_execution"`
	StructuredOutput  StructuredOutputCapability `json:"structured_output"`
	WebResearch       WebResearchCapability      `json:"web_research"`
	EffectivePolicy   EffectivePolicy            `json:"effective_policy"`
	Protocol          ProviderProtocol           `json:"protocol"`
}

func ReadOnlyCapability(providerID string) ProviderCapability {
	return ProviderCapability{
		ContractVersion: CapabilityContractVersion, ProviderID: providerID,
		Eligible: true, AdvisoryExecution: true,
		StructuredOutput: StructuredOutputCapability{
			Supported: true, Mode: StructuredOutputMode, SchemaVersion: StructuredOutputSchemaVersion,
		},
		WebResearch:     WebResearchCapability{},
		EffectivePolicy: EffectivePolicy{FilesystemRead: true},
		Protocol:        ProviderProtocol{ProtocolVersion: ContractVersion, SchemaVersion: StructuredOutputSchemaVersion},
	}
}

func IneligibleCapability(providerID, reasonCode, reason string) ProviderCapability {
	capability := ReadOnlyCapability(providerID)
	capability.Eligible = false
	capability.AdvisoryExecution = false
	capability.StructuredOutput = StructuredOutputCapability{}
	capability.EffectivePolicy = EffectivePolicy{}
	capability.ReasonCode = reasonCode
	capability.Reason = reason
	return capability
}

func (c ProviderCapability) Valid() bool {
	return c.ContractVersion == CapabilityContractVersion && c.ProviderID != "" &&
		c.Protocol.ProtocolVersion == ContractVersion && c.Protocol.SchemaVersion == StructuredOutputSchemaVersion &&
		((c.AdvisoryExecution && c.Eligible && c.StructuredOutput.Supported && c.StructuredOutput.Mode == StructuredOutputMode && c.StructuredOutput.SchemaVersion == StructuredOutputSchemaVersion) ||
			(!c.AdvisoryExecution && !c.Eligible && c.ReasonCode != "" && c.Reason != ""))
}

func (c ProviderCapability) AdvisoryEligible(webResearch bool) bool {
	if !c.Valid() || !c.Eligible || !c.AdvisoryExecution ||
		!c.StructuredOutput.Supported || c.StructuredOutput.Mode != StructuredOutputMode ||
		c.EffectivePolicy.RepositoryWrite || c.EffectivePolicy.GitMutation ||
		c.EffectivePolicy.NetworkAccess || c.EffectivePolicy.SecretValueAccess ||
		c.EffectivePolicy.ExternalStateMutation || c.EffectivePolicy.ApprovalRequests {
		return false
	}
	if !webResearch {
		return true
	}
	return c.WebResearch.Available && c.WebResearch.Enforced && c.WebResearch.MaxSources > 0 && c.WebResearch.MaxBytes > 0
}

type ProviderProfile struct {
	ID           string             `json:"id"`
	ModelID      string             `json:"model_id,omitempty"`
	ModelIDs     []string           `json:"model_ids,omitempty"`
	ModelAliases []string           `json:"model_aliases,omitempty"`
	Available    bool               `json:"available"`
	Capabilities ProviderCapability `json:"capabilities"`
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
	SchemaDigest       string    `json:"schema_digest"`
	PolicyDigest       string    `json:"policy_digest"`
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
