package main

import (
	"context"
	"os"
	"testing"

	"github.com/berkayahi/agentbridge/internal/config"
	"github.com/berkayahi/agentbridge/internal/kernel"
	"github.com/berkayahi/agentbridge/internal/localcontrol"
	"github.com/berkayahi/agentbridge/internal/provider"
	bridgeRuntime "github.com/berkayahi/agentbridge/internal/runtime"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

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
