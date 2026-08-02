package executioncontract

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func validRequest() ExecutionRequest {
	return ExecutionRequest{
		ContractVersion:     RequestContractVersion,
		ExecutionID:         "execution-1",
		CorrelationID:       "run-1",
		UnitID:              "unit-1",
		RepositoryProfileID: "repo-1",
		BaseSHA:             "0123456789abcdef0123456789abcdef01234567",
		RootScope:           "src",
		Branch:              "delivery/run-1/unit-1",
		Worktree:            ".kovan/worktrees/unit-1",
		AccessMode:          AccessWorktreeWrite,
		RoleProfile:         "implementer",
		SkillBundle:         BundleRef{ID: "skill-1", Digest: "sha256-skill", Version: "1.0.0"},
		ContextBundle:       BundleRef{ID: "context-1", Digest: "sha256-context"},
		OutputSchema:        json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}}}`),
		ToolPolicy:          Policy{AllowedTools: []string{"read", "write"}, NetworkAccess: false},
		TimeoutSeconds:      300,
		Cancellation:        CancelCheckpointThenStop,
		IdempotencyKey:      "idempotency-1",
	}
}

func TestValidateRequestRequiresExplicitSafeBoundary(t *testing.T) {
	request := validRequest()
	if err := ValidateRequest(request); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	request.ToolPolicy.SecretValueAccess = true
	if !errors.Is(ValidateRequest(request), ErrInvalidRequest) {
		t.Fatal("secret access was not rejected")
	}
	request = validRequest()
	request.OutputSchema = json.RawMessage(`["not","an","object"]`)
	if !errors.Is(ValidateRequest(request), ErrInvalidRequest) {
		t.Fatal("non-object output schema was not rejected")
	}
	request = validRequest()
	request.BaseSHA = "not-a-sha"
	if !errors.Is(ValidateRequest(request), ErrInvalidRequest) {
		t.Fatal("invalid base sha was not rejected")
	}
}

func TestValidateRequestRejectsTraversalScopesAndPaths(t *testing.T) {
	request := validRequest()
	request.RootScope = "src/../secrets"
	if !errors.Is(ValidateRequest(request), ErrInvalidRequest) {
		t.Fatal("traversal root scope was accepted")
	}
	request = validRequest()
	result := ExecutionResult{
		ContractVersion: ResultContractVersion,
		ExecutionID:     request.ExecutionID,
		State:           StateCompleted,
		Summary:         "done",
		Output:          json.RawMessage(`{"summary":"done"}`),
		CandidateSHA:    request.BaseSHA,
		ChangedPaths:    []string{"src/../secrets.txt"},
		FinishedAt:      time.Now().UTC(),
	}
	if !errors.Is(ValidateResult(request, result), ErrInvalidResult) {
		t.Fatal("traversal changed path was accepted")
	}
}

func TestValidateResultRejectsUnstructuredOrUncommittedCompletion(t *testing.T) {
	request := validRequest()
	result := ExecutionResult{
		ContractVersion: ResultContractVersion,
		ExecutionID:     request.ExecutionID,
		State:           StateCompleted,
		Summary:         "done",
		Output:          json.RawMessage(`{"summary":"done"}`),
		FinishedAt:      time.Now().UTC(),
	}
	if !errors.Is(ValidateResult(request, result), ErrInvalidResult) {
		t.Fatal("completed result without candidate sha was accepted")
	}
	result.CandidateSHA = request.BaseSHA
	if err := ValidateResult(request, result); err != nil {
		t.Fatalf("valid completed result rejected: %v", err)
	}
	result.Output = json.RawMessage(`"free form prose"`)
	if !errors.Is(ValidateResult(request, result), ErrInvalidResult) {
		t.Fatal("unstructured result was accepted")
	}
}

func TestStoreInterfaceRemainsTransportNeutral(t *testing.T) {
	var _ Store = fakeStore{}
	_ = context.Background()
}

type fakeStore struct{}

func (fakeStore) CreateExecution(context.Context, ExecutionRequest, time.Time) (ExecutionRecord, error) {
	return ExecutionRecord{}, nil
}
func (fakeStore) GetExecution(context.Context, string) (ExecutionRecord, error) {
	return ExecutionRecord{}, nil
}
func (fakeStore) SaveExecutionResult(context.Context, string, int64, ExecutionResult, time.Time) (ExecutionRecord, error) {
	return ExecutionRecord{}, nil
}
func (fakeStore) RecoverExecutions(context.Context, time.Time) ([]ExecutionRecord, error) {
	return nil, nil
}
func (fakeStore) AcquireResourceLease(context.Context, ResourceLeaseRequest, time.Time) (ResourceLease, error) {
	return ResourceLease{}, nil
}
func (fakeStore) HeartbeatResourceLease(context.Context, string, string, int, time.Time) (ResourceLease, error) {
	return ResourceLease{}, nil
}
func (fakeStore) ReleaseResourceLease(context.Context, string, string, time.Time) error {
	return nil
}
func (fakeStore) ExpiredResourceLeases(context.Context, time.Time) ([]ResourceLease, error) {
	return nil, nil
}
