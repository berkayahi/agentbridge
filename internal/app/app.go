// Package app composes AgentBridge's durable task workflow.
package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/berkayahi/agentbridge/internal/approval"
	"github.com/berkayahi/agentbridge/internal/attachment"
	"github.com/berkayahi/agentbridge/internal/events"
	"github.com/berkayahi/agentbridge/internal/provider"
	"github.com/berkayahi/agentbridge/internal/scheduler"
	"github.com/berkayahi/agentbridge/internal/security"
	"github.com/berkayahi/agentbridge/internal/store"
	"github.com/berkayahi/agentbridge/internal/task"
	"github.com/berkayahi/agentbridge/internal/telegram"
)

var (
	ErrNotStarted       = errors.New("app: not started")
	ErrClosed           = errors.New("app: closed")
	ErrUnknownProvider  = errors.New("app: provider is not configured")
	ErrNoDefaultProfile = errors.New("app: default repository is not configured")
	ErrLeaseLost        = errors.New("app: durable repository lease lost")
	errAuthSuspended    = errors.New("app: provider suspended for authentication recovery")
)

type Config struct {
	DefaultRepository string
	BotUsername       string
	Listen            string
	QueueSize         int
	LeaseTTL          time.Duration
	LeaseHeartbeat    time.Duration
	NewID             func() string
	Models            map[task.Provider]string
	DeploymentURLs    map[string]string
}

type Store interface {
	store.Store
	SaveWorkspace(context.Context, string, string, string) error
	SaveTelegramMessage(context.Context, string, int64) error
	SaveProviderSession(context.Context, string, task.Session) error
	SaveDelivery(context.Context, string, string, string, string) error
	SaveFailure(context.Context, string, string) error
	Close() error
}

type Workspace struct{ BaseSHA, Path string }
type WorkspaceInspection struct {
	Exists         bool
	BaseMatches    bool
	ProcessRunning bool
}
type WorkspacePort interface {
	Prepare(context.Context, string, string) (Workspace, error)
	Inspect(context.Context, task.Task) (WorkspaceInspection, error)
}
type DeliveryPort interface {
	Changed(context.Context, task.Task, Workspace) (bool, error)
	Verify(context.Context, task.Task, Workspace) error
	Commit(context.Context, task.Task, Workspace) (string, error)
	Push(context.Context, task.Task, Workspace, string) (string, error)
}
type Authorizer interface{ Authorize(telegram.Update) error }
type AttachmentSaver interface {
	Save(context.Context, attachment.IncomingFile) (task.Attachment, error)
	SaveForTask(context.Context, string, attachment.IncomingFile) (task.Attachment, error)
}
type UpdateTransport interface {
	Run(context.Context)
	Next(context.Context) (telegram.Update, error)
}
type HTTPServer interface {
	Listen(string) error
	ShutdownWithContext(context.Context) error
}

type Dependencies struct {
	Store       Store
	Messenger   telegram.Messenger
	Providers   map[task.Provider]provider.Provider
	Workspace   WorkspacePort
	Delivery    DeliveryPort
	Authorizer  Authorizer
	Signer      *telegram.CallbackSigner
	Attachments AttachmentSaver
	Approvals   *approval.Broker
	AuthFailure func(context.Context, task.Provider, error)
	// BeforeStoreClose stops every component that can still write to Store.
	// It runs after task workers stop and before the live bus and Store close.
	BeforeStoreClose func(context.Context) error
	Clock            func() time.Time
	Logger           *slog.Logger
	Redactor         *security.Redactor
	Files            fs.FS
	Live             *events.Bus
}

type activeTask struct {
	provider provider.Provider
	session  provider.Session
	cancel   context.CancelCauseFunc
	done     <-chan struct{}
}
type queuedTask struct {
	id     string
	resume bool
	input  string
}

type App struct {
	config Config
	deps   Dependencies
	newID  func() string
	idMu   sync.Mutex

	mu           sync.Mutex
	started      bool
	closed       bool
	ctx          context.Context
	cancel       context.CancelFunc
	queue        chan queuedTask
	scheduler    *scheduler.Scheduler
	active       map[string]activeTask
	wg           sync.WaitGroup
	closeOnce    sync.Once
	shutdownDone chan struct{}
	shutdownErr  error
}

