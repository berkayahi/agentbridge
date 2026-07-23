package approval

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/store"
	"github.com/berkayahi/agentbridge/internal/telegram"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

func TestBrokerPersistsAndSendsBeforeResolvingApproval(t *testing.T) {
	now := time.Unix(1_000, 0).UTC()
	store := &recordingStore{}
	messenger := newRecordingMessenger(store)
	signer := telegram.NewCallbackSigner([]byte("a sufficiently long signing secret"), func() time.Time { return now })
	broker, err := New(Config{
		Store: store, Messenger: messenger, Signer: signer,
		Clock: func() time.Time { return now }, NewID: func() string { return "approve-1" },
		AuthorizeUser: func(userID string) bool { return userID == "42" },
	})
	if err != nil {
		t.Fatal(err)
	}

	resultCh := make(chan requestResult, 1)
	go func() {
		result, err := broker.Request(context.Background(), Request{
			TaskID: "task-1", ChatID: 100, ProviderRequestID: "claude-request-9",
			Kind: "shell", Summary: "run tests with github_pat_abcdefghijklmnopqrstuvwxyz",
		})
		resultCh <- requestResult{result: result, err: err}
	}()

	message := messenger.next(t)
	if got := store.operations(); len(got) < 2 || got[0] != "persist:pending" || got[1] != "send" {
		t.Fatalf("operation order = %v, want persistence before send", got)
	}
	if message.ChatID != 100 || len(message.InlineKeyboard) != 1 || len(message.InlineKeyboard[0]) != 2 {
		t.Fatalf("message = %#v", message)
	}
	if contains(message.Text, "github_pat_") || !contains(message.Text, "[REDACTED:github-token]") {
		t.Fatalf("message was not redacted: %q", message.Text)
	}
	approve, err := signer.Verify(message.InlineKeyboard[0][0].CallbackData)
	if err != nil {
		t.Fatal(err)
	}
	if approve.TaskID != "task-1" || approve.ApprovalID != "approve-1" || approve.Action != "approve" {
		t.Fatalf("callback = %#v", approve)
	}

	if err := broker.HandleDecision(context.Background(), "task-1", "approve-1", "42", true); err != nil {
		t.Fatal(err)
	}
	completed := <-resultCh
	if completed.err != nil || !completed.result.Approved || completed.result.Reason != "approved by Telegram operator" {
		t.Fatalf("result = %#v, error = %v", completed.result, completed.err)
	}
	record := store.latest()
	if record.Status != workmodel.ApprovalApproved || record.ResolvedAt == nil || record.ResolvedAt.UTC() != now {
		t.Fatalf("approval = %#v", record)
	}
	if contains(string(record.RequestPayload), "github_pat_") || contains(string(record.DecisionPayload), "github_pat_") {
		t.Fatalf("secret reached durable payload: request=%s decision=%s", record.RequestPayload, record.DecisionPayload)
	}
	var payload struct {
		ProviderRequestID string `json:"provider_request_id"`
		Summary           string `json:"summary"`
	}
	if err := json.Unmarshal(record.RequestPayload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ProviderRequestID != "claude-request-9" || !contains(payload.Summary, "[REDACTED:github-token]") {
		t.Fatalf("request payload = %#v", payload)
	}
}

func TestBrokerRejectsMismatchesUnauthorizedUsersAndReplays(t *testing.T) {
	store := &recordingStore{}
	messenger := newRecordingMessenger(store)
	broker := mustBroker(t, Config{
		Store: store, Messenger: messenger, NewID: func() string { return "approve-2" },
		AuthorizeUser: func(userID string) bool { return userID == "42" },
	})
	resultCh := make(chan requestResult, 1)
	go func() {
		result, err := broker.Request(context.Background(), Request{TaskID: "task-2", ChatID: 100, ProviderRequestID: "provider-2", Kind: "write", Summary: "edit file"})
		resultCh <- requestResult{result: result, err: err}
	}()
	messenger.next(t)

	if err := broker.HandleDecision(context.Background(), "other-task", "approve-2", "42", true); !errors.Is(err, ErrMismatch) {
		t.Fatalf("wrong task error = %v", err)
	}
	if err := broker.HandleDecision(context.Background(), "task-2", "approve-2", "7", true); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong user error = %v", err)
	}
	if err := broker.HandleDecision(context.Background(), "task-2", "approve-2", "42", false); err != nil {
		t.Fatal(err)
	}
	completed := <-resultCh
	if completed.err != nil || completed.result.Approved || completed.result.Reason != "rejected by Telegram operator" {
		t.Fatalf("result = %#v, error = %v", completed.result, completed.err)
	}
	if err := broker.HandleDecision(context.Background(), "task-2", "approve-2", "42", true); !errors.Is(err, ErrNotPending) {
		t.Fatalf("replay error = %v", err)
	}
}

