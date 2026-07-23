package codex

import (
	"context"
	"time"

	"github.com/berkayahi/agentbridge/internal/kernel"
	"github.com/berkayahi/agentbridge/internal/provider"
	bridgeRuntime "github.com/berkayahi/agentbridge/internal/runtime"
)

type RuntimeAdapter struct{ native *Adapter }

func NewRuntimeAdapter(native *Adapter) *RuntimeAdapter { return &RuntimeAdapter{native: native} }
func (a *RuntimeAdapter) ID() string                    { return "codex" }
func (a *RuntimeAdapter) Detect(context.Context) (bridgeRuntime.Installation, error) {
	return bridgeRuntime.Installation{ID: a.ID()}, nil
}
func (a *RuntimeAdapter) Capabilities(context.Context) (bridgeRuntime.Capabilities, error) {
	return bridgeRuntime.Capabilities{RuntimeVersion: "codex-jsonrpc-v2", ObservedAt: time.Now().UTC(), Start: true, Resume: true, Steer: true, Interrupt: true, Close: true, Approvals: true, Usage: true, NativeApprovalModes: []bridgeRuntime.ApprovalMode{bridgeRuntime.ApprovalProviderDefault}}, nil
}
func (a *RuntimeAdapter) Start(ctx context.Context, request bridgeRuntime.StartRequest, sink kernel.EventSink) (bridgeRuntime.Session, error) {
	taskID, err := provider.NewID(request.TaskID)
	if err != nil {
		return bridgeRuntime.Session{}, err
	}
	session, events, err := a.native.Start(ctx, provider.StartRequest{TaskID: taskID, Input: bridgeRuntime.ProviderInput(request.Input), WorkingDirectory: request.WorkingDirectory, Model: request.Model})
	if err != nil {
		return bridgeRuntime.Session{}, err
	}
	go bridgeRuntime.RelayProviderEvents(ctx, request.ExecutionID, events, sink)
	return bridgeRuntime.RuntimeSession(session, a.ID()), nil
}
func (a *RuntimeAdapter) Resume(ctx context.Context, request bridgeRuntime.ResumeRequest, sink kernel.EventSink) (bridgeRuntime.Session, error) {
	taskID, err := provider.NewID(request.TaskID)
	if err != nil {
		return bridgeRuntime.Session{}, err
	}
	native, ok := bridgeRuntime.ProviderSession(request.Session)
	if !ok {
		return bridgeRuntime.Session{}, bridgeRuntime.ErrInvalidSession
	}
	session, events, err := a.native.Resume(ctx, provider.ResumeRequest{TaskID: taskID, Session: native, Input: bridgeRuntime.ProviderInput(request.Input)})
	if err != nil {
		return bridgeRuntime.Session{}, err
	}
	go bridgeRuntime.RelayProviderEvents(ctx, request.ExecutionID, events, sink)
	return bridgeRuntime.RuntimeSession(session, a.ID()), nil
}
func (a *RuntimeAdapter) Steer(ctx context.Context, session bridgeRuntime.Session, input kernel.Input) error {
	native, ok := bridgeRuntime.ProviderSession(session)
	if !ok {
		return bridgeRuntime.ErrInvalidSession
	}
	return a.native.Steer(ctx, native, bridgeRuntime.ProviderInput(input))
}
func (a *RuntimeAdapter) Interrupt(ctx context.Context, session bridgeRuntime.Session) error {
	native, ok := bridgeRuntime.ProviderSession(session)
	if !ok {
		return bridgeRuntime.ErrInvalidSession
	}
	return a.native.Interrupt(ctx, native)
}
func (a *RuntimeAdapter) Close(ctx context.Context, session bridgeRuntime.Session) error {
	return a.Interrupt(ctx, session)
}
func (a *RuntimeAdapter) Fork(context.Context, bridgeRuntime.StartRequest, kernel.EventSink) (bridgeRuntime.Session, error) {
	return bridgeRuntime.Session{}, bridgeRuntime.ErrUnsupported
}
func (a *RuntimeAdapter) ResolveApproval(ctx context.Context, decision bridgeRuntime.ApprovalDecision) error {
	requestID, err := provider.NewID(decision.RequestID)
	if err != nil {
		return err
	}
	taskID, err := provider.NewID(decision.TaskID)
	if err != nil {
		return err
	}
	return a.native.ResolveApproval(ctx, provider.ApprovalDecision{RequestID: requestID, TaskID: taskID, UserID: decision.UserID, Allow: decision.Allow, DecidedAt: time.Now().UTC()})
}
func (a *RuntimeAdapter) Usage(ctx context.Context) (bridgeRuntime.Usage, error) {
	value, err := a.native.Usage(ctx)
	if err != nil {
		return bridgeRuntime.Usage{}, err
	}
	result := bridgeRuntime.Usage{RuntimeID: a.ID(), Observed: value.ObservedAt}
	for _, window := range value.Windows {
		result.Windows = append(result.Windows, bridgeRuntime.UsageWindow{Name: window.Name, UsedPercent: window.UsedPercent, ResetsAt: window.ResetsAt})
	}
	return result, nil
}
func (a *RuntimeAdapter) AuthStatus(ctx context.Context) (bridgeRuntime.AuthStatus, error) {
	value, err := a.native.AuthStatus(ctx)
	if err != nil {
		return bridgeRuntime.AuthStatus{}, err
	}
	return bridgeRuntime.AuthStatus{Authenticated: value.Authenticated, Account: value.Account, CheckedAt: value.CheckedAt}, nil
}
