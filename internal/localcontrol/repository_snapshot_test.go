package localcontrol_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/berkayahi/agentbridge/internal/localcontrol"
	"github.com/berkayahi/agentbridge/internal/repositorysnapshot"
	"github.com/berkayahi/agentbridge/internal/store/sqlite"
)

func TestRepositorySnapshotHTTPContractIsAuthenticatedTypedAndPathFree(t *testing.T) {
	data, err := sqlite.OpenV2(context.Background(), filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	authority := &snapshotAuthority{response: repositorysnapshot.Response{
		ContractVersion: repositorysnapshot.RepositorySnapshotV1,
		OperationID:     "snapshot-1",
		Repository:      repositorysnapshot.RepositoryIdentity{ProfileID: "fixture"},
		ExactCommitSHA:  "0123456789012345678901234567890123456789",
		ScopedRoot:      ".",
		AnalyzerVersion: "analyzer-v1",
		Detectors:       []repositorysnapshot.Detector{},
		Observations:    []repositorysnapshot.Observation{},
		Limitations:     []repositorysnapshot.Limitation{},
	}}
	service, err := localcontrol.New(localcontrol.Config{
		Store: data, Runtimes: fakeCatalog{}, RepositorySnapshots: authority,
	})
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("01234567890123456789012345678901")
	handler, err := localcontrol.NewHTTPHandler(service, secret)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(repositorysnapshot.Request{
		RepositoryProfileID: "fixture", RequestedRef: "refs/heads/main",
		ScopedRoot: ".", AnalyzerVersion: "analyzer-v1", IdempotencyKey: "snapshot-key",
	})

	unauthenticated := httptest.NewRequest(http.MethodPost, "/v1/repository-snapshots", bytes.NewReader(body))
	unauthenticatedResponse := httptest.NewRecorder()
	handler.ServeHTTP(unauthenticatedResponse, unauthenticated)
	if unauthenticatedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", unauthenticatedResponse.Code)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/repository-snapshots", bytes.NewReader(body))
	request.Header.Set("X-AgentBridge-Local-Auth", string(secret))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if authority.calls != 1 || stringsContainsAny(response.Body.String(), "checkout_path", "/internal/checkout") {
		t.Fatalf("authority calls/body = %d/%s", authority.calls, response.Body.String())
	}

	rawPathBody := `{
		"repository_profile_id":"fixture",
		"requested_ref":"refs/heads/main",
		"scoped_root":".",
		"analyzer_version":"analyzer-v1",
		"idempotency_key":"snapshot-key",
		"checkout_path":"/internal/checkout"
	}`
	rawPathRequest := httptest.NewRequest(http.MethodPost, "/v1/repository-snapshots", bytes.NewBufferString(rawPathBody))
	rawPathRequest.Header.Set("X-AgentBridge-Local-Auth", string(secret))
	rawPathResponse := httptest.NewRecorder()
	handler.ServeHTTP(rawPathResponse, rawPathRequest)
	if rawPathResponse.Code != http.StatusBadRequest || authority.calls != 1 {
		t.Fatalf("raw path status/calls = %d/%d body=%s", rawPathResponse.Code, authority.calls, rawPathResponse.Body.String())
	}

	server := httptest.NewServer(handler)
	defer server.Close()
	client, err := localcontrol.NewClient(server.URL, secret, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	typed, err := client.CreateRepositorySnapshot(context.Background(), repositorysnapshot.Request{
		RepositoryProfileID: "fixture", RequestedRef: "refs/heads/main",
		ScopedRoot: ".", AnalyzerVersion: "analyzer-v1", IdempotencyKey: "typed-key",
	})
	if err != nil || typed.OperationID != "snapshot-1" {
		t.Fatalf("typed response = %#v err=%v", typed, err)
	}
}

func TestHealthReportsLocalAPIVersionTwo(t *testing.T) {
	data, err := sqlite.OpenV2(context.Background(), filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	service, err := localcontrol.New(localcontrol.Config{Store: data, Runtimes: fakeCatalog{}})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := localcontrol.NewHTTPHandler(service, []byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !stringsContainsAny(response.Body.String(), `"local_api_version":2`) {
		t.Fatalf("health = %d %s", response.Code, response.Body.String())
	}
}

type snapshotAuthority struct {
	response repositorysnapshot.Response
	calls    int
}

func (a *snapshotAuthority) Snapshot(context.Context, repositorysnapshot.Request) (repositorysnapshot.Response, error) {
	a.calls++
	return a.response, nil
}

func stringsContainsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}
