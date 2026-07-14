package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	bridgeapp "github.com/berkayahi/agentbridge/internal/app"
	"github.com/berkayahi/agentbridge/internal/approval"
	"github.com/berkayahi/agentbridge/internal/attachment"
	"github.com/berkayahi/agentbridge/internal/auth"
	"github.com/berkayahi/agentbridge/internal/buildinfo"
	"github.com/berkayahi/agentbridge/internal/config"
	"github.com/berkayahi/agentbridge/internal/controlsocket"
	"github.com/berkayahi/agentbridge/internal/events"
	bridgegit "github.com/berkayahi/agentbridge/internal/git"
	"github.com/berkayahi/agentbridge/internal/process"
	"github.com/berkayahi/agentbridge/internal/provider"
	"github.com/berkayahi/agentbridge/internal/provider/claude"
	"github.com/berkayahi/agentbridge/internal/provider/codex"
	"github.com/berkayahi/agentbridge/internal/security"
	"github.com/berkayahi/agentbridge/internal/store"
	"github.com/berkayahi/agentbridge/internal/store/sqlite"
	"github.com/berkayahi/agentbridge/internal/task"
	"github.com/berkayahi/agentbridge/internal/telegram"
	"github.com/berkayahi/agentbridge/internal/verify"
	"github.com/berkayahi/agentbridge/internal/web"
	"github.com/gofiber/fiber/v3"
)

const maxAttachmentBytes = 20 << 20

type composedDaemon struct {
	application *bridgeapp.App
	telegram    *telegram.Client
	dashboard   *web.Server
	control     *controlsocket.Server
	auth        *auth.Service
	closers     []io.Closer
	providers   []task.Provider
	listen      string

	monitorMu      sync.Mutex
	monitorCancel  context.CancelFunc
	monitorDone    chan error
	dependencyOnce sync.Once
	dependencyErr  error
}

func (d *composedDaemon) Start(context.Context) error { return nil }
func (d *composedDaemon) Run(ctx context.Context) error {
	monitorCtx, cancel := context.WithCancel(ctx)
	monitorDone := make(chan error, 1)
	d.monitorMu.Lock()
	d.monitorCancel, d.monitorDone = cancel, monitorDone
	d.monitorMu.Unlock()
	go func() { monitorDone <- d.auth.Monitor(monitorCtx, 5*time.Minute, d.providers...) }()
	return d.application.Run(ctx, d.telegram, fiberRuntime{app: d.dashboard.App()})
}
func (d *composedDaemon) Shutdown(ctx context.Context) error {
	return errors.Join(d.application.Shutdown(ctx), d.closeDependencies(ctx))
}

func (d *composedDaemon) closeDependencies(ctx context.Context) error {
	d.dependencyOnce.Do(func() {
		d.monitorMu.Lock()
		cancel, done := d.monitorCancel, d.monitorDone
		d.monitorMu.Unlock()
		if cancel != nil {
			cancel()
		}
		if done != nil {
			select {
			case monitorErr := <-done:
				if !errors.Is(monitorErr, context.Canceled) {
					d.dependencyErr = errors.Join(d.dependencyErr, monitorErr)
				}
			case <-ctx.Done():
				d.dependencyErr = errors.Join(d.dependencyErr, ctx.Err())
			}
		}
		if d.auth != nil {
			d.dependencyErr = errors.Join(d.dependencyErr, d.auth.Close())
		}
		if d.control != nil {
			d.control.Close()
		}
		for _, closer := range d.closers {
			d.dependencyErr = errors.Join(d.dependencyErr, closer.Close())
		}
	})
	return d.dependencyErr
}

type fiberRuntime struct{ app *fiber.App }

func (r fiberRuntime) Listen(address string) error { return r.app.Listen(address) }
func (r fiberRuntime) ShutdownWithContext(ctx context.Context) error {
	return r.app.ShutdownWithContext(ctx)
}

