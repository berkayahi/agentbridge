package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/berkayahi/agentbridge/internal/approval"
	bridgeapp "github.com/berkayahi/agentbridge/internal/controller/standalone"
	"github.com/berkayahi/agentbridge/internal/deviceidentity"
	"github.com/berkayahi/agentbridge/internal/kernel"
	"github.com/berkayahi/agentbridge/internal/localcontrol"
	"github.com/berkayahi/agentbridge/internal/managed"
	"github.com/berkayahi/agentbridge/internal/provider"
	"github.com/berkayahi/agentbridge/internal/provider/claude"
	bridgeRuntime "github.com/berkayahi/agentbridge/internal/runtime"
	"github.com/berkayahi/agentbridge/internal/store/sqlite"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

// localRuntimeExecutor is the production bridge between the transport-neutral
// local controller and the registered runtime adapters. It is deliberately
// the only component that resolves an opaque repository binding to a
// configured checkout/worktree profile.
type localRuntimeExecutor struct {
	store     *sqlite.RuntimeStore
	runtimes  *bridgeRuntime.Registry
	workspace *workspaceAdapter
	models    map[workmodel.Provider]string
	approvals *approval.Broker
	// approvalUser is the provider-native identity used by this runtime. The
	// local API carries localcontrol.LocalAuthorityUserID over the controller
	// boundary, then this executor maps it to Telegram/Codex or headless
	// provider identity without weakening provider-side checks.
	approvalUser string

	mu       sync.Mutex
	ctx      context.Context
	sessions map[string]bridgeRuntime.Session
}

func newLocalRuntimeExecutor(data *sqlite.RuntimeStore, runtimes *bridgeRuntime.Registry, workspace *workspaceAdapter, models map[workmodel.Provider]string, approvalUser string) *localRuntimeExecutor {
	return &localRuntimeExecutor{store: data, runtimes: runtimes, workspace: workspace, models: models, approvalUser: strings.TrimSpace(approvalUser), sessions: make(map[string]bridgeRuntime.Session)}
}

func newLocalRemoteDeviceFactory(data *sqlite.RuntimeStore, controllerIdentity deviceidentity.Key) localcontrol.RemoteDeviceFactory {
	return func(ctx context.Context, view localcontrol.TaskView) (localcontrol.DeviceRuntime, error) {
		if data == nil || !controllerIdentity.HasPrivate() || view.TargetDeviceID == localcontrol.LocalDeviceID {
			return nil, localcontrol.ErrNotConfigured
		}
		device, err := data.GetDevice(ctx, view.TargetDeviceID)
		if err != nil {
			return nil, err
		}
		if device.Kind != localcontrol.DeviceKindRaspberryPi || device.ConnectionEpoch != view.TargetEpoch {
			return nil, localcontrol.ErrDeviceFence
		}
		if strings.TrimSpace(device.Endpoint) == "" {
			return nil, localcontrol.ErrNotConfigured
		}
		peerPublicKey, err := data.DevicePublicKey(ctx, device.ID)
		if err != nil {
			return nil, err
		}
		link, err := localcontrol.NewWebSocketDeviceLink(ctx, localcontrol.WebSocketDeviceLinkConfig{
			Identity: controllerIdentity, PeerPublicKey: peerPublicKey,
			OrganizationID: "local", DeviceID: device.ID,
			ConnectionEpoch: view.TargetEpoch, ControllerEpoch: 1, Endpoint: device.Endpoint,
			NextSequence: func(sequenceContext context.Context) (uint64, uint64, error) {
				return data.NextDeviceLinkSequence(sequenceContext, device.ID)
			},
		})
		if err != nil {
			markPairedDeviceUnreachable(ctx, data, device.ID)
			return nil, fmt.Errorf("connect paired device %q: %w", device.ID, errors.Join(localcontrol.ErrDeviceUnreachable, err))
		}
		runtime, err := localcontrol.NewFencedLinkedRuntime(device.ID, view.TargetEpoch, link, link.Close)
		if err != nil {
			_ = link.Close()
			return nil, err
		}
		return &reachabilityDeviceRuntime{
			DeviceRuntime: runtime,
			markUnreachable: func(markContext context.Context) {
				markPairedDeviceUnreachable(markContext, data, device.ID)
			},
		}, nil
	}
}

type reachabilityDeviceRuntime struct {
	localcontrol.DeviceRuntime
	markUnreachable func(context.Context)
}

func (r *reachabilityDeviceRuntime) observe(ctx context.Context, err error) error {
	if err != nil && isDeviceTransportFailure(err) && r.markUnreachable != nil {
		r.markUnreachable(ctx)
	}
	if err != nil && isDeviceTransportFailure(err) {
		return errors.Join(localcontrol.ErrDeviceUnreachable, err)
	}
	return err
}

