package app

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/berkayahi/agentbridge/internal/attachment"
	"github.com/berkayahi/agentbridge/internal/provider"
	providerfake "github.com/berkayahi/agentbridge/internal/provider/fake"
	"github.com/berkayahi/agentbridge/internal/store"
	"github.com/berkayahi/agentbridge/internal/task"
	"github.com/berkayahi/agentbridge/internal/telegram"
)

func TestTelegramPromptCompletesDurableDelivery(t *testing.T) {
	fixture := newFixture(t, []provider.Event{
		{ID: provider.MustID("event-message"), Type: provider.EventAssistantMessage, Message: "Implemented the requested change."},
		{ID: provider.MustID("event-complete"), Type: provider.EventCompleted},
	})
	fixture.start(t)

	id, err := fixture.app.HandleUpdate(context.Background(), promptUpdate(1, "/codex fix the menu button"))
	if err != nil {
		t.Fatal(err)
	}
	got := fixture.wait(t, id)
	if got.State != task.Completed || got.BaseSHA != fixture.workspace.result.BaseSHA || got.CommitSHA != "commit-sha" || got.PushRef != "refs/heads/staging" {
		t.Fatalf("completed task = %#v", got)
	}
	if got.ProviderSessionID != "session-1" {
		t.Fatalf("provider session = %q", got.ProviderSessionID)
	}
	if calls := fixture.delivery.calls(); len(calls) != 3 || calls[0] != "verify" || calls[1] != "commit" || calls[2] != "push" {
		t.Fatalf("delivery calls = %v", calls)
	}
	if len(fixture.messenger.messages()) == 0 {
		t.Fatal("Telegram received no durable status projection")
	}
	events, err := fixture.store.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !hasEvent(events, task.EventProviderMessage) || !hasEvent(events, task.EventPushCompleted) {
		t.Fatalf("events = %#v", events)
	}
}

