package main

import (
	"context"
	"testing"

	"github.com/berkayahi/agentbridge/internal/config"
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