func New(config Config, deps Dependencies) (*App, error) {
	if config.DefaultRepository == "" {
		return nil, ErrNoDefaultProfile
	}
	if deps.Store == nil || deps.Messenger == nil || len(deps.Providers) == 0 || deps.Workspace == nil || deps.Delivery == nil || deps.Files == nil {
		return nil, errors.New("app: incomplete dependencies")
	}
	if deps.Clock == nil {
		deps.Clock = func() time.Time { return time.Now().UTC() }
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Redactor == nil {
		deps.Redactor = security.NewRedactor(security.Config{})
	}
	if config.NewID == nil {
		config.NewID = randomID
	}
	if config.QueueSize < 1 {
		config.QueueSize = 16
	}
	return &App{config: config, deps: deps, newID: config.NewID, queue: make(chan queuedTask, config.QueueSize), active: make(map[string]activeTask), shutdownDone: make(chan struct{})}, nil
}

func randomID() string {
	value := make([]byte, 9)
	if _, err := rand.Read(value); err == nil {
		return base64.RawURLEncoding.EncodeToString(value)
	}
	return fmt.Sprintf("%x", time.Now().UnixNano())[:16]
}

func (a *App) Start(ctx context.Context) error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return ErrClosed
	}
	if a.started {
		a.mu.Unlock()
		return nil
	}
	a.ctx, a.cancel = context.WithCancel(ctx)
	a.started = true
	a.scheduler = scheduler.New(a.deps.Store, "daemon-"+a.nextID(), a.config.LeaseTTL, a.config.LeaseHeartbeat)
	a.mu.Unlock()
	workers := a.config.QueueSize
	if workers > 4 {
		workers = 4
	}
	for range workers {
		a.wg.Add(1)
		go a.worker()
	}
	if err := a.Reconcile(a.ctx); err != nil {
		a.cancel()
		a.wg.Wait()
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = a.scheduler.Close(closeCtx)
		closeCancel()
		a.mu.Lock()
		a.started = false
		a.ctx, a.cancel, a.scheduler = nil, nil, nil
		a.mu.Unlock()
		return err
	}
	return nil
}

