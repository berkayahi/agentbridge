package localcontrol_test

import (
	"bytes"
	"context"
	"encoding/base64"
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

func TestRepositoryUnderstandingMatchesPlatformFlatContract(t *testing.T) {
	handler, snapshot := understandingHandler(t)
	secret := []byte("01234567890123456789012345678901")

	unauthenticated := httptest.NewRequest(http.MethodPost, "/v1/repository-understanding", strings.NewReader(`{}`))
	unauthenticatedResponse := httptest.NewRecorder()
	handler.ServeHTTP(unauthenticatedResponse, unauthenticated)
	if unauthenticatedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", unauthenticatedResponse.Code)
	}

	roles := []repositorysnapshot.AnalysisRole{
		repositorysnapshot.RoleCartographer,
		repositorysnapshot.RoleProductArchaeologist,
		repositorysnapshot.RoleQualityOperations,
	}
	prior := make([]localcontrol.RepositoryUnderstandingOutputReference, 0, len(roles))
	for _, role := range roles {
		request := platformRequest(snapshot, role, "platform-understanding-"+string(role), nil)
		response, raw := postUnderstanding(t, handler, secret, request)
		assertPlatformResponse(t, raw, response, role, snapshot)
		if len(response.Findings) != len(repositorysnapshot.RequiredCoverage(role)) {
			t.Fatalf("%s findings = %d", role, len(response.Findings))
		}
		prior = append(prior, localcontrol.RepositoryUnderstandingOutputReference{
			Role: role, OperationID: response.OperationID, ResultDigest: response.ResultDigest,
		})
	}

	synthesisRequest := platformRequest(snapshot, repositorysnapshot.RoleSynthesizer, "platform-understanding-synthesizer", prior)
	synthesis, raw := postUnderstanding(t, handler, secret, synthesisRequest)
	assertPlatformResponse(t, raw, synthesis, repositorysnapshot.RoleSynthesizer, snapshot)
	states := make(map[repositorysnapshot.KnowledgeState]bool)
	for _, finding := range synthesis.Findings {
		states[finding.EvidenceState] = true
		if (finding.EvidenceState == repositorysnapshot.KnowledgeObserved || finding.EvidenceState == repositorysnapshot.KnowledgeDeclared) && len(finding.Evidence) == 0 {
			t.Fatalf("%s finding lacks evidence: %#v", finding.EvidenceState, finding)
		}
	}
	for _, state := range []repositorysnapshot.KnowledgeState{repositorysnapshot.KnowledgeObserved, repositorysnapshot.KnowledgeDeclared, repositorysnapshot.KnowledgeInferred, repositorysnapshot.KnowledgeUnknown, repositorysnapshot.KnowledgeConflicting} {
		if !states[state] {
			t.Fatalf("synthesizer omitted state %q", state)
		}
	}
	statuses := make(map[repositorysnapshot.CapabilityImplementationStatus]bool)
	for _, capability := range synthesis.Capabilities {
		statuses[capability.ImplementationStatus] = true
		if (capability.EvidenceState == repositorysnapshot.KnowledgeObserved || capability.EvidenceState == repositorysnapshot.KnowledgeDeclared) && len(capability.Evidence) == 0 {
			t.Fatalf("%s capability lacks evidence: %#v", capability.EvidenceState, capability)
		}
	}
	for _, status := range []repositorysnapshot.CapabilityImplementationStatus{repositorysnapshot.CapabilityVerified, repositorysnapshot.CapabilityPartial, repositorysnapshot.CapabilityAbsent, repositorysnapshot.CapabilityUnknown, repositorysnapshot.CapabilityConflicting} {
		if !statuses[status] {
			t.Fatalf("synthesizer omitted capability status %q", status)
		}
	}

	forged := synthesisRequest
	forged.PriorOutputs[0].ResultDigest = "sha256:" + strings.Repeat("0", 64)
	requestBody, err := json.Marshal(forged)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/repository-understanding", bytes.NewReader(requestBody))
	request.Header.Set("X-AgentBridge-Local-Auth", base64.RawURLEncoding.EncodeToString(secret))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("forged prior status = %d body=%s", response.Code, response.Body.String())
	}

	for _, retired := range []string{"/v1/understanding/roles", "/v1/understanding/synthesize"} {
		request := httptest.NewRequest(http.MethodPost, retired, strings.NewReader(`{}`))
		request.Header.Set("X-AgentBridge-Local-Auth", base64.RawURLEncoding.EncodeToString(secret))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("retired route %s status = %d", retired, response.Code)
		}
	}
}

