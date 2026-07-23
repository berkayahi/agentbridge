package standalone

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/provider"
	"github.com/berkayahi/agentbridge/internal/security"
	"github.com/berkayahi/agentbridge/internal/telegram"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

func TestNativeApprovalPersistsDecisionBeforeProviderRelease(t *testing.T) {
	var mu sync.Mutex
	var order []string
	base := newMemoryStore()
	values := &approvalOrderStore{memoryStore: base, record: func(value string) {
		mu.Lock()
		order = append(order, value)
		mu.Unlock()
	}}
	providerPort := &approvalProvider{resolve: func(context.Context, provider.ApprovalDecision) error {
		values.record("release")
		return nil
	}}
	fixture := newFixtureWithProvider(t, providerPort)
	fixture.store = base
	fixture.app.deps.Store = values
	fixture.start(t)
	value := seedPendingApproval(t, fixture.app, base, providerPort)

	err := fixture.app.resolveApproval(context.Background(), approvalUpdate(), telegram.Command{
		Kind: telegram.KindApprove, TaskID: value.ID, ApprovalID: "approval-1", CallbackID: "callback-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := append([]string(nil), order...)
	mu.Unlock()
	if want := []string{"persist:approved", "release"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("approval order = %v, want %v", got, want)
	}
}

func TestNativeApprovalReleaseFailureCompensatesDurableDecision(t *testing.T) {
	providerPort := &approvalProvider{resolve: func(context.Context, provider.ApprovalDecision) error {
		return errors.New("rpc response failed")
	}}
	fixture := newFixtureWithProvider(t, providerPort)
	fixture.start(t)
	value := seedPendingApproval(t, fixture.app, fixture.store, providerPort)

	err := fixture.app.resolveApproval(context.Background(), approvalUpdate(), telegram.Command{
		Kind: telegram.KindApprove, TaskID: value.ID, ApprovalID: "approval-1", CallbackID: "callback-1",
	})
	if err == nil {
		t.Fatal("approval release failure succeeded")
	}
	approval := fixture.store.approval("approval-1")
	if approval.Status == workmodel.ApprovalApproved || approval.Status == workmodel.ApprovalPending {
		t.Fatalf("approval status = %q, want fail-closed terminal status", approval.Status)
	}
	if got, getErr := fixture.store.Task(context.Background(), value.ID); getErr != nil || got.State != workmodel.Failed {
		t.Fatalf("task = %#v, err = %v", got, getErr)
	}
}

func TestNativeApprovalPersistenceFailureNeverReleasesProvider(t *testing.T) {
	base := newMemoryStore()
	values := &approvalDecisionFailureStore{memoryStore: base}
	released := false
	providerPort := &approvalProvider{resolve: func(context.Context, provider.ApprovalDecision) error {
		released = true
		return nil
	}}
	fixture := newFixtureWithProvider(t, providerPort)
	fixture.store = base
	fixture.app.deps.Store = values
	fixture.start(t)
	value := seedPendingApproval(t, fixture.app, base, providerPort)

	err := fixture.app.resolveApproval(context.Background(), approvalUpdate(), telegram.Command{
		Kind: telegram.KindApprove, TaskID: value.ID, ApprovalID: "approval-1", CallbackID: "callback-1",
	})
	if err == nil {
		t.Fatal("approval persistence failure succeeded")
	}
	if released {
		t.Fatal("provider was released before durable approval")
	}
	if got := base.approval("approval-1"); got.Status != workmodel.ApprovalPending {
		t.Fatalf("approval status = %s, want pending fail-closed state", got.Status)
	}
}

func TestApprovalTaskAwaitsDecisionBeforeKeyboardPublication(t *testing.T) {
	providerPort := newApprovalBlockingProvider()
	fixture := newFixtureWithProvider(t, providerPort)
	fixture.app.deps.Signer = telegram.NewCallbackSigner([]byte("01234567890123456789012345678901"), nil)
	states := make(chan workmodel.State, 1)
	fixture.app.deps.Messenger = &stateInspectingMessenger{next: fixture.messenger, inspect: func() {
		values, _ := fixture.store.NonterminalTasks(context.Background())
		for _, value := range values {
			if value.State == workmodel.Running || value.State == workmodel.AwaitingApproval {
				states <- value.State
				return
			}
		}
	}}
	fixture.start(t)
	if _, err := fixture.app.HandleUpdate(context.Background(), promptUpdate(201, "/codex approve this")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-states:
		if got != workmodel.AwaitingApproval {
			t.Fatalf("state at keyboard publication = %s, want %s", got, workmodel.AwaitingApproval)
		}
	case <-time.After(time.Second):
		t.Fatal("approval keyboard was not published")
	}
}

func TestApprovalTimeoutExpiresRecordAndFailsTask(t *testing.T) {
	fixture := newFixture(t, []provider.Event{
		{ID: provider.MustID("approval-event"), RequestID: provider.MustID("appr-time"), Type: provider.EventApprovalRequired, Message: "run command"},
		{ID: provider.MustID("timeout-event"), RequestID: provider.MustID("appr-time"), Type: provider.EventApprovalExpired, Message: "approval request expired"},
	})
	fixture.app.deps.Signer = telegram.NewCallbackSigner([]byte("01234567890123456789012345678901"), nil)
	fixture.start(t)
	id, err := fixture.app.HandleUpdate(context.Background(), promptUpdate(202, "/codex timed approval"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fixture.wait(t, id); got.State != workmodel.Failed {
		t.Fatalf("task state = %s, want %s", got.State, workmodel.Failed)
	}
	if got := fixture.store.approval("appr-time"); got.Status != workmodel.ApprovalExpired || got.ResolvedAt == nil {
		t.Fatalf("approval = %#v", got)
	}
}

func TestProviderOutputIsRedactedBeforeDurableAndTelegramPublication(t *testing.T) {
	const secret = "recognizable-bot-token-literal"
	providerPort := newApprovalBlockingProvider()
	providerPort.prelude = "assistant leaked " + secret
	providerPort.summary = "run with " + secret
	fixture := newFixtureWithProvider(t, providerPort)
	fixture.app.deps.Signer = telegram.NewCallbackSigner([]byte("01234567890123456789012345678901"), nil)
	fixture.app.deps.Redactor = security.NewRedactor(security.Config{Secrets: []string{secret}, MaxFieldRunes: 256, MaxPayloadRunes: 1024})
	fixture.start(t)
	id, err := fixture.app.HandleUpdate(context.Background(), promptUpdate(203, "/codex redact output"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fixture.wait(t, id); got.State != workmodel.AwaitingApproval {
		t.Fatalf("task state = %s", got.State)
	}
	events, err := fixture.store.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if strings.Contains(string(event.Payload), secret) {
			t.Fatalf("durable event contains secret: %s", event.Payload)
		}
	}
	approval := fixture.store.approval("appr-req")
	if strings.Contains(string(approval.RequestPayload), secret) || !strings.Contains(string(approval.RequestPayload), "REDACTED") {
		t.Fatalf("approval payload = %s", approval.RequestPayload)
	}
	for _, message := range fixture.messenger.messages() {
		if strings.Contains(message.Text, secret) {
			t.Fatalf("Telegram message contains secret: %q", message.Text)
		}
	}
}

func seedPendingApproval(t *testing.T, application *App, values *memoryStore, providerPort provider.Provider) workmodel.Task {
	t.Helper()
	now := time.Unix(100, 0).UTC()
	value := workmodel.Task{ID: "task-approval", RepoProfileID: "sample", Prompt: "change", State: workmodel.Queued, Provider: workmodel.CodexSubscription, TelegramChatID: 100, CreatedAt: now, UpdatedAt: now}
	if err := values.CreateTask(context.Background(), value, workmodel.Event{ID: "created", TaskID: value.ID, Type: workmodel.EventTaskCreated, Visibility: workmodel.VisibilityUser, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for index, state := range []workmodel.State{workmodel.Preparing, workmodel.Running, workmodel.AwaitingApproval} {
		if err := values.Transition(context.Background(), value.ID, state, workmodel.Event{ID: "transition-" + string(rune('0'+index)), TaskID: value.ID, Type: workmodel.EventStateTransitioned, Visibility: workmodel.VisibilityUser, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	expires := now.Add(time.Minute)
	if err := values.UpsertApproval(context.Background(), workmodel.Approval{ID: "approval-1", TaskID: value.ID, Kind: "provider", Status: workmodel.ApprovalPending, RequestedAt: now, ExpiresAt: &expires}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	close(done)
	application.rememberActive(value.ID, providerPort, provider.Session{ID: provider.MustID("session-approval"), TaskID: provider.MustID(value.ID), Provider: workmodel.CodexSubscription}, func(error) {}, done)
	value.State = workmodel.AwaitingApproval
	return value
}

func approvalUpdate() telegram.Update {
	return telegram.Update{Callback: &telegram.CallbackQuery{ID: "callback-1", From: telegram.User{ID: 42}, Message: telegram.IncomingMessage{Chat: telegram.Chat{ID: 100, Type: telegram.ChatPrivate}}}}
}

type approvalOrderStore struct {
	*memoryStore
	record func(string)
}

type approvalDecisionFailureStore struct{ *memoryStore }

func (s *approvalDecisionFailureStore) UpsertApproval(ctx context.Context, value workmodel.Approval) error {
	if value.Status != workmodel.ApprovalPending {
		return errors.New("decision write failed")
	}
	return s.memoryStore.UpsertApproval(ctx, value)
}

func (s *approvalOrderStore) UpsertApproval(ctx context.Context, value workmodel.Approval) error {
	if value.Status != workmodel.ApprovalPending {
		s.record("persist:" + string(value.Status))
	}
	return s.memoryStore.UpsertApproval(ctx, value)
}

type approvalProvider struct {
	resolve func(context.Context, provider.ApprovalDecision) error
}

func (*approvalProvider) Name() workmodel.Provider { return workmodel.CodexSubscription }
func (*approvalProvider) Start(context.Context, provider.StartRequest) (provider.Session, <-chan provider.Event, error) {
	return provider.Session{}, nil, errors.New("unexpected start")
}
func (*approvalProvider) Resume(context.Context, provider.ResumeRequest) (provider.Session, <-chan provider.Event, error) {
	return provider.Session{}, nil, errors.New("unexpected resume")
}
func (*approvalProvider) Steer(context.Context, provider.Session, provider.Input) error { return nil }
func (*approvalProvider) Interrupt(context.Context, provider.Session) error             { return nil }
func (p *approvalProvider) ResolveApproval(ctx context.Context, decision provider.ApprovalDecision) error {
	return p.resolve(ctx, decision)
}
func (*approvalProvider) Usage(context.Context) (provider.Usage, error) { return provider.Usage{}, nil }
func (*approvalProvider) AuthStatus(context.Context) (provider.AuthStatus, error) {
	return provider.AuthStatus{}, nil
}

type approvalBlockingProvider struct {
	started chan struct{}
	events  chan provider.Event
	prelude string
	summary string
}

func newApprovalBlockingProvider() *approvalBlockingProvider {
	return &approvalBlockingProvider{started: make(chan struct{}), events: make(chan provider.Event, 2), summary: "run command"}
}
func (*approvalBlockingProvider) Name() workmodel.Provider { return workmodel.CodexSubscription }
func (p *approvalBlockingProvider) Start(_ context.Context, request provider.StartRequest) (provider.Session, <-chan provider.Event, error) {
	close(p.started)
	if p.prelude != "" {
		p.events <- provider.Event{ID: provider.MustID("assistant-event"), Type: provider.EventAssistantMessage, Message: p.prelude, Tool: p.prelude}
	}
	p.events <- provider.Event{ID: provider.MustID("approval-event"), RequestID: provider.MustID("appr-req"), Type: provider.EventApprovalRequired, Message: p.summary}
	return provider.Session{ID: provider.MustID("session-approval"), TaskID: request.TaskID, ExternalID: "session-approval", Provider: workmodel.CodexSubscription}, p.events, nil
}
func (*approvalBlockingProvider) Resume(context.Context, provider.ResumeRequest) (provider.Session, <-chan provider.Event, error) {
	return provider.Session{}, nil, errors.New("unexpected resume")
}
func (*approvalBlockingProvider) Steer(context.Context, provider.Session, provider.Input) error {
	return nil
}
func (p *approvalBlockingProvider) Interrupt(context.Context, provider.Session) error {
	select {
	case <-p.events:
	default:
	}
	return nil
}
func (*approvalBlockingProvider) ResolveApproval(context.Context, provider.ApprovalDecision) error {
	return nil
}
func (*approvalBlockingProvider) Usage(context.Context) (provider.Usage, error) {
	return provider.Usage{}, nil
}
func (*approvalBlockingProvider) AuthStatus(context.Context) (provider.AuthStatus, error) {
	return provider.AuthStatus{}, nil
}

type stateInspectingMessenger struct {
	next    telegram.Messenger
	inspect func()
}

func (m *stateInspectingMessenger) Send(ctx context.Context, value telegram.Message) (telegram.MessageRef, error) {
	if len(value.InlineKeyboard) > 0 {
		m.inspect()
	}
	return m.next.Send(ctx, value)
}
func (m *stateInspectingMessenger) Edit(ctx context.Context, ref telegram.MessageRef, value telegram.Message) error {
	return m.next.Edit(ctx, ref, value)
}
func (m *stateInspectingMessenger) AnswerCallback(ctx context.Context, id, text string) error {
	return m.next.AnswerCallback(ctx, id, text)
}
func (m *stateInspectingMessenger) SendDocument(ctx context.Context, document telegram.Document) error {
	return m.next.SendDocument(ctx, document)
}

func (s *memoryStore) approval(id string) workmodel.Approval {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.approvals[id]
}