func TestBrokerDoesNotReleaseBeforeDurableDecisionEvent(t *testing.T) {
	store := &recordingStore{eventErr: errors.New("event write failed")}
	messenger := newRecordingMessenger(store)
	broker := mustBroker(t, Config{
		Store: store, Messenger: messenger, NewID: func() string { return "approve-5" },
		AuthorizeUser: func(userID string) bool { return userID == "42" },
	})
	resultCh := make(chan requestResult, 1)
	go func() {
		result, err := broker.Request(context.Background(), Request{
			TaskID: "task-event", ChatID: 100, ProviderRequestID: "provider-event",
			Kind: "write", Summary: "edit file",
		})
		resultCh <- requestResult{result: result, err: err}
	}()
	messenger.next(t)

	if err := broker.HandleDecision(context.Background(), "task-event", "approve-5", "42", true); err == nil {
		t.Fatal("decision succeeded without durable resolution event")
	}
	select {
	case result := <-resultCh:
		t.Fatalf("provider released before event persistence: %#v", result)
	case <-time.After(20 * time.Millisecond):
	}

	store.setEventError(nil)
	if err := broker.HandleDecision(context.Background(), "task-event", "approve-5", "42", true); err != nil {
		t.Fatal(err)
	}
	completed := <-resultCh
	if completed.err != nil || !completed.result.Approved {
		t.Fatalf("result = %#v, error = %v", completed.result, completed.err)
	}
}

func TestBrokerRetriesAfterAmbiguousDecisionEventWrite(t *testing.T) {
	store := &recordingStore{eventErrAfterAppend: errors.New("ambiguous event write")}
	messenger := newRecordingMessenger(store)
	broker := mustBroker(t, Config{
		Store: store, Messenger: messenger, NewID: func() string { return "approve-6" },
		AuthorizeUser: func(userID string) bool { return userID == "42" },
	})
	resultCh := make(chan requestResult, 1)
	go func() {
		result, err := broker.Request(context.Background(), Request{
			TaskID: "task-6", ChatID: 100, ProviderRequestID: "provider-ambiguous",
			Kind: "write", Summary: "edit file",
		})
		resultCh <- requestResult{result: result, err: err}
	}()
	select {
	case <-messenger.sent:
	case result := <-resultCh:
		t.Fatalf("request failed before sending approval: result=%#v error=%v", result.result, result.err)
	case <-time.After(time.Second):
		t.Fatal("Telegram message was not sent")
	}

	if err := broker.HandleDecision(context.Background(), "task-6", "approve-6", "42", true); err == nil {
		t.Fatal("ambiguous event write unexpectedly succeeded")
	}
	select {
	case result := <-resultCh:
		t.Fatalf("provider released after ambiguous event write: %#v", result)
	case <-time.After(20 * time.Millisecond):
	}
	if err := broker.HandleDecision(context.Background(), "task-6", "approve-6", "42", true); err != nil {
		t.Fatalf("idempotent retry failed: %v", err)
	}
	completed := <-resultCh
	if completed.err != nil || !completed.result.Approved {
		t.Fatalf("result = %#v, error = %v", completed.result, completed.err)
	}
}

