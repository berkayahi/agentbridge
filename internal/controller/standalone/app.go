// Package standalone adapts local presentation transports to the durable workflow.
package standalone

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
	"github.com/berkayahi/agentbridge/internal/telegram"
	"github.com/berkayahi/agentbridge/internal/workmodel"
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
	Models            map[workmodel.Provider]string
	DeploymentURLs    map[string]string
}

type Store interface {
	store.Store
	SaveWorkspace(context.Context, string, string, string) error
	SaveTelegramMessage(context.Context, string, int64) error
	SaveProviderSession(context.Context, string, workmodel.Session) error
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
	Inspect(context.Context, workmodel.Task) (WorkspaceInspection, error)
}
type DeliveryPort interface {
	Changed(context.Context, workmodel.Task, Workspace) (bool, error)
	Verify(context.Context, workmodel.Task, Workspace) error
	Commit(context.Context, workmodel.Task, Workspace) (string, error)
	Push(context.Context, workmodel.Task, Workspace, string) (string, error)
}
type Authorizer interface{ Authorize(telegram.Update) error }
type AttachmentSaver interface {
	Save(context.Context, attachment.IncomingFile) (workmodel.Attachment, error)
	SaveForTask(context.Context, string, attachment.IncomingFile) (workmodel.Attachment, error)
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
	Providers   map[workmodel.Provider]provider.Provider
	Workspace   WorkspacePort
	Delivery    DeliveryPort
	Authorizer  Authorizer
	Signer      *telegram.CallbackSigner
	Attachments AttachmentSaver
	Approvals   *approval.Broker
	AuthFailure func(context.Context, workmodel.Provider, error)
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
type pendingPrompt struct {
	provider workmodel.Provider
	prompt   string
	expires  time.Time
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
	pendingMu    sync.Mutex
	pending      map[int64]pendingPrompt
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
	return &App{config: config, deps: deps, newID: config.NewID, queue: make(chan queuedTask, config.QueueSize), active: make(map[string]activeTask), pending: make(map[int64]pendingPrompt), shutdownDone: make(chan struct{})}, nil
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
		return a.chooseSession(ctx, update.Message, command)
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
	case telegram.KindRename:
		if update.Message == nil {
			return "", errors.New("app: rename message is missing")
		}
		return "", a.renameTask(ctx, command.TaskID, command.Argument, update.Message.Chat.ID)
	case telegram.KindSessionSelect:
		return "", a.resolveSessionChoice(ctx, update)
	default:
		return "", errors.New("app: command is not implemented by daemon")
	}
}

func (a *App) renameTask(ctx context.Context, id, title string, chatID int64) error {
	value, err := a.deps.Store.Task(ctx, id)
	if err != nil {
		return err
	}
	if value.TelegramChatID != chatID {
		return errors.New("app: task belongs to another chat")
	}
	renamer, ok := a.deps.Store.(interface {
		RenameTask(context.Context, string, string) error
	})
	if !ok {
		return errors.New("app: task rename is unavailable")
	}
	title = workmodel.Title(title, workmodel.DefaultTitleRunes)
	if err := renamer.RenameTask(ctx, id, title); err != nil {
		return err
	}
	_, err = a.deps.Messenger.Send(ctx, telegram.Message{ChatID: chatID, Text: fmt.Sprintf("Task renamed: %s", title)})
	return err
}

