package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/berkayahi/agentbridge/internal/approval"
	"github.com/berkayahi/agentbridge/internal/attachment"
	"github.com/berkayahi/agentbridge/internal/auth"
	"github.com/berkayahi/agentbridge/internal/buildinfo"
	"github.com/berkayahi/agentbridge/internal/config"
	bridgeapp "github.com/berkayahi/agentbridge/internal/controller/standalone"
	"github.com/berkayahi/agentbridge/internal/controlsocket"
	"github.com/berkayahi/agentbridge/internal/events"
	bridgegit "github.com/berkayahi/agentbridge/internal/git"
	"github.com/berkayahi/agentbridge/internal/kernel"
	"github.com/berkayahi/agentbridge/internal/localcontrol"
	"github.com/berkayahi/agentbridge/internal/process"
	"github.com/berkayahi/agentbridge/internal/provider"
	"github.com/berkayahi/agentbridge/internal/provider/claude"
	"github.com/berkayahi/agentbridge/internal/provider/codex"
	bridgeRuntime "github.com/berkayahi/agentbridge/internal/runtime"
	"github.com/berkayahi/agentbridge/internal/security"
	"github.com/berkayahi/agentbridge/internal/store"
	"github.com/berkayahi/agentbridge/internal/store/sqlite"
	"github.com/berkayahi/agentbridge/internal/telegram"
	"github.com/berkayahi/agentbridge/internal/verify"
	"github.com/berkayahi/agentbridge/internal/web"
	"github.com/berkayahi/agentbridge/internal/workmodel"
	"github.com/gofiber/fiber/v3"
)

const maxAttachmentBytes = 20 << 20

// The device execution projection has no Telegram presentation. The positive
// value keeps the shared approval record shape valid; headlessMessenger never
// sends it to an external service.
const headlessApprovalChatID int64 = 1

type composedDaemon struct {
	application   *bridgeapp.App
	kernel        *kernel.Kernel
	controller    *bridgeapp.Controller
	runtimes      *bridgeRuntime.Registry
	telegram      *telegram.Client
	dashboard     *web.Server
	control       *controlsocket.Server
	auth          *auth.Service
	localAPI      *localcontrol.UnixServer
	localAPIPath  string
	localHandler  http.Handler
	localDone     chan error
	localExecutor *localRuntimeExecutor
	deviceServer  *http.Server
	deviceCert    string
	deviceKey     string
	deviceDone    chan error
	serveCancel   context.CancelFunc
	closers       []io.Closer
	providers     []workmodel.Provider
	listen        string

	monitorMu      sync.Mutex
	monitorCancel  context.CancelFunc
	monitorDone    chan error
	serveMu        sync.Mutex
	dependencyOnce sync.Once
	dependencyErr  error
}