// Run owns every long-lived transport and returns only after their goroutines
// observe cancellation and durable shutdown completes.
func (a *App) Run(ctx context.Context, updates UpdateTransport, http HTTPServer) error {
	if updates == nil || http == nil || a.config.Listen == "" {
		return errors.New("app: incomplete runtime transports")
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := a.Start(runCtx); err != nil {
		return err
	}
	transportDone := make(chan struct{})
	go func() {
		defer close(transportDone)
		updates.Run(runCtx)
	}()
	httpDone := make(chan error, 1)
	go func() { httpDone <- http.Listen(a.config.Listen) }()
	type intakeResult struct {
		update telegram.Update
		err    error
	}
	intake := make(chan intakeResult, 1)
	go func() {
		for {
			update, err := updates.Next(runCtx)
			select {
			case intake <- intakeResult{update: update, err: err}:
			case <-runCtx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	var result error
	running := true
	for running {
		select {
		case <-runCtx.Done():
			running = false
		case err := <-httpDone:
			if err != nil {
				result = fmt.Errorf("dashboard server: %w", err)
			}
			running = false
		case incoming := <-intake:
			if incoming.err != nil {
				if runCtx.Err() == nil {
					result = fmt.Errorf("Telegram update intake: %w", incoming.err)
				}
				running = false
				continue
			}
			if _, err := a.HandleUpdate(runCtx, incoming.update); err != nil {
				a.deps.Logger.Warn("Telegram update rejected", "error_type", fmt.Sprintf("%T", err))
			}
		}
	}
	cancel()
	shutdownCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer stop()
	if err := http.ShutdownWithContext(shutdownCtx); err != nil && result == nil {
		result = err
	}
	select {
	case <-transportDone:
	case <-shutdownCtx.Done():
		if result == nil {
			result = shutdownCtx.Err()
		}
	}
	if err := a.Shutdown(shutdownCtx); err != nil && result == nil {
		result = err
	}
	return result
}

func (a *App) HandleUpdate(ctx context.Context, update telegram.Update) (string, error) {
	a.mu.Lock()
	started, closed := a.started, a.closed
	a.mu.Unlock()
	if !started {
		return "", ErrNotStarted
	}
	if closed {
		return "", ErrClosed
	}
	if a.deps.Authorizer != nil {
		if err := a.deps.Authorizer.Authorize(update); err != nil {
			return "", err
		}
	}
	if update.Message != nil && update.Message.Attachment != nil {
		input := strings.TrimSpace(update.Message.Text)
		if input == "" {
			input = strings.TrimSpace(update.Message.Caption)
		}
		if !strings.HasPrefix(input, "/") {
			if a.deps.Attachments == nil {
				return "", errors.New("app: attachment service is unavailable")
			}
			if _, err := a.deps.Attachments.Save(ctx, incomingFile(update.Message)); err != nil {
				_, _ = a.deps.Messenger.Send(ctx, telegram.Message{ChatID: update.Message.Chat.ID, Text: "Could not associate that attachment. Reply to a task status message or include task:<id> in the caption."})
				return "", err
			}
			_, err := a.deps.Messenger.Send(ctx, telegram.Message{ChatID: update.Message.Chat.ID, Text: "Attachment added to task."})
			return "", err
		}
	}
	command, err := telegram.ParseUpdate(update, a.config.BotUsername, a.deps.Signer)
	if err != nil {
		return "", err
	}
	switch command.Kind {
	case telegram.KindPrompt:
		if update.Message == nil {
			return "", errors.New("app: prompt message is missing")
		}
		return a.createTask(ctx, update.Message, command)
	case telegram.KindCancel:
		return "", a.cancelTask(ctx, command.TaskID)
	case telegram.KindUsage:
		if update.Message == nil {
			return "", errors.New("app: usage message is missing")
		}
		return "", a.sendUsage(ctx, update.Message.Chat.ID, command.Provider)
	case telegram.KindApprove, telegram.KindReject:
		return "", a.resolveApproval(ctx, update, command)
	case telegram.KindStatus, telegram.KindTasks, telegram.KindSessions, telegram.KindDiff, telegram.KindLogs, telegram.KindHealth, telegram.KindRetry, telegram.KindHelp:
		return "", a.handleDirectCommand(ctx, update, command)
	case telegram.KindChat:
		if update.Message == nil {
			return "", errors.New("app: chat message is missing")
		}
		return "", a.continueTask(ctx, command.TaskID, command.Argument, update.Message.Chat.ID)
	default:
		return "", errors.New("app: command is not implemented by daemon")
	}
}

func (a *App) createTask(ctx context.Context, message *telegram.IncomingMessage, command telegram.Command) (string, error) {
	if _, ok := a.deps.Providers[command.Provider]; !ok {
		return "", ErrUnknownProvider
	}
	at := a.deps.Clock().UTC()
	id := a.nextID()
	value := task.Task{ID: id, RepoProfileID: a.config.DefaultRepository, Title: task.Title(command.Argument, task.DefaultTitleRunes), Prompt: command.Argument, State: task.Queued, Provider: command.Provider, TelegramChatID: message.Chat.ID, CreatedAt: at, UpdatedAt: at}
	event := a.event(id, task.EventTaskCreated, task.VisibilityUser, map[string]any{"title": value.Title})
	if err := a.deps.Store.CreateTask(ctx, value, event); err != nil {
		return "", err
	}
	if err := a.publish(ctx, event); err != nil {
		a.deps.Logger.Warn("could not publish task event", "task", id)
	}
	if err := a.project(ctx, value, "queued", true); err != nil {
		a.pause(value, "initial status delivery failed; manual retry required")
		return "", err
	}
	if message.Attachment != nil {
		if a.deps.Attachments == nil {
			return "", errors.New("app: attachment service is unavailable")
		}
		if _, err := a.deps.Attachments.SaveForTask(ctx, id, incomingFile(message)); err != nil {
			a.pause(value, "requested attachment could not be persisted; manual retry required")
			_, _ = a.deps.Messenger.Send(ctx, telegram.Message{ChatID: message.Chat.ID, Text: "Could not save that attachment. Please retry the command with the image."})
			return "", err
		}
	}
	if err := a.enqueue(queuedTask{id: id}); err != nil {
		return "", err
	}
	return id, nil
}

func incomingFile(message *telegram.IncomingMessage) attachment.IncomingFile {
	return attachment.IncomingFile{
		FileID: message.Attachment.FileID, RemoteFilename: message.Attachment.Filename,
		DeclaredMediaType: message.Attachment.MediaType, Caption: message.Caption,
		ChatID: message.Chat.ID, ReplyToMessageID: message.ReplyToMessageID,
		MediaGroupID: message.MediaGroupID, ReceivedAt: message.ReceivedAt,
	}
}

func (a *App) enqueue(value queuedTask) error {
	select {
	case <-a.ctx.Done():
		return ErrClosed
	case a.queue <- value:
		return nil
	}
}

func (a *App) worker() {
	defer a.wg.Done()
	for {
		select {
		case <-a.ctx.Done():
			return
		case value := <-a.queue:
			a.execute(value)
		}
	}
}

func (a *App) execute(job queuedTask) {
	value, err := a.deps.Store.Task(a.ctx, job.id)
	if err != nil {
		return
	}
	ctx, cancel := context.WithCancelCause(a.ctx)
	defer cancel(nil)
	permit, err := a.scheduler.Acquire(ctx, scheduler.Request{TaskID: value.ID, Repository: value.RepoProfileID})
	if err != nil {
		if errors.Is(err, scheduler.ErrLeaseUnavailable) {
			a.requeueAfter(value.ID, job.resume)
			return
		}
		a.executionFailure(ctx, value, err)
		return
	}
	defer permit.Release()
	stopLeaseWatch := make(chan struct{})
	leaseWatchDone := make(chan struct{})
	go func() {
		defer close(leaseWatchDone)
		select {
		case <-permit.Done():
			cancel(ErrLeaseLost)
		case <-stopLeaseWatch:
		case <-ctx.Done():
		}
	}()
	defer func() {
		close(stopLeaseWatch)
		<-leaseWatchDone
	}()
	if job.resume {
		a.resume(ctx, value, cancel, job.input)
		return
	}
	if value.State == task.Verifying {
		a.deliver(ctx, value, Workspace{BaseSHA: value.BaseSHA, Path: value.WorktreePath})
		return
	}
	if value.State != task.Queued && value.State != task.Preparing {
		return
	}
	if value.State == task.Queued && !a.transition(ctx, &value, task.Preparing, "preparing isolated worktree") {
		return
	}
	workspace, err := a.deps.Workspace.Prepare(ctx, value.RepoProfileID, value.ID)
	if err != nil {
		a.executionFailure(ctx, value, err)
		return
	}
	if err := a.deps.Store.SaveWorkspace(ctx, value.ID, workspace.BaseSHA, workspace.Path); err != nil {
		a.executionFailure(ctx, value, err)
		return
	}
	value.BaseSHA, value.WorktreePath = workspace.BaseSHA, workspace.Path
	if !a.transition(ctx, &value, task.Running, "provider session started") {
		return
	}
	p := a.deps.Providers[value.Provider]
	taskID, err := provider.NewID(value.ID)
	if err != nil {
		a.executionFailure(ctx, value, err)
		return
	}
	input, err := a.providerInput(ctx, value)
	if err != nil {
		a.executionFailure(ctx, value, err)
		return
	}
	session, stream, err := p.Start(ctx, provider.StartRequest{TaskID: taskID, Input: input, WorkingDirectory: workspace.Path, Model: a.config.Models[value.Provider]})
	if err != nil {
		a.executionFailure(ctx, value, err)
		return
	}
	done := make(chan struct{})
	defer close(done)
	a.rememberActive(value.ID, p, session, cancel, done)
	defer func() {
		if _, ok := a.takeActive(value.ID); !ok {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = p.Interrupt(cleanupCtx, session)
		cleanupCancel()
	}()
	if err := a.persistSession(ctx, value, session); err != nil {
		a.executionFailure(ctx, value, err)
		return
	}
	a.consume(ctx, value, workspace, stream)
}

func (a *App) requeueAfter(id string, resume bool) {
	delay := a.config.LeaseHeartbeat
	if delay <= 0 {
		delay = time.Second
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	a.wg.Add(1)
	a.mu.Unlock()
	go func() {
		defer a.wg.Done()
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-a.ctx.Done():
			return
		case <-timer.C:
			_ = a.enqueue(queuedTask{id: id, resume: resume})
		}
	}()
}

func (a *App) resume(ctx context.Context, value task.Task, cancel context.CancelCauseFunc, input string) {
	p := a.deps.Providers[value.Provider]
	taskID, err := provider.NewID(value.ID)
	if err != nil {
		a.executionFailure(ctx, value, err)
		return
	}
	sessionID, err := provider.NewID(value.ProviderSessionID)
	if err != nil {
		a.pause(value, "saved provider session is invalid")
		return
	}
	saved := provider.Session{ID: sessionID, TaskID: taskID, ExternalID: value.ProviderSessionID, ThreadID: value.ProviderThreadID, Provider: value.Provider}
	if strings.TrimSpace(input) == "" {
		input = "Continue the interrupted task from the durable session."
	}
	session, stream, err := p.Resume(ctx, provider.ResumeRequest{TaskID: taskID, Session: saved, Input: provider.Input{Text: input}})
	if err != nil {
		a.pause(value, "saved provider session could not be resumed safely")
		return
	}
	done := make(chan struct{})
	defer close(done)
	a.rememberActive(value.ID, p, session, cancel, done)
	defer func() {
		if _, ok := a.takeActive(value.ID); !ok {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = p.Interrupt(cleanupCtx, session)
		cleanupCancel()
	}()
	if err := a.persistSession(ctx, value, session); err != nil {
		a.executionFailure(ctx, value, err)
		return
	}
	a.consume(ctx, value, Workspace{BaseSHA: value.BaseSHA, Path: value.WorktreePath}, stream)
}

func (a *App) consume(ctx context.Context, value task.Task, workspace Workspace, stream <-chan provider.Event) {
	for {
		select {
		case <-ctx.Done():
			if errors.Is(context.Cause(ctx), errAuthSuspended) {
				return
			}
			if errors.Is(context.Cause(ctx), ErrLeaseLost) {
				a.executionFailure(ctx, value, context.Cause(ctx))
				return
			}
			if current, err := a.deps.Store.Task(context.WithoutCancel(ctx), value.ID); err == nil && current.State != task.Canceled {
				a.pause(current, "daemon stopped while provider was active")
			}
			return
		case observed, ok := <-stream:
			if !ok {
				a.executionFailure(ctx, value, errors.New("provider session ended before completion"))
				return
			}
			switch observed.Type {
			case provider.EventCompleted:
				if current, err := a.deps.Store.Task(ctx, value.ID); err == nil {
					value = current
				}
				a.deliver(ctx, value, workspace)
				return
			case provider.EventAuthRequired:
				a.appendProviderEvent(ctx, value.ID, observed)
				a.transition(ctx, &value, task.AwaitingAuth, "subscription authentication requires recovery")
				if a.deps.AuthFailure != nil {
					a.deps.AuthFailure(ctx, value.Provider, errors.New("subscription login required"))
				}
				return
			case provider.EventApprovalRequired:
				a.appendProviderEvent(ctx, value.ID, observed)
				if err := a.requestApproval(ctx, &value, observed); err != nil {
					a.fail(value, err)
					return
				}
			case provider.EventApprovalExpired:
				a.appendProviderEvent(ctx, value.ID, observed)
				if err := a.expireApproval(ctx, value, observed); err != nil {
					a.executionFailure(ctx, value, err)
				}
				return
			case provider.EventError:
				if current, err := a.deps.Store.Task(ctx, value.ID); err == nil {
					value = current
				}
				a.appendProviderEvent(ctx, value.ID, observed)
				a.executionFailure(ctx, value, errors.New("provider reported a redacted failure"))
				return
			default:
				a.appendProviderEvent(ctx, value.ID, observed)
			}
		}
	}
}

func (a *App) providerInput(ctx context.Context, value task.Task) (provider.Input, error) {
	records, err := a.deps.Store.Attachments(ctx, value.ID)
	if err != nil {
		return provider.Input{}, err
	}
	attachments := make([]provider.LocalAttachment, 0, len(records))
	for _, record := range records {
		local, err := provider.NewLocalAttachment(record.StoragePath, record.MediaType)
		if err != nil {
			return provider.Input{}, err
		}
		attachments = append(attachments, local)
	}
	input := provider.Input{Text: value.Prompt, Attachments: attachments}
	return input, input.Validate()
}

func (a *App) requestApproval(ctx context.Context, value *task.Task, observed provider.Event) error {
	if a.deps.Signer == nil {
		return errors.New("app: approval signer is unavailable")
	}
	id := observed.RequestID.String()
	if id == "" {
		id = a.nextID()
	}
	now := a.deps.Clock().UTC()
	expires := now.Add(10 * time.Minute)
	summary := a.deps.Redactor.RedactString(observed.Message)
	payload, _ := json.Marshal(map[string]string{"summary": summary})
	record := task.Approval{ID: id, TaskID: value.ID, Kind: "provider", Status: task.ApprovalPending, RequestPayload: payload, RequestedAt: now, ExpiresAt: &expires}
	if err := a.deps.Store.UpsertApproval(ctx, record); err != nil {
		return err
	}
	if !a.transition(ctx, value, task.AwaitingApproval, "operator approval required") {
		return errors.Join(store.ErrInvalidTransition, a.finishApproval(ctx, &record, task.ApprovalRejected, false, "publication_failed"))
	}
	keyboard, err := telegram.ApprovalKeyboard(a.deps.Signer, value.ID, id, 10*time.Minute)
	if err != nil {
		return errors.Join(err, a.finishApproval(ctx, &record, task.ApprovalRejected, false, "publication_failed"))
	}
	if _, err := a.deps.Messenger.Send(ctx, telegram.Message{ChatID: value.TelegramChatID, Text: "Approval required: " + summary, InlineKeyboard: keyboard}); err != nil {
		return errors.Join(err, a.finishApproval(ctx, &record, task.ApprovalRejected, false, "publication_failed"))
	}
	return nil
}

func (a *App) resolveApproval(ctx context.Context, update telegram.Update, command telegram.Command) error {
	if update.Callback == nil {
		return errors.New("app: approval callback is missing")
	}
	pending, err := a.deps.Store.PendingApprovals(ctx)
	if err != nil {
		return err
	}
	var record task.Approval
	for _, value := range pending {
		if value.ID == command.ApprovalID && value.TaskID == command.TaskID {
			record = value
			break
		}
	}
	if record.ID == "" {
		return store.ErrNotFound
	}
	allow := command.Kind == telegram.KindApprove
	userID := fmt.Sprint(update.Callback.From.ID)
	if a.deps.Approvals != nil {
		err := a.deps.Approvals.HandleDecision(ctx, command.TaskID, command.ApprovalID, userID, allow)
		if err == nil {
			if err := a.deps.Messenger.AnswerCallback(ctx, command.CallbackID, "Decision recorded"); err != nil {
				return err
			}
			event := a.event(command.TaskID, task.EventApprovalResolved, task.VisibilityUser, map[string]any{"approved": allow})
			_ = a.deps.Store.AppendEvent(ctx, event)
			_ = a.publish(ctx, event)
			return nil
		}
		if !errors.Is(err, approval.ErrNotPending) {
			return err
		}
	}
	value, err := a.deps.Store.Task(ctx, command.TaskID)
	if err != nil {
		return err
	}
	a.mu.Lock()
	active, ok := a.active[value.ID]
	a.mu.Unlock()
	if !ok {
		return errors.New("app: provider session is not active")
	}
	requestID, err := provider.NewID(record.ID)
	if err != nil {
		return err
	}
	taskID, err := provider.NewID(value.ID)
	if err != nil {
		return err
	}
	decision := provider.ApprovalDecision{RequestID: requestID, TaskID: taskID, UserID: userID, Allow: allow, DecidedAt: a.deps.Clock().UTC()}
	status := task.ApprovalRejected
	if allow {
		status = task.ApprovalApproved
	}
	if err := a.finishApproval(ctx, &record, status, allow, ""); err != nil {
		return err
	}
	if err := active.provider.ResolveApproval(ctx, decision); err != nil {
		compensationErr := a.finishApproval(ctx, &record, task.ApprovalRejected, false, "provider_release_failed")
		a.fail(value, err)
		return errors.Join(err, compensationErr)
	}
	event := a.event(value.ID, task.EventApprovalResolved, task.VisibilityUser, map[string]any{"approved": allow})
	_ = a.deps.Store.AppendEvent(ctx, event)
	_ = a.publish(ctx, event)
	if allow {
		if !a.transition(ctx, &value, task.Running, "approval granted") {
			return store.ErrInvalidTransition
		}
	} else {
		a.fail(value, errors.New("operator rejected approval"))
	}
	return a.deps.Messenger.AnswerCallback(ctx, command.CallbackID, "Decision recorded")
}

func (a *App) finishApproval(ctx context.Context, record *task.Approval, status task.ApprovalStatus, approved bool, reason string) error {
	resolvedAt := a.deps.Clock().UTC()
	payload := map[string]any{"approved": approved}
	if reason != "" {
		payload["reason"] = reason
	}
	record.Status = status
	record.ResolvedAt = &resolvedAt
	record.DecisionPayload, _ = json.Marshal(payload)
	return a.deps.Store.UpsertApproval(ctx, *record)
}

func (a *App) expireApproval(ctx context.Context, value task.Task, observed provider.Event) error {
	pending, err := a.deps.Store.PendingApprovals(ctx)
	if err != nil {
		return err
	}
	for i := range pending {
		record := &pending[i]
		if record.ID != observed.RequestID.String() || record.TaskID != value.ID {
			continue
		}
		if err := a.finishApproval(ctx, record, task.ApprovalExpired, false, "expired"); err != nil {
			return err
		}
		event := a.event(value.ID, task.EventApprovalResolved, task.VisibilityUser, map[string]any{"approved": false, "expired": true})
		_ = a.deps.Store.AppendEvent(ctx, event)
		_ = a.publish(ctx, event)
		a.fail(value, errors.New("provider approval expired"))
		return nil
	}
	return nil
}

func (a *App) deliver(ctx context.Context, value task.Task, workspace Workspace) {
	changed, err := a.deps.Delivery.Changed(ctx, value, workspace)
	if err != nil {
		a.executionFailure(ctx, value, err)
		return
	}
	if !changed {
		a.transition(ctx, &value, task.Completed, "provider completed without repository changes")
		return
	}
	if value.State == task.Running && !a.transition(ctx, &value, task.Verifying, "running configured verification") {
		return
	}
	if err := a.deps.Delivery.Verify(ctx, value, workspace); err != nil {
		a.executionFailure(ctx, value, err)
		return
	}
	verification := a.event(value.ID, task.EventVerification, task.VisibilityUser, map[string]any{"status": "passed"})
	_ = a.deps.Store.AppendEvent(ctx, verification)
	_ = a.publish(ctx, verification)
	if !a.transition(ctx, &value, task.Committing, "creating verified commit") {
		return
	}
	commit, err := a.deps.Delivery.Commit(ctx, value, workspace)
	if err != nil {
		a.executionFailure(ctx, value, err)
		return
	}
	commitEvent := a.event(value.ID, task.EventCommitCreated, task.VisibilityUser, map[string]any{"commit": commit})
	_ = a.deps.Store.AppendEvent(ctx, commitEvent)
	_ = a.publish(ctx, commitEvent)
	if !a.transition(ctx, &value, task.Pushing, "pushing exact configured ref") {
		return
	}
	ref, err := a.deps.Delivery.Push(ctx, value, workspace, commit)
	if err != nil {
		a.executionFailure(ctx, value, err)
		return
	}
	if err := a.deps.Store.SaveDelivery(ctx, value.ID, commit, ref, a.config.DeploymentURLs[value.RepoProfileID]); err != nil {
		a.executionFailure(ctx, value, err)
		return
	}
	push := a.event(value.ID, task.EventPushCompleted, task.VisibilityUser, map[string]any{"ref": ref})
	_ = a.deps.Store.AppendEvent(ctx, push)
	_ = a.publish(ctx, push)
	a.transition(ctx, &value, task.Completed, "delivery completed")
}

func (a *App) persistSession(ctx context.Context, value task.Task, session provider.Session) error {
	at := a.deps.Clock().UTC()
	record := task.Session{ID: session.ID.String(), TaskID: value.ID, Provider: value.Provider, ProviderSessionID: session.ExternalID, ProviderThreadID: session.ThreadID, Status: "running", Resumable: true, CreatedAt: at, UpdatedAt: at}
	if record.ProviderSessionID == "" {
		record.ProviderSessionID = session.ID.String()
	}
	return a.deps.Store.SaveProviderSession(ctx, value.ID, record)
}

func (a *App) appendProviderEvent(ctx context.Context, id string, observed provider.Event) {
	typeOfEvent := task.EventProviderMessage
	if observed.Type == provider.EventApprovalRequired {
		typeOfEvent = task.EventApprovalRequested
	}
	if observed.Type == provider.EventAuthRequired {
		typeOfEvent = task.EventAuthRequired
	}
	event := a.event(id, typeOfEvent, task.VisibilityUser, map[string]any{
		"type": observed.Type, "message": a.deps.Redactor.RedactString(observed.Message),
		"tool": a.deps.Redactor.RedactString(observed.Tool), "path": a.deps.Redactor.RedactString(observed.Path),
	})
	event.ProviderEventID = observed.ID.String()
	if err := a.deps.Store.AppendEvent(ctx, event); err == nil {
		_ = a.publish(ctx, event)
	}
	if observed.Type == provider.EventAssistantMessage && strings.TrimSpace(observed.Message) != "" {
		if value, err := a.deps.Store.Task(ctx, id); err == nil {
			_, _ = a.deps.Messenger.Send(ctx, telegram.Message{ChatID: value.TelegramChatID, Text: a.deps.Redactor.RedactString(observed.Message)})
		}
	}
}

func (a *App) transition(ctx context.Context, value *task.Task, state task.State, action string) bool {
	event := a.event(value.ID, task.EventStateTransitioned, task.VisibilityUser, map[string]any{"state": state, "action": action})
	if err := a.deps.Store.Transition(ctx, value.ID, state, event); err != nil {
		a.deps.Logger.Error("task transition failed", "task", value.ID, "state", state)
		return false
	}
	value.State, value.UpdatedAt = state, event.CreatedAt
	_ = a.publish(ctx, event)
	_ = a.project(ctx, *value, action, true)
	return true
}

func (a *App) fail(value task.Task, cause error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(a.ctx), 5*time.Second)
	defer cancel()
	reason := "task execution failed; inspect redacted events"
	_ = a.deps.Store.SaveFailure(ctx, value.ID, reason)
	current, err := a.deps.Store.Task(ctx, value.ID)
	if err == nil {
		value = current
	}
	event := a.event(value.ID, task.EventFailure, task.VisibilityUser, map[string]any{"reason": reason})
	if task.CanTransition(value.State, task.Failed) {
		_ = a.deps.Store.Transition(ctx, value.ID, task.Failed, event)
		_ = a.publish(ctx, event)
		value.State = task.Failed
		_ = a.project(ctx, value, reason, true)
	}
	a.deps.Logger.Error("task failed", "task", value.ID, "error_type", fmt.Sprintf("%T", cause))
}

func (a *App) executionFailure(ctx context.Context, value task.Task, cause error) {
	switch {
	case errors.Is(context.Cause(ctx), errAuthSuspended):
		return
	case errors.Is(context.Cause(ctx), ErrLeaseLost):
		a.pause(value, "durable repository lease lost; task stopped before further mutation")
		return
	default:
		a.fail(value, cause)
	}
}

func (a *App) pause(value task.Task, reason string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(a.ctx), 5*time.Second)
	defer cancel()
	_ = a.deps.Store.SaveFailure(ctx, value.ID, reason)
	if current, err := a.deps.Store.Task(ctx, value.ID); err == nil {
		value = current
	}
	event := a.event(value.ID, task.EventStateTransitioned, task.VisibilityUser, map[string]any{"state": task.Paused, "reason": reason})
	if task.CanTransition(value.State, task.Paused) {
		_ = a.deps.Store.Transition(ctx, value.ID, task.Paused, event)
		_ = a.publish(ctx, event)
		value.State = task.Paused
		_ = a.project(ctx, value, reason, true)
	}
}

func (a *App) cancelTask(ctx context.Context, id string) error {
	value, err := a.deps.Store.Task(ctx, id)
	if err != nil {
		return err
	}
	event := a.event(id, task.EventStateTransitioned, task.VisibilityUser, map[string]any{"state": task.Canceled, "action": "canceled by operator"})
	if err := a.deps.Store.Transition(ctx, id, task.Canceled, event); err != nil {
		return err
	}
	// Persist cancellation before interrupting the provider. Interrupt may close
	// its event stream synchronously; consumers must then observe Canceled and
	// never race the operator transition with a Running -> Failed transition.
	active, ok := a.takeActive(id)
	if ok {
		active.cancel(context.Canceled)
		_ = active.provider.Interrupt(ctx, active.session)
	}
	value.State = task.Canceled
	_ = a.publish(ctx, event)
	return a.project(ctx, value, "canceled by operator", true)
}

func (a *App) sendUsage(ctx context.Context, chatID int64, selected task.Provider) error {
	sent := false
	for name, value := range a.deps.Providers {
		if selected.Valid() && selected != name {
			continue
		}
		usage, err := value.Usage(ctx)
		if err != nil {
			text := fmt.Sprintf("%s usage unavailable: no fresh CLI snapshot.", name)
			if auth, authErr := value.AuthStatus(ctx); authErr == nil && !auth.Authenticated {
				text = fmt.Sprintf("%s usage unavailable: authentication required.", name)
			}
			if _, sendErr := a.deps.Messenger.Send(ctx, telegram.Message{ChatID: chatID, Text: text}); sendErr != nil {
				return sendErr
			}
			sent = true
			continue
		}
		text := renderUsage(name, usage)
		if _, err := a.deps.Messenger.Send(ctx, telegram.Message{ChatID: chatID, Text: text}); err != nil {
			return err
		}
		sent = true
	}
	if !sent {
		return ErrUnknownProvider
	}
	return nil
}

func renderUsage(name task.Provider, usage provider.Usage) string {
	var text strings.Builder
	fmt.Fprintf(&text, "%s usage\n", name)
	if usage.ObservedAt.IsZero() {
		text.WriteString("Observed: unavailable\n")
	} else {
		fmt.Fprintf(&text, "Observed: %s\n", usage.ObservedAt.UTC().Format(time.RFC3339))
	}
	if len(usage.Windows) == 0 {
		text.WriteString("Windows: no usage windows reported\n")
	}
	for _, window := range usage.Windows {
		fmt.Fprintf(&text, "%s: %.1f%% used", window.Name, window.UsedPercent)
		if window.ResetsAt.IsZero() {
			text.WriteString("; reset time unavailable\n")
		} else {
			fmt.Fprintf(&text, "; resets %s\n", window.ResetsAt.UTC().Format(time.RFC3339))
		}
	}
	if usage.Credits != nil {
		fmt.Fprintf(&text, "Credits: %.2f\n", *usage.Credits)
	}
	return strings.TrimSpace(text.String())
}

func (a *App) project(ctx context.Context, value task.Task, action string, important bool) error {
	status := telegram.TaskStatus{TaskID: value.ID, ChatID: value.TelegramChatID, State: value.State, CurrentAction: action, RepoProfile: value.RepoProfileID, DeliveryRef: value.PushRef, Important: important}
	if value.StartedAt != nil {
		status.StartedAt = *value.StartedAt
	}
	text := renderTaskStatus(status, a.deps.Clock())
	if value.TelegramMessageID == 0 {
		ref, err := a.deps.Messenger.Send(ctx, telegram.Message{ChatID: value.TelegramChatID, Text: text})
		if err != nil {
			return err
		}
		return a.deps.Store.SaveTelegramMessage(ctx, value.ID, ref.MessageID)
	}
	return a.deps.Messenger.Edit(ctx, telegram.MessageRef{ChatID: value.TelegramChatID, MessageID: value.TelegramMessageID}, telegram.Message{ChatID: value.TelegramChatID, Text: text})
}

func renderTaskStatus(value telegram.TaskStatus, now time.Time) string {
	elapsed := time.Duration(0)
	if !value.StartedAt.IsZero() && now.After(value.StartedAt) {
		elapsed = now.Sub(value.StartedAt)
	}
	return fmt.Sprintf("Task: %s\nState: %s\nElapsed: %s\nRepository: %s\nAction: %s", value.TaskID, value.State, elapsed.Round(time.Second), value.RepoProfile, value.CurrentAction)
}

func (a *App) event(id string, kind task.EventType, visibility task.EventVisibility, payload any) task.Event {
	encoded, _ := json.Marshal(payload)
	return task.Event{ID: a.nextID(), TaskID: id, Type: kind, Visibility: visibility, Payload: encoded, CreatedAt: a.deps.Clock().UTC()}
}

func (a *App) nextID() string {
	a.idMu.Lock()
	defer a.idMu.Unlock()
	return a.newID()
}
func (a *App) publish(ctx context.Context, value task.Event) error {
	if a.deps.Live == nil {
		return nil
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return a.deps.Live.Publish(ctx, events.Event{ID: value.ID, Type: string(value.Type), Payload: payload})
}
func (a *App) rememberActive(id string, p provider.Provider, session provider.Session, cancel context.CancelCauseFunc, done <-chan struct{}) {
	a.mu.Lock()
	a.active[id] = activeTask{provider: p, session: session, cancel: cancel, done: done}
	a.mu.Unlock()
}
func (a *App) takeActive(id string) (activeTask, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	value, ok := a.active[id]
	delete(a.active, id)
	return value, ok
}

// SuspendProvider stops all live sessions for a provider without changing
// durable task state. auth.Service owns the subsequent Running -> AwaitingAuth
// transition, so recovery cannot race an old session.
func (a *App) SuspendProvider(ctx context.Context, providerName task.Provider) error {
	a.mu.Lock()
	active := make([]activeTask, 0)
	for id, value := range a.active {
		if value.provider.Name() == providerName {
			active = append(active, value)
			delete(a.active, id)
		}
	}
	a.mu.Unlock()
	for _, value := range active {
		value.cancel(errAuthSuspended)
		_ = value.provider.Interrupt(ctx, value.session)
	}
	for _, value := range active {
		select {
		case <-value.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (a *App) Wait(ctx context.Context, id string) (task.Task, error) {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		value, err := a.deps.Store.Task(ctx, id)
		if err != nil {
			return task.Task{}, err
		}
		if value.State.Terminal() || value.State == task.Failed || value.State == task.Paused || value.State == task.AwaitingAuth || value.State == task.AwaitingApproval {
			return value, nil
		}
		select {
		case <-ctx.Done():
			return task.Task{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (a *App) Shutdown(ctx context.Context) error {
	a.closeOnce.Do(func() {
		a.mu.Lock()
		a.closed = true
		a.mu.Unlock()
		go a.finishShutdown()
	})
	select {
	case <-a.shutdownDone:
		return a.shutdownErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *App) finishShutdown() {
	var result error
	defer func() {
		a.shutdownErr = result
		close(a.shutdownDone)
	}()
	a.mu.Lock()
	cancel := a.cancel
	active := make([]activeTask, 0, len(a.active))
	for id, value := range a.active {
		active = append(active, value)
		delete(a.active, id)
	}
	a.mu.Unlock()
	for _, value := range active {
		value.cancel(context.Canceled)
		interruptCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
		result = errors.Join(result, value.provider.Interrupt(interruptCtx, value.session))
		done()
	}
	if cancel != nil {
		cancel()
	}
	a.wg.Wait()
	if a.scheduler != nil {
		closeCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
		result = errors.Join(result, a.scheduler.Close(closeCtx))
		done()
	}
	if a.deps.BeforeStoreClose != nil {
		closeCtx, done := context.WithTimeout(context.Background(), 20*time.Second)
		result = errors.Join(result, a.deps.BeforeStoreClose(closeCtx))
		done()
	}
	if a.deps.Live != nil {
		a.deps.Live.Close()
	}
	result = errors.Join(result, a.deps.Store.Close())
}
