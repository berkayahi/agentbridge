package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/approval"
	"github.com/berkayahi/agentbridge/internal/provider"
	"github.com/berkayahi/agentbridge/internal/task"
	"github.com/berkayahi/agentbridge/internal/telegram"
)

func TestStandaloneContract(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T)
	}{
		{name: "rejects commands before startup", run: func(t *testing.T) {
			fixture := newFixture(t, nil)
			_, err := fixture.app.CreateTask(context.Background(), CreateTaskRequest{
				Provider: task.ProviderCodex,
				Prompt:   "not started",
			})
			if !errors.Is(err, ErrNotStarted) {
				t.Fatalf("error = %v, want %v", err, ErrNotStarted)
			}
		}},

		{name: "create start verify commit push and resume", run: func(t *testing.T) {
			fixture := newFixture(t, []provider.Event{
				{ID: provider.MustID("standalone-message"), Type: provider.EventAssistantMessage, Message: "done"},
				{ID: provider.MustID("standalone-complete"), Type: provider.EventCompleted},
			})
			fixture.start(t)

			id, err := fixture.app.CreateTask(context.Background(), CreateTaskRequest{
				Provider: task.ProviderCodex,
				Prompt:   "change the public runtime",
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := fixture.wait(t, id); got.State != task.Completed {
				t.Fatalf("state = %s, want %s", got.State, task.Completed)
			}
			assertDurableEvents(t, fixture.store, id,
				task.EventTaskCreated,
				task.EventVerification,
				task.EventCommitCreated,
				task.EventPushCompleted,
			)

			if err := fixture.app.ContinueTask(context.Background(), id, "verify the completed change again"); err != nil {
				t.Fatal(err)
			}
			if got := fixture.wait(t, id); got.State != task.Completed {
				t.Fatalf("resumed state = %s, want %s", got.State, task.Completed)
			}
			assertDurableEvents(t, fixture.store, id, task.EventProviderMessage, task.EventPushCompleted)
		}},

		{name: "approval and cancellation", run: func(t *testing.T) {
			providerPort := newApprovalBlockingProvider()
			fixture := newFixtureWithProvider(t, providerPort)
			fixture.app.deps.Signer = telegram.NewCallbackSigner([]byte("01234567890123456789012345678901"), nil)
			fixture.start(t)

			id, err := fixture.app.CreateTask(context.Background(), CreateTaskRequest{
				Provider: task.ProviderCodex,
				Prompt:   "request approval",
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := fixture.wait(t, id); got.State != task.AwaitingApproval {
				t.Fatalf("state = %s, want %s", got.State, task.AwaitingApproval)
			}
			if err := fixture.app.DecideApproval(context.Background(), ApprovalDecisionRequest{
				TaskID: id, ApprovalID: "appr-req", UserID: "local-operator", Allow: true,
			}); err != nil {
				t.Fatal(err)
			}
			if err := fixture.app.CancelTask(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			if got := fixture.wait(t, id); got.State != task.Canceled {
				t.Fatalf("state = %s, want %s", got.State, task.Canceled)
			}
			assertDurableEvents(t, fixture.store, id,
				task.EventApprovalRequested,
				task.EventApprovalResolved,
				task.EventStateTransitioned,
			)
		}},

		{name: "does not require Telegram projection or signing", run: func(t *testing.T) {
			providerPort := newApprovalBlockingProvider()
			fixture := newFixtureWithProvider(t, providerPort)
			fixture.app.deps.Messenger = rejectingMessenger{}
			fixture.start(t)

			id, err := fixture.app.CreateTask(context.Background(), CreateTaskRequest{
				Provider: task.ProviderCodex,
				Prompt:   "standalone approval",
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := fixture.wait(t, id); got.State != task.AwaitingApproval {
				t.Fatalf("state = %s, want %s", got.State, task.AwaitingApproval)
			}
		}},

		{name: "routes native approval around Telegram broker authorization", run: func(t *testing.T) {
			providerPort := newTrackingApprovalProvider()
			fixture := newFixtureWithProvider(t, providerPort)
			signer := telegram.NewCallbackSigner([]byte("01234567890123456789012345678901"), nil)
			broker, err := approval.New(approval.Config{
				Store: fixture.store, Messenger: fixture.messenger, Signer: signer,
				AuthorizeUser: func(string) bool { return false },
			})
			if err != nil {
				t.Fatal(err)
			}
			fixture.app.deps.Approvals = broker
			fixture.start(t)

			id, err := fixture.app.CreateTask(context.Background(), CreateTaskRequest{
				Provider: task.ProviderCodex,
				Prompt:   "native approval with production broker",
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := fixture.wait(t, id); got.State != task.AwaitingApproval {
				t.Fatalf("state = %s, want %s", got.State, task.AwaitingApproval)
			}
			if err := fixture.app.DecideApproval(context.Background(), ApprovalDecisionRequest{
				TaskID: id, ApprovalID: "appr-req", UserID: "local-operator", Allow: true,
			}); err != nil {
				t.Fatal(err)
			}
			if providerPort.resolveCount() != 1 {
				t.Fatalf("provider releases = %d, want 1", providerPort.resolveCount())
			}
			if err := fixture.app.CancelTask(context.Background(), id); err != nil {
				t.Fatal(err)
			}
		}},

		{name: "routes broker-owned approval while provider is active", run: func(t *testing.T) {
			providerPort := newTrackingApprovalProvider()
			fixture := newFixtureWithProvider(t, providerPort)
			signer := telegram.NewCallbackSigner([]byte("01234567890123456789012345678901"), nil)
			broker, err := approval.New(approval.Config{
				Store: fixture.store, Messenger: fixture.messenger, Signer: signer,
				AuthorizeUser: func(userID string) bool { return userID == "42" },
				NewID:         func() string { return "broker-1" },
			})
			if err != nil {
				t.Fatal(err)
			}
			fixture.app.deps.Approvals = broker
			fixture.start(t)

			id, err := fixture.app.CreateTask(context.Background(), CreateTaskRequest{
				Provider: task.ProviderCodex,
				Prompt:   "provider and broker approval",
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := fixture.wait(t, id); got.State != task.AwaitingApproval {
				t.Fatalf("state = %s, want %s", got.State, task.AwaitingApproval)
			}

			requestCtx, cancelRequest := context.WithCancel(context.Background())
			defer cancelRequest()
			brokerResult := make(chan error, 1)
			go func() {
				_, requestErr := broker.Request(requestCtx, approval.Request{
					TaskID: id, ChatID: 100, ProviderRequestID: "mcp-request",
					Kind: "tool", Summary: "run tool",
				})
				brokerResult <- requestErr
			}()
			select {
			case requestErr := <-brokerResult:
				t.Fatalf("broker request ended before decision: %v", requestErr)
			case <-time.After(10 * time.Millisecond):
			}
			waitForApproval(t, fixture.store, "broker-1")
			if err := fixture.app.DecideApproval(context.Background(), ApprovalDecisionRequest{
				TaskID: id, ApprovalID: "broker-1", UserID: "42", Allow: true,
			}); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-brokerResult:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("broker approval was not released")
			}
			if providerPort.resolveCount() != 0 {
				t.Fatalf("native provider releases = %d, want 0", providerPort.resolveCount())
			}
			if err := fixture.app.CancelTask(context.Background(), id); err != nil {
				t.Fatal(err)
			}
		}},

		{name: "does not release provider before durable approval evidence", run: func(t *testing.T) {
			providerPort := newTrackingApprovalProvider()
			fixture := newFixtureWithProvider(t, providerPort)
			fixture.app.deps.Store = &approvalEventFailureStore{memoryStore: fixture.store}
			fixture.start(t)

			id, err := fixture.app.CreateTask(context.Background(), CreateTaskRequest{
				Provider: task.ProviderCodex,
				Prompt:   "approval evidence failure",
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := fixture.wait(t, id); got.State != task.AwaitingApproval {
				t.Fatalf("state = %s, want %s", got.State, task.AwaitingApproval)
			}
			err = fixture.app.DecideApproval(context.Background(), ApprovalDecisionRequest{
				TaskID: id, ApprovalID: "appr-req", UserID: "local-operator", Allow: true,
			})
			if err == nil {
				t.Fatal("approval succeeded without durable resolved event")
			}
			if providerPort.resolveCount() != 0 {
				t.Fatalf("provider releases = %d, want 0", providerPort.resolveCount())
			}
			if got, getErr := fixture.store.Task(context.Background(), id); getErr != nil || got.State != task.AwaitingApproval {
				t.Fatalf("task = %#v, error = %v", got, getErr)
			}
			if got := fixture.store.approval("appr-req"); got.Status != task.ApprovalPending {
				t.Fatalf("approval status = %s, want %s", got.Status, task.ApprovalPending)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, test.run)
	}
}

func waitForApproval(t *testing.T, values *memoryStore, id string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if values.approval(id).ID == id {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("approval %s was not persisted", id)
}

func assertDurableEvents(t *testing.T, values *memoryStore, id string, required ...task.EventType) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	events, err := values.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, eventType := range required {
		if !hasEvent(events, eventType) {
			t.Fatalf("task %s has no durable %s event: %#v", id, eventType, events)
		}
	}
}

type rejectingMessenger struct{}

func (rejectingMessenger) Send(context.Context, telegram.Message) (telegram.MessageRef, error) {
	return telegram.MessageRef{}, errors.New("Telegram unavailable")
}
func (rejectingMessenger) Edit(context.Context, telegram.MessageRef, telegram.Message) error {
	return errors.New("Telegram unavailable")
}
func (rejectingMessenger) AnswerCallback(context.Context, string, string) error {
	return errors.New("Telegram unavailable")
}
func (rejectingMessenger) SendDocument(context.Context, telegram.Document) error {
	return errors.New("Telegram unavailable")
}

type trackingApprovalProvider struct {
	*approvalBlockingProvider
	mu       sync.Mutex
	resolved int
}

func newTrackingApprovalProvider() *trackingApprovalProvider {
	return &trackingApprovalProvider{approvalBlockingProvider: newApprovalBlockingProvider()}
}

func (p *trackingApprovalProvider) ResolveApproval(context.Context, provider.ApprovalDecision) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resolved++
	return nil
}

func (p *trackingApprovalProvider) resolveCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.resolved
}

type approvalEventFailureStore struct {
	*memoryStore
}

func (s *approvalEventFailureStore) AppendEvent(ctx context.Context, value task.Event) error {
	if value.Type == task.EventApprovalResolved {
		return errors.New("approval event write failed")
	}
	return s.memoryStore.AppendEvent(ctx, value)
}
