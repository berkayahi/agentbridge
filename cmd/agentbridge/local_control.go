package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/berkayahi/agentbridge/internal/approval"
	"github.com/berkayahi/agentbridge/internal/config"
	bridgeapp "github.com/berkayahi/agentbridge/internal/controller/standalone"
	"github.com/berkayahi/agentbridge/internal/deviceidentity"
	bridgegit "github.com/berkayahi/agentbridge/internal/git"
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
	// writable is the extra roots each repository's sessions may write to,
	// keyed by profile id: a repository's toolchain caches live outside the
	// worktree, and without them every build escalates to an approval.
	writable  map[string][]string
	approvals *approval.Broker
	// approvalUser is the provider-native identity used by this runtime. The
	// local API carries localcontrol.LocalAuthorityUserID over the controller
	// boundary, then this executor maps it to Telegram/Codex or headless
	// provider identity without weakening provider-side checks.
	approvalUser string

	// progress makes locally executed work observable on the task's own event
	// log. Without it a local run is durable but invisible.
	progress localcontrol.LocalProgress

	mu       sync.Mutex
	ctx      context.Context
	sessions map[string]bridgeRuntime.Session
}

// observationSink records provider evidence in the execution journal and then
// projects it onto the local task, so a keeper can watch a bee work.
//
// It also releases the runtime session once her turn ends. The session is only
// needed while she can still be steered or interrupted; holding it afterwards
// leaked one entry per bee for the life of the process, which for a hive left
// open all day is a slow leak of exactly the thing that grows with use.
func (e *localRuntimeExecutor) observationSink(view localcontrol.TaskView) kernel.EventSink {
	durable := kernel.NewDurableEventSink(e.store)
	if e.progress == nil {
		return durable
	}
	return localcontrol.NewLocalObservationSink(sessionReleasingSink{inner: durable, executor: e, taskID: view.ID}, e.progress, view.ID)
}

// sessionReleasingSink drops the held session when the provider reports the turn
// is over. It wraps the durable sink rather than the projection so evidence is
// still committed first.
type sessionReleasingSink struct {
	inner    kernel.EventSink
	executor *localRuntimeExecutor
	taskID   string
}

func (s sessionReleasingSink) Append(ctx context.Context, event kernel.Event) error {
	err := s.inner.Append(ctx, event)
	switch event.Type {
	case "provider_completed", "provider_error":
		s.executor.releaseSession(s.taskID)
	}
	return err
}

func (e *localRuntimeExecutor) releaseSession(taskID string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	delete(e.sessions, taskID)
	e.mu.Unlock()
}

// HeldSessions reports how many provider sessions this executor is holding. It
// exists so a leak is observable rather than a matter of belief.
func (e *localRuntimeExecutor) HeldSessions() int {
	if e == nil {
		return 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.sessions)
}