func (a *App) createTask(ctx context.Context, message *telegram.IncomingMessage, command telegram.Command) (string, error) {
	value, err := a.createTaskRecord(ctx, command.Provider, command.Argument, message.Chat.ID, true)
	if err != nil {
		return "", err
	}
	id := value.ID
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

func (a *App) createTaskRecord(ctx context.Context, providerName workmodel.Provider, prompt string, chatID int64, project bool) (workmodel.Task, error) {
	if _, ok := a.deps.Providers[providerName]; !ok {
		return workmodel.Task{}, ErrUnknownProvider
	}
	at := a.deps.Clock().UTC()
	id := a.nextID()
	value := workmodel.Task{ID: id, RepoProfileID: a.config.DefaultRepository, Title: workmodel.Title(prompt, workmodel.DefaultTitleRunes), Prompt: prompt, State: workmodel.Queued, Provider: providerName, TelegramChatID: chatID, CreatedAt: at, UpdatedAt: at}
	event := a.event(id, workmodel.EventTaskCreated, workmodel.VisibilityUser, map[string]any{"title": value.Title})
	if err := a.deps.Store.CreateTask(ctx, value, event); err != nil {
		return workmodel.Task{}, err
	}
	if err := a.publish(ctx, event); err != nil {
		a.deps.Logger.Warn("could not publish task event", "task", id)
	}
	if project {
		if err := a.project(ctx, value, "queued", true); err != nil {
			a.pause(value, "initial status delivery failed; manual retry required")
			return workmodel.Task{}, err
		}
	}
	return value, nil
}

func (a *App) chooseSession(ctx context.Context, message *telegram.IncomingMessage, command telegram.Command) (string, error) {
	tasks, err := a.deps.Store.ListTasks(ctx, store.ListFilter{Limit: 20})
	if err != nil {
		return "", err
	}
	options := make([]workmodel.Task, 0, 8)
	for _, value := range tasks {
		if value.Provider == command.Provider && value.TelegramChatID == message.Chat.ID && value.ProviderSessionID != "" && value.State != workmodel.Failed && value.State != workmodel.Canceled {
			options = append(options, value)
			if len(options) == 8 {
				break
			}
		}
	}
	if len(options) == 0 {
		return a.createTask(ctx, message, command)
	}
	a.pendingMu.Lock()
	a.pending[message.Chat.ID] = pendingPrompt{provider: command.Provider, prompt: command.Argument, expires: time.Now().Add(10 * time.Minute)}
	a.pendingMu.Unlock()
	keyboard, err := telegram.SessionKeyboard(a.deps.Signer, command.Provider, options, 10*time.Minute)
	if err != nil {
		return "", err
	}
	_, err = a.deps.Messenger.Send(ctx, telegram.Message{ChatID: message.Chat.ID, Text: fmt.Sprintf("Select %s session for this task:", command.Provider), InlineKeyboard: keyboard})
	return "", err
}

func (a *App) resolveSessionChoice(ctx context.Context, update telegram.Update) error {
	if update.Callback == nil {
		return errors.New("app: session callback is missing")
	}
	chatID := update.Callback.Message.Chat.ID
	a.pendingMu.Lock()
	pending, ok := a.pending[chatID]
	if ok {
		delete(a.pending, chatID)
	}
	a.pendingMu.Unlock()
	if !ok || time.Now().After(pending.expires) {
		return errors.New("app: session selection expired")
	}
	_, _ = a.deps.Messenger.Send(ctx, telegram.Message{ChatID: chatID, Text: fmt.Sprintf("Session selected (%s). Starting your task now…", pending.provider)})
	var err error
	updateCommand, parseErr := telegram.ParseUpdate(update, a.config.BotUsername, a.deps.Signer)
	if parseErr == nil && updateCommand.Provider.Valid() {
		message := update.Callback.Message
		_, err = a.createTask(ctx, &message, telegram.Command{Kind: telegram.KindPrompt, Provider: pending.provider, Argument: pending.prompt})
	} else if parseErr == nil {
		err = a.continueTask(ctx, updateCommand.TaskID, pending.prompt, chatID)
	} else {
		err = parseErr
	}
	_ = a.deps.Messenger.AnswerCallback(ctx, update.Callback.ID, "Session selected")
	return err
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
	if value.State == workmodel.Verifying {
		a.deliver(ctx, value, Workspace{BaseSHA: value.BaseSHA, Path: value.WorktreePath})
		return
	}
	if value.State != workmodel.Queued && value.State != workmodel.Preparing {
		return
	}
	if value.State == workmodel.Queued && !a.transition(ctx, &value, workmodel.Preparing, "preparing isolated worktree") {
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
	if !a.transition(ctx, &value, workmodel.Running, "provider session started") {
		return
	}
	p := a.deps.Providers[value.Provider]
	taskID, err := provider.NewID(value.ID)
	if err != nil {
		a.executionFailure(ctx, value, err)
		return
	}
	input, err := a.providerInput(ctx, value, workspace.Path)
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

func (a *App) resume(ctx context.Context, value workmodel.Task, cancel context.CancelCauseFunc, input string) {
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
	input = agentContext(value, value.WorktreePath, input)
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

func (a *App) consume(ctx context.Context, value workmodel.Task, workspace Workspace, stream <-chan provider.Event) {
	var assistant strings.Builder
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
			if current, err := a.deps.Store.Task(context.WithoutCancel(ctx), value.ID); err == nil && current.State != workmodel.Canceled {
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
				if text := strings.TrimSpace(assistant.String()); text != "" {
					if current, err := a.deps.Store.Task(ctx, value.ID); err == nil {
						_, _ = a.deps.Messenger.Send(ctx, telegram.Message{ChatID: current.TelegramChatID, Text: a.deps.Redactor.RedactString(text)})
					}
				}
				if current, err := a.deps.Store.Task(ctx, value.ID); err == nil {
					value = current
				}
				a.deliver(ctx, value, workspace)
				return
			case provider.EventAuthRequired:
				a.appendProviderEvent(ctx, value.ID, observed)
				a.transition(ctx, &value, workmodel.AwaitingAuth, "subscription authentication requires recovery")
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
				if observed.Type == provider.EventAssistantMessage {
					assistant.WriteString(observed.Message)
				}
				a.appendProviderEvent(ctx, value.ID, observed)
			}
		}
	}
}

func (a *App) providerInput(ctx context.Context, value workmodel.Task, workspacePath string) (provider.Input, error) {
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
	contextBlock := agentContext(value, workspacePath, value.Prompt)
	input := provider.Input{Text: contextBlock, Attachments: attachments}
	return input, input.Validate()
}

func agentContext(value workmodel.Task, workspacePath, prompt string) string {
	return fmt.Sprintf(`AgentBridge operating context:
- You are the %s coding agent operating inside repository profile %q.
- Current isolated workspace: %s
- Use the configured default repository unless the operator explicitly requests another configured repository; always follow the selected repository profile.
- First inspect the repository's AGENTS.md, CLAUDE.md, README, and existing conventions before changing code.
- Use the existing installed skills/workflows when they match the task. If no suitable skill exists, explicitly tell the operator before proceeding and then use the safest documented fallback.
- Report what you are doing, keep changes scoped to the selected workspace, run relevant verification, and never invent successful results.

Operator task:
%s`, value.Provider, value.RepoProfileID, workspacePath, prompt)
}

func (a *App) requestApproval(ctx context.Context, value *workmodel.Task, observed provider.Event) error {
	id := observed.RequestID.String()
	if id == "" {
		id = a.nextID()
	}
	now := a.deps.Clock().UTC()
	expires := now.Add(10 * time.Minute)
	summary := a.deps.Redactor.RedactString(observed.Message)
	payload, _ := json.Marshal(map[string]string{"summary": summary})
	record := workmodel.Approval{ID: id, TaskID: value.ID, Kind: "provider", Status: workmodel.ApprovalPending, RequestPayload: payload, RequestedAt: now, ExpiresAt: &expires}
	if err := a.deps.Store.UpsertApproval(ctx, record); err != nil {
		return err
	}
	if !a.transition(ctx, value, workmodel.AwaitingApproval, "operator approval required") {
		return errors.Join(store.ErrInvalidTransition, a.finishApproval(ctx, &record, workmodel.ApprovalRejected, false, "publication_failed"))
	}
	if value.TelegramChatID == 0 {
		return nil
	}
	if a.deps.Signer == nil {
		return errors.Join(errors.New("app: approval signer is unavailable"), a.finishApproval(ctx, &record, workmodel.ApprovalRejected, false, "publication_failed"))
	}
	keyboard, err := telegram.ApprovalKeyboard(a.deps.Signer, value.ID, id, 10*time.Minute)
	if err != nil {
		return errors.Join(err, a.finishApproval(ctx, &record, workmodel.ApprovalRejected, false, "publication_failed"))
	}
	if _, err := a.deps.Messenger.Send(ctx, telegram.Message{ChatID: value.TelegramChatID, Text: "Approval required: " + summary, InlineKeyboard: keyboard}); err != nil {
		return errors.Join(err, a.finishApproval(ctx, &record, workmodel.ApprovalRejected, false, "publication_failed"))
	}
	return nil
}

func (a *App) resolveApproval(ctx context.Context, update telegram.Update, command telegram.Command) error {
	if update.Callback == nil {
		return errors.New("app: approval callback is missing")
	}
	err := a.DecideApproval(ctx, ApprovalDecisionRequest{
		TaskID:     command.TaskID,
		ApprovalID: command.ApprovalID,
		UserID:     fmt.Sprint(update.Callback.From.ID),
		Allow:      command.Kind == telegram.KindApprove,
	})
	if err != nil {
		return err
	}
	return a.deps.Messenger.AnswerCallback(ctx, command.CallbackID, "Decision recorded")
}

func (a *App) finishApproval(ctx context.Context, record *workmodel.Approval, status workmodel.ApprovalStatus, approved bool, reason string) error {
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

func (a *App) expireApproval(ctx context.Context, value workmodel.Task, observed provider.Event) error {
	pending, err := a.deps.Store.PendingApprovals(ctx)
	if err != nil {
		return err
	}
	for i := range pending {
		record := &pending[i]
		if record.ID != observed.RequestID.String() || record.TaskID != value.ID {
			continue
		}
		if err := a.finishApproval(ctx, record, workmodel.ApprovalExpired, false, "expired"); err != nil {
			return err
		}
		event := a.event(value.ID, workmodel.EventApprovalResolved, workmodel.VisibilityUser, map[string]any{"approved": false, "expired": true})
		_ = a.deps.Store.AppendEvent(ctx, event)
		_ = a.publish(ctx, event)
		a.fail(value, errors.New("provider approval expired"))
		return nil
	}
	return nil
}

func (a *App) deliver(ctx context.Context, value workmodel.Task, workspace Workspace) {
	changed, err := a.deps.Delivery.Changed(ctx, value, workspace)
	if err != nil {
		a.executionFailure(ctx, value, err)
		return
	}
	if !changed {
		a.transition(ctx, &value, workmodel.Completed, "provider completed without repository changes")
		return
	}
	if value.State == workmodel.Running && !a.transition(ctx, &value, workmodel.Verifying, "running configured verification") {
		return
	}
	if err := a.deps.Delivery.Verify(ctx, value, workspace); err != nil {
		a.executionFailure(ctx, value, err)
		return
	}
	verification := a.event(value.ID, workmodel.EventVerification, workmodel.VisibilityUser, map[string]any{"status": "passed"})
	_ = a.deps.Store.AppendEvent(ctx, verification)
	_ = a.publish(ctx, verification)
	if !a.transition(ctx, &value, workmodel.Committing, "creating verified commit") {
		return
	}
	commit, err := a.deps.Delivery.Commit(ctx, value, workspace)
	if err != nil {
		a.executionFailure(ctx, value, err)
		return
	}
	commitEvent := a.event(value.ID, workmodel.EventCommitCreated, workmodel.VisibilityUser, map[string]any{"commit": commit})
	_ = a.deps.Store.AppendEvent(ctx, commitEvent)
	_ = a.publish(ctx, commitEvent)
	if !a.transition(ctx, &value, workmodel.Pushing, "pushing exact configured ref") {
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
	push := a.event(value.ID, workmodel.EventPushCompleted, workmodel.VisibilityUser, map[string]any{"ref": ref})
	_ = a.deps.Store.AppendEvent(ctx, push)
	_ = a.publish(ctx, push)
	a.transition(ctx, &value, workmodel.Completed, "delivery completed")
}

func (a *App) persistSession(ctx context.Context, value workmodel.Task, session provider.Session) error {
	at := a.deps.Clock().UTC()
	record := workmodel.Session{ID: session.ID.String(), TaskID: value.ID, Provider: value.Provider, ProviderSessionID: session.ExternalID, ProviderThreadID: session.ThreadID, Status: "running", Resumable: true, CreatedAt: at, UpdatedAt: at}
	if record.ProviderSessionID == "" {
		record.ProviderSessionID = session.ID.String()
	}
	return a.deps.Store.SaveProviderSession(ctx, value.ID, record)
}

func (a *App) appendProviderEvent(ctx context.Context, id string, observed provider.Event) {
	typeOfEvent := workmodel.EventProviderMessage
	if observed.Type == provider.EventApprovalRequired {
		typeOfEvent = workmodel.EventApprovalRequested
	}
	if observed.Type == provider.EventAuthRequired {
		typeOfEvent = workmodel.EventAuthRequired
	}
	event := a.event(id, typeOfEvent, workmodel.VisibilityUser, map[string]any{
		"type": observed.Type, "message": a.deps.Redactor.RedactString(observed.Message),
		"tool": a.deps.Redactor.RedactString(observed.Tool), "path": a.deps.Redactor.RedactString(observed.Path),
	})
	event.ProviderEventID = observed.ID.String()
	if err := a.deps.Store.AppendEvent(ctx, event); err == nil {
		_ = a.publish(ctx, event)
	}
}

func (a *App) transition(ctx context.Context, value *workmodel.Task, state workmodel.State, action string) bool {
	event := a.event(value.ID, workmodel.EventStateTransitioned, workmodel.VisibilityUser, map[string]any{"state": state, "action": action})
	if err := a.deps.Store.Transition(ctx, value.ID, state, event); err != nil {
		a.deps.Logger.Error("task transition failed", "task", value.ID, "state", state)
		return false
	}
	value.State, value.UpdatedAt = state, event.CreatedAt
	_ = a.publish(ctx, event)
	_ = a.project(ctx, *value, action, true)
	return true
}

func (a *App) fail(value workmodel.Task, cause error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(a.ctx), 5*time.Second)
	defer cancel()
	reason := "task execution failed; inspect redacted events"
	_ = a.deps.Store.SaveFailure(ctx, value.ID, reason)
	current, err := a.deps.Store.Task(ctx, value.ID)
	if err == nil {
		value = current
	}
	event := a.event(value.ID, workmodel.EventFailure, workmodel.VisibilityUser, map[string]any{"reason": reason})
	if workmodel.CanTransition(value.State, workmodel.Failed) {
		_ = a.deps.Store.Transition(ctx, value.ID, workmodel.Failed, event)
		_ = a.publish(ctx, event)
		value.State = workmodel.Failed
		_ = a.project(ctx, value, reason, true)
	}
	notification := fmt.Sprintf("Task failed: %s\nTitle: %s\nReason: %s\nUse /logs %s for redacted details.", value.ID, value.Title, reason, value.ID)
	if _, err := a.deps.Messenger.Send(ctx, telegram.Message{ChatID: value.TelegramChatID, Text: notification}); err != nil {
		a.deps.Logger.Warn("failed to notify Telegram about task failure", "task", value.ID, "error_type", fmt.Sprintf("%T", err))
	}
	a.deps.Logger.Error("task failed", "task", value.ID, "error_type", fmt.Sprintf("%T", cause))
}

func (a *App) executionFailure(ctx context.Context, value workmodel.Task, cause error) {
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

func (a *App) pause(value workmodel.Task, reason string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(a.ctx), 5*time.Second)
	defer cancel()
	_ = a.deps.Store.SaveFailure(ctx, value.ID, reason)
	if current, err := a.deps.Store.Task(ctx, value.ID); err == nil {
		value = current
	}
	event := a.event(value.ID, workmodel.EventStateTransitioned, workmodel.VisibilityUser, map[string]any{"state": workmodel.Paused, "reason": reason})
	if workmodel.CanTransition(value.State, workmodel.Paused) {
		_ = a.deps.Store.Transition(ctx, value.ID, workmodel.Paused, event)
		_ = a.publish(ctx, event)
		value.State = workmodel.Paused
		_ = a.project(ctx, value, reason, true)
	}
}

func (a *App) cancelTask(ctx context.Context, id string) error {
	value, err := a.deps.Store.Task(ctx, id)
	if err != nil {
		return err
	}
	event := a.event(id, workmodel.EventStateTransitioned, workmodel.VisibilityUser, map[string]any{"state": workmodel.Canceled, "action": "canceled by operator"})
	if err := a.deps.Store.Transition(ctx, id, workmodel.Canceled, event); err != nil {
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
	value.State = workmodel.Canceled
	_ = a.publish(ctx, event)
	return a.project(ctx, value, "canceled by operator", true)
}

func (a *App) sendUsage(ctx context.Context, chatID int64, selected workmodel.Provider) error {
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

func renderUsage(name workmodel.Provider, usage provider.Usage) string {
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

func (a *App) project(ctx context.Context, value workmodel.Task, action string, important bool) error {
	if value.TelegramChatID == 0 {
		return nil
	}
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

func (a *App) event(id string, kind workmodel.EventType, visibility workmodel.EventVisibility, payload any) workmodel.Event {
	encoded, _ := json.Marshal(payload)
	return workmodel.Event{ID: a.nextID(), TaskID: id, Type: kind, Visibility: visibility, Payload: encoded, CreatedAt: a.deps.Clock().UTC()}
}

func (a *App) nextID() string {
	a.idMu.Lock()
	defer a.idMu.Unlock()
	return a.newID()
}
func (a *App) publish(ctx context.Context, value workmodel.Event) error {
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
func (a *App) SuspendProvider(ctx context.Context, providerName workmodel.Provider) error {
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

func (a *App) Wait(ctx context.Context, id string) (workmodel.Task, error) {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		value, err := a.deps.Store.Task(ctx, id)
		if err != nil {
			return workmodel.Task{}, err
		}
		if value.State.Terminal() || value.State == workmodel.Failed || value.State == workmodel.Paused || value.State == workmodel.AwaitingAuth || value.State == workmodel.AwaitingApproval {
			return value, nil
		}
		select {
		case <-ctx.Done():
			return workmodel.Task{}, ctx.Err()
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