func buildDaemon(ctx context.Context, cfg config.Config, paths runtimePaths, credential config.Credential, environment []string) (daemonRuntime, error) {
	data, err := sqlite.Open(ctx, paths.database)
	if err != nil {
		return nil, err
	}
	fail := func(cause error, closers ...io.Closer) (daemonRuntime, error) {
		for _, closer := range closers {
			_ = closer.Close()
		}
		_ = data.Close()
		return nil, cause
	}
	client, err := telegram.NewClient(credential.Value(), telegram.ClientOptions{})
	if err != nil {
		return fail(err)
	}
	live := events.NewBus(256)
	csrfSecret, callbackSecret, err := randomSecrets()
	if err != nil {
		return fail(err)
	}
	callbackSigner := telegram.NewCallbackSigner(callbackSecret, nil)
	redactor := security.NewRedactor(security.Config{Secrets: []string{credential.Value()}})
	logger := slog.New(security.NewLogHandler(slog.Default().Handler(), redactor))
	allowedUser := strconv.FormatInt(cfg.Telegram.AllowedUserIDs[0], 10)
	approvalBroker, err := approval.New(approval.Config{
		Store: data, Messenger: client, Signer: callbackSigner,
		Redactor:      redactor,
		AuthorizeUser: func(value string) bool { return value == allowedUser },
	})
	if err != nil {
		return fail(err)
	}
	claudeUsage := claude.NewUsageCache()
	control := controlsocket.NewServer(paths.controlSocket, controlHandler{store: data, messenger: client, claudeUsage: claudeUsage, approvals: approvalBroker, redactor: redactor})
	if err := control.Start(); err != nil {
		return fail(err)
	}

	authCommands := auth.ExecCommandRunner{Executables: configuredExecutables(cfg), Environment: environment}
	providers, providerClosers, err := composeProviders(ctx, cfg, paths, environment, data, control, claudeUsage, redactor, authCommands)
	if err != nil {
		control.Close()
		return fail(err, providerClosers...)
	}
	profiles := composeProfiles(cfg, paths)
	workspace := &workspaceAdapter{
		profiles: profiles, manager: bridgegit.WorkspaceManager{Git: bridgegit.Runner{}, Port: data},
		processes: procTaskInspector{root: "/proc", maxEntries: 4096},
	}
	delivery := &deliveryAdapter{profiles: profiles, config: cfg.Repositories, git: bridgegit.Runner{}}
	attachments, err := attachment.NewService(paths.attachments, maxAttachmentBytes, 2*time.Minute, data, client, attachment.NewStoreTaskLocator(data), nil, nil)
	if err != nil {
		control.Close()
		return fail(err, providerClosers...)
	}
	content, err := attachment.NewContentReader(paths.attachments, maxAttachmentBytes)
	if err != nil {
		control.Close()
		return fail(err, providerClosers...)
	}

	models, deployments := make(map[task.Provider]string), make(map[string]string)
	for name, value := range cfg.Providers {
		models[task.Provider(name)] = value.Model
	}
	for name, value := range cfg.Repositories {
		deployments[name] = value.DeploymentURL
	}
	var authService *auth.Service
	daemon := &composedDaemon{
		telegram: client, control: control, closers: providerClosers,
	}
	application, err := bridgeapp.New(bridgeapp.Config{
		DefaultRepository: cfg.DefaultRepository, Listen: cfg.Server.Listen, QueueSize: 16,
		Models: models, DeploymentURLs: deployments,
	}, bridgeapp.Dependencies{
		Store: data, Messenger: client, Providers: providers, Workspace: workspace, Delivery: delivery,
		Authorizer: telegram.NewAuthorizer(cfg.Telegram.AllowedUserIDs, cfg.Telegram.PairedChatID, 512, 10*time.Minute, nil),
		Signer:     callbackSigner, Approvals: approvalBroker, Attachments: attachments,
		AuthFailure: func(failureCtx context.Context, providerName task.Provider, cause error) {
			if authService != nil {
				_, _ = authService.HandleProviderError(failureCtx, providerName, cause)
			}
		},
		BeforeStoreClose: daemon.closeDependencies,
		Logger:           logger, Redactor: redactor, Files: os.DirFS("/"), Live: live,
	})
	if err != nil {
		control.Close()
		return fail(err, providerClosers...)
	}
	daemon.application = application

	recoveryResumer := &appRecoveryResumer{application: application}
	authService, err = auth.NewService(auth.Options{
		Commands: authCommands,
		Tasks:    data, Incidents: auth.NewDurableIncidentStore(data),
		Notifier:   authNotifier{messenger: client, chatID: cfg.Telegram.PairedChatID},
		Resumer:    recoveryResumer,
		Suspender:  application,
		PTY:        auth.ExecPTY{Executables: configuredExecutables(cfg), Environment: environment},
		Authorizer: identityAuthorizer{allowed: identitySet(cfg.Server.AllowedTailscaleIdentities)},
		Logger:     logger,
	})
	if err != nil {
		control.Close()
		return fail(err, providerClosers...)
	}
	daemon.auth = authService
	dashboard, err := web.New(web.Config{
		AllowedIdentities: cfg.Server.AllowedTailscaleIdentities, ServeMode: true, CSRFSecret: csrfSecret,
	}, web.Dependencies{
		Store: data, Health: healthAdapter{store: data, providers: providers}, Usage: usageAdapter{providers: providers},
		Recovery: recoveryAdapter{service: authService}, Live: live, Content: content,
	})
	if err != nil {
		_ = authService.Close()
		control.Close()
		return fail(err, providerClosers...)
	}
	providerNames := make([]task.Provider, 0, len(providers))
	for name := range providers {
		providerNames = append(providerNames, name)
	}
	daemon.dashboard, daemon.providers, daemon.listen = dashboard, providerNames, cfg.Server.Listen
	return daemon, nil
}