func (d *composedDaemon) Start(ctx context.Context) error {
	if d == nil {
		return nil
	}
	serveCtx, cancel := context.WithCancel(ctx)
	d.serveMu.Lock()
	d.serveCancel = cancel
	deviceServer := d.deviceServer
	d.serveMu.Unlock()
	if d.localExecutor != nil {
		d.localExecutor.SetContext(serveCtx)
	}
	if deviceServer == nil && d.localHandler != nil {
		server, err := localcontrol.ListenUnix(d.localAPIPath, d.localHandler)
		if err != nil {
			cancel()
			return err
		}
		d.serveMu.Lock()
		d.localAPI = server
		d.localDone = make(chan error, 1)
		localDone := d.localDone
		d.serveMu.Unlock()
		go func() { localDone <- server.Serve() }()
	}
	if deviceServer == nil && d.localHandler == nil {
		cancel()
		return errors.New("daemon has no local or device transport")
	}
	if deviceServer != nil {
		listener, err := net.Listen("tcp", deviceServer.Addr)
		if err != nil {
			cancel()
			_ = d.closeLocalAPI(context.Background())
			return fmt.Errorf("listen device-agent WSS: %w", err)
		}
		certificate, err := tls.LoadX509KeyPair(d.deviceCert, d.deviceKey)
		if err != nil {
			cancel()
			_ = listener.Close()
			_ = d.closeLocalAPI(context.Background())
			return fmt.Errorf("load device-agent TLS certificate: %w", err)
		}
		listener = tls.NewListener(listener, &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{certificate},
		})
		d.serveMu.Lock()
		d.deviceDone = make(chan error, 1)
		deviceDone := d.deviceDone
		d.serveMu.Unlock()
		go func() { deviceDone <- deviceServer.Serve(listener) }()
	}
	go func() {
		<-serveCtx.Done()
		_ = d.closeLocalAPI(context.Background())
		_ = d.closeDeviceServer(context.Background())
	}()
	return nil
}
func (d *composedDaemon) Run(ctx context.Context) error {
	// A device-agent process is an execution endpoint, not a second standalone
	// controller. Its shadow task rows are evidence/session state for the
	// signed WSS handler; starting App would reconcile those rows and launch a
	// duplicate provider worker (and would expose Telegram/Desktop authority on
	// the Pi). Keep the process headless and wait only on the device listener.
	d.serveMu.Lock()
	deviceServer, deviceDone := d.deviceServer, d.deviceDone
	d.serveMu.Unlock()
	if deviceServer != nil {
		if deviceDone == nil {
			return errors.New("device-agent WSS server has not started")
		}
		select {
		case err := <-deviceDone:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, http.ErrServerClosed) {
				return errors.New("device-agent WSS server stopped unexpectedly")
			}
			if err == nil {
				return errors.New("device-agent WSS server stopped unexpectedly")
			}
			return fmt.Errorf("device-agent WSS server: %w", err)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if d.auth == nil || d.application == nil || d.telegram == nil || d.dashboard == nil {
		return errors.New("standalone controller is not configured")
	}
	monitorCtx, cancel := context.WithCancel(ctx)
	monitorDone := make(chan error, 1)
	d.monitorMu.Lock()
	d.monitorCancel, d.monitorDone = cancel, monitorDone
	d.monitorMu.Unlock()
	go func() { monitorDone <- d.auth.Monitor(monitorCtx, 5*time.Minute, d.providers...) }()
	return d.application.Run(ctx, d.telegram, fiberRuntime{app: d.dashboard.App()})
}
func (d *composedDaemon) Shutdown(ctx context.Context) error {
	if d == nil {
		return nil
	}
	d.serveMu.Lock()
	cancel := d.serveCancel
	d.serveCancel = nil
	d.serveMu.Unlock()
	if cancel != nil {
		cancel()
	}
	// Stop authenticated local/device transports before App closes the shared
	// SQLite store; otherwise a request racing shutdown can write after the
	// store has already been closed.
	transportErr := errors.Join(d.closeLocalAPI(ctx), d.closeDeviceServer(ctx))
	var applicationErr error
	if d.application != nil {
		applicationErr = d.application.Shutdown(ctx)
	}
	return errors.Join(transportErr, applicationErr, d.closeDependencies(ctx))
}

func (d *composedDaemon) closeLocalAPI(ctx context.Context) error {
	if d == nil {
		return nil
	}
	d.serveMu.Lock()
	server, done := d.localAPI, d.localDone
	d.localAPI, d.localDone = nil, nil
	d.serveMu.Unlock()
	if server == nil {
		return nil
	}
	err := server.Close(ctx)
	if done != nil {
		select {
		case serveErr := <-done:
			if err == nil {
				err = serveErr
			}
		case <-ctx.Done():
			if err == nil {
				err = ctx.Err()
			}
		}
	}
	return err
}

func (d *composedDaemon) closeDeviceServer(ctx context.Context) error {
	if d == nil {
		return nil
	}
	d.serveMu.Lock()
	server, done := d.deviceServer, d.deviceDone
	d.deviceServer, d.deviceDone = nil, nil
	d.serveMu.Unlock()
	if server == nil {
		return nil
	}
	err := server.Shutdown(ctx)
	if done != nil {
		select {
		case serveErr := <-done:
			if err == nil && !errors.Is(serveErr, http.ErrServerClosed) {
				err = serveErr
			}
		case <-ctx.Done():
			if err == nil {
				err = ctx.Err()
			}
		}
	}
	return err
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

func openStandaloneStore(ctx context.Context, path string) (*sqlite.RuntimeStore, error) {
	return sqlite.OpenV2RuntimeWithRuntimeLock(ctx, path)
}

func loadLocalAPISecret(path string) ([]byte, error) {
	for attempt := 0; attempt < 2; attempt++ {
		info, err := os.Lstat(path)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return nil, errors.New("local API secret is not a regular file")
			}
			if info.Mode().Perm() != 0o600 {
				if err := os.Chmod(path, 0o600); err != nil {
					return nil, fmt.Errorf("secure local API secret: %w", err)
				}
			}
			secret, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("read local API secret: %w", err)
			}
			if len(secret) < 32 {
				return nil, errors.New("local API secret is too short")
			}
			return secret, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("inspect local API secret: %w", err)
		}
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, fmt.Errorf("generate local API secret: %w", err)
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("create local API secret: %w", err)
		}
		if _, err := file.Write(secret); err != nil {
			_ = file.Close()
			_ = os.Remove(path)
			return nil, fmt.Errorf("write local API secret: %w", err)
		}
		if err := file.Close(); err != nil {
			return nil, fmt.Errorf("close local API secret: %w", err)
		}
		return secret, nil
	}
	return nil, errors.New("local API secret creation raced repeatedly")
}

