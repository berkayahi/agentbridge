package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/berkayahi/agentbridge/internal/advisory"
	"github.com/berkayahi/agentbridge/internal/config"
	"github.com/berkayahi/agentbridge/internal/localcontrol"
	"github.com/berkayahi/agentbridge/internal/store/sqlite"
)

func TestComposeProvidersFixtureDoesNotStartExternalProcess(t *testing.T) {
	cfg := config.Config{Providers: map[string]config.ProviderConfig{
		"codex": {AnalysisFixture: config.AnalysisFixtureConfig{Environment: "fixture", Implementation: "in_process_deterministic_v1"}},
	}}
	providers, runtimes, closers, err := composeProviders(context.Background(), cfg, runtimePaths{}, nil, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 0 || runtimes == nil || len(runtimes.IDs()) != 0 || len(closers) != 0 {
		t.Fatalf("fixture composed external runtime: providers=%#v runtimes=%#v closers=%d", providers, runtimes.IDs(), len(closers))
	}
}

func TestComposeProvidersFixtureServesProviderCatalogWithoutTaskRuntime(t *testing.T) {
	cfg := config.Config{Providers: map[string]config.ProviderConfig{
		"codex": {AnalysisFixture: config.AnalysisFixtureConfig{Environment: "fixture", Implementation: "in_process_deterministic_v1"}},
	}}
	providers, runtimes, closers, err := composeProviders(context.Background(), cfg, runtimePaths{}, nil, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 0 || runtimes == nil || len(runtimes.IDs()) != 0 || len(closers) != 0 {
		t.Fatalf("fixture composed external runtime: providers=%#v runtimes=%#v closers=%d", providers, runtimes.IDs(), len(closers))
	}

	data, err := sqlite.OpenV2Runtime(context.Background(), filepath.Join(t.TempDir(), "fixture.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = data.Close() })
	authority, err := composeAdvisoryAuthority(cfg, providers, providerCatalog{providers: cfg.Providers, live: providers, runtimes: runtimes})
	if err != nil || authority == nil {
		t.Fatalf("fixture advisory authority = %#v err=%v", authority, err)
	}
	service, err := localcontrol.New(localcontrol.Config{
		Store: data, Runtimes: runtimes,
		Providers: providerCatalog{providers: cfg.Providers, live: providers, runtimes: runtimes},
		Advisory:  authority,
	})
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("01234567890123456789012345678901")
	handler, err := localcontrol.NewHTTPHandler(service, secret)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/providers", nil)
	request.Header.Set("X-AgentBridge-Local-Auth", string(secret))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("provider catalog status = %d body=%s", response.Code, response.Body.String())
	}
	var result localcontrol.ProvidersResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Providers) != 1 || result.Providers[0].ID != "codex" || result.Providers[0].Available {
		t.Fatalf("fixture provider catalog = %#v", result.Providers)
	}
	session, err := service.ExecuteAdvisorySession(context.Background(), advisory.SessionRequest{
		ProviderID: "codex", ModelID: "deterministic-v1", Prompt: "return the fixture",
		OutputSchema:  json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}`),
		SchemaVersion: "fixture-schema-v1", IdempotencyKey: "fixture-advisory-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(session.Output) != `{"answer":"deterministic-fixture"}` || session.Receipt.ProviderID != "codex" || session.Receipt.ModelID != "deterministic-v1" {
		t.Fatalf("fixture advisory session = %#v", session)
	}

	ordinary := providerCatalog{
		providers: map[string]config.ProviderConfig{"codex": {Executable: os.Args[0], Model: "gpt-5.6-terra"}},
		live:      providers,
		runtimes:  runtimes,
	}
	if _, err := ordinary.ProviderProfiles(context.Background()); err == nil {
		t.Fatal("ordinary Codex without a runtime was accepted as a fixture")
	}
}