func (r *reachabilityDeviceRuntime) Start(ctx context.Context, view localcontrol.TaskView, request localcontrol.StartRequest) error {
	return r.observe(ctx, r.DeviceRuntime.Start(ctx, view, request))
}
func (r *reachabilityDeviceRuntime) Resume(ctx context.Context, view localcontrol.TaskView, request localcontrol.ResumeRequest) error {
	return r.observe(ctx, r.DeviceRuntime.Resume(ctx, view, request))
}
func (r *reachabilityDeviceRuntime) Approve(ctx context.Context, view localcontrol.TaskView, approvalID, userID string, allow bool) error {
	return r.observe(ctx, r.DeviceRuntime.Approve(ctx, view, approvalID, userID, allow))
}
func (r *reachabilityDeviceRuntime) Cancel(ctx context.Context, view localcontrol.TaskView) error {
	return r.observe(ctx, r.DeviceRuntime.Cancel(ctx, view))
}
func (r *reachabilityDeviceRuntime) Verify(ctx context.Context, view localcontrol.TaskView) (localcontrol.VerificationReceipt, error) {
	receipt, err := r.DeviceRuntime.Verify(ctx, view)
	return receipt, r.observe(ctx, err)
}
func (r *reachabilityDeviceRuntime) Commit(ctx context.Context, view localcontrol.TaskView) (localcontrol.CommitReceipt, error) {
	receipt, err := r.DeviceRuntime.Commit(ctx, view)
	return receipt, r.observe(ctx, err)
}

func (r *reachabilityDeviceRuntime) Observe(ctx context.Context, view localcontrol.TaskView, after uint64) (localcontrol.DeviceObservation, error) {
	observer, ok := r.DeviceRuntime.(localcontrol.DeviceObserver)
	if !ok {
		return localcontrol.DeviceObservation{}, r.observe(ctx, localcontrol.ErrNotConfigured)
	}
	value, err := observer.Observe(ctx, view, after)
	return value, r.observe(ctx, err)
}

func isDeviceTransportFailure(err error) bool {
	if err == nil || errors.Is(err, localcontrol.ErrDeviceFence) || errors.Is(err, localcontrol.ErrDeviceLinkUnauthenticated) || errors.Is(err, localcontrol.ErrDeviceLinkProtocol) {
		return false
	}
	if errors.Is(err, localcontrol.ErrDeviceLinkUnavailable) || errors.Is(err, managed.ErrTransportClosed) {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError) || errors.Is(err, os.ErrDeadlineExceeded)
}

func markPairedDeviceUnreachable(ctx context.Context, data *sqlite.RuntimeStore, deviceID string) {
	if data == nil || strings.TrimSpace(deviceID) == "" {
		return
	}
	markContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	_ = data.MarkDeviceUnreachable(markContext, deviceID)
}

func (e *localRuntimeExecutor) SetContext(ctx context.Context) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ctx = ctx
}

func (e *localRuntimeExecutor) runtimeContext() context.Context {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ctx == nil {
		return context.Background()
	}
	return e.ctx
}

func (e *localRuntimeExecutor) Start(ctx context.Context, view localcontrol.TaskView, request localcontrol.StartRequest) error {
	if e == nil || e.store == nil || e.runtimes == nil || e.workspace == nil {
		return localcontrol.ErrNotConfigured
	}
	if view.TargetDeviceID != localcontrol.LocalDeviceID {
		return fmt.Errorf("target device %q requires a paired execution link: %w", view.TargetDeviceID, localcontrol.ErrNotConfigured)
	}
	target, err := e.repositoryTarget(ctx, view.RepositoryID)
	if err != nil {
		return err
	}
	task, err := e.store.Task(ctx, view.ID)
	if err != nil {
		return err
	}
	workspace, err := e.workspace.Prepare(ctx, target.profileID, view.ID)
	if err != nil {
		return err
	}
	if err := e.store.SaveWorkspace(ctx, view.ID, workspace.BaseSHA, workspace.Path); err != nil {
		return err
	}
	adapter, err := e.runtimes.Get(view.RuntimeID)
	if err != nil {
		return fmt.Errorf("local start runtime: %w", err)
	}
	input := strings.TrimSpace(request.Input)
	if input == "" {
		input = task.Prompt
	}
	model := strings.TrimSpace(request.Model)
	if model == "" {
		model = e.models[view.Provider]
	}
	startCtx := e.runtimeContext()
	session, err := adapter.Start(startCtx, bridgeRuntime.StartRequest{
		TaskID: view.ID, ExecutionID: view.ExecutionID, WorkingDirectory: workspace.Path,
		Model: model, Input: kernel.Input{Text: input},
	}, kernel.NewDurableEventSink(e.store))
	if err != nil {
		return err
	}
	providerSessionID := session.ExternalID
	if providerSessionID == "" {
		providerSessionID = session.ID
	}
	if err := e.persistRuntimeSession(ctx, view, session); err != nil {
		_ = adapter.Interrupt(context.WithoutCancel(startCtx), session)
		return err
	}
	e.mu.Lock()
	e.sessions[view.ID] = session
	e.mu.Unlock()
	return nil
}

