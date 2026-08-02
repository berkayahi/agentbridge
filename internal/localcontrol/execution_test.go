package localcontrol_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/executioncontract"
	"github.com/berkayahi/agentbridge/internal/localcontrol"
	"github.com/berkayahi/agentbridge/internal/store/sqlite"
)

func TestGenericExecutionHTTPBoundaryPersistsTypedResultAndLeases(t *testing.T) {
	ctx := context.Background()
	data, err := sqlite.OpenV2Runtime(ctx, filepath.Join(t.TempDir(), "execution-api.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	service, err := localcontrol.New(localcontrol.Config{Store: data, Runtimes: fakeCatalog{}})
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("01234567890123456789012345678901")
	handler, err := localcontrol.NewHTTPHandler(service, secret)
	if err != nil {
		t.Fatal(err)
	}

	request := executioncontract.ExecutionRequest{
		ContractVersion:     executioncontract.RequestContractVersion,
		ExecutionID:         "execution-http-1",
		CorrelationID:       "run-http-1",
		UnitID:              "unit-http-1",
		RepositoryProfileID: "repo-http",
		BaseSHA:             "0123456789abcdef0123456789abcdef01234567",
		RootScope:           ".",
		Branch:              "delivery/run-1/unit-1",
		Worktree:            ".kovan/worktrees/unit-1",
		AccessMode:          executioncontract.AccessWorktreeWrite,
		RoleProfile:         "implementer",
		SkillBundle:         executioncontract.BundleRef{ID: "skill", Digest: "skill-digest"},
		ContextBundle:       executioncontract.BundleRef{ID: "context", Digest: "context-digest"},
		OutputSchema:        json.RawMessage(`{"type":"object"}`),
		ToolPolicy:          executioncontract.Policy{AllowedTools: []string{"read", "write"}},
		TimeoutSeconds:      120,
		Cancellation:        executioncontract.CancelCooperative,
		IdempotencyKey:      "execution-http-key",
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	post := func(path string, payload []byte) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		httpRequest := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(payload))
		httpRequest.Header.Set("X-AgentBridge-Local-Auth", string(secret))
		handler.ServeHTTP(recorder, httpRequest)
		return recorder
	}
	createdResponse := post("/v1/executions", body)
	if createdResponse.Code != http.StatusAccepted {
		t.Fatalf("create status = %d, body = %s", createdResponse.Code, createdResponse.Body.String())
	}
	var created executioncontract.ExecutionRecord
	if err := json.NewDecoder(createdResponse.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.State != executioncontract.StateAccepted || created.Revision != 1 {
		t.Fatalf("created = %#v", created)
	}

	leaseBody, _ := json.Marshal(executioncontract.ResourceLeaseRequest{
		LeaseID: "lease-http-1", ResourceKey: "worktree:repo-http:unit-http-1",
		OwnerExecutionID: request.ExecutionID, Mode: executioncontract.LeaseExclusive, TTLSeconds: 30,
	})
	leaseResponse := post("/v1/resource-leases/acquire", leaseBody)
	if leaseResponse.Code != http.StatusCreated {
		t.Fatalf("lease status = %d, body = %s", leaseResponse.Code, leaseResponse.Body.String())
	}

	resultBody, _ := json.Marshal(localcontrol.ExecutionResultRequest{
		ExpectedRevision: created.Revision,
		Result: executioncontract.ExecutionResult{
			ContractVersion: executioncontract.ResultContractVersion,
			ExecutionID:     request.ExecutionID,
			State:           executioncontract.StateCompleted,
			Summary:         "completed at an exact candidate",
			Output:          json.RawMessage(`{"status":"ok"}`),
			CandidateSHA:    request.BaseSHA,
			FinishedAt:      time.Now().UTC(),
		},
	})
	resultResponse := post("/v1/executions/"+request.ExecutionID+"/result", resultBody)
	if resultResponse.Code != http.StatusOK {
		t.Fatalf("result status = %d, body = %s", resultResponse.Code, resultResponse.Body.String())
	}
	var finished executioncontract.ExecutionRecord
	if err := json.NewDecoder(resultResponse.Body).Decode(&finished); err != nil {
		t.Fatal(err)
	}
	if finished.State != executioncontract.StateCompleted || finished.Result == nil {
		t.Fatalf("finished = %#v", finished)
	}

	invalidBody, _ := json.Marshal(localcontrol.ExecutionResultRequest{
		ExpectedRevision: finished.Revision,
		Result: executioncontract.ExecutionResult{
			ContractVersion: executioncontract.ResultContractVersion,
			ExecutionID:     request.ExecutionID,
			State:           executioncontract.StateCompleted,
			Summary:         "invalid output",
			Output:          json.RawMessage(`"free form"`),
			CandidateSHA:    request.BaseSHA,
			FinishedAt:      time.Now().UTC(),
		},
	})
	invalidResponse := post("/v1/executions/"+request.ExecutionID+"/result", invalidBody)
	if invalidResponse.Code != http.StatusBadRequest {
		t.Fatalf("invalid result status = %d, body = %s", invalidResponse.Code, invalidResponse.Body.String())
	}
}
