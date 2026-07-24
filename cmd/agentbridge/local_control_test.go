package main

import (
	"context"
	"testing"

	"github.com/berkayahi/agentbridge/internal/kernel"
	"github.com/berkayahi/agentbridge/internal/localcontrol"
	bridgeRuntime "github.com/berkayahi/agentbridge/internal/runtime"
)

func TestLocalRuntimeExecutorMapsLocalAuthorityToProviderIdentity(t *testing.T) {
	adapter := &approvalCaptureRuntime{}
	runtimes, err := bridgeRuntime.NewRegistry(adapter)
	if err != nil {
		t.Fatal(err)
	}
	executor := newLocalRuntimeExecutor(nil, runtimes, nil, nil, "987654321")
	if err := executor.Approve(context.Background(), localcontrol.TaskView{
		ID: "task-1", TargetDeviceID: localcontrol.LocalDeviceID, RuntimeID: "codex",
	}, "approval-1", localcontrol.LocalAuthorityUserID, true); err != nil {
		t.Fatal(err)
	}
	if adapter.userID != "987654321" {
		t.Fatalf("provider approval user = %q, want configured native identity", adapter.userID)
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
