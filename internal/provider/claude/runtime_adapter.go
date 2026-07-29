package claude

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/berkayahi/agentbridge/internal/kernel"
	"github.com/berkayahi/agentbridge/internal/provider"
	bridgeRuntime "github.com/berkayahi/agentbridge/internal/runtime"
)

type RuntimeAdapter struct{ native *Adapter }

func NewRuntimeAdapter(native *Adapter) *RuntimeAdapter { return &RuntimeAdapter{native: native} }
func (a *RuntimeAdapter) ID() string                    { return "claude" }
func (a *RuntimeAdapter) Detect(ctx context.Context) (bridgeRuntime.Installation, error) {
	executable := strings.TrimSpace(a.native.process.Executable)
	if executable == "" {
		executable = "claude"
	}
	path, err := exec.LookPath(executable)
	if err != nil {
		return bridgeRuntime.Installation{}, err
	}
	output, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return bridgeRuntime.Installation{}, err
	}
	version := strings.TrimSpace(string(output))
	version = strings.TrimSpace(strings.TrimSuffix(version, "(Claude Code)"))
	return bridgeRuntime.Installation{ID: a.ID(), Version: version, Path: path}, nil
}
func (a *RuntimeAdapter) Capabilities(ctx context.Context) (bridgeRuntime.Capabilities, error) {
	installation, err := a.Detect(ctx)
	if err != nil {
		return bridgeRuntime.Capabilities{}, err
	}
	catalog, err := a.native.ExecutionCatalog(ctx)
	if err != nil {
		return bridgeRuntime.Capabilities{}, err
	}
	capabilities := bridgeRuntime.Capabilities{
		RuntimeVersion: installation.Version, ObservedAt: time.Now().UTC(),
		Start: true, Resume: true, Steer: true, Interrupt: true, Close: true, AuthRecovery: true, Usage: true,
	}
	seenEfforts := make(map[string]struct{})
	for _, model := range catalog.Models {
		capabilities.Models = append(capabilities.Models, bridgeRuntime.Model{ID: model.ID})
		for _, effort := range model.ReasoningEfforts {
			if _, seen := seenEfforts[effort.ID]; seen {
				continue
			}
			seenEfforts[effort.ID] = struct{}{}
			capabilities.ReasoningProfiles = append(capabilities.ReasoningProfiles, bridgeRuntime.ReasoningProfile{ID: effort.ID})
		}
	}
	for _, mode := range catalog.ApprovalModes {
		capabilities.NativeApprovalModes = append(capabilities.NativeApprovalModes, bridgeRuntime.ApprovalMode(mode.ID))
	}
	return capabilities, nil
}
func (a *RuntimeAdapter) Start(ctx context.Context, request bridgeRuntime.StartRequest, sink kernel.EventSink) (bridgeRuntime.Session, error) {
	taskID, err := provider.NewID(request.TaskID)
	if err != nil {
		return bridgeRuntime.Session{}, err
	}
	session, events, err := a.native.Start(ctx, provider.StartRequest{
		TaskID: taskID, Input: bridgeRuntime.ProviderInput(request.Input), WorkingDirectory: request.WorkingDirectory,
		Model: request.Model, ExecutionProfile: request.ExecutionProfile, WritablePaths: request.WritablePaths,
	})
	if err != nil {
		return bridgeRuntime.Session{}, err
	}
	go bridgeRuntime.RelayProviderEventsLogged(ctx, request.ExecutionID, events, sink, nil)
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
	session, events, err := a.native.Resume(ctx, provider.ResumeRequest{
		TaskID: taskID, Session: native, Input: bridgeRuntime.ProviderInput(request.Input),
		WorkingDirectory: request.WorkingDirectory,
		Model:            request.Model, ExecutionProfile: request.ExecutionProfile, WritablePaths: request.WritablePaths,
	})
	if err != nil {
		return bridgeRuntime.Session{}, err
	}
	go bridgeRuntime.RelayProviderEventsLogged(ctx, request.ExecutionID, events, sink, nil)
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
func (a *RuntimeAdapter) ResolveApproval(context.Context, bridgeRuntime.ApprovalDecision) error {
	return ErrApprovalViaMCP
}
func (a *RuntimeAdapter) Usage(ctx context.Context) (bridgeRuntime.Usage, error) {
	value, err := a.native.Usage(ctx)
	if err != nil {
		return bridgeRuntime.Usage{}, err
	}
	return bridgeRuntime.Usage{RuntimeID: a.ID(), Observed: value.ObservedAt}, nil
}
func (a *RuntimeAdapter) AuthStatus(ctx context.Context) (bridgeRuntime.AuthStatus, error) {
	value, err := a.native.AuthStatus(ctx)
	if err != nil {
		return bridgeRuntime.AuthStatus{}, err
	}
	return bridgeRuntime.AuthStatus{Authenticated: value.Authenticated, Account: value.Account, CheckedAt: value.CheckedAt}, nil
}
