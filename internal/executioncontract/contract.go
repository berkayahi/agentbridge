// Package executioncontract defines the product-neutral boundary between a
// controller and an execution host. It carries correlation and repository
// facts, but no product-specific orchestration or presentation concepts.
package executioncontract

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
)

const (
	RequestContractVersion = "agentbridge.execution-request.v1"
	ResultContractVersion  = "agentbridge.execution-result.v1"
)

var (
	ErrInvalidRequest      = errors.New("executioncontract: invalid request")
	ErrInvalidResult       = errors.New("executioncontract: invalid result")
	ErrInvalidLease        = errors.New("executioncontract: invalid lease")
	ErrNotFound            = errors.New("executioncontract: not found")
	ErrConflict            = errors.New("executioncontract: conflict")
	ErrLeaseHeld           = errors.New("executioncontract: resource lease held")
	ErrInvalidState        = errors.New("executioncontract: invalid state")
	ErrIdempotencyConflict = errors.New("executioncontract: idempotency conflict")
)

type AccessMode string

const (
	AccessReadOnly      AccessMode = "read_only"
	AccessWorktreeWrite AccessMode = "worktree_write"
)

type ExecutionState string

const (
	StateAccepted      ExecutionState = "accepted"
	StateRunning       ExecutionState = "running"
	StateCompleted     ExecutionState = "completed"
	StateFailed        ExecutionState = "failed"
	StateCanceled      ExecutionState = "canceled"
	StateHumanRequired ExecutionState = "human_required"
)

type LeaseMode string

const (
	LeaseExclusive LeaseMode = "exclusive"
	LeaseShared    LeaseMode = "shared"
)

type CancellationPolicy string

const (
	CancelCooperative        CancellationPolicy = "cooperative"
	CancelCheckpointThenStop CancellationPolicy = "checkpoint_then_stop"
)

type BundleRef struct {
	ID      string `json:"id"`
	Digest  string `json:"digest"`
	Version string `json:"version,omitempty"`
}

type Policy struct {
	AllowedTools          []string `json:"allowed_tools,omitempty"`
	NetworkAccess         bool     `json:"network_access"`
	SecretValueAccess     bool     `json:"secret_value_access"`
	ExternalStateMutation bool     `json:"external_state_mutation"`
}

// ExecutionRequest is the complete typed handoff for one isolated unit of
// work. The controller owns meaning; AgentBridge owns validation, durability,
// fencing, and execution-host capabilities.
type ExecutionRequest struct {
	ContractVersion     string             `json:"contract_version"`
	ExecutionID         string             `json:"execution_id"`
	CorrelationID       string             `json:"correlation_id"`
	UnitID              string             `json:"unit_id"`
	RepositoryProfileID string             `json:"repository_profile_id"`
	BaseSHA             string             `json:"base_sha"`
	RootScope           string             `json:"root_scope"`
	Branch              string             `json:"branch"`
	Worktree            string             `json:"worktree"`
	AccessMode          AccessMode         `json:"access_mode"`
	RoleProfile         string             `json:"role_profile"`
	SkillBundle         BundleRef          `json:"skill_bundle"`
	ContextBundle       BundleRef          `json:"context_bundle"`
	OutputSchema        json.RawMessage    `json:"output_schema"`
	ToolPolicy          Policy             `json:"tool_policy"`
	TimeoutSeconds      int                `json:"timeout_seconds"`
	Cancellation        CancellationPolicy `json:"cancellation_policy"`
	IdempotencyKey      string             `json:"idempotency_key"`
}

type EvidenceRef struct {
	ID     string `json:"id"`
	Digest string `json:"digest"`
	Kind   string `json:"kind,omitempty"`
}

type CheckResult struct {
	Name    string `json:"name"`
	Command string `json:"command,omitempty"`
	Status  string `json:"status"`
	Summary string `json:"summary,omitempty"`
}

type ExecutionResult struct {
	ContractVersion string          `json:"contract_version"`
	ExecutionID     string          `json:"execution_id"`
	State           ExecutionState  `json:"state"`
	Summary         string          `json:"summary"`
	Output          json.RawMessage `json:"output"`
	ChangedPaths    []string        `json:"changed_paths,omitempty"`
	CandidateSHA    string          `json:"candidate_sha,omitempty"`
	Evidence        []EvidenceRef   `json:"evidence,omitempty"`
	Checks          []CheckResult   `json:"checks,omitempty"`
	Blockers        []string        `json:"blockers,omitempty"`
	HumanRequests   []string        `json:"human_requests,omitempty"`
	FailureCategory string          `json:"failure_category,omitempty"`
	FinishedAt      time.Time       `json:"finished_at"`
}

type ExecutionRecord struct {
	Request       ExecutionRequest `json:"request"`
	Result        *ExecutionResult `json:"result,omitempty"`
	State         ExecutionState   `json:"state"`
	Revision      int64            `json:"revision"`
	RecoveryCount int              `json:"recovery_count"`
	CreatedAt     time.Time        `json:"created_at"`
	UpdatedAt     time.Time        `json:"updated_at"`
}

type ResourceLeaseRequest struct {
	LeaseID          string    `json:"lease_id"`
	ResourceKey      string    `json:"resource_key"`
	OwnerExecutionID string    `json:"owner_execution_id"`
	Mode             LeaseMode `json:"mode"`
	TTLSeconds       int       `json:"ttl_seconds"`
}