func TestProviderAuthFailureAwaitsRecovery(t *testing.T) {
	fixture := newFixture(t, []provider.Event{{ID: provider.MustID("auth"), Type: provider.EventAuthRequired, Message: "login required"}})
	fixture.start(t)
	id, err := fixture.app.HandleUpdate(context.Background(), promptUpdate(2, "/codex continue work"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fixture.wait(t, id); got.State != task.AwaitingAuth {
		t.Fatalf("state = %s", got.State)
	}
}

func TestApprovalRejectionFailsWithoutDelivery(t *testing.T) {
	fixture := newFixture(t, []provider.Event{
		{ID: provider.MustID("approval"), Type: provider.EventApprovalRequired, Message: "push requested"},
		{ID: provider.MustID("rejected"), Type: provider.EventError, Message: "approval rejected"},
	})
	fixture.start(t)
	id, err := fixture.app.HandleUpdate(context.Background(), promptUpdate(3, "/codex risky change"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fixture.wait(t, id); got.State != task.Failed {
		t.Fatalf("state = %s", got.State)
	}
	if calls := fixture.delivery.calls(); len(calls) != 0 {
		t.Fatalf("delivery calls = %v", calls)
	}
}

func TestVerificationFailureNeverCommits(t *testing.T) {
	fixture := newFixture(t, []provider.Event{{ID: provider.MustID("done"), Type: provider.EventCompleted}})
	fixture.delivery.verifyErr = errors.New("checks failed")
	fixture.start(t)
	id, err := fixture.app.HandleUpdate(context.Background(), promptUpdate(4, "/codex break checks"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fixture.wait(t, id); got.State != task.Failed || got.FailureReason == "" {
		t.Fatalf("failed task = %#v", got)
	}
	if calls := fixture.delivery.calls(); len(calls) != 1 || calls[0] != "verify" {
		t.Fatalf("delivery calls = %v", calls)
	}
}

func TestCancellationInterruptsActiveProvider(t *testing.T) {
	blocking := newBlockingProvider()
	fixture := newFixtureWithProvider(t, blocking)
	fixture.start(t)
	id, err := fixture.app.HandleUpdate(context.Background(), promptUpdate(5, "/codex wait"))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-blocking.started:
	case <-time.After(time.Second):
		t.Fatal("provider did not start")
	}
	_, err = fixture.app.HandleUpdate(context.Background(), promptUpdate(6, "/cancel "+id))
	if err != nil {
		t.Fatal(err)
	}
	if got := fixture.wait(t, id); got.State != task.Canceled {
		t.Fatalf("state = %s", got.State)
	}
	if !blocking.wasInterrupted() {
		t.Fatal("provider was not interrupted")
	}
}

func TestLeaseLossInterruptsProviderAndPausesTask(t *testing.T) {
	blocking := newBlockingProvider()
	fixture := newFixtureWithProvider(t, blocking)
	fixture.app.config.LeaseTTL = 30 * time.Millisecond
	fixture.app.config.LeaseHeartbeat = 5 * time.Millisecond
	fixture.start(t)
	id, err := fixture.app.HandleUpdate(context.Background(), promptUpdate(7, "/codex wait"))
	if err != nil {
		t.Fatal(err)
	}
	<-blocking.started
	fixture.store.mu.Lock()
	fixture.store.heartbeatErr = errors.New("lease owner changed")
	fixture.store.mu.Unlock()

	if got := fixture.wait(t, id); got.State != task.Paused || !strings.Contains(got.FailureReason, "lease") {
		t.Fatalf("task after lease loss = %#v", got)
	}
	deadline := time.Now().Add(time.Second)
	for !blocking.wasInterrupted() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !blocking.wasInterrupted() {
		t.Fatal("lease loss did not interrupt provider before returning its permit")
	}
}

func TestSuspendProviderStopsSessionWithoutChangingDurableState(t *testing.T) {
	blocking := newBlockingProvider()
	fixture := newFixtureWithProvider(t, blocking)
	fixture.start(t)
	id, err := fixture.app.HandleUpdate(context.Background(), promptUpdate(8, "/codex wait"))
	if err != nil {
		t.Fatal(err)
	}
	<-blocking.started
	if err := fixture.app.SuspendProvider(context.Background(), task.ProviderCodex); err != nil {
		t.Fatal(err)
	}
	value, err := fixture.store.Task(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if value.State != task.Running {
		t.Fatalf("state = %s, auth service must own AwaitingAuth transition", value.State)
	}
	if !blocking.wasInterrupted() {
		t.Fatal("auth suspension did not interrupt provider")
	}
}

func TestRunStopsIntakeHTTPAndStoreOnCancellation(t *testing.T) {
	fixture := newFixture(t, []provider.Event{{ID: provider.MustID("done"), Type: provider.EventCompleted}})
	transport := &fakeTransport{updates: make(chan telegram.Update)}
	http := &fakeHTTP{listening: make(chan struct{}), stop: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- fixture.app.Run(ctx, transport, http) }()
	select {
	case <-http.listening:
	case <-time.After(time.Second):
		t.Fatal("HTTP did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("daemon did not stop")
	}
	if !http.wasShutdown() || !transport.wasStopped() {
		t.Fatalf("shutdown http=%v transport=%v", http.wasShutdown(), transport.wasStopped())
	}
}

func TestShutdownClosesRuntimeDependenciesBeforeStore(t *testing.T) {
	var mu sync.Mutex
	var order []string
	record := func(value string) {
		mu.Lock()
		order = append(order, value)
		mu.Unlock()
	}
	values := &orderedCloseStore{memoryStore: newMemoryStore(), close: func() error { record("store"); return nil }}
	application, err := New(Config{DefaultRepository: "sample", QueueSize: 1}, Dependencies{
		Store: values, Messenger: &fakeMessenger{},
		Providers: map[task.Provider]provider.Provider{
			task.ProviderCodex: providerfake.New(task.ProviderCodex, provider.MustID("session"), nil),
		},
		Workspace: &fakeWorkspace{}, Delivery: &fakeDelivery{}, Files: fstest.MapFS{},
		BeforeStoreClose: func(context.Context) error { record("runtime"); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := application.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := application.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := strings.Join(order, ",")
	mu.Unlock()
	if got != "runtime,store" {
		t.Fatalf("shutdown order = %q", got)
	}
}

func TestShutdownTimeoutCanBeRetriedUntilStoreCleanupFinishes(t *testing.T) {
	closed := make(chan struct{})
	values := &orderedCloseStore{memoryStore: newMemoryStore(), close: func() error { close(closed); return nil }}
	application, err := New(Config{DefaultRepository: "sample", QueueSize: 1}, Dependencies{
		Store: values, Messenger: &fakeMessenger{},
		Providers: map[task.Provider]provider.Provider{
			task.ProviderCodex: providerfake.New(task.ProviderCodex, provider.MustID("session"), nil),
		},
		Workspace: &fakeWorkspace{}, Delivery: &fakeDelivery{}, Files: fstest.MapFS{},
	})
	if err != nil {
		t.Fatal(err)
	}
	application.wg.Add(1)
	first, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if err := application.Shutdown(first); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first shutdown error = %v", err)
	}
	select {
	case <-closed:
		t.Fatal("store closed while an owned worker was still active")
	default:
	}
	application.wg.Done()
	retry, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := application.Shutdown(retry); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	default:
		t.Fatal("retry did not observe completed store cleanup")
	}
}

func TestAttachmentCommandUsesExplicitTaskAndLaterPhotoUsesAssociation(t *testing.T) {
	blocking := newBlockingProvider()
	fixture := newFixtureWithProvider(t, blocking)
	saver := &fakeAttachmentSaver{}
	fixture.app.deps.Attachments = saver
	fixture.start(t)
	command := promptUpdate(90, "")
	command.Message.Caption = "/codex inspect screenshot"
	command.Message.Attachment = &telegram.IncomingAttachment{FileID: "first", MediaType: "image/png"}
	id, err := fixture.app.HandleUpdate(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if saver.explicitTask != id {
		t.Fatalf("explicit task = %q, want %q", saver.explicitTask, id)
	}
	select {
	case <-blocking.started:
	case <-time.After(time.Second):
		t.Fatal("provider did not start")
	}
	later := promptUpdate(91, "")
	later.Message.Attachment = &telegram.IncomingAttachment{FileID: "later", MediaType: "image/png"}
	if _, err := fixture.app.HandleUpdate(context.Background(), later); err != nil {
		t.Fatal(err)
	}
	if saver.associated != 1 {
		t.Fatalf("associated saves = %d", saver.associated)
	}
	_ = fixture.app.cancelTask(context.Background(), id)
}

func TestAttachmentFailurePausesCreatedTaskAndExplainsAssociationFailure(t *testing.T) {
	fixture := newFixture(t, nil)
	saver := &fakeAttachmentSaver{err: errors.New("ambiguous")}
	fixture.app.deps.Attachments = saver
	fixture.start(t)
	command := promptUpdate(92, "")
	command.Message.Caption = "/codex inspect screenshot"
	command.Message.Attachment = &telegram.IncomingAttachment{FileID: "bad", MediaType: "image/png"}
	if _, err := fixture.app.HandleUpdate(context.Background(), command); err == nil {
		t.Fatal("attachment command succeeded")
	}
	values, _ := fixture.store.ListTasks(context.Background(), store.ListFilter{})
	if len(values) != 1 || values[0].State != task.Paused || values[0].FailureReason == "" {
		t.Fatalf("task = %#v", values)
	}
	later := promptUpdate(93, "")
	later.Message.Attachment = &telegram.IncomingAttachment{FileID: "orphan", MediaType: "image/png"}
	if _, err := fixture.app.HandleUpdate(context.Background(), later); err == nil {
		t.Fatal("orphan attachment succeeded")
	}
	messages := fixture.messenger.messages()
	if !strings.Contains(messages[len(messages)-1].Text, "Reply to a task status") {
		t.Fatalf("message = %q", messages[len(messages)-1].Text)
	}
}

type fixture struct {
	app       *App
	store     *memoryStore
	messenger *fakeMessenger
	workspace *fakeWorkspace
	delivery  *fakeDelivery
	cancel    context.CancelFunc
}

func newFixture(t *testing.T, events []provider.Event) *fixture {
	t.Helper()
	return newFixtureWithProvider(t, providerfake.New(task.ProviderCodex, provider.MustID("session-1"), events))
}

func newFixtureWithProvider(t *testing.T, p provider.Provider) *fixture {
	t.Helper()
	values := newMemoryStore()
	messenger := &fakeMessenger{}
	workspace := &fakeWorkspace{result: Workspace{BaseSHA: "0123456789012345678901234567890123456789", Path: "/work/task"}}
	delivery := &fakeDelivery{}
	sequence := 0
	application, err := New(Config{DefaultRepository: "sample", Listen: "127.0.0.1:8787", QueueSize: 4, NewID: func() string {
		sequence++
		return "id-" + time.Unix(int64(sequence), 0).UTC().Format("150405")
	}}, Dependencies{
		Store: values, Messenger: messenger, Providers: map[task.Provider]provider.Provider{task.ProviderCodex: p},
		Workspace: workspace, Delivery: delivery, Clock: func() time.Time { return time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC) },
		Logger: slog.New(slog.NewTextHandler(discardWriter{}, nil)), Files: fstest.MapFS{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{app: application, store: values, messenger: messenger, workspace: workspace, delivery: delivery}
}

func (f *fixture) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	if err := f.app.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		shutdownCtx, done := context.WithTimeout(context.Background(), time.Second)
		defer done()
		_ = f.app.Shutdown(shutdownCtx)
	})
}

func (f *fixture) wait(t *testing.T, id string) task.Task {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	value, err := f.app.Wait(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func promptUpdate(id int64, text string) telegram.Update {
	return telegram.Update{ID: id, Message: &telegram.IncomingMessage{ID: id, Chat: telegram.Chat{ID: 100, Type: telegram.ChatPrivate}, From: telegram.User{ID: 42}, Text: text}}
}

func hasEvent(values []task.Event, eventType task.EventType) bool {
	for _, value := range values {
		if value.Type == eventType {
			return true
		}
	}
	return false
}

type fakeWorkspace struct{ result Workspace }

func (f *fakeWorkspace) Prepare(context.Context, string, string) (Workspace, error) {
	return f.result, nil
}

type orderedCloseStore struct {
	*memoryStore
	close func() error
}

func (s *orderedCloseStore) Close() error { return s.close() }
func (f *fakeWorkspace) Inspect(context.Context, task.Task) (WorkspaceInspection, error) {
	return WorkspaceInspection{Exists: f.result.Path != "", BaseMatches: f.result.BaseSHA != ""}, nil
}

type fakeDelivery struct {
	mu        sync.Mutex
	log       []string
	verifyErr error
}

func (f *fakeDelivery) Changed(context.Context, task.Task, Workspace) (bool, error) {
	return true, nil
}

func (f *fakeDelivery) Verify(context.Context, task.Task, Workspace) error {
	f.record("verify")
	return f.verifyErr
}
func (f *fakeDelivery) Commit(context.Context, task.Task, Workspace) (string, error) {
	f.record("commit")
	return "commit-sha", nil
}
func (f *fakeDelivery) Push(context.Context, task.Task, Workspace, string) (string, error) {
	f.record("push")
	return "refs/heads/staging", nil
}
func (f *fakeDelivery) record(value string) { f.mu.Lock(); f.log = append(f.log, value); f.mu.Unlock() }
func (f *fakeDelivery) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

type fakeMessenger struct {
	mu   sync.Mutex
	sent []telegram.Message
}

type fakeAttachmentSaver struct {
	explicitTask string
	associated   int
	err          error
}

func (f *fakeAttachmentSaver) Save(context.Context, attachment.IncomingFile) (task.Attachment, error) {
	f.associated++
	return task.Attachment{}, f.err
}
func (f *fakeAttachmentSaver) SaveForTask(_ context.Context, id string, _ attachment.IncomingFile) (task.Attachment, error) {
	f.explicitTask = id
	return task.Attachment{}, f.err
}

func (f *fakeMessenger) Send(_ context.Context, value telegram.Message) (telegram.MessageRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, value)
	return telegram.MessageRef{ChatID: value.ChatID, MessageID: int64(len(f.sent))}, nil
}
func (*fakeMessenger) Edit(context.Context, telegram.MessageRef, telegram.Message) error { return nil }
func (*fakeMessenger) AnswerCallback(context.Context, string, string) error              { return nil }
func (*fakeMessenger) SendDocument(context.Context, telegram.Document) error             { return nil }
func (f *fakeMessenger) messages() []telegram.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]telegram.Message(nil), f.sent...)
}

type blockingProvider struct {
	started     chan struct{}
	events      chan provider.Event
	mu          sync.Mutex
	interrupted bool
}

func newBlockingProvider() *blockingProvider {
	return &blockingProvider{started: make(chan struct{}), events: make(chan provider.Event)}
}
func (*blockingProvider) Name() task.Provider { return task.ProviderCodex }
func (p *blockingProvider) Start(context.Context, provider.StartRequest) (provider.Session, <-chan provider.Event, error) {
	close(p.started)
	return provider.Session{ID: provider.MustID("blocking-session"), ExternalID: "blocking-session", Provider: task.ProviderCodex}, p.events, nil
}
func (p *blockingProvider) Resume(context.Context, provider.ResumeRequest) (provider.Session, <-chan provider.Event, error) {
	return provider.Session{}, nil, errors.New("unexpected resume")
}
func (*blockingProvider) Steer(context.Context, provider.Session, provider.Input) error { return nil }
func (p *blockingProvider) Interrupt(context.Context, provider.Session) error {
	p.mu.Lock()
	p.interrupted = true
	p.mu.Unlock()
	close(p.events)
	return nil
}
func (*blockingProvider) ResolveApproval(context.Context, provider.ApprovalDecision) error {
	return nil
}
func (*blockingProvider) Usage(context.Context) (provider.Usage, error) { return provider.Usage{}, nil }
func (*blockingProvider) AuthStatus(context.Context) (provider.AuthStatus, error) {
	return provider.AuthStatus{}, nil
}
func (p *blockingProvider) wasInterrupted() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.interrupted
}

type discardWriter struct{}

func (discardWriter) Write(value []byte) (int, error) { return len(value), nil }

var _ fs.FS = fstest.MapFS{}
var _ = store.ErrNotFound

type fakeTransport struct {
	updates chan telegram.Update
	mu      sync.Mutex
	stopped bool
}

func (f *fakeTransport) Run(ctx context.Context) {
	<-ctx.Done()
	f.mu.Lock()
	f.stopped = true
	f.mu.Unlock()
}
func (f *fakeTransport) Next(ctx context.Context) (telegram.Update, error) {
	select {
	case value := <-f.updates:
		return value, nil
	case <-ctx.Done():
		return telegram.Update{}, ctx.Err()
	}
}
func (f *fakeTransport) wasStopped() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.stopped }

type fakeHTTP struct {
	listening chan struct{}
	stop      chan struct{}
	once      sync.Once
	mu        sync.Mutex
	shutdown  bool
}

func (f *fakeHTTP) Listen(string) error { close(f.listening); <-f.stop; return nil }
func (f *fakeHTTP) ShutdownWithContext(context.Context) error {
	f.mu.Lock()
	f.shutdown = true
	f.mu.Unlock()
	f.once.Do(func() { close(f.stop) })
	return nil
}
func (f *fakeHTTP) wasShutdown() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.shutdown }