func TestBrokerDecisionClaimWinsConcurrentTimeout(t *testing.T) {
	decisionStarted := make(chan struct{})
	decisionRelease := make(chan struct{})
	store := &recordingStore{
		decisionStarted: decisionStarted,
		decisionRelease: decisionRelease,
	}
	messenger := newRecordingMessenger(store)
	broker := mustBroker(t, Config{
		Store: store, Messenger: messenger, NewID: func() string { return "approve-7" },
		Timeout:       20 * time.Millisecond,
		AuthorizeUser: func(userID string) bool { return userID == "42" },
	})
	resultCh := make(chan requestResult, 1)
	go func() {
		result, err := broker.Request(context.Background(), Request{
			TaskID: "task-7", ChatID: 100, ProviderRequestID: "provider-race",
			Kind: "write", Summary: "edit file",
		})
		resultCh <- requestResult{result: result, err: err}
	}()
	messenger.next(t)

	decisionErrCh := make(chan error, 1)
	go func() {
		decisionErrCh <- broker.HandleDecision(context.Background(), "task-7", "approve-7", "42", true)
	}()
	<-decisionStarted
	time.Sleep(30 * time.Millisecond)
	close(decisionRelease)

	if err := <-decisionErrCh; err != nil {
		t.Fatalf("claimed decision lost to timeout: %v", err)
	}
	completed := <-resultCh
	if completed.err != nil || !completed.result.Approved {
		t.Fatalf("result = %#v, error = %v", completed.result, completed.err)
	}
	for _, record := range store.all() {
		if record.Status == workmodel.ApprovalExpired {
			t.Fatalf("claimed approval was persisted expired: %#v", record)
		}
	}
}

