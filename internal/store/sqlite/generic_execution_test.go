package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/executioncontract"
)

func genericExecutionRequest(id, idempotency string) executioncontract.ExecutionRequest {
	return executioncontract.ExecutionRequest{
		ContractVersion:     executioncontract.RequestContractVersion,
		ExecutionID:         id,
		CorrelationID:       "run-" + id,
		UnitID:              "unit-" + id,
		RepositoryProfileID: "repo-profile",
		BaseSHA:             "0123456789abcdef0123456789abcdef01234567",
		RootScope:           ".kovan",
		Branch:              "delivery/run-1/unit-1",
		Worktree:            ".kovan/worktrees/unit-1",
		AccessMode:          executioncontract.AccessWorktreeWrite,
		RoleProfile:         "implementer",
		SkillBundle:         executioncontract.BundleRef{ID: "skill", Digest: "skill-digest"},
		ContextBundle:       executioncontract.BundleRef{ID: "context", Digest: "context-digest"},
		OutputSchema:        json.RawMessage(`{"type":"object"}`),
		ToolPolicy:          executioncontract.Policy{AllowedTools: []string{"read", "write"}},
		TimeoutSeconds:      60,
		Cancellation:        executioncontract.CancelCooperative,
		IdempotencyKey:      idempotency,
	}
}

func TestGenericExecutionPersistsResultsRecoveryAndIdempotency(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "generic.db")
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	data, err := OpenV2Runtime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	request := genericExecutionRequest("execution-1", "idempotency-1")
	created, err := data.CreateExecution(ctx, request, now)
	if err != nil {
		t.Fatal(err)
	}
	if created.State != executioncontract.StateAccepted || created.Revision != 1 {
		t.Fatalf("created record = %#v", created)
	}
	replayed, err := data.CreateExecution(ctx, request, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Revision != created.Revision || replayed.CreatedAt != created.CreatedAt {
		t.Fatalf("idempotent replay changed record: %#v", replayed)
	}
	conflict := request
	conflict.ExecutionID = "execution-other"
	if _, err := data.CreateExecution(ctx, conflict, now); !errors.Is(err, executioncontract.ErrIdempotencyConflict) {
		t.Fatalf("idempotency conflict = %v", err)
	}

	recovered, err := data.RecoverExecutions(ctx, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 || recovered[0].RecoveryCount != 1 || recovered[0].Revision != 2 {
		t.Fatalf("recovery = %#v", recovered)
	}
	result := executioncontract.ExecutionResult{
		ContractVersion: executioncontract.ResultContractVersion,
		ExecutionID:     request.ExecutionID,
		State:           executioncontract.StateCompleted,
		Summary:         "candidate is ready",
		Output:          json.RawMessage(`{"status":"ok"}`),
		CandidateSHA:    request.BaseSHA,
		ChangedPaths:    []string{"internal/example.go"},
		FinishedAt:      now.Add(2 * time.Minute),
	}
	if _, err := data.SaveExecutionResult(ctx, request.ExecutionID, created.Revision, result, now.Add(2*time.Minute)); !errors.Is(err, executioncontract.ErrConflict) {
		t.Fatalf("stale result revision = %v", err)
	}
	finished, err := data.SaveExecutionResult(ctx, request.ExecutionID, recovered[0].Revision, result, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if finished.State != executioncontract.StateCompleted || finished.Result == nil {
		t.Fatalf("finished record = %#v", finished)
	}
	if err := data.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenV2Runtime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	persisted, err := reopened.GetExecution(ctx, request.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Result == nil || persisted.Result.CandidateSHA != request.BaseSHA {
		t.Fatalf("persisted result = %#v", persisted.Result)
	}
}

func TestResourceLeasesFenceWritersAndAllowSharedReaders(t *testing.T) {
	ctx := context.Background()
	data, err := OpenV2Runtime(ctx, filepath.Join(t.TempDir(), "leases.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	for _, id := range []string{"execution-1", "execution-2", "execution-3"} {
		if _, err := data.CreateExecution(ctx, genericExecutionRequest(id, "key-"+id), now); err != nil {
			t.Fatal(err)
		}
	}
	exclusive, err := data.AcquireResourceLease(ctx, executioncontract.ResourceLeaseRequest{
		LeaseID: "lease-1", ResourceKey: "worktree:repo:branch", OwnerExecutionID: "execution-1",
		Mode: executioncontract.LeaseExclusive, TTLSeconds: 30,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := data.AcquireResourceLease(ctx, executioncontract.ResourceLeaseRequest{
		LeaseID: "lease-2", ResourceKey: exclusive.ResourceKey, OwnerExecutionID: "execution-2",
		Mode: executioncontract.LeaseShared, TTLSeconds: 30,
	}, now); !errors.Is(err, executioncontract.ErrLeaseHeld) {
		t.Fatalf("shared reader bypassed writer lease: %v", err)
	}
	if err := data.ReleaseResourceLease(ctx, exclusive.LeaseID, exclusive.OwnerExecutionID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := data.AcquireResourceLease(ctx, executioncontract.ResourceLeaseRequest{
		LeaseID: "lease-2", ResourceKey: exclusive.ResourceKey, OwnerExecutionID: "execution-2",
		Mode: executioncontract.LeaseShared, TTLSeconds: 30,
	}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := data.AcquireResourceLease(ctx, executioncontract.ResourceLeaseRequest{
		LeaseID: "lease-3", ResourceKey: exclusive.ResourceKey, OwnerExecutionID: "execution-3",
		Mode: executioncontract.LeaseShared, TTLSeconds: 30,
	}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := data.AcquireResourceLease(ctx, executioncontract.ResourceLeaseRequest{
		LeaseID: "lease-4", ResourceKey: exclusive.ResourceKey, OwnerExecutionID: "execution-1",
		Mode: executioncontract.LeaseExclusive, TTLSeconds: 30,
	}, now); !errors.Is(err, executioncontract.ErrLeaseHeld) {
		t.Fatalf("writer bypassed shared leases: %v", err)
	}
	heartbeat, err := data.HeartbeatResourceLease(ctx, "lease-2", "execution-2", 60, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !heartbeat.ExpiresAt.After(now.Add(30 * time.Second)) {
		t.Fatalf("heartbeat did not extend lease: %v", heartbeat.ExpiresAt)
	}
}
