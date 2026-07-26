package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/berkayahi/agentbridge/internal/config"
	bridgegit "github.com/berkayahi/agentbridge/internal/git"
	"github.com/berkayahi/agentbridge/internal/kernel"
	"github.com/berkayahi/agentbridge/internal/localcontrol"
	"github.com/berkayahi/agentbridge/internal/provider"
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
		live: map[workmodel.Provider]provider.Provider{workmodel.CodexSubscription: live},
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
}

type executionCatalogProvider struct {
	provider.Provider
	catalog provider.ExecutionCatalog
}

func (p executionCatalogProvider) ExecutionCatalog(context.Context) (provider.ExecutionCatalog, error) {
	return p.catalog, nil
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
