package localcontrol_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/localcontrol"
	"github.com/berkayahi/agentbridge/internal/repositorysnapshot"
	"github.com/berkayahi/agentbridge/internal/store/sqlite"
)

func TestRepositoryUnderstandingHTTPIsAuthenticatedAndTyped(t *testing.T) {
	data, err := sqlite.OpenV2(context.Background(), filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	authority := &understandingHTTPAuthority{response: repositorysnapshot.AnalysisResponse{
		ContractVersion: repositorysnapshot.UnderstandingContractV1,
		OperationID:     "understanding-1",
		Role:            repositorysnapshot.RoleCartographer,
		ExactCommitSHA:  "0123456789012345678901234567890123456789",
		Evidence:        []repositorysnapshot.EvidenceReference{},
		Findings:        []repositorysnapshot.Finding{},
		Capabilities:    []repositorysnapshot.Capability{}, Assumptions: []string{}, Conflicts: []string{}, Unknowns: []string{},
		Provider: repositorysnapshot.ProviderMetadata{ID: "fixture", Model: "fixture-model", Status: "completed"},
		Status:   "completed", ResultDigest: "sha256:fixture",
	}}
	service, err := localcontrol.New(localcontrol.Config{Store: data, Runtimes: fakeCatalog{}, RepositoryUnderstanding: authority})
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("01234567890123456789012345678901")
	handler, err := localcontrol.NewHTTPHandler(service, secret)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(repositorysnapshot.UnderstandingRequest{
		RepositoryProfileID: "fixture", ExpectedCommitSHA: "0123456789012345678901234567890123456789",
		Paths: []string{"README.md"}, Role: repositorysnapshot.RoleCartographer,
		ProviderID: "fixture", IdempotencyKey: "understanding-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	unauthenticated := httptest.NewRequest(http.MethodPost, "/v1/repository-understanding", bytes.NewReader(body))
	unauthenticatedResponse := httptest.NewRecorder()
	handler.ServeHTTP(unauthenticatedResponse, unauthenticated)
	if unauthenticatedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", unauthenticatedResponse.Code)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/repository-understanding", bytes.NewReader(body))
	request.Header.Set("X-AgentBridge-Local-Auth", string(secret))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || authority.calls != 1 {
		t.Fatalf("authenticated status/calls = %d/%d body=%s", response.Code, authority.calls, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "checkout_path") || strings.Contains(response.Body.String(), "provider transcript") {
		t.Fatalf("response exposed forbidden detail: %s", response.Body.String())
	}

	server := httptest.NewServer(handler)
	defer server.Close()
	client, err := localcontrol.NewClient(server.URL, secret, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	typed, err := client.UnderstandRepository(context.Background(), repositorysnapshot.UnderstandingRequest{
		RepositoryProfileID: "fixture", ExpectedCommitSHA: "0123456789012345678901234567890123456789",
		Paths: []string{"README.md"}, Role: repositorysnapshot.RoleCartographer,
		ProviderID: "fixture", IdempotencyKey: "typed-understanding-key",
	})
	if err != nil || typed.OperationID != "understanding-1" {
		t.Fatalf("typed response = %#v err=%v", typed, err)
	}
	fixture, err := os.ReadFile("../../protocol/fixtures/v1/repository-snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot repositorysnapshot.Response
	if err := json.Unmarshal(fixture, &snapshot); err != nil {
		t.Fatal(err)
	}
	roleBody, err := json.Marshal(localcontrol.UnderstandingRoleRequest{
		ScopeID: "acceptance-scope", Role: repositorysnapshot.RoleCartographer, RepositoryProfileID: snapshot.Repository.ProfileID,
		SnapshotCommit: snapshot.ExactCommitSHA, SnapshotDigest: snapshot.ResultDigest, Snapshot: snapshot,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, productField := range []string{`"project_id"`, "Repository Cartographer", `"public_eligible"`, `"marketing_claim_eligible"`} {
		if strings.Contains(string(roleBody), productField) {
			t.Fatalf("generic engine request contains product field %q: %s", productField, roleBody)
		}
	}
	for _, path := range []string{"/v1/understanding/roles", "/v1/understanding/synthesize"} {
		unauthenticated := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(roleBody))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, unauthenticated)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s unauthenticated status = %d", path, response.Code)
		}
	}
}

func TestUnderstandingRoleBindsPersistedSnapshotAndUsesSnapshotDigest(t *testing.T) {
	data, err := sqlite.OpenV2(context.Background(), filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()

	fixture, err := os.ReadFile("../../protocol/fixtures/v1/repository-snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot repositorysnapshot.Response
	if err := json.Unmarshal(fixture, &snapshot); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := data.SaveRepositorySnapshot(context.Background(), repositorysnapshot.Operation{
		ID: snapshot.OperationID, IdempotencyKey: "persisted-fixture", RepositoryProfileID: snapshot.Repository.ProfileID,
		RequestedRef: snapshot.Ref.Requested, ScopedRoot: snapshot.ScopedRoot, AnalyzerVersion: snapshot.AnalyzerVersion,
		RequestDigest: "sha256:persisted-fixture", ExactCommitSHA: snapshot.ExactCommitSHA, ResultDigest: snapshot.ResultDigest,
		Status: "completed", Response: snapshot, RequestedAt: now, CompletedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	snapshots, err := repositorysnapshot.New(repositorysnapshot.Config{
		Store: data, Catalog: roleSnapshotCatalog{}, Inspector: roleSnapshotInspector{},
	})
	if err != nil {
		t.Fatal(err)
	}
	understanding, err := repositorysnapshot.NewUnderstandingService(repositorysnapshot.UnderstandingConfig{
		Store: data, Catalog: roleSnapshotCatalog{}, Evidence: roleEvidence{},
		Providers: map[string]repositorysnapshot.AnalysisProvider{"fixture": roleProvider{}}, DefaultProvider: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := localcontrol.New(localcontrol.Config{
		Store: data, Runtimes: fakeCatalog{}, RepositorySnapshots: snapshots, RepositoryUnderstanding: understanding,
	})
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("01234567890123456789012345678901")
	handler, err := localcontrol.NewHTTPHandler(service, secret)
	if err != nil {
		t.Fatal(err)
	}
	requestBody, err := json.Marshal(localcontrol.UnderstandingRoleRequest{
		ScopeID: "acceptance-scope", Role: repositorysnapshot.RoleCartographer, RepositoryProfileID: snapshot.Repository.ProfileID,
		SnapshotCommit: snapshot.ExactCommitSHA, SnapshotDigest: snapshot.ResultDigest, SnapshotOperationID: snapshot.OperationID, IdempotencyKey: "custom-cartographer",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/understanding/roles", bytes.NewReader(requestBody))
	request.Header.Set("X-AgentBridge-Local-Auth", string(secret))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("role status = %d body=%s", response.Code, response.Body.String())
	}
	var roleResponse localcontrol.UnderstandingRoleResponse
	if err := json.Unmarshal(response.Body.Bytes(), &roleResponse); err != nil {
		t.Fatal(err)
	}
	for _, productField := range []string{`"project_id"`, "Repository Cartographer", `"review_state"`, `"public_eligible"`, `"marketing_claim_eligible"`} {
		if strings.Contains(response.Body.String(), productField) {
			t.Fatalf("generic engine response contains product field %q: %s", productField, response.Body.String())
		}
	}
	if len(roleResponse.Output.Claims) != len(repositorysnapshot.RequiredCoverage(repositorysnapshot.RoleCartographer)) || len(roleResponse.Output.Capabilities) != 1 {
		t.Fatalf("role output = %#v", roleResponse.Output)
	}
	claim := roleResponse.Output.Claims[0]
	if claim.EvidenceDigest != snapshot.ResultDigest || len(claim.Evidence) != 1 || claim.Evidence[0].Digest != "sha256:fixture" {
		t.Fatalf("claim evidence digest = %#v, want %q", claim, snapshot.ResultDigest)
	}
	if got := roleResponse.Output.Capabilities[0].EvidenceDigest; got != snapshot.ResultDigest {
		t.Fatalf("capability evidence digest = %q, want %q", got, snapshot.ResultDigest)
	}
	capability := roleResponse.Output.Capabilities[0]
	if capability.ID != "fixture-capability" || capability.Actor != "operator" || capability.ImplementationStatus != repositorysnapshot.CapabilityPartial || capability.EvidenceState != repositorysnapshot.KnowledgeObserved || capability.Confidence == nil || *capability.Confidence != 0.75 || len(capability.Evidence) != 1 {
		t.Fatalf("typed capability projection = %#v", capability)
	}
	if len(roleResponse.Output.Conflicts) != 1 || len(roleResponse.Output.Unknowns) != 1 {
		t.Fatalf("conflicts/unknowns were dropped: %#v", roleResponse.Output)
	}
	for _, value := range []struct {
		role repositorysnapshot.AnalysisRole
		key  string
	}{
		{role: repositorysnapshot.RoleProductArchaeologist, key: "custom-archaeologist"},
		{role: repositorysnapshot.RoleQualityOperations, key: "custom-quality"},
	} {
		body, err := json.Marshal(localcontrol.UnderstandingRoleRequest{
			ScopeID: "acceptance-scope", Role: value.role, RepositoryProfileID: snapshot.Repository.ProfileID,
			SnapshotCommit: snapshot.ExactCommitSHA, SnapshotDigest: snapshot.ResultDigest, SnapshotOperationID: snapshot.OperationID, IdempotencyKey: value.key,
		})
		if err != nil {
			t.Fatal(err)
		}
		roleRequest := httptest.NewRequest(http.MethodPost, "/v1/understanding/roles", bytes.NewReader(body))
		roleRequest.Header.Set("X-AgentBridge-Local-Auth", string(secret))
		roleResult := httptest.NewRecorder()
		handler.ServeHTTP(roleResult, roleRequest)
		if roleResult.Code != http.StatusCreated {
			t.Fatalf("%s status = %d body=%s", value.role, roleResult.Code, roleResult.Body.String())
		}
	}
	for _, key := range []string{"custom-cartographer", "custom-archaeologist", "custom-quality"} {
		if _, err := understanding.LoadOperation(context.Background(), key); err != nil {
			t.Fatalf("load custom role operation %q: %v", key, err)
		}
	}
	synthesisBody, err := json.Marshal(localcontrol.UnderstandingRoleRequest{
		ScopeID: "acceptance-scope", Role: repositorysnapshot.RoleSynthesizer, RepositoryProfileID: snapshot.Repository.ProfileID,
		SnapshotCommit: snapshot.ExactCommitSHA, SnapshotDigest: snapshot.ResultDigest, SnapshotOperationID: snapshot.OperationID, IdempotencyKey: "custom-synth",
		PriorOutputs: []localcontrol.UnderstandingPriorOutputReference{
			{Role: repositorysnapshot.RoleCartographer, IdempotencyKey: "custom-cartographer"},
			{Role: repositorysnapshot.RoleProductArchaeologist, IdempotencyKey: "custom-archaeologist"},
			{Role: repositorysnapshot.RoleQualityOperations, IdempotencyKey: "custom-quality"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	synthesisRequest := httptest.NewRequest(http.MethodPost, "/v1/understanding/synthesize", bytes.NewReader(synthesisBody))
	synthesisRequest.Header.Set("X-AgentBridge-Local-Auth", string(secret))
	synthesisResult := httptest.NewRecorder()
	handler.ServeHTTP(synthesisResult, synthesisRequest)
	if synthesisResult.Code != http.StatusCreated {
		t.Fatalf("synthesis status = %d body=%s", synthesisResult.Code, synthesisResult.Body.String())
	}

	forged := snapshot
	forged.ExactCommitSHA = strings.Repeat("a", 40)
	forgedRequest, err := json.Marshal(localcontrol.UnderstandingRoleRequest{
		ScopeID: "forged-scope", Role: repositorysnapshot.RoleCartographer, RepositoryProfileID: snapshot.Repository.ProfileID,
		SnapshotCommit: forged.ExactCommitSHA, SnapshotDigest: forged.ResultDigest, Snapshot: forged,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/understanding/roles", "/v1/understanding/synthesize"} {
		forgedHTTP := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(forgedRequest))
		forgedHTTP.Header.Set("X-AgentBridge-Local-Auth", string(secret))
		forgedResponse := httptest.NewRecorder()
		handler.ServeHTTP(forgedResponse, forgedHTTP)
		if forgedResponse.Code != http.StatusConflict {
			t.Fatalf("%s forged snapshot status = %d body=%s", path, forgedResponse.Code, forgedResponse.Body.String())
		}
	}
}

type roleSnapshotCatalog struct{}

func (roleSnapshotCatalog) ResolveRepositoryProfile(context.Context, string) (repositorysnapshot.ConfiguredRepository, error) {
	return repositorysnapshot.ConfiguredRepository{ProfileID: "fixture-repository", CheckoutPath: "/tmp/fixture-checkout", AllowedRef: "refs/heads/main"}, nil
}

type roleSnapshotInspector struct{}

func (roleSnapshotInspector) Inspect(context.Context, repositorysnapshot.ConfiguredRepository, repositorysnapshot.Request) (repositorysnapshot.Response, error) {
	return repositorysnapshot.Response{}, nil
}

type roleEvidence struct{}

func (roleEvidence) RetrieveEvidence(_ context.Context, _ repositorysnapshot.ConfiguredRepository, request repositorysnapshot.EvidenceRequest) (repositorysnapshot.EvidencePacket, error) {
	files := make([]repositorysnapshot.EvidenceFile, 0, len(request.Paths))
	for _, path := range request.Paths {
		files = append(files, repositorysnapshot.EvidenceFile{Path: path, Content: "fixture evidence", ContentDigest: "sha256:fixture", Size: 16})
	}
	return repositorysnapshot.EvidencePacket{ContractVersion: repositorysnapshot.EvidenceContractV1, RepositoryProfileID: request.RepositoryProfileID, ExactCommitSHA: request.ExpectedCommitSHA, Files: files}, nil
}

type roleProvider struct{}

func (roleProvider) Analyze(_ context.Context, request repositorysnapshot.ProviderRequest) (repositorysnapshot.ProviderResult, error) {
	areas := repositorysnapshot.RequiredCoverage(request.Role)
	findings := make([]repositorysnapshot.Finding, 0, len(areas))
	for index, area := range areas {
		finding := repositorysnapshot.Finding{ID: "finding-" + string(rune('a'+index)), Area: area, Statement: "fixture unknown for " + area, KnowledgeState: repositorysnapshot.KnowledgeUnknown}
		if index == 0 {
			finding.Statement = "fixture observation"
			finding.KnowledgeState = repositorysnapshot.KnowledgeObserved
			if request.Role == repositorysnapshot.RoleSynthesizer {
				finding.EvidencePaths = []string{"m1-snapshot.json"}
			} else {
				finding.EvidencePaths = []string{"openapi.yaml"}
			}
		}
		findings = append(findings, finding)
	}
	confidence := 0.75
	capabilities := []repositorysnapshot.Capability{{
		ID: "fixture-capability", Name: "Fixture capability", Actor: "operator", VerifiedBehavior: "Fixture behavior is partially implemented.",
		ImplementationStatus: repositorysnapshot.CapabilityPartial, KnowledgeState: repositorysnapshot.KnowledgeObserved,
		EvidencePaths: []string{"openapi.yaml"}, Confidence: &confidence,
	}}
	if request.Role == repositorysnapshot.RoleSynthesizer {
		capabilities[0].EvidencePaths = []string{"m1-snapshot.json"}
	}
	output, err := json.Marshal(repositorysnapshot.StructuredOutput{
		Role:         request.Role,
		Findings:     findings,
		Capabilities: capabilities, Assumptions: []string{}, Conflicts: []string{"fixture conflict"}, Unknowns: []string{"fixture unknown"},
	})
	if err != nil {
		return repositorysnapshot.ProviderResult{}, err
	}
	return repositorysnapshot.ProviderResult{ProviderID: "fixture", Model: "fixture-model", Output: output}, nil
}

type understandingHTTPAuthority struct {
	response repositorysnapshot.AnalysisResponse
	calls    int
}

func (a *understandingHTTPAuthority) Understand(context.Context, repositorysnapshot.UnderstandingRequest) (repositorysnapshot.AnalysisResponse, error) {
	a.calls++
	return a.response, nil
}