func randomSecrets() ([]byte, []byte, error) {
	csrf, callback := make([]byte, 32), make([]byte, 32)
	if _, err := rand.Read(csrf); err != nil {
		return nil, nil, err
	}
	if _, err := rand.Read(callback); err != nil {
		return nil, nil, err
	}
	return csrf, callback, nil
}

func configuredExecutables(cfg config.Config) map[string]string {
	values := make(map[string]string, len(cfg.Providers))
	for name, providerConfig := range cfg.Providers {
		values[name] = providerConfig.Executable
	}
	return values
}

func claudeSubscriptionAuthChecker(commands auth.CommandRunner, now func() time.Time) claude.AuthChecker {
	if now == nil {
		now = time.Now
	}
	return func(ctx context.Context) (provider.AuthStatus, error) {
		if commands == nil {
			return provider.AuthStatus{}, errors.New("Claude subscription auth checker is not configured")
		}
		checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		output, commandErr := commands.Run(checkCtx, "claude", "auth", "status", "--json")
		var response struct {
			LoggedIn *bool `json:"loggedIn"`
		}
		if err := json.Unmarshal(output, &response); err == nil && response.LoggedIn != nil {
			return provider.AuthStatus{Authenticated: *response.LoggedIn, CheckedAt: now().UTC()}, nil
		}
		if commandErr != nil {
			return provider.AuthStatus{}, fmt.Errorf("check Claude subscription authentication: %w", commandErr)
		}
		return provider.AuthStatus{}, errors.New("check Claude subscription authentication: invalid status response")
	}
}

func composeProviders(ctx context.Context, cfg config.Config, paths runtimePaths, environment []string, data *sqlite.Store, control *controlsocket.Server, claudeUsage *claude.UsageCache, redactor *security.Redactor, authCommands auth.CommandRunner) (map[task.Provider]provider.Provider, []io.Closer, error) {
	providers := make(map[task.Provider]provider.Provider)
	var closers []io.Closer
	sink := providerSessionSink{store: data}
	if value, ok := cfg.Providers[string(task.ProviderCodex)]; ok {
		process, err := codex.StartAppServer(ctx, value.Executable, environment)
		if err != nil {
			return nil, closers, err
		}
		closers = append(closers, process)
		providers[task.ProviderCodex] = codex.NewAdapter(process.Client, codex.AdapterConfig{
			Sessions: sink, Approvals: approvalSink{store: data, redactor: redactor},
			ApprovalUser: func(provider.ID) string { return strconv.FormatInt(cfg.Telegram.AllowedUserIDs[0], 10) },
		})
	}
	if value, ok := cfg.Providers[string(task.ProviderClaude)]; ok {
		executable, err := os.Executable()
		if err != nil {
			return nil, closers, err
		}
		if err := claude.EnsureStatuslineSettings(paths.claudeConfig, executable); err != nil {
			return nil, closers, err
		}
		mcpConfig, err := claude.WriteMCPConfig(paths.mcpConfig, executable)
		if err != nil {
			return nil, closers, err
		}
		providers[task.ProviderClaude] = claude.NewAdapter(claude.AdapterConfig{
			Process:  claude.ProcessConfig{Executable: value.Executable, MCPConfigPath: mcpConfig, ClaudeConfigDir: paths.claudeConfig, Model: value.Model, Environment: environment},
			Sessions: sink, Usage: claudeUsage, Auth: claudeSubscriptionAuthChecker(authCommands, nil),
			Scope: func(id provider.ID) (claude.TaskScope, error) {
				capability := make([]byte, 32)
				if _, err := rand.Read(capability); err != nil {
					return claude.TaskScope{}, err
				}
				control.Grant(id.String(), string(task.ProviderClaude), capability)
				return claude.TaskScope{ControlSocket: paths.controlSocket, Capability: capability, Revoke: func() { control.Revoke(id.String()) }}, nil
			},
		})
	}
	if len(providers) == 0 {
		return nil, closers, errors.New("no supported provider is configured")
	}
	return providers, closers, nil
}

