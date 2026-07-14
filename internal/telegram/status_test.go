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