func understandingHandler(t *testing.T) (http.Handler, repositorysnapshot.Response) {
	t.Helper()
	data, err := sqlite.OpenV2(context.Background(), filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = data.Close() })
	contents, err := os.ReadFile("../../protocol/fixtures/v1/repository-snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot repositorysnapshot.Response
	if err := json.Unmarshal(contents, &snapshot); err != nil {
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
	snapshots, err := repositorysnapshot.New(repositorysnapshot.Config{Store: data, Catalog: roleSnapshotCatalog{}, Inspector: roleSnapshotInspector{}})
	if err != nil {
		t.Fatal(err)
	}
	understanding, err := repositorysnapshot.NewUnderstandingService(repositorysnapshot.UnderstandingConfig{
		Store: data, Catalog: roleSnapshotCatalog{}, Evidence: roleEvidence{},
		Providers: map[string]repositorysnapshot.AnalysisProvider{
			"codex": repositorysnapshot.NewDeterministicFixtureProvider("agentbridge-fixture", "deterministic-v1"),
		},
		DefaultProvider: "codex",
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
	handler, err := localcontrol.NewHTTPHandler(service, []byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatal(err)
	}
	return handler, snapshot
}

func platformRequest(snapshot repositorysnapshot.Response, role repositorysnapshot.AnalysisRole, key string, prior []localcontrol.RepositoryUnderstandingOutputReference) localcontrol.RepositoryUnderstandingRequest {
	return localcontrol.RepositoryUnderstandingRequest{
		ScopeID: "11111111-1111-4111-8111-111111111111", Role: role,
		RepositoryProfileID: snapshot.Repository.ProfileID, SnapshotCommit: snapshot.ExactCommitSHA,
		SnapshotDigest: snapshot.ResultDigest, SnapshotOperationID: snapshot.OperationID,
		IdempotencyKey: key, PriorOutputs: prior,
	}
}

func postUnderstanding(t *testing.T, handler http.Handler, secret []byte, request localcontrol.RepositoryUnderstandingRequest) (localcontrol.RepositoryUnderstandingResponse, string) {
	t.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{`"project_id"`, `"public_eligible"`, `"marketing_claim_eligible"`, `"review_state"`, `"snapshot"`} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("Platform request contains forbidden field %s: %s", forbidden, body)
		}
	}
	httpRequest := httptest.NewRequest(http.MethodPost, "/v1/repository-understanding", bytes.NewReader(body))
	httpRequest.Header.Set("X-AgentBridge-Local-Auth", base64.RawURLEncoding.EncodeToString(secret))
	httpResponse := httptest.NewRecorder()
	handler.ServeHTTP(httpResponse, httpRequest)
	if httpResponse.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", httpResponse.Code, httpResponse.Body.String())
	}
	var response localcontrol.RepositoryUnderstandingResponse
	if err := json.Unmarshal(httpResponse.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response, httpResponse.Body.String()
}

func assertPlatformResponse(t *testing.T, raw string, response localcontrol.RepositoryUnderstandingResponse, role repositorysnapshot.AnalysisRole, snapshot repositorysnapshot.Response) {
	t.Helper()
	if response.ContractVersion != repositorysnapshot.UnderstandingContractV1 || response.ScopeID != "11111111-1111-4111-8111-111111111111" || response.Role != role || response.ExactCommitSHA != snapshot.ExactCommitSHA || response.Provider.ID != "agentbridge-fixture" || response.Status != "completed" || !strings.HasPrefix(response.ResultDigest, "sha256:") {
		t.Fatalf("flat response mismatch: %#v", response)
	}
	for _, required := range []string{`"scope_id"`, `"operation_id"`, `"evidence_state"`, `"implementation_status"`} {
		if !strings.Contains(raw, required) {
			t.Fatalf("Platform response omitted %s: %s", required, raw)
		}
	}
	for _, forbidden := range []string{`"output"`, `"project_id"`, `"review_state"`, `"public_eligible"`, `"marketing_claim_eligible"`} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("generic response contains forbidden field %s: %s", forbidden, raw)
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
		files = append(files, repositorysnapshot.EvidenceFile{Path: path, Content: "fixture evidence", ContentDigest: "sha256:" + strings.Repeat("a", 64), Size: 16})
	}
	return repositorysnapshot.EvidencePacket{ContractVersion: repositorysnapshot.EvidenceContractV1, RepositoryProfileID: request.RepositoryProfileID, ExactCommitSHA: request.ExpectedCommitSHA, Files: files}, nil
}