type providerSessionSink struct{ store *sqlite.Store }

func (s providerSessionSink) SaveSession(ctx context.Context, value provider.Session) error {
	now := time.Now().UTC()
	return s.store.UpsertSession(ctx, task.Session{ID: value.ID.String(), TaskID: value.TaskID.String(), Provider: value.Provider, ProviderSessionID: value.ExternalID, ProviderThreadID: value.ThreadID, Status: "running", Resumable: true, CreatedAt: now, UpdatedAt: now})
}

type approvalSink struct {
	store    *sqlite.Store
	redactor *security.Redactor
}

func (s approvalSink) SaveApproval(ctx context.Context, value codex.ApprovalRequest) error {
	redactor := s.redactor
	if redactor == nil {
		redactor = security.NewRedactor(security.Config{})
	}
	payload, _ := json.Marshal(struct {
		Summary string `json:"summary"`
	}{redactor.RedactString(value.Summary)})
	expires := value.ExpiresAt
	return s.store.UpsertApproval(ctx, task.Approval{ID: value.ID.String(), TaskID: value.TaskID.String(), Kind: value.Kind, Status: task.ApprovalPending, RequestPayload: payload, RequestedAt: value.CreatedAt, ExpiresAt: &expires})
}

type controlHandler struct {
	store interface {
		Task(context.Context, string) (task.Task, error)
	}
	messenger   telegram.Messenger
	claudeUsage *claude.UsageCache
	approvals   *approval.Broker
	redactor    *security.Redactor
}

func (h controlHandler) Handle(ctx context.Context, request controlsocket.Request) (any, error) {
	value, err := h.store.Task(ctx, request.TaskID)
	if err != nil || string(value.Provider) != request.Provider {
		return nil, controlsocket.ErrUnauthorized
	}
	switch request.Tool {
	case "get_task_context":
		return map[string]any{"task_id": value.ID, "repository": value.RepoProfileID, "summary": value.Title}, nil
	case "notify_telegram":
		var input struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(request.Params, &input) != nil || strings.TrimSpace(input.Message) == "" {
			return nil, controlsocket.ErrInvalid
		}
		redactor := h.redactor
		if redactor == nil {
			redactor = security.NewRedactor(security.Config{})
		}
		message := redactor.RedactString(input.Message)
		_, err := h.messenger.Send(ctx, telegram.Message{ChatID: value.TelegramChatID, Text: message})
		return map[string]bool{"sent": err == nil}, err
	case "send_artifact":
		var input struct {
			Path string `json:"path"`
			Name string `json:"name"`
		}
		if json.Unmarshal(request.Params, &input) != nil {
			return nil, controlsocket.ErrInvalid
		}
		file, name, err := openTaskArtifact(value.WorktreePath, input.Path, input.Name)
		if err != nil {
			return nil, controlsocket.ErrInvalid
		}
		defer file.Close()
		err = h.messenger.SendDocument(ctx, telegram.Document{ChatID: value.TelegramChatID, Filename: name, Caption: "Task artifact", Data: file})
		return map[string]bool{"sent": err == nil}, err
	case "request_telegram_approval":
		var input struct {
			ProviderRequestID string `json:"provider_request_id"`
			Kind              string `json:"kind"`
			Summary           string `json:"summary"`
		}
		if json.Unmarshal(request.Params, &input) != nil || h.approvals == nil {
			return nil, controlsocket.ErrInvalid
		}
		if strings.TrimSpace(input.ProviderRequestID) == "" {
			input.ProviderRequestID = randomProviderRequestID()
		}
		result, err := h.approvals.Request(ctx, approval.Request{
			TaskID: value.ID, ChatID: value.TelegramChatID, ProviderRequestID: input.ProviderRequestID,
			Kind: input.Kind, Summary: input.Summary,
		})
		return result, err
	case "claude_statusline":
		var snapshot claude.UsageSnapshot
		if json.Unmarshal(request.Params, &snapshot) != nil || snapshot.ObservedAt.IsZero() || h.claudeUsage == nil {
			return nil, controlsocket.ErrInvalid
		}
		h.claudeUsage.Update(snapshot)
		return map[string]bool{"captured": true}, nil
	default:
		return nil, controlsocket.ErrInvalid
	}
}