func buildDaemon(ctx context.Context, cfg config.Config, paths runtimePaths, credential config.Credential, environment []string) (daemonRuntime, error) {
	if cfg.Mode == "managed" {
		return buildManagedDaemon(ctx, cfg, paths)
	}
	if cfg.DeviceAgent.Enabled {
		return buildHeadlessDeviceDaemon(ctx, cfg, paths, environment)
	}
	data, err := openStandaloneStore(ctx, paths.database)
	if err != nil {
		return nil, err
	}
	controllerIdentity, err := loadOrCreateDeviceKey(paths.controllerKey)
	if err != nil {
		_ = data.Close()
		return nil, fmt.Errorf("load local controller identity: %w", err)
	}
	fail := func(cause error, closers ...io.Closer) (daemonRuntime, error) {
		for _, closer := range closers {
			_ = closer.Close()
		}
		_ = data.Close()
		return nil, cause
	}
	for id, profile := range cfg.Repositories {
		if err := data.EnsureRepositoryBinding(ctx, id, profile.Remote); err != nil {
			return fail(err)
		}
	}
	client, err := telegram.NewClient(credential.Value(), telegram.ClientOptions{ForceIPv4: true})
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
	providers, runtimes, providerClosers, err := composeProviders(ctx, cfg, paths, environment, data, control, claudeUsage, redactor, authCommands)
	if err != nil {
		control.Close()
		return fail(err, providerClosers...)
	}
	bridgeKernel, err := kernel.New(kernel.Config{Work: data, Owner: "standalone-runtime", IntentTTL: 24 * time.Hour})
	if err != nil {
		control.Close()
		return fail(err, providerClosers...)
	}
	bridgeController := bridgeapp.NewKernelController(bridgeKernel)
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

	models, deployments := make(map[workmodel.Provider]string), make(map[string]string)
	for name, value := range cfg.Providers {
		models[workmodel.Provider(name)] = value.Model
	}
	for name, value := range cfg.Repositories {
		deployments[name] = value.DeploymentURL
	}
	var authService *auth.Service
	daemon := &composedDaemon{
		kernel: bridgeKernel, controller: bridgeController, runtimes: runtimes, telegram: client, control: control, closers: providerClosers,
	}
	localSecret, err := loadLocalAPISecret(paths.localAPISecret)
	if err != nil {
		control.Close()
		return fail(err, providerClosers...)
	}
	localExecutor := newLocalRuntimeExecutor(data, runtimes, workspace, models, configuredApprovalUser(cfg))
	localExecutor.approvals = approvalBroker
	localOperations := localRepositoryOperations{store: data, workspace: workspace, delivery: delivery}
	localService, err := localcontrol.New(localcontrol.Config{
		Store: data, Identity: controllerIdentity, Runtimes: runtimes, Controller: bridgeController, Executor: localExecutor,
		Verifier: localVerifier{operations: localOperations}, Committer: localCommitter{operations: localOperations},
		RemoteDeviceFactory: newLocalRemoteDeviceFactory(data, controllerIdentity),
	})
	if err != nil {
		control.Close()
		return fail(err, providerClosers...)
	}
	deviceServer, err := composeDeviceAgent(cfg.DeviceAgent, data, localExecutor, localVerifier{operations: localOperations}, localCommitter{operations: localOperations})
	if err != nil {
		control.Close()
		return fail(err, providerClosers...)
	}
	localHandler, err := localcontrol.NewHTTPHandler(localService, localSecret)
	if err != nil {
		control.Close()
		return fail(err, providerClosers...)
	}
	daemon.localAPIPath, daemon.localHandler, daemon.localExecutor = paths.localAPI, localHandler, localExecutor
	daemon.deviceServer, daemon.deviceCert, daemon.deviceKey = deviceServer, cfg.DeviceAgent.TLSCertPath, cfg.DeviceAgent.TLSKeyPath
	application, err := bridgeapp.New(bridgeapp.Config{
		DefaultRepository: cfg.DefaultRepository, Listen: cfg.Server.Listen, QueueSize: 16,
		Models: models, DeploymentURLs: deployments,
	}, bridgeapp.Dependencies{
		Store: data, Messenger: client, Providers: providers, Workspace: workspace, Delivery: delivery,
		Authorizer: telegram.NewAuthorizer(cfg.Telegram.AllowedUserIDs, cfg.Telegram.PairedChatID, 512, 10*time.Minute, nil),
		Signer:     callbackSigner, Approvals: approvalBroker, Attachments: attachments,
		AuthFailure: func(failureCtx context.Context, providerName workmodel.Provider, cause error) {
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
	providerNames := make([]workmodel.Provider, 0, len(providers))
	for name := range providers {
		providerNames = append(providerNames, name)
	}
	daemon.dashboard, daemon.providers, daemon.listen = dashboard, providerNames, cfg.Server.Listen
	return daemon, nil
}

// buildHeadlessDeviceDaemon composes only the provider/runtime, owner-only
// control socket, local SQLite projection, and signed device WSS endpoint.
// Telegram, dashboard, standalone reconciliation, and the owner local API
// are deliberately absent from this process.
func buildHeadlessDeviceDaemon(ctx context.Context, cfg config.Config, paths runtimePaths, environment []string) (daemonRuntime, error) {
	data, err := openStandaloneStore(ctx, paths.database)
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
	for id, profile := range cfg.Repositories {
		if err := data.EnsureRepositoryBinding(ctx, id, profile.Remote); err != nil {
			return fail(err)
		}
	}
	_, callbackSecret, err := randomSecrets()
	if err != nil {
		return fail(err)
	}
	redactor := security.NewRedactor(security.Config{})
	messenger := headlessMessenger{}
	approvalBroker, err := approval.New(approval.Config{
		Store: data, Messenger: messenger, Signer: telegram.NewCallbackSigner(callbackSecret, nil),
		Redactor: redactor, AllowNonNumericUserIDs: true, NoExternalPresentation: true,
		AuthorizeUser: func(value string) bool { return strings.TrimSpace(value) != "" },
	})
	if err != nil {
		return fail(err)
	}
	claudeUsage := claude.NewUsageCache()
	control := controlsocket.NewServer(paths.controlSocket, controlHandler{
		store: data, messenger: messenger, claudeUsage: claudeUsage, approvals: approvalBroker, redactor: redactor,
	})
	if err := control.Start(); err != nil {
		return fail(err)
	}
	closeOnError := func(cause error, closers ...io.Closer) (daemonRuntime, error) {
		control.Close()
		return fail(cause, closers...)
	}
	authCommands := auth.ExecCommandRunner{Executables: configuredExecutables(cfg), Environment: environment}
	_, runtimes, providerClosers, err := composeProviders(ctx, cfg, paths, environment, data, control, claudeUsage, redactor, authCommands)
	if err != nil {
		return closeOnError(err, providerClosers...)
	}
	profiles := composeProfiles(cfg, paths)
	workspace := &workspaceAdapter{
		profiles: profiles, manager: bridgegit.WorkspaceManager{Git: bridgegit.Runner{}, Port: data},
		processes: procTaskInspector{root: "/proc", maxEntries: 4096},
	}
	delivery := &deliveryAdapter{profiles: profiles, config: cfg.Repositories, git: bridgegit.Runner{}}
	models := make(map[workmodel.Provider]string, len(cfg.Providers))
	for name, value := range cfg.Providers {
		models[workmodel.Provider(name)] = value.Model
	}
	localExecutor := newLocalRuntimeExecutor(data, runtimes, workspace, models, configuredApprovalUser(cfg))
	localExecutor.approvals = approvalBroker
	localOperations := localRepositoryOperations{store: data, workspace: workspace, delivery: delivery}
	deviceServer, err := composeDeviceAgent(cfg.DeviceAgent, data, localExecutor, localVerifier{operations: localOperations}, localCommitter{operations: localOperations})
	if err != nil {
		return closeOnError(err, providerClosers...)
	}
	daemonClosers := append(append([]io.Closer(nil), providerClosers...), data)
	return &composedDaemon{
		runtimes: runtimes, control: control, closers: daemonClosers, localExecutor: localExecutor,
		deviceServer: deviceServer, deviceCert: cfg.DeviceAgent.TLSCertPath, deviceKey: cfg.DeviceAgent.TLSKeyPath,
	}, nil
}

// headlessMessenger is a local sink for the shared approval/control contract.
// Controller decisions arrive over the authenticated device link; nothing is
// published to Telegram from a paired device.
type headlessMessenger struct{}

func (headlessMessenger) Send(context.Context, telegram.Message) (telegram.MessageRef, error) {
	return telegram.MessageRef{}, nil
}

func (headlessMessenger) Edit(context.Context, telegram.MessageRef, telegram.Message) error {
	return nil
}

func (headlessMessenger) AnswerCallback(context.Context, string, string) error { return nil }

func (headlessMessenger) SendDocument(context.Context, telegram.Document) error { return nil }

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

func configuredApprovalUser(cfg config.Config) string {
	if len(cfg.Telegram.AllowedUserIDs) > 0 && cfg.Telegram.AllowedUserIDs[0] > 0 {
		return strconv.FormatInt(cfg.Telegram.AllowedUserIDs[0], 10)
	}
	return localcontrol.LocalAuthorityUserID
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

func composeProviders(ctx context.Context, cfg config.Config, paths runtimePaths, environment []string, data *sqlite.RuntimeStore, control *controlsocket.Server, claudeUsage *claude.UsageCache, redactor *security.Redactor, authCommands auth.CommandRunner) (map[workmodel.Provider]provider.Provider, *bridgeRuntime.Registry, []io.Closer, error) {
	providers := make(map[workmodel.Provider]provider.Provider)
	adapters := make([]bridgeRuntime.Adapter, 0, 2)
	var closers []io.Closer
	sink := providerSessionSink{store: data}
	if value, ok := cfg.Providers[string(workmodel.CodexSubscription)]; ok {
		process, err := codex.StartAppServer(ctx, value.Executable, environment)
		if err != nil {
			return nil, nil, closers, err
		}
		closers = append(closers, process)
		adapter := codex.NewAdapter(process.Client, codex.AdapterConfig{
			Sessions: sink, Approvals: approvalSink{store: data, redactor: redactor},
			ApprovalUser: func(provider.ID) string { return configuredApprovalUser(cfg) },
		})
		providers[workmodel.CodexSubscription] = adapter
		adapters = append(adapters, codex.NewRuntimeAdapter(adapter))
	}
	if value, ok := cfg.Providers[string(workmodel.ClaudeSubscription)]; ok {
		executable, err := os.Executable()
		if err != nil {
			return nil, nil, closers, err
		}
		if err := claude.EnsureStatuslineSettings(paths.claudeConfig, executable); err != nil {
			return nil, nil, closers, err
		}
		mcpConfig, err := claude.WriteMCPConfig(paths.mcpConfig, executable)
		if err != nil {
			return nil, nil, closers, err
		}
		adapter := claude.NewAdapter(claude.AdapterConfig{
			Process:  claude.ProcessConfig{Executable: value.Executable, MCPConfigPath: mcpConfig, ClaudeConfigDir: paths.claudeConfig, Model: value.Model, Environment: environment},
			Sessions: sink, Usage: claudeUsage, Auth: claudeSubscriptionAuthChecker(authCommands, nil),
			Scope: func(id provider.ID) (claude.TaskScope, error) {
				capability := make([]byte, 32)
				if _, err := rand.Read(capability); err != nil {
					return claude.TaskScope{}, err
				}
				control.Grant(id.String(), string(workmodel.ClaudeSubscription), capability)
				return claude.TaskScope{ControlSocket: paths.controlSocket, Capability: capability, Revoke: func() { control.Revoke(id.String()) }}, nil
			},
		})
		providers[workmodel.ClaudeSubscription] = adapter
		adapters = append(adapters, claude.NewRuntimeAdapter(adapter))
	}
	if len(providers) == 0 {
		return nil, nil, closers, errors.New("no supported provider is configured")
	}
	runtimes, err := bridgeRuntime.NewRegistry(adapters...)
	if err != nil {
		return nil, nil, closers, fmt.Errorf("register runtime adapters: %w", err)
	}
	return providers, runtimes, closers, nil
}

type providerSessionSink struct {
	store interface {
		UpsertSession(context.Context, workmodel.Session) error
	}
}

func (s providerSessionSink) SaveSession(ctx context.Context, value provider.Session) error {
	now := time.Now().UTC()
	return s.store.UpsertSession(ctx, workmodel.Session{ID: value.ID.String(), TaskID: value.TaskID.String(), Provider: value.Provider, ProviderSessionID: value.ExternalID, ProviderThreadID: value.ThreadID, Status: "running", Resumable: true, CreatedAt: now, UpdatedAt: now})
}

type approvalSink struct {
	store interface {
		UpsertApproval(context.Context, workmodel.Approval) error
	}
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
	return s.store.UpsertApproval(ctx, workmodel.Approval{ID: value.ID.String(), TaskID: value.TaskID.String(), Kind: value.Kind, Status: workmodel.ApprovalPending, RequestPayload: payload, RequestedAt: value.CreatedAt, ExpiresAt: &expires})
}

type controlHandler struct {
	store interface {
		Task(context.Context, string) (workmodel.Task, error)
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
		chatID := value.TelegramChatID
		if chatID == 0 {
			// Headless device projections have no Telegram presentation row. The
			// broker still requires a positive scoped chat slot, which the
			// headless messenger consumes locally and never transmits.
			chatID = headlessApprovalChatID
		}
		result, err := h.approvals.Request(ctx, approval.Request{
			TaskID: value.ID, ChatID: chatID, ProviderRequestID: input.ProviderRequestID,
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
	NonterminalTasks(context.Context) ([]workmodel.Task, error)
}

type healthAdapter struct {
	store     healthTaskStore
	providers map[workmodel.Provider]provider.Provider
}

func (h healthAdapter) Health(ctx context.Context) (web.Health, error) {
	values, err := h.store.NonterminalTasks(ctx)
	if err != nil {
		return web.Health{}, err
	}
	active := 0
	for _, value := range values {
		if value.State == workmodel.Running {
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
	providers map[workmodel.Provider]provider.Provider
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

func (r *appRecoveryResumer) ValidateResume(ctx context.Context, value workmodel.Task) error {
	return r.application.ValidateResume(ctx, value)
}
func (r *appRecoveryResumer) ResumeTask(ctx context.Context, value workmodel.Task) error {
	return r.application.ResumeTask(ctx, value)
}

type recoveryAdapter struct{ service *auth.Service }

func (r recoveryAdapter) Start(ctx context.Context, providerName, identity string) (web.RecoveryView, error) {
	id, err := r.service.StartRecovery(ctx, identity, workmodel.Provider(providerName))
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
func (w *workspaceAdapter) Inspect(ctx context.Context, value workmodel.Task) (bridgeapp.WorkspaceInspection, error) {
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

func (d *deliveryAdapter) Changed(ctx context.Context, value workmodel.Task, workspace bridgeapp.Workspace) (bool, error) {
	if _, ok := d.config[value.RepoProfileID]; !ok {
		return false, bridgegit.ErrInvalidProfile
	}
	result, err := d.git.Run(ctx, workspace.Path, "status", "--porcelain=v1", "-z")
	if err != nil {
		return false, err
	}
	return result.Stdout != "", nil
}

func (d *deliveryAdapter) Verify(ctx context.Context, value workmodel.Task, workspace bridgeapp.Workspace) error {
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
func (d *deliveryAdapter) Commit(ctx context.Context, value workmodel.Task, workspace bridgeapp.Workspace) (string, error) {
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
func (d *deliveryAdapter) Push(ctx context.Context, value workmodel.Task, workspace bridgeapp.Workspace, commit string) (string, error) {
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

func (d *deliveryAdapter) deliveryRequest(value workmodel.Task, workspace bridgeapp.Workspace, commands []verify.Command) (bridgegit.Delivery, bridgegit.DeliveryRequest) {
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
var _ store.RuntimeStore = (*sqlite.RuntimeStore)(nil)
