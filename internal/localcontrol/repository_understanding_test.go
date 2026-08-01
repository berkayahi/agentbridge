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
		Capabilities:    []string{}, Assumptions: []string{}, Conflicts: []string{}, Unknowns: []string{},
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
		ProjectID: "platform-project", Role: "Repository Cartographer", RepositoryProfileID: snapshot.Repository.ProfileID,
		SnapshotCommit: snapshot.ExactCommitSHA, SnapshotDigest: snapshot.ResultDigest, Snapshot: snapshot,
	})
	if err != nil {
		t.Fatal(err)
	}
	roleRequest := httptest.NewRequest(http.MethodPost, "/v1/understanding/roles", bytes.NewReader(roleBody))
	roleRequest.Header.Set("X-AgentBridge-Local-Auth", string(secret))
	roleResponse := httptest.NewRecorder()
	handler.ServeHTTP(roleResponse, roleRequest)
	if roleResponse.Code != http.StatusCreated || !strings.Contains(roleResponse.Body.String(), `"role":"Repository Cartographer"`) {
		t.Fatalf("role route status/body = %d/%s", roleResponse.Code, roleResponse.Body.String())
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

type understandingHTTPAuthority struct {
	response repositorysnapshot.AnalysisResponse
	calls    int
}

func (a *understandingHTTPAuthority) Understand(context.Context, repositorysnapshot.UnderstandingRequest) (repositorysnapshot.AnalysisResponse, error) {
	a.calls++
	return a.response, nil
}