func randomProviderRequestID() string {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return ""
	}
	return fmt.Sprintf("mcp-%x", value)
}

var artifactNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func openTaskArtifact(worktree, candidate, requestedName string) (*os.File, string, error) {
	if !filepath.IsAbs(worktree) || !filepath.IsAbs(candidate) {
		return nil, "", errors.New("artifact paths must be absolute")
	}
	root, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		return nil, "", err
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return nil, "", err
	}
	relative, err := filepath.Rel(root, resolved)
	if err != nil || relative == "." || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, "", errors.New("artifact escapes task worktree")
	}
	rootHandle, err := os.OpenRoot(root)
	if err != nil {
		return nil, "", err
	}
	defer rootHandle.Close()
	current := ""
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := rootHandle.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil, "", errors.New("artifact path is unsafe")
		}
	}
	file, err := rootHandle.Open(relative)
	if err != nil {
		return nil, "", err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > provider.MaxAttachmentBytes {
		file.Close()
		return nil, "", errors.New("artifact is not a bounded regular file")
	}
	name := requestedName
	if name == "" {
		name = filepath.Base(relative)
	}
	if filepath.Base(name) != name || !artifactNamePattern.MatchString(name) {
		file.Close()
		return nil, "", errors.New("artifact display name is unsafe")
	}
	return file, name, nil
}

type healthTaskStore interface {
	NonterminalTasks(context.Context) ([]task.Task, error)
}

type healthAdapter struct {
	store     healthTaskStore
	providers map[task.Provider]provider.Provider
}

func (h healthAdapter) Health(ctx context.Context) (web.Health, error) {
	values, err := h.store.NonterminalTasks(ctx)
	if err != nil {
		return web.Health{}, err
	}
	active := 0
	for _, value := range values {
		if value.State == task.Running {
			active++
		}
	}
	status := "ok"
	components := make(map[string]any, len(h.providers))
	for name, value := range h.providers {
		authStatus, authErr := value.AuthStatus(ctx)
		component := map[string]any{"authenticated": authErr == nil && authStatus.Authenticated}
		if !authStatus.CheckedAt.IsZero() {
			component["checked_at"] = authStatus.CheckedAt
		}
		if authErr != nil {
			component["status"] = "unavailable"
		}
		if authErr != nil || !authStatus.Authenticated {
			status = "degraded"
		}
		components[string(name)+"_auth"] = component
	}
	return web.Health{Status: status, Version: buildinfo.Version, QueueDepth: len(values) - active, ActiveTasks: active, Components: components}, nil
}

type usageAdapter struct {
	providers map[task.Provider]provider.Provider
}

func (u usageAdapter) Usage(ctx context.Context) ([]web.ProviderUsage, error) {
	result := make([]web.ProviderUsage, 0, len(u.providers))
	for name, value := range u.providers {
		usage, err := value.Usage(ctx)
		if err != nil {
			continue
		}
		for _, window := range usage.Windows {
			result = append(result, web.ProviderUsage{Provider: string(name) + ":" + window.Name, UsedPercent: window.UsedPercent, ResetsAt: window.ResetsAt})
		}
	}
	return result, nil
}