type ResourceLease struct {
	LeaseID          string    `json:"lease_id"`
	ResourceKey      string    `json:"resource_key"`
	OwnerExecutionID string    `json:"owner_execution_id"`
	Mode             LeaseMode `json:"mode"`
	AcquiredAt       time.Time `json:"acquired_at"`
	HeartbeatAt      time.Time `json:"heartbeat_at"`
	ExpiresAt        time.Time `json:"expires_at"`
	Revision         int64     `json:"revision"`
}

type Store interface {
	CreateExecution(context.Context, ExecutionRequest, time.Time) (ExecutionRecord, error)
	GetExecution(context.Context, string) (ExecutionRecord, error)
	SaveExecutionResult(context.Context, string, int64, ExecutionResult, time.Time) (ExecutionRecord, error)
	RecoverExecutions(context.Context, time.Time) ([]ExecutionRecord, error)
	AcquireResourceLease(context.Context, ResourceLeaseRequest, time.Time) (ResourceLease, error)
	HeartbeatResourceLease(context.Context, string, string, int, time.Time) (ResourceLease, error)
	ReleaseResourceLease(context.Context, string, string, time.Time) error
	ExpiredResourceLeases(context.Context, time.Time) ([]ResourceLease, error)
}

func ValidateRequest(request ExecutionRequest) error {
	if request.ContractVersion != RequestContractVersion ||
		!validToken(request.ExecutionID) ||
		!validToken(request.CorrelationID) ||
		!validToken(request.UnitID) ||
		!validToken(request.RepositoryProfileID) ||
		!validSHA(request.BaseSHA) ||
		!validScope(request.RootScope) ||
		!validToken(request.RoleProfile) ||
		!validBundle(request.SkillBundle) ||
		!validBundle(request.ContextBundle) ||
		!validJSONSchema(request.OutputSchema) ||
		request.TimeoutSeconds < 1 || request.TimeoutSeconds > 24*60*60 ||
		!validToken(request.IdempotencyKey) ||
		request.ToolPolicy.SecretValueAccess ||
		len(request.ToolPolicy.AllowedTools) == 0 {
		return ErrInvalidRequest
	}
	switch request.AccessMode {
	case AccessReadOnly, AccessWorktreeWrite:
	default:
		return ErrInvalidRequest
	}
	if request.AccessMode == AccessWorktreeWrite && (!validToken(request.Branch) || !validScope(request.Worktree)) {
		return ErrInvalidRequest
	}
	switch request.Cancellation {
	case CancelCooperative, CancelCheckpointThenStop:
	default:
		return ErrInvalidRequest
	}
	for _, tool := range request.ToolPolicy.AllowedTools {
		if !validToken(tool) {
			return ErrInvalidRequest
		}
	}
	return nil
}

func ValidateResult(request ExecutionRequest, result ExecutionResult) error {
	if err := ValidateRequest(request); err != nil {
		return fmt.Errorf("%w: request: %v", ErrInvalidResult, err)
	}
	if result.ContractVersion != ResultContractVersion ||
		result.ExecutionID != request.ExecutionID ||
		!validSummary(result.Summary) ||
		!validStructuredJSON(result.Output) ||
		result.FinishedAt.IsZero() {
		return ErrInvalidResult
	}
	switch result.State {
	case StateCompleted, StateFailed, StateCanceled, StateHumanRequired:
	default:
		return ErrInvalidResult
	}
	if result.State == StateCompleted && !validSHA(result.CandidateSHA) {
		return ErrInvalidResult
	}
	for _, changed := range result.ChangedPaths {
		if !validRelativePath(changed) {
			return ErrInvalidResult
		}
	}
	return nil
}

func ValidateLeaseRequest(request ResourceLeaseRequest) error {
	if !validToken(request.LeaseID) || !validToken(request.OwnerExecutionID) ||
		!validResource(request.ResourceKey) || request.TTLSeconds < 1 || request.TTLSeconds > 24*60*60 {
		return ErrInvalidLease
	}
	switch request.Mode {
	case LeaseExclusive, LeaseShared:
		return nil
	default:
		return ErrInvalidLease
	}
}

func (request ExecutionRequest) Digest() (string, error) {
	if err := ValidateRequest(request); err != nil {
		return "", err
	}
	body, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("marshal execution request: %w", err)
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

func validBundle(bundle BundleRef) bool {
	return validToken(bundle.ID) && validToken(bundle.Digest)
}

func validJSONSchema(value json.RawMessage) bool {
	if !validStructuredJSON(value) {
		return false
	}
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(value, &schema); err != nil {
		return false
	}
	_, hasType := schema["type"]
	return hasType
}

func validStructuredJSON(value json.RawMessage) bool {
	if len(value) == 0 || !json.Valid(value) || string(value) == "null" {
		return false
	}
	var object map[string]json.RawMessage
	return json.Unmarshal(value, &object) == nil
}

func validSHA(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validScope(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || path.IsAbs(value) {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == ".." {
			return false
		}
	}
	return true
}

func validRelativePath(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || path.IsAbs(value) || value == "." {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == ".." {
			return false
		}
	}
	return true
}

func validResource(value string) bool {
	return validToken(value) && !strings.ContainsAny(value, "\r\n")
}

func validSummary(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= 4096 && !strings.ContainsRune(value, 0)
}

func validToken(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 256 || strings.ContainsAny(value, "\r\n\x00") {
		return false
	}
	return true
}