func TestBrokerTimeoutDeniesAndExpiresApproval(t *testing.T) {
	store := &recordingStore{}
	messenger := newRecordingMessenger(store)
	broker := mustBroker(t, Config{Store: store, Messenger: messenger, NewID: func() string { return "approve-3" }, Timeout: 20 * time.Millisecond})

	result, err := broker.Request(context.Background(), Request{TaskID: "task-3", ChatID: 100, ProviderRequestID: "provider-3", Kind: "network", Summary: "open connection"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Approved || result.Reason != "approval timed out" {
		t.Fatalf("result = %#v", result)
	}
	if got := store.latest().Status; got != workmodel.ApprovalExpired {
		t.Fatalf("status = %q, want %q", got, workmodel.ApprovalExpired)
	}
	if err := broker.HandleDecision(context.Background(), "task-3", "approve-3", "42", true); !errors.Is(err, ErrNotPending) {
		t.Fatalf("late decision error = %v", err)
	}
}

func TestBrokerCancellationDeniesAndInvalidRequestsNeverPersist(t *testing.T) {
	store := &recordingStore{}
	messenger := newRecordingMessenger(store)
	broker := mustBroker(t, Config{Store: store, Messenger: messenger, NewID: func() string { return "approve-4" }, Timeout: time.Minute})

	if _, err := broker.Request(context.Background(), Request{TaskID: "", ChatID: 100, ProviderRequestID: "provider", Kind: "write", Summary: "edit"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("invalid request error = %v", err)
	}
	if got := len(store.all()); got != 0 {
		t.Fatalf("invalid request persisted %d records", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan requestResult, 1)
	go func() {
		result, err := broker.Request(ctx, Request{TaskID: "task-4", ChatID: 100, ProviderRequestID: "provider-4", Kind: "write", Summary: "edit"})
		resultCh <- requestResult{result: result, err: err}
	}()
	messenger.next(t)
	cancel()
	completed := <-resultCh
	if completed.err != nil || completed.result.Approved || completed.result.Reason != "approval canceled" {
		t.Fatalf("result = %#v, error = %v", completed.result, completed.err)
	}
	if got := store.latest().Status; got != workmodel.ApprovalExpired {
		t.Fatalf("status = %q, want %q", got, workmodel.ApprovalExpired)
	}
}

type requestResult struct {
	result Result
	err    error
}

type recordingStore struct {
	mu       sync.Mutex
	records  []workmodel.Approval
	events   []workmodel.Event
	ops      []string
	eventErr error

	eventErrAfterAppend error
	decisionStarted     chan struct{}
	decisionRelease     chan struct{}
	decisionOnce        sync.Once
}

func (s *recordingStore) UpsertApproval(_ context.Context, value workmodel.Approval) error {
	if value.Status == workmodel.ApprovalApproved || value.Status == workmodel.ApprovalRejected {
		if s.decisionStarted != nil {
			s.decisionOnce.Do(func() { close(s.decisionStarted) })
		}
		if s.decisionRelease != nil {
			<-s.decisionRelease
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, value)
	s.ops = append(s.ops, "persist:"+string(value.Status))
	return nil
}

func (s *recordingStore) AppendEvent(_ context.Context, value workmodel.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.eventErr != nil {
		return s.eventErr
	}
	for _, existing := range s.events {
		if existing.ID == value.ID {
			return store.ErrDuplicateEvent
		}
	}
	s.events = append(s.events, value)
	s.ops = append(s.ops, "event:"+string(value.Type))
	if s.eventErrAfterAppend != nil {
		err := s.eventErrAfterAppend
		s.eventErrAfterAppend = nil
		return err
	}
	return nil
}

func (s *recordingStore) Events(_ context.Context, taskID string) ([]workmodel.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var values []workmodel.Event
	for _, value := range s.events {
		if value.TaskID == taskID {
			values = append(values, value)
		}
	}
	return values, nil
}

func (s *recordingStore) setEventError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eventErr = err
}

func (s *recordingStore) latest() workmodel.Approval {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.records[len(s.records)-1]
}

func (s *recordingStore) all() []workmodel.Approval {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]workmodel.Approval(nil), s.records...)
}

func (s *recordingStore) operations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ops...)
}

type recordingMessenger struct {
	store *recordingStore
	sent  chan telegram.Message
}

func newRecordingMessenger(store *recordingStore) *recordingMessenger {
	return &recordingMessenger{store: store, sent: make(chan telegram.Message, 4)}
}

func (m *recordingMessenger) Send(_ context.Context, message telegram.Message) (telegram.MessageRef, error) {
	m.store.mu.Lock()
	m.store.ops = append(m.store.ops, "send")
	m.store.mu.Unlock()
	m.sent <- message
	return telegram.MessageRef{ChatID: message.ChatID, MessageID: 1}, nil
}

func (m *recordingMessenger) next(t *testing.T) telegram.Message {
	t.Helper()
	select {
	case message := <-m.sent:
		return message
	case <-time.After(time.Second):
		t.Fatal("Telegram message was not sent")
		return telegram.Message{}
	}
}

func (m *recordingMessenger) Edit(context.Context, telegram.MessageRef, telegram.Message) error {
	return nil
}
func (m *recordingMessenger) AnswerCallback(context.Context, string, string) error  { return nil }
func (m *recordingMessenger) SendDocument(context.Context, telegram.Document) error { return nil }

func mustBroker(t *testing.T, config Config) *Broker {
	t.Helper()
	if config.Signer == nil {
		config.Signer = telegram.NewCallbackSigner([]byte("a sufficiently long signing secret"), time.Now)
	}
	if config.AuthorizeUser == nil {
		config.AuthorizeUser = func(userID string) bool { return userID == "42" }
	}
	broker, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return broker
}

func contains(value, part string) bool {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return true
		}
	}
	return false
}