type identityAuthorizer struct{ allowed map[string]struct{} }

func (a identityAuthorizer) AuthorizeRecovery(_ context.Context, principal string) error {
	if _, ok := a.allowed[principal]; !ok {
		return auth.ErrForbidden
	}
	return nil
}
func identitySet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

type authNotifier struct {
	messenger telegram.Messenger
	chatID    int64
}

func (n authNotifier) AuthIncident(ctx context.Context, value auth.IncidentSummary) error {
	_, err := n.messenger.Send(ctx, telegram.Message{ChatID: n.chatID, Text: fmt.Sprintf("%s subscription authentication requires recovery (%d affected tasks)", value.Provider, value.Affected)})
	return err
}

type appRecoveryResumer struct{ application *bridgeapp.App }

func (r *appRecoveryResumer) ValidateResume(ctx context.Context, value task.Task) error {
	return r.application.ValidateResume(ctx, value)
}
func (r *appRecoveryResumer) ResumeTask(ctx context.Context, value task.Task) error {
	return r.application.ResumeTask(ctx, value)
}

type recoveryAdapter struct{ service *auth.Service }

func (r recoveryAdapter) Start(ctx context.Context, providerName, identity string) (web.RecoveryView, error) {
	id, err := r.service.StartRecovery(ctx, identity, task.Provider(providerName))
	if err != nil {
		return web.RecoveryView{}, err
	}
	return r.view(ctx, providerName, id, identity)
}
func (r recoveryAdapter) Inspect(ctx context.Context, providerName, id, identity string) (web.RecoveryView, error) {
	return r.view(ctx, providerName, id, identity)
}
func (r recoveryAdapter) Submit(ctx context.Context, providerName, id, identity, code string) (web.RecoveryView, error) {
	if err := r.service.SubmitCode(ctx, identity, id, code); err != nil {
		return web.RecoveryView{}, err
	}
	return r.view(ctx, providerName, id, identity)
}
func (r recoveryAdapter) Cancel(ctx context.Context, _ string, id, identity string) error {
	return r.service.CancelRecovery(ctx, identity, id)
}
func (r recoveryAdapter) view(ctx context.Context, providerName, id, identity string) (web.RecoveryView, error) {
	value, err := r.service.Recovery(ctx, identity, id)
	if err != nil {
		return web.RecoveryView{}, err
	}
	return web.RecoveryView{ID: value.ID, Provider: providerName, State: string(value.Status), Prompt: value.Transcript, ExpiresAt: value.StartedAt.Add(10 * time.Minute)}, nil
}

func composeProfiles(cfg config.Config, paths runtimePaths) map[string]bridgegit.RepositoryProfile {
	result := make(map[string]bridgegit.RepositoryProfile, len(cfg.Repositories))
	for name, value := range cfg.Repositories {
		result[name] = bridgegit.RepositoryProfile{ControlCheckout: value.CheckoutPath, Remote: value.Remote, BaseRef: value.BaseRef, WorktreeRoot: filepath.Join(paths.worktrees, name)}
	}
	return result
}

type workspaceAdapter struct {
	profiles  map[string]bridgegit.RepositoryProfile
	manager   bridgegit.WorkspaceManager
	processes taskProcessInspector
}

func (w *workspaceAdapter) Prepare(ctx context.Context, profileID, taskID string) (bridgeapp.Workspace, error) {
	profile, ok := w.profiles[profileID]
	if !ok {
		return bridgeapp.Workspace{}, bridgegit.ErrInvalidProfile
	}
	value, err := w.manager.Prepare(ctx, profile, taskID)
	return bridgeapp.Workspace{BaseSHA: value.BaseSHA, Path: value.Path}, err
}
func (w *workspaceAdapter) Inspect(ctx context.Context, value task.Task) (bridgeapp.WorkspaceInspection, error) {
	profile, ok := w.profiles[value.RepoProfileID]
	if !ok {
		return bridgeapp.WorkspaceInspection{}, bridgegit.ErrInvalidProfile
	}
	rel, err := filepath.Rel(profile.WorktreeRoot, value.WorktreePath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return bridgeapp.WorkspaceInspection{}, bridgegit.ErrPathCollision
	}
	info, err := os.Stat(value.WorktreePath)
	if errors.Is(err, os.ErrNotExist) {
		return bridgeapp.WorkspaceInspection{}, nil
	}
	if err != nil {
		return bridgeapp.WorkspaceInspection{}, err
	}
	resolved, err := w.manager.Git.Run(ctx, value.WorktreePath, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return bridgeapp.WorkspaceInspection{}, err
	}
	processRunning := true
	if w.processes != nil {
		processRunning, err = w.processes.Running(ctx, value.ID, value.Provider, value.WorktreePath)
		if err != nil {
			return bridgeapp.WorkspaceInspection{}, err
		}
	}
	return bridgeapp.WorkspaceInspection{Exists: info.IsDir(), BaseMatches: strings.TrimSpace(resolved.Stdout) == value.BaseSHA, ProcessRunning: processRunning}, nil
}