func (e *localRuntimeExecutor) Resume(ctx context.Context, view localcontrol.TaskView, request localcontrol.ResumeRequest) error {
	if e == nil || e.store == nil || e.runtimes == nil || e.workspace == nil {
		return localcontrol.ErrNotConfigured
	}
	if view.TargetDeviceID != localcontrol.LocalDeviceID {
		return fmt.Errorf("target device %q requires a paired execution link: %w", view.TargetDeviceID, localcontrol.ErrNotConfigured)
	}
	if _, err := e.repositoryTarget(ctx, view.RepositoryID); err != nil {
		return err
	}
	task, err := e.store.Task(ctx, view.ID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(task.WorktreePath) == "" || strings.TrimSpace(task.ProviderSessionID) == "" {
		return fmt.Errorf("task has no durable resumable session: %w", localcontrol.ErrNotConfigured)
	}
	adapter, err := e.runtimes.Get(view.RuntimeID)
	if err != nil {
		return err
	}
	providerTaskID, err := provider.NewID(view.ID)
	if err != nil {
		return err
	}
	providerSessionID, err := provider.NewID(task.ProviderSessionID)
	if err != nil {
		return err
	}
	input := strings.TrimSpace(request.Input)
	if input == "" {
		input = "Continue the interrupted task from the durable session."
	}
	startCtx := e.runtimeContext()
	session, err := adapter.Resume(startCtx, bridgeRuntime.ResumeRequest{
		TaskID: view.ID, ExecutionID: view.ExecutionID,
		Session: bridgeRuntime.Session{
			ID: providerSessionID.String(), TaskID: view.ID, ExternalID: task.ProviderSessionID,
			ThreadID: task.ProviderThreadID, RuntimeID: view.RuntimeID,
			Native: provider.Session{ID: providerSessionID, TaskID: providerTaskID, ExternalID: task.ProviderSessionID, ThreadID: task.ProviderThreadID, Provider: view.Provider},
		},
		Input: kernel.Input{Text: input},
	}, kernel.NewDurableEventSink(e.store))
	if err != nil {
		return err
	}
	if err := e.persistRuntimeSession(ctx, view, session); err != nil {
		_ = adapter.Interrupt(context.WithoutCancel(startCtx), session)
		return err
	}
	e.mu.Lock()
	e.sessions[view.ID] = session
	e.mu.Unlock()
	return nil
}

func (e *localRuntimeExecutor) persistRuntimeSession(ctx context.Context, view localcontrol.TaskView, session bridgeRuntime.Session) error {
	providerSessionID := session.ExternalID
	if providerSessionID == "" {
		providerSessionID = session.ID
	}
	now := time.Now().UTC()
	return e.store.SaveProviderSession(ctx, view.ID, workmodel.Session{
		ID: view.SessionID, TaskID: view.ID, Provider: view.Provider,
		ProviderSessionID: providerSessionID, ProviderThreadID: session.ThreadID,
		Status: "running", Resumable: true, CreatedAt: now, UpdatedAt: now,
	})
}

func (e *localRuntimeExecutor) Approve(ctx context.Context, view localcontrol.TaskView, approvalID, userID string, allow bool) error {
	if e == nil || e.runtimes == nil {
		return localcontrol.ErrNotConfigured
	}
	if view.TargetDeviceID != localcontrol.LocalDeviceID {
		return fmt.Errorf("target device %q requires a paired execution link: %w", view.TargetDeviceID, localcontrol.ErrNotConfigured)
	}
	adapter, err := e.runtimes.Get(view.RuntimeID)
	if err != nil {
		return err
	}
	providerUserID := strings.TrimSpace(userID)
	if providerUserID == localcontrol.LocalAuthorityUserID && strings.TrimSpace(e.approvalUser) != "" {
		providerUserID = e.approvalUser
	}
	err = adapter.ResolveApproval(ctx, bridgeRuntime.ApprovalDecision{
		RequestID: approvalID, TaskID: view.ID, ExecutionID: view.ExecutionID, UserID: providerUserID, Allow: allow,
	})
	if errors.Is(err, claude.ErrApprovalViaMCP) && e.approvals != nil {
		return e.approvals.HandleDecision(ctx, view.ID, approvalID, providerUserID, allow)
	}
	return err
}

func (e *localRuntimeExecutor) Cancel(ctx context.Context, view localcontrol.TaskView) error {
	if e == nil || e.runtimes == nil {
		return nil
	}
	if view.TargetDeviceID != localcontrol.LocalDeviceID {
		return nil
	}
	e.mu.Lock()
	session, ok := e.sessions[view.ID]
	delete(e.sessions, view.ID)
	e.mu.Unlock()
	if !ok {
		// A restart may have already torn down the native provider process. The
		// durable controller state is authoritative, so there is no native
		// session to interrupt in this process.
		return nil
	}
	adapter, err := e.runtimes.Get(view.RuntimeID)
	if err != nil {
		return err
	}
	if err := adapter.Interrupt(ctx, session); errors.Is(err, bridgeRuntime.ErrInvalidSession) {
		return nil
	} else {
		return err
	}
}

type localRepositoryTarget struct {
	profileID string
}

func (e *localRuntimeExecutor) repositoryTarget(ctx context.Context, repositoryID string) (localRepositoryTarget, error) {
	if e.workspace == nil {
		return localRepositoryTarget{}, localcontrol.ErrNotConfigured
	}
	if _, ok := e.workspace.profiles[repositoryID]; ok {
		return localRepositoryTarget{profileID: repositoryID}, nil
	}
	repository, err := e.store.GetRepository(ctx, repositoryID)
	if err != nil {
		return localRepositoryTarget{}, err
	}
	var match string
	for profileID, profile := range e.workspace.profiles {
		if profile.Remote != repository.Remote {
			continue
		}
		if match != "" {
			return localRepositoryTarget{}, fmt.Errorf("repository binding %q maps to multiple configured profiles: %w", repositoryID, localcontrol.ErrInvalidRequest)
		}
		match = profileID
	}
	if match == "" {
		return localRepositoryTarget{}, fmt.Errorf("repository binding %q is not configured: %w", repositoryID, localcontrol.ErrInvalidRequest)
	}
	return localRepositoryTarget{profileID: match}, nil
}

type localRepositoryOperations struct {
	store     *sqlite.RuntimeStore
	workspace *workspaceAdapter
	delivery  *deliveryAdapter
}

func (o localRepositoryOperations) taskWorkspace(ctx context.Context, view localcontrol.TaskView) (workmodel.Task, bridgeapp.Workspace, error) {
	task, err := o.store.Task(ctx, view.ID)
	if err != nil {
		return workmodel.Task{}, bridgeapp.Workspace{}, err
	}
	if strings.TrimSpace(task.WorktreePath) == "" || strings.TrimSpace(task.BaseSHA) == "" {
		return workmodel.Task{}, bridgeapp.Workspace{}, fmt.Errorf("task workspace is not prepared: %w", localcontrol.ErrNotConfigured)
	}
	target := localRuntimeExecutor{store: o.store, workspace: o.workspace}
	profile, err := target.repositoryTarget(ctx, view.RepositoryID)
	if err != nil {
		return workmodel.Task{}, bridgeapp.Workspace{}, err
	}
	task.RepoProfileID = profile.profileID
	return task, bridgeapp.Workspace{BaseSHA: task.BaseSHA, Path: task.WorktreePath}, nil
}

type localVerifier struct{ operations localRepositoryOperations }

func (v localVerifier) Verify(ctx context.Context, view localcontrol.TaskView) (localcontrol.VerificationReceipt, error) {
	task, workspace, err := v.operations.taskWorkspace(ctx, view)
	if err != nil {
		return localcontrol.VerificationReceipt{}, err
	}
	if err := v.operations.delivery.Verify(ctx, task, workspace); err != nil {
		return localcontrol.VerificationReceipt{}, err
	}
	return localcontrol.VerificationReceipt{Passed: true, Summary: "configured verification passed", ObservedAt: time.Now().UTC()}, nil
}

type localCommitter struct{ operations localRepositoryOperations }

func (c localCommitter) Commit(ctx context.Context, view localcontrol.TaskView) (localcontrol.CommitReceipt, error) {
	task, workspace, err := c.operations.taskWorkspace(ctx, view)
	if err != nil {
		return localcontrol.CommitReceipt{}, err
	}
	commit, err := c.operations.delivery.Commit(ctx, task, workspace)
	if err != nil {
		return localcontrol.CommitReceipt{}, err
	}
	ref, err := c.operations.delivery.Push(ctx, task, workspace, commit)
	if err != nil {
		return localcontrol.CommitReceipt{}, err
	}
	return localcontrol.CommitReceipt{CommitSHA: commit, RemoteRef: ref, ObservedAt: time.Now().UTC()}, nil
}

var _ localcontrol.Executor = (*localRuntimeExecutor)(nil)
var _ localcontrol.Verifier = localVerifier{}
var _ localcontrol.Committer = localCommitter{}
var _ localcontrol.DeviceObserver = (*reachabilityDeviceRuntime)(nil)
