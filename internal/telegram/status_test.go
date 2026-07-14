package telegram

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/task"
)

func TestStatusProjectorSendsOnceThenThrottlesCoalescesAndSuppressesUnchanged(t *testing.T) {
	now := time.Unix(1_000, 0)
	messenger := &recordingMessenger{}
	p := NewStatusProjector(messenger, time.Minute, func() time.Time { return now })
	status := TaskStatus{TaskID: "task-1", ChatID: 100, State: task.Running, CurrentAction: "testing", StartedAt: now.Add(-65 * time.Second), RepoProfile: "sample"}
	if err := p.Project(context.Background(), status); err != nil {
		t.Fatal(err)
	}
	if err := p.Project(context.Background(), status); err != nil {
		t.Fatal(err)
	}
	status.CurrentAction = "building"
	if err := p.Project(context.Background(), status); err != nil {
		t.Fatal(err)
	}
	if len(messenger.sent) != 1 || len(messenger.edited) != 0 {
		t.Fatalf("sent=%d edited=%d", len(messenger.sent), len(messenger.edited))
	}
	now = now.Add(time.Minute)
	if err := p.Flush(context.Background(), "task-1"); err != nil {
		t.Fatal(err)
	}
	if len(messenger.edited) != 1 || !strings.Contains(messenger.edited[0].Text, "building") {
		t.Fatalf("edits=%#v", messenger.edited)
	}
}

func TestStatusProjectorDeliversImportantAndFinalTransitionsImmediately(t *testing.T) {
	now := time.Unix(1_000, 0)
	messenger := &recordingMessenger{}
	p := NewStatusProjector(messenger, time.Hour, func() time.Time { return now })
	base := TaskStatus{TaskID: "task-1", ChatID: 100, State: task.Running, StartedAt: now.Add(-time.Second), RepoProfile: "sample"}
	if err := p.Project(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	base.State = task.AwaitingApproval
	base.Important = true
	if err := p.Project(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	base.State = task.Completed
	base.Important = false
	base.DeliveryRef = "refs/heads/staging"
	if err := p.Project(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	if len(messenger.edited) != 2 || !strings.Contains(messenger.edited[1].Text, "refs/heads/staging") {
		t.Fatalf("edits=%#v", messenger.edited)
	}
}

func TestCallbackSignerRoundTripExpiryTamperAndSize(t *testing.T) {
	now := time.Unix(2_000, 0)
	signer := NewCallbackSigner([]byte("a sufficiently long signing secret"), func() time.Time { return now })
	token, err := signer.Sign(CallbackAction{Action: "approve", TaskID: "t42", ApprovalID: "a9"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(token) > 64 || strings.ContainsAny(token, "/ ") || strings.Contains(token, "approve") {
		t.Fatalf("unsafe callback token %q", token)
	}
	got, err := signer.Verify(token)
	if err != nil || got.Action != "approve" || got.TaskID != "t42" || got.ApprovalID != "a9" {
		t.Fatalf("got=%#v err=%v", got, err)
	}
	if _, err := signer.Verify(token[:len(token)-1] + "x"); err == nil {
		t.Fatal("tamper accepted")
	}
	now = now.Add(2 * time.Minute)
	if _, err := signer.Verify(token); err == nil {
		t.Fatal("expired accepted")
	}
}

func TestApprovalKeyboardSignsApproveAndRejectCallbacks(t *testing.T) {
	now := time.Unix(2_000, 0)
	signer := NewCallbackSigner([]byte("a sufficiently long signing secret"), func() time.Time { return now })
	keyboard, err := ApprovalKeyboard(signer, "t42", "a9", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(keyboard) != 1 || len(keyboard[0]) != 2 {
		t.Fatalf("keyboard = %#v", keyboard)
	}
	for index, want := range []string{"approve", "reject"} {
		action, err := signer.Verify(keyboard[0][index].CallbackData)
		if err != nil {
			t.Fatal(err)
		}
		if action.Action != want || action.TaskID != "t42" || action.ApprovalID != "a9" {
			t.Fatalf("action = %#v", action)
		}
	}
}

func TestStatusProjectorDoesNotBlockOtherTasksOnSlowNetwork(t *testing.T) {
	messenger := &blockingMessenger{started: make(chan struct{}), release: make(chan struct{})}
	p := NewStatusProjector(messenger, time.Minute, time.Now)
	doneFirst := make(chan struct{})
	go func() {
		_ = p.Project(context.Background(), TaskStatus{TaskID: "slow", ChatID: 1, State: task.Running, RepoProfile: "one"})
		close(doneFirst)
	}()
	<-messenger.started
	doneSecond := make(chan struct{})
	go func() {
		_ = p.Project(context.Background(), TaskStatus{TaskID: "fast", ChatID: 2, State: task.Running, RepoProfile: "two"})
		close(doneSecond)
	}()
	select {
	case <-doneSecond:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("independent task blocked by slow network call")
	}
	close(messenger.release)
	<-doneFirst
}

type recordingMessenger struct {
	mu     sync.Mutex
	sent   []Message
	edited []Message
	next   int64
}

func (m *recordingMessenger) Send(ctx context.Context, msg Message) (MessageRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, msg)
	m.next++
	return MessageRef{ChatID: msg.ChatID, MessageID: m.next}, nil
}
func (m *recordingMessenger) Edit(ctx context.Context, ref MessageRef, msg Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.edited = append(m.edited, msg)
	return nil
}
func (*recordingMessenger) AnswerCallback(context.Context, string, string) error { return nil }
func (*recordingMessenger) SendDocument(context.Context, Document) error         { return nil }

type blockingMessenger struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (m *blockingMessenger) Send(ctx context.Context, msg Message) (MessageRef, error) {
	if msg.ChatID == 1 {
		m.once.Do(func() { close(m.started) })
		select {
		case <-m.release:
		case <-ctx.Done():
			return MessageRef{}, ctx.Err()
		}
	}
	return MessageRef{ChatID: msg.ChatID, MessageID: 1}, nil
}
func (*blockingMessenger) Edit(context.Context, MessageRef, Message) error      { return nil }
func (*blockingMessenger) AnswerCallback(context.Context, string, string) error { return nil }
func (*blockingMessenger) SendDocument(context.Context, Document) error         { return nil }