type deliveryAdapter struct {
	profiles map[string]bridgegit.RepositoryProfile
	config   map[string]config.RepositoryProfile
	git      bridgegit.Runner
}

func (d *deliveryAdapter) Verify(ctx context.Context, value task.Task, workspace bridgeapp.Workspace) error {
	profile, ok := d.config[value.RepoProfileID]
	if !ok {
		return bridgegit.ErrInvalidProfile
	}
	commands := make([]verify.Command, 0, len(profile.Verification))
	for _, command := range profile.Verification {
		commands = append(commands, verify.Command{Argv: command.Argv, Dir: command.Dir})
	}
	delivery, request := d.deliveryRequest(value, workspace, commands)
	return delivery.Verify(ctx, request)
}
func (d *deliveryAdapter) Commit(ctx context.Context, value task.Task, workspace bridgeapp.Workspace) (string, error) {
	profile, ok := d.config[value.RepoProfileID]
	if !ok {
		return "", bridgegit.ErrInvalidProfile
	}
	commands := make([]verify.Command, 0, len(profile.Verification))
	for _, command := range profile.Verification {
		commands = append(commands, verify.Command{Argv: command.Argv, Dir: command.Dir})
	}
	delivery, request := d.deliveryRequest(value, workspace, commands)
	result, err := delivery.Commit(ctx, request)
	if err != nil {
		return "", err
	}
	return result.CommitSHA, nil
}
func (d *deliveryAdapter) Push(ctx context.Context, value task.Task, workspace bridgeapp.Workspace, commit string) (string, error) {
	profile, ok := d.config[value.RepoProfileID]
	if !ok {
		return "", bridgegit.ErrInvalidProfile
	}
	commands := make([]verify.Command, 0, len(profile.Verification))
	for _, command := range profile.Verification {
		commands = append(commands, verify.Command{Argv: command.Argv, Dir: command.Dir})
	}
	delivery, request := d.deliveryRequest(value, workspace, commands)
	result, err := delivery.Push(ctx, request, commit)
	if err != nil {
		return "", err
	}
	return result.PushRef, nil
}

func (d *deliveryAdapter) deliveryRequest(value task.Task, workspace bridgeapp.Workspace, commands []verify.Command) (bridgegit.Delivery, bridgegit.DeliveryRequest) {
	profile := d.config[value.RepoProfileID]
	gitProfile := d.profiles[value.RepoProfileID]
	return bridgegit.Delivery{Git: d.git, Verifier: verificationPort{commands: commands}}, bridgegit.DeliveryRequest{
		Profile:   bridgegit.DeliveryProfile{RepositoryProfile: gitProfile, Enabled: profile.Delivery.Enabled, AllowedRef: profile.Delivery.AllowedRef},
		Workspace: bridgegit.Workspace{BaseSHA: workspace.BaseSHA, Path: workspace.Path}, CommitMessage: fmt.Sprintf("fix(%s): apply agent task %s", value.RepoProfileID, value.ID),
	}
}

type verificationPort struct{ commands []verify.Command }

func (v verificationPort) Verify(ctx context.Context, worktree string) error {
	_, err := (verify.Runner{Supervisor: process.Supervisor{}}).Run(ctx, worktree, v.commands)
	return err
}

var _ fs.FS = os.DirFS("/")
var _ store.Store = (*sqlite.Store)(nil)