func newLocalRuntimeExecutor(data *sqlite.RuntimeStore, runtimes *bridgeRuntime.Registry, workspace *workspaceAdapter, models map[workmodel.Provider]string, writable map[string][]string, approvalUser string) *localRuntimeExecutor {
	return &localRuntimeExecutor{store: data, runtimes: runtimes, workspace: workspace, models: models, writable: writable, approvalUser: strings.TrimSpace(approvalUser), sessions: make(map[string]bridgeRuntime.Session)}
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

// chosenModel resolves which model a session flies with. The keeper's choice at
// dispatch is a fact about the task, so it outlives the process and a resume
// after a restart reaches the same model; the configured default applies only
// when nothing was ever chosen.
func chosenModel(requested, task, configured string) string {
	for _, candidate := range []string{requested, task, configured} {
		if strings.TrimSpace(candidate) != "" {
			return strings.TrimSpace(candidate)
		}
	}
	return ""
}

func chosenExecutionProfile(task workmodel.ExecutionProfile, requestedModel, configuredModel string) workmodel.ExecutionProfile {
	if !task.Empty() {
		return task
	}
	return workmodel.ExecutionProfile{Model: chosenModel(requestedModel, "", configuredModel)}
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
	if strings.TrimSpace(task.WorktreePath) == "" && strings.TrimSpace(task.ProviderSessionID) == "" {
		if err := e.workspace.RecoverUnrecorded(ctx, target.profileID, view.ID); err != nil {
			return err
		}
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
	input = executionInput(input)
	profile := chosenExecutionProfile(task.ExecutionProfile, strings.TrimSpace(request.Model), e.models[view.Provider])
	startCtx := e.runtimeContext()
	session, err := adapter.Start(startCtx, bridgeRuntime.StartRequest{
		TaskID: view.ID, ExecutionID: view.ExecutionID, WorkingDirectory: workspace.Path,
		Model: profile.Model, ExecutionProfile: profile,
		WritablePaths: e.writable[target.profileID], Input: kernel.Input{Text: input},
	}, e.observationSink(view))
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

func executionInput(input string) string {
	return `AgentBridge owns Git delivery for this isolated workspace.
- Do not run git commit, git push, or modify local or remote refs.
- Leave the completed changes in the worktree and report when implementation and relevant verification are finished.
- AgentBridge will scan, commit, and push the work only through the configured delivery boundary.

Operator task:
` + strings.TrimSpace(input)
}

func (e *localRuntimeExecutor) Resume(ctx context.Context, view localcontrol.TaskView, request localcontrol.ResumeRequest) error {
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
	profile := chosenExecutionProfile(task.ExecutionProfile, "", e.models[view.Provider])
	session, err := adapter.Resume(startCtx, bridgeRuntime.ResumeRequest{
		TaskID: view.ID, ExecutionID: view.ExecutionID, Model: profile.Model, ExecutionProfile: profile,
		WritablePaths: e.writable[target.profileID],
		Session: bridgeRuntime.Session{
			ID: providerSessionID.String(), TaskID: view.ID, ExternalID: task.ProviderSessionID,
			ThreadID: task.ProviderThreadID, RuntimeID: view.RuntimeID,
			Native: provider.Session{ID: providerSessionID, TaskID: providerTaskID, ExternalID: task.ProviderSessionID, ThreadID: task.ProviderThreadID, Provider: view.Provider},
		},
		Input: kernel.Input{Text: input},
	}, e.observationSink(view))
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

// Steer carries the keeper's instruction into the live provider session. It
// requires a session held by this process: after a restart the native process
// is gone and the honest answer is that there is nobody left to talk to.
func (e *localRuntimeExecutor) Steer(ctx context.Context, view localcontrol.TaskView, request localcontrol.SteerRequest) error {
	if e == nil || e.runtimes == nil {
		return localcontrol.ErrNotConfigured
	}
	if view.TargetDeviceID != localcontrol.LocalDeviceID {
		return fmt.Errorf("target device %q requires a paired execution link: %w", view.TargetDeviceID, localcontrol.ErrNotConfigured)
	}
	e.mu.Lock()
	session, ok := e.sessions[view.ID]
	e.mu.Unlock()
	if !ok {
		return fmt.Errorf("task has no live provider session: %w", localcontrol.ErrNotConfigured)
	}
	adapter, err := e.runtimes.Get(view.RuntimeID)
	if err != nil {
		return err
	}
	return adapter.Steer(e.runtimeContext(), session, kernel.Input{Text: request.Input})
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

// repositoryCatalog reports the configured profiles from the same map the
// executor resolves against, so the authority can never register a repository
// id that repositoryTarget would later refuse.
type repositoryCatalog struct {
	workspace *workspaceAdapter
}

func (c repositoryCatalog) RepositoryProfiles(context.Context) ([]localcontrol.RepositoryProfile, error) {
	if c.workspace == nil {
		return nil, localcontrol.ErrNotConfigured
	}
	ids := make([]string, 0, len(c.workspace.profiles))
	for id := range c.workspace.profiles {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := make([]localcontrol.RepositoryProfile, 0, len(ids))
	for _, id := range ids {
		profile := c.workspace.profiles[id]
		result = append(result, localcontrol.RepositoryProfile{ID: id, Remote: profile.Remote, BaseRef: profile.BaseRef})
	}
	return result, nil
}

// providerCatalog reports configured runtimes and executable availability. A
// live provider catalog is authoritative when the protocol exposes one; static
// configuration remains the compatibility fallback for other providers.
type providerCatalog struct {
	providers map[string]config.ProviderConfig
	live      map[workmodel.Provider]provider.Provider
	runtimes  *bridgeRuntime.Registry
}

func (c providerCatalog) ProviderProfiles(ctx context.Context) ([]localcontrol.ProviderInfo, error) {
	ids := make([]string, 0, len(c.providers))
	for id := range c.providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := make([]localcontrol.ProviderInfo, 0, len(ids))
	for _, id := range ids {
		value := c.providers[id]
		info, err := os.Stat(value.Executable)
		available := err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0
		profile := localcontrol.ProviderInfo{ID: id, DefaultModel: value.Model, Models: value.Catalog(), Available: available}
		if c.runtimes != nil {
			adapter, runtimeErr := c.runtimes.Get(id)
			if runtimeErr != nil {
				return nil, fmt.Errorf("load %s runtime capabilities: %w", id, runtimeErr)
			}
			capabilities, capabilityErr := adapter.Capabilities(ctx)
			if capabilityErr != nil {
				return nil, fmt.Errorf("load %s approval modes: %w", id, capabilityErr)
			}
			for _, mode := range capabilities.NativeApprovalModes {
				detail := localcontrol.ProviderApprovalMode{ID: string(mode)}
				switch mode {
				case bridgeRuntime.ApprovalAskEveryTime:
					detail.DisplayName = "Ask me"
					detail.Description = "Pause when the provider needs authority beyond the current sandbox."
				case bridgeRuntime.ApprovalAutoWithinPolicy:
					detail.DisplayName = "Auto-review"
					detail.Description = "Let the provider review routine requests inside the current sandbox policy."
				case bridgeRuntime.ApprovalProviderDefault:
					detail.DisplayName = "Provider default"
					detail.Description = "Use the provider account's own approval configuration."
				}
				profile.ApprovalModes = append(profile.ApprovalModes, detail)
			}
		}
		native := c.live[workmodel.Provider(id)]
		cataloger, ok := native.(provider.ExecutionCatalogProvider)
		if available && ok {
			catalog, catalogErr := cataloger.ExecutionCatalog(ctx)
			if catalogErr != nil {
				return nil, fmt.Errorf("load %s execution catalog: %w", id, catalogErr)
			}
			profile.Models = profile.Models[:0]
			profile.ModelAliases = append([]string(nil), catalog.ModelAliases...)
			profile.ModelProfiles = make([]localcontrol.ProviderModel, 0, len(catalog.Models))
			profile.DefaultApprovalMode = catalog.DefaultApprovalMode
			profile.ApprovalModes = make([]localcontrol.ProviderApprovalMode, 0, len(catalog.ApprovalModes))
			for _, mode := range catalog.ApprovalModes {
				profile.ApprovalModes = append(profile.ApprovalModes, localcontrol.ProviderApprovalMode{
					ID: mode.ID, Description: mode.Description,
				})
			}
			configuredDefaultAvailable := false
			for _, model := range catalog.Models {
				profile.Models = append(profile.Models, model.ID)
				if model.ID == profile.DefaultModel || slices.Contains(model.Aliases, profile.DefaultModel) {
					configuredDefaultAvailable = true
				}
				detail := localcontrol.ProviderModel{
					ID: model.ID, DisplayName: model.DisplayName, Description: model.Description,
					Aliases:                append([]string(nil), model.Aliases...),
					DefaultReasoningEffort: model.DefaultReasoningEffort,
					SupportedApprovalModes: append([]string(nil), model.ApprovalModes...),
				}
				for _, effort := range model.ReasoningEfforts {
					detail.SupportedReasoningEfforts = append(detail.SupportedReasoningEfforts, localcontrol.ProviderReasoningEffort{
						ID: effort.ID, Description: effort.Description, Kind: string(effort.Kind),
					})
				}
				profile.ModelProfiles = append(profile.ModelProfiles, detail)
			}
			if !configuredDefaultAvailable {
				profile.DefaultModel = catalog.DefaultModel
			}
		}
		result = append(result, profile)
	}
	return result, nil
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
		// Committing writes to the repository and is opt-in per profile. Report
		// that as an actionable refusal rather than an opaque failure.
		if errors.Is(err, bridgegit.ErrDeliveryDisabled) {
			return localcontrol.CommitReceipt{}, errors.Join(localcontrol.ErrDeliveryNotEnabled, err)
		}
		return localcontrol.CommitReceipt{}, err
	}
	ref, err := c.operations.delivery.Push(ctx, task, workspace, commit)
	if err != nil {
		return localcontrol.CommitReceipt{}, err
	}
	return localcontrol.CommitReceipt{CommitSHA: commit, RemoteRef: ref, ObservedAt: time.Now().UTC()}, nil
}

type localRepositoryIntegrator struct{ operations localRepositoryOperations }

func (i localRepositoryIntegrator) Integrate(ctx context.Context, request localcontrol.IntegrateRepositoryRequest) (localcontrol.IntegrationReceipt, error) {
	if i.operations.workspace == nil || i.operations.delivery == nil {
		return localcontrol.IntegrationReceipt{}, localcontrol.ErrNotConfigured
	}
	profile, ok := i.operations.workspace.profiles[request.RepositoryID]
	if !ok {
		return localcontrol.IntegrationReceipt{}, localcontrol.ErrRepositoryNotConfigured
	}
	git := i.operations.delivery.git
	remote := profile.Remote
	sourceTracking := remoteTrackingRef(remote, request.SourceRef)
	targetTracking := remoteTrackingRef(remote, request.TargetRef)
	if _, err := git.Run(ctx, profile.ControlCheckout, "fetch", "--no-tags", remote,
		"+"+request.SourceRef+":"+sourceTracking,
		"+"+request.TargetRef+":"+targetTracking); err != nil {
		return localcontrol.IntegrationReceipt{}, fmt.Errorf("fetch integration refs: %w", err)
	}
	sourceSHA, err := resolveCommit(ctx, git, profile.ControlCheckout, sourceTracking)
	if err != nil {
		return localcontrol.IntegrationReceipt{}, err
	}
	targetSHA, err := resolveCommit(ctx, git, profile.ControlCheckout, targetTracking)
	if err != nil {
		return localcontrol.IntegrationReceipt{}, err
	}
	if sourceSHA != request.ExpectedSourceSHA {
		recovered, recoverErr := alignedIntegrationRecovery(ctx, git, profile.ControlCheckout, request, sourceSHA, targetSHA)
		if recoverErr != nil {
			return localcontrol.IntegrationReceipt{}, recoverErr
		}
		if !recovered {
			return localcontrol.IntegrationReceipt{}, fmt.Errorf("source ref changed: %w", localcontrol.ErrStaleRevision)
		}
		return localcontrol.IntegrationReceipt{
			ID: integrationReceiptID(request.IdempotencyKey), RepositoryID: request.RepositoryID,
			SourceRef: request.SourceRef, TargetRef: request.TargetRef, SourceSHA: request.ExpectedSourceSHA,
			PreviousTargetSHA: request.ExpectedTargetSHA, MergeSHA: targetSHA,
			Verification: "configured verification passed before the recovered durable target push",
			ObservedAt:   time.Now().UTC(),
		}, nil
	}
	expectedTargetSHA := request.ExpectedTargetSHA
	if expectedTargetSHA == "" {
		expectedTargetSHA = targetSHA
	}

	// A crash may happen after the target push but before the idempotency
	// response is stored. If the target now contains the exact source, accept
	// that durable Git fact and finish the optional source alignment instead of
	// creating a second merge commit.
	if _, ancestorErr := git.Run(ctx, profile.ControlCheckout, "merge-base", "--is-ancestor", sourceSHA, targetSHA); ancestorErr == nil {
		updated, updateErr := alignIntegrationSource(ctx, git, profile, request, sourceSHA, targetSHA)
		if updateErr != nil {
			return localcontrol.IntegrationReceipt{}, updateErr
		}
		return localcontrol.IntegrationReceipt{
			ID: integrationReceiptID(request.IdempotencyKey), RepositoryID: request.RepositoryID,
			SourceRef: request.SourceRef, TargetRef: request.TargetRef, SourceSHA: sourceSHA,
			PreviousTargetSHA: expectedTargetSHA, MergeSHA: targetSHA, SourceUpdated: updated,
			Verification: "configured verification passed before the durable target push",
			ObservedAt:   time.Now().UTC(),
		}, nil
	}
	if targetSHA != expectedTargetSHA {
		return localcontrol.IntegrationReceipt{}, fmt.Errorf("target ref changed: %w", localcontrol.ErrStaleRevision)
	}

	worktree := filepath.Join(profile.WorktreeRoot, "integrate-"+integrationPathID(request.IdempotencyKey))
	// A stale path from a terminated integration is routine recovery. Git owns
	// the cleanup and refuses paths outside this profile's worktree root.
	if rel, relErr := filepath.Rel(profile.WorktreeRoot, worktree); relErr != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return localcontrol.IntegrationReceipt{}, bridgegit.ErrPathCollision
	}
	_, _ = git.Run(context.WithoutCancel(ctx), profile.ControlCheckout, "worktree", "remove", "--force", worktree)
	if _, err := git.Run(ctx, profile.ControlCheckout, "worktree", "add", "--detach", worktree, targetSHA); err != nil {
		return localcontrol.IntegrationReceipt{}, fmt.Errorf("prepare integration worktree: %w", err)
	}
	defer func() {
		_, _ = git.Run(context.Background(), profile.ControlCheckout, "worktree", "remove", "--force", worktree)
	}()
	if _, err := git.Run(ctx, worktree, "merge", "--no-ff", sourceSHA, "-m", request.Message); err != nil {
		return localcontrol.IntegrationReceipt{}, fmt.Errorf("merge source into target: %w", err)
	}
	mergeSHA, err := resolveCommit(ctx, git, worktree, "HEAD")
	if err != nil {
		return localcontrol.IntegrationReceipt{}, err
	}
	if err := i.operations.delivery.Verify(ctx, workmodel.Task{RepoProfileID: request.RepositoryID}, bridgeapp.Workspace{
		BaseSHA: targetSHA, Path: worktree,
	}); err != nil {
		return localcontrol.IntegrationReceipt{}, fmt.Errorf("verify integrated result: %w", err)
	}
	lease := "--force-with-lease=" + request.TargetRef + ":" + targetSHA
	if _, err := git.Run(ctx, worktree, "push", lease, remote, mergeSHA+":"+request.TargetRef); err != nil {
		return localcontrol.IntegrationReceipt{}, fmt.Errorf("push integrated target: %w", err)
	}
	updated, err := alignIntegrationSource(ctx, git, profile, request, sourceSHA, mergeSHA)
	if err != nil {
		return localcontrol.IntegrationReceipt{}, err
	}
	return localcontrol.IntegrationReceipt{
		ID: integrationReceiptID(request.IdempotencyKey), RepositoryID: request.RepositoryID,
		SourceRef: request.SourceRef, TargetRef: request.TargetRef, SourceSHA: sourceSHA,
		PreviousTargetSHA: targetSHA, MergeSHA: mergeSHA, SourceUpdated: updated,
		Verification: "configured verification passed", ObservedAt: time.Now().UTC(),
	}, nil
}

// alignedIntegrationRecovery closes the crash window after both durable pushes
// succeeded but before the local idempotency response was stored. In that
// state UpdateSource has moved the source ref onto the target merge, so the
// original expected source no longer equals either live ref. A retry is safe
// only when the aligned tip is the exact no-ff merge AgentBridge was asked to
// create: the expected source must be a direct parent and the subject must
// still equal the bounded integration message.
func alignedIntegrationRecovery(
	ctx context.Context,
	git bridgegit.Runner,
	checkout string,
	request localcontrol.IntegrateRepositoryRequest,
	sourceSHA, targetSHA string,
) (bool, error) {
	if !request.UpdateSource || sourceSHA != targetSHA || sourceSHA == request.ExpectedSourceSHA {
		return false, nil
	}
	result, err := git.Run(ctx, checkout, "rev-list", "--parents", "-n", "1", targetSHA)
	if err != nil {
		return false, fmt.Errorf("inspect aligned integration parents: %w", err)
	}
	fields := strings.Fields(result.Stdout)
	if len(fields) < 3 || !slices.Contains(fields[1:], request.ExpectedSourceSHA) {
		return false, nil
	}
	subject, err := git.Run(ctx, checkout, "show", "-s", "--format=%s", targetSHA)
	if err != nil {
		return false, fmt.Errorf("inspect aligned integration message: %w", err)
	}
	return strings.TrimSpace(subject.Stdout) == request.Message, nil
}

func remoteTrackingRef(remote, head string) string {
	return "refs/remotes/" + remote + "/" + strings.TrimPrefix(head, "refs/heads/")
}

func resolveCommit(ctx context.Context, git bridgegit.Runner, directory, ref string) (string, error) {
	result, err := git.Run(ctx, directory, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", ref, err)
	}
	return strings.ToLower(strings.TrimSpace(result.Stdout)), nil
}

func alignIntegrationSource(ctx context.Context, git bridgegit.Runner, profile bridgegit.RepositoryProfile, request localcontrol.IntegrateRepositoryRequest, sourceSHA, mergeSHA string) (bool, error) {
	if !request.UpdateSource || sourceSHA == mergeSHA {
		return false, nil
	}
	lease := "--force-with-lease=" + request.SourceRef + ":" + sourceSHA
	if _, err := git.Run(ctx, profile.ControlCheckout, "push", lease, profile.Remote, mergeSHA+":"+request.SourceRef); err != nil {
		return false, fmt.Errorf("align source after integration: %w", err)
	}
	return true, nil
}

func integrationPathID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8])
}

func integrationReceiptID(key string) string {
	return "integration-" + integrationPathID(key)
}

var _ localcontrol.Executor = (*localRuntimeExecutor)(nil)
var _ localcontrol.Verifier = localVerifier{}
var _ localcontrol.Committer = localCommitter{}
var _ localcontrol.RepositoryIntegrator = localRepositoryIntegrator{}
var _ localcontrol.DeviceObserver = (*reachabilityDeviceRuntime)(nil)

// maxDiffBytes bounds what a keeper is shown. A huge patch is reported as
// truncated rather than silently cut, so the hive never presents part of a
// change as the whole of it.
const maxDiffBytes = 256 << 10

// Diff reports what the bee has changed in her own worktree, so a keeper can
// judge her work without opening a terminal. It reads only: the worktree is
// hers until the keeper decides to land it.
func (e *localRuntimeExecutor) Diff(ctx context.Context, view localcontrol.TaskView) (localcontrol.TaskDiff, error) {
	if e == nil || e.store == nil {
		return localcontrol.TaskDiff{}, localcontrol.ErrNotConfigured
	}
	task, err := e.store.Task(ctx, view.ID)
	if err != nil {
		return localcontrol.TaskDiff{}, err
	}
	worktree := strings.TrimSpace(task.WorktreePath)
	if worktree == "" {
		return localcontrol.TaskDiff{}, fmt.Errorf("task has no prepared worktree: %w", localcontrol.ErrNotConfigured)
	}
	git := bridgegit.Runner{}
	// Both staged and unstaged work counts: a bee may have staged part of it.
	stat, err := git.Run(ctx, worktree, "diff", "HEAD", "--numstat")
	if err != nil {
		return localcontrol.TaskDiff{}, fmt.Errorf("read task diff summary: %w", err)
	}
	diff := localcontrol.TaskDiff{Files: make([]localcontrol.DiffFile, 0, 8)}
	for _, line := range strings.Split(strings.TrimSpace(stat.Stdout), "\n") {
		fields := strings.SplitN(strings.TrimSpace(line), "\t", 3)
		if len(fields) != 3 {
			continue
		}
		file := localcontrol.DiffFile{Path: fields[2]}
		if fields[0] == "-" || fields[1] == "-" {
			file.Binary = true
		} else {
			file.Added, _ = strconv.Atoi(fields[0])
			file.Removed, _ = strconv.Atoi(fields[1])
		}
		diff.Files = append(diff.Files, file)
	}
	if len(diff.Files) == 0 {
		return diff, nil
	}
	patch, err := git.Run(ctx, worktree, "diff", "HEAD")
	if err != nil {
		return localcontrol.TaskDiff{}, fmt.Errorf("read task diff: %w", err)
	}
	diff.Patch = patch.Stdout
	if len(diff.Patch) > maxDiffBytes {
		diff.Patch = diff.Patch[:maxDiffBytes]
		diff.Truncated = true
	}
	return diff, nil
}
