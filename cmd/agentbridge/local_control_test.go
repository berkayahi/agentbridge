package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/berkayahi/agentbridge/internal/config"
	bridgegit "github.com/berkayahi/agentbridge/internal/git"
	"github.com/berkayahi/agentbridge/internal/kernel"
	"github.com/berkayahi/agentbridge/internal/localcontrol"
	"github.com/berkayahi/agentbridge/internal/provider"
	"github.com/berkayahi/agentbridge/internal/repositorysnapshot"
	bridgeRuntime "github.com/berkayahi/agentbridge/internal/runtime"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

func TestAlignedIntegrationRecoveryRequiresExactMergeEvidence(t *testing.T) {
	ctx := context.Background()
	checkout := filepath.Join(t.TempDir(), "repository")
	runGit := func(args ...string) string {
		t.Helper()
		result, err := (bridgegit.Runner{}).Run(ctx, checkout, args...)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(result.Stdout)
	}
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit("init", "-b", "main")
	runGit("config", "user.name", "AgentBridge Test")
	runGit("config", "user.email", "agentbridge@example.invalid")
	if err := os.WriteFile(filepath.Join(checkout, "base.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "base.txt")
	runGit("commit", "-m", "test: base")
	runGit("checkout", "-b", "hive/landing")
	if err := os.WriteFile(filepath.Join(checkout, "source.txt"), []byte("source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "source.txt")
	runGit("commit", "-m", "test: source")
	sourceSHA := runGit("rev-parse", "HEAD")
	runGit("checkout", "main")
	if err := os.WriteFile(filepath.Join(checkout, "target.txt"), []byte("target\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "target.txt")
	runGit("commit", "-m", "test: target")
	message := "merge: integrate objective"
	runGit("merge", "--no-ff", sourceSHA, "-m", message)
	mergeSHA := runGit("rev-parse", "HEAD")

	request := localcontrol.IntegrateRepositoryRequest{
		ExpectedSourceSHA: sourceSHA,
		Message:           message,
		UpdateSource:      true,
	}
	recovered, err := alignedIntegrationRecovery(ctx, bridgegit.Runner{}, checkout, request, mergeSHA, mergeSHA)
	if err != nil {
		t.Fatal(err)
	}
	if !recovered {
		t.Fatal("exact aligned merge was not recovered")
	}
	request.Message = "merge: a different objective"
	recovered, err = alignedIntegrationRecovery(ctx, bridgegit.Runner{}, checkout, request, mergeSHA, mergeSHA)
	if err != nil {
		t.Fatal(err)
	}
	if recovered {
		t.Fatal("aligned merge with a different message was recovered")
	}
	request.Message = message
	if err := os.WriteFile(filepath.Join(checkout, "later.txt"), []byte("later\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "later.txt")
	runGit("commit", "-m", "test: target advances")
	advancedTargetSHA := runGit("rev-parse", "HEAD")
	recovered, err = alignedIntegrationRecovery(ctx, bridgegit.Runner{}, checkout, request, mergeSHA, advancedTargetSHA)
	if err != nil {
		t.Fatal(err)
	}
	if !recovered {
		t.Fatal("exact aligned merge was not recovered after the target advanced")
	}
}

func TestLocalRuntimeExecutorMapsLocalAuthorityToProviderIdentity(t *testing.T) {
	adapter := &approvalCaptureRuntime{}
	runtimes, err := bridgeRuntime.NewRegistry(adapter)
	if err != nil {
		t.Fatal(err)
	}
	executor := newLocalRuntimeExecutor(nil, runtimes, nil, nil, nil, "987654321")
	if err := executor.Approve(context.Background(), localcontrol.TaskView{
		ID: "task-1", TargetDeviceID: localcontrol.LocalDeviceID, RuntimeID: "codex",
	}, "approval-1", localcontrol.LocalAuthorityUserID, true); err != nil {
		t.Fatal(err)
	}
	if adapter.userID != "987654321" {
		t.Fatalf("provider approval user = %q, want configured native identity", adapter.userID)
	}
}

func TestProviderCatalogUsesLivePerModelCapabilities(t *testing.T) {
	runtimes, err := bridgeRuntime.NewRegistry(&catalogRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	live := executionCatalogProvider{catalog: provider.ExecutionCatalog{
		DefaultModel: "gpt-5.6-sol",
		Models: []provider.Model{
			{
				ID: "gpt-5.6-sol", DefaultReasoningEffort: "low",
				ReasoningEfforts: []provider.ReasoningEffort{
					{ID: "max", Kind: provider.ReasoningEffortStandard},
					{ID: "ultra", Kind: provider.ReasoningEffortOrchestration},
				},
			},
			{
				ID: "gpt-5.6-luna", DefaultReasoningEffort: "medium",
				ReasoningEfforts: []provider.ReasoningEffort{
					{ID: "max", Kind: provider.ReasoningEffortStandard},
				},
			},
		},
	}}
	catalog := providerCatalog{
		providers: map[string]config.ProviderConfig{
			"codex": {Executable: os.Args[0], Model: "gpt-5.6-sol", Models: []string{"stale-model"}},
		},
		live:     map[workmodel.Provider]provider.Provider{workmodel.CodexSubscription: live},
		runtimes: runtimes,
	}
	profiles, err := catalog.ProviderProfiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 1 || len(profiles[0].Models) != 2 || profiles[0].Models[1] != "gpt-5.6-luna" {
		t.Fatalf("provider profiles = %#v", profiles)
	}
	if got := profiles[0].ModelProfiles[0].SupportedReasoningEfforts[1]; got.ID != "ultra" || got.Kind != "orchestration" {
		t.Fatalf("Sol Ultra = %#v", got)
	}
	for _, effort := range profiles[0].ModelProfiles[1].SupportedReasoningEfforts {
		if effort.ID == "ultra" {
			t.Fatalf("Luna advertised Ultra: %#v", profiles[0].ModelProfiles[1])
		}
	}
	gotModes := make([]string, 0, len(profiles[0].ApprovalModes))
	for _, mode := range profiles[0].ApprovalModes {
		gotModes = append(gotModes, mode.ID)
	}
	wantModes := []string{"auto_within_policy", "ask_every_time", "provider_default"}
	if !slices.Equal(gotModes, wantModes) {
		t.Fatalf("Codex approval modes = %v, want %v", gotModes, wantModes)
	}
}

type catalogRuntime struct{ approvalCaptureRuntime }

func (*catalogRuntime) Capabilities(context.Context) (bridgeRuntime.Capabilities, error) {
	return bridgeRuntime.Capabilities{NativeApprovalModes: []bridgeRuntime.ApprovalMode{
		bridgeRuntime.ApprovalAutoWithinPolicy,
		bridgeRuntime.ApprovalAskEveryTime,
		bridgeRuntime.ApprovalProviderDefault,
	}}, nil
}

type executionCatalogProvider struct {
	provider.Provider
	catalog       provider.ExecutionCatalog
	authenticated *bool
	authErr       error
}

func (p executionCatalogProvider) ExecutionCatalog(context.Context) (provider.ExecutionCatalog, error) {
	return p.catalog, nil
}

func (p executionCatalogProvider) AuthStatus(context.Context) (provider.AuthStatus, error) {
	if p.authErr != nil {
		return provider.AuthStatus{}, p.authErr
	}
	authenticated := true
	if p.authenticated != nil {
		authenticated = *p.authenticated
	}
	return provider.AuthStatus{Authenticated: authenticated}, nil
}

func TestProviderCatalogFailsClosedWhenAuthenticationCannotBeConfirmed(t *testing.T) {
	live := executionCatalogProvider{authErr: errors.New("auth probe unavailable")}
	catalog := providerCatalog{
		providers: map[string]config.ProviderConfig{
			"codex": {Executable: os.Args[0], Model: "gpt-5.6-terra", Models: []string{"gpt-5.6-terra"}},
		},
		live: map[workmodel.Provider]provider.Provider{workmodel.CodexSubscription: live},
	}
	profiles, err := catalog.ProviderProfiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 1 || profiles[0].Available {
		t.Fatalf("provider with unknown authentication = %#v", profiles)
	}
}

func TestProviderCatalogDoesNotAdvertiseUnauthenticatedRuntimeAsAvailable(t *testing.T) {
	authenticated := false
	live := executionCatalogProvider{authenticated: &authenticated}
	runtimes, err := bridgeRuntime.NewRegistry(&approvalCaptureRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	catalog := providerCatalog{
		providers: map[string]config.ProviderConfig{
			"codex": {Executable: os.Args[0], Model: "gpt-5.6-terra", Models: []string{"gpt-5.6-terra"}},
		},
		live: map[workmodel.Provider]provider.Provider{workmodel.CodexSubscription: live}, runtimes: runtimes,
	}
	profiles, err := catalog.ProviderProfiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 1 || profiles[0].Available {
		t.Fatalf("unauthenticated provider = %#v", profiles)
	}
}

type approvalCaptureRuntime struct {
	userID string
}

func (*approvalCaptureRuntime) ID() string { return "codex" }
func (*approvalCaptureRuntime) Detect(context.Context) (bridgeRuntime.Installation, error) {
	return bridgeRuntime.Installation{}, bridgeRuntime.ErrUnsupported
}
func (*approvalCaptureRuntime) Capabilities(context.Context) (bridgeRuntime.Capabilities, error) {
	return bridgeRuntime.Capabilities{}, bridgeRuntime.ErrUnsupported
}
func (*approvalCaptureRuntime) Start(context.Context, bridgeRuntime.StartRequest, kernel.EventSink) (bridgeRuntime.Session, error) {
	return bridgeRuntime.Session{}, bridgeRuntime.ErrUnsupported
}
func (*approvalCaptureRuntime) Resume(context.Context, bridgeRuntime.ResumeRequest, kernel.EventSink) (bridgeRuntime.Session, error) {
	return bridgeRuntime.Session{}, bridgeRuntime.ErrUnsupported
}
func (*approvalCaptureRuntime) Steer(context.Context, bridgeRuntime.Session, kernel.Input) error {
	return bridgeRuntime.ErrUnsupported
}
func (*approvalCaptureRuntime) Interrupt(context.Context, bridgeRuntime.Session) error {
	return bridgeRuntime.ErrUnsupported
}
func (*approvalCaptureRuntime) Close(context.Context, bridgeRuntime.Session) error {
	return bridgeRuntime.ErrUnsupported
}
func (*approvalCaptureRuntime) Fork(context.Context, bridgeRuntime.StartRequest, kernel.EventSink) (bridgeRuntime.Session, error) {
	return bridgeRuntime.Session{}, bridgeRuntime.ErrUnsupported
}
func (a *approvalCaptureRuntime) ResolveApproval(_ context.Context, decision bridgeRuntime.ApprovalDecision) error {
	a.userID = decision.UserID
	return nil
}
func (*approvalCaptureRuntime) Usage(context.Context) (bridgeRuntime.Usage, error) {
	return bridgeRuntime.Usage{}, bridgeRuntime.ErrUnsupported
}
func (*approvalCaptureRuntime) AuthStatus(context.Context) (bridgeRuntime.AuthStatus, error) {
	return bridgeRuntime.AuthStatus{}, bridgeRuntime.ErrUnsupported
}

var _ bridgeRuntime.Adapter = (*approvalCaptureRuntime)(nil)

// A client that cannot locate a repository on disk cannot show anything the
// repository itself holds, which is what kept Kovan from ever reading the hive's
// own memory files. The checkout is reported; the id remains the only thing work
// is resolved against.
func TestRepositoryProfilesReportTheControlCheckout(t *testing.T) {
	catalog := repositoryCatalog{workspace: &workspaceAdapter{
		profiles: map[string]bridgegit.RepositoryProfile{
			"platform": {
				ControlCheckout: "/Users/keeper/kovan-hive/checkouts/platform",
				Remote:          "origin",
				BaseRef:         "refs/heads/hive/landing",
				WorktreeRoot:    "/Users/keeper/kovan-hive/worktrees/platform",
			},
		},
	}}

	profiles, err := catalog.RepositoryProfiles(context.Background())
	if err != nil {
		t.Fatalf("profiles: %v", err)
	}
	if len(profiles) != 1 {
		t.Fatalf("expected one profile, got %d", len(profiles))
	}
	got := profiles[0]
	if got.ID != "platform" {
		t.Fatalf("id must stay the configured id, got %q", got.ID)
	}
	if got.CheckoutPath != "/Users/keeper/kovan-hive/checkouts/platform" {
		t.Fatalf("checkout path not reported, got %q", got.CheckoutPath)
	}
	if got.Remote != "origin" || got.BaseRef != "refs/heads/hive/landing" {
		t.Fatalf("existing fields must be unchanged, got %+v", got)
	}
}

func TestRepositoryCatalogResolvesSnapshotsThroughActiveWorkspaceProfiles(t *testing.T) {
	catalog := repositoryCatalog{workspace: &workspaceAdapter{
		profiles: map[string]bridgegit.RepositoryProfile{
			"platform": {
				ControlCheckout: "/Users/keeper/kovan-hive/checkouts/platform",
				Remote:          "origin",
				BaseRef:         "refs/heads/hive/landing",
				WorktreeRoot:    "/Users/keeper/kovan-hive/worktrees/platform",
			},
		},
	}}
	resolved, err := catalog.ResolveRepositoryProfile(context.Background(), "platform")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ProfileID != "platform" || resolved.CheckoutPath == "" || resolved.Remote != "origin" || resolved.AllowedRef != "refs/heads/hive/landing" {
		t.Fatalf("resolved profile = %#v", resolved)
	}
	encoded, err := json.Marshal(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "checkout") || strings.Contains(string(encoded), "/Users/") {
		t.Fatalf("internal resolved profile is not JSON path-safe: %s", encoded)
	}
	if _, err := catalog.ResolveRepositoryProfile(context.Background(), "missing"); !errors.Is(err, repositorysnapshot.ErrNotConfigured) {
		t.Fatalf("missing profile error = %v", err)
	}
}

// Adding a repository was possible only at first open. What the configurator
// refuses matters more than what it accepts: a bee stranded at dispatch is worse
// than a registration turned down.
func TestConfigureRepositoryRefusesWhatCannotHoldAWorktree(t *testing.T) {
	configurator := repositoryConfigurator{configPath: "/nonexistent/config.yaml"}

	_, err := configurator.ConfigureRepository(context.Background(), localcontrol.ConfigureRepositoryRequest{
		ID: "platform", CheckoutPath: "relative/path", BaseRef: "refs/heads/hive/landing",
		Verification: []string{"go", "test", "./..."},
	})
	if err == nil {
		t.Fatal("a relative checkout must be refused")
	}

	// An existing directory that is not a checkout cannot hold a worktree, and
	// finding that out at dispatch would strand a bee.
	_, err = configurator.ConfigureRepository(context.Background(), localcontrol.ConfigureRepositoryRequest{
		ID: "platform", CheckoutPath: t.TempDir(), BaseRef: "refs/heads/hive/landing",
		Verification: []string{"go", "test", "./..."},
	})
	if err == nil {
		t.Fatal("a folder that is not a Git checkout must be refused")
	}

	// A repository with no verification can never produce durable evidence, so it
	// is refused rather than configured into uselessness.
	checkout := t.TempDir()
	if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o755); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	_, err = configurator.ConfigureRepository(context.Background(), localcontrol.ConfigureRepositoryRequest{
		ID: "platform", CheckoutPath: checkout, BaseRef: "refs/heads/hive/landing",
	})
	if err == nil {
		t.Fatal("a repository without a verification command must be refused")
	}
}
