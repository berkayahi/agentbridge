package provider_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/provider"
	"github.com/berkayahi/agentbridge/internal/provider/fake"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

func TestFakeImplementsProviderOperationsAndPreservesEventOrder(t *testing.T) {
	taskID := provider.MustID("task-1")
	sessionID := provider.MustID("session-1")
	script := []provider.Event{
		{Type: provider.EventAssistantMessage, Message: "working"},
		{Type: provider.EventHeartbeat},
		{Type: provider.EventCompleted},
	}
	p := fake.New(workmodel.CodexSubscription, sessionID, script)
	input := provider.Input{Text: "inspect the repository"}

	session, events, err := p.Start(context.Background(), provider.StartRequest{TaskID: taskID, Input: input})
	if err != nil {
		t.Fatal(err)
	}
	if session.ID != sessionID || session.TaskID != taskID {
		t.Fatalf("session = %#v", session)
	}
	var got []provider.EventType
	for event := range events {
		got = append(got, event.Type)
	}
	want := []provider.EventType{provider.EventAssistantMessage, provider.EventHeartbeat, provider.EventCompleted}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("event order = %v, want %v", got, want)
	}

	_, _, err = p.Resume(context.Background(), provider.ResumeRequest{TaskID: taskID, Session: session, Input: input})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Steer(context.Background(), session, input); err != nil {
		t.Fatal(err)
	}
	if err := p.Interrupt(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	decision := provider.ApprovalDecision{RequestID: provider.MustID("approval-1"), TaskID: taskID, Allow: false}
	if err := p.ResolveApproval(context.Background(), decision); err != nil {
		t.Fatal(err)
	}
	usage, err := p.Usage(context.Background())
	if err != nil || usage.Provider != workmodel.CodexSubscription {
		t.Fatalf("usage = %#v, err = %v", usage, err)
	}
	auth, err := p.AuthStatus(context.Background())
	if err != nil || !auth.Authenticated {
		t.Fatalf("auth = %#v, err = %v", auth, err)
	}

	wantCalls := []string{"start", "resume", "steer", "interrupt", "resolve_approval", "usage", "auth_status"}
	if got := p.Calls(); !reflect.DeepEqual(got, wantCalls) {
		t.Fatalf("calls = %v, want %v", got, wantCalls)
	}
}

func TestProviderOperationsHonorCanceledContext(t *testing.T) {
	p := fake.New(workmodel.ClaudeSubscription, provider.MustID("session-1"), nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := p.Start(ctx, provider.StartRequest{TaskID: provider.MustID("task-1"), Input: provider.Input{Text: "hello"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Start error = %v, want context.Canceled", err)
	}
	if err := p.Interrupt(ctx, provider.Session{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Interrupt error = %v, want context.Canceled", err)
	}
}

func TestInputValidatesLocalAttachments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "screen shot.png")
	if err := os.WriteFile(path, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	attachment, err := provider.NewLocalAttachment(path, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	input := provider.Input{Attachments: []provider.LocalAttachment{attachment}}
	if err := input.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if _, err := provider.NewLocalAttachment("relative.png", "image/png"); err == nil {
		t.Fatal("relative attachment accepted")
	}
}

func TestEventContractContainsOnlyObservableFields(t *testing.T) {
	event := provider.Event{
		ID:        provider.MustID("event-1"),
		TaskID:    provider.MustID("task-1"),
		Type:      provider.EventToolStarted,
		Message:   "read file",
		Tool:      "read_file",
		CreatedAt: time.Unix(1, 0).UTC(),
	}
	typeOfEvent := reflect.TypeOf(event)
	for _, forbidden := range []string{"Reasoning", "Thinking", "ChainOfThought"} {
		if _, ok := typeOfEvent.FieldByName(forbidden); ok {
			t.Fatalf("event exposes hidden reasoning field %q", forbidden)
		}
	}
}

func TestIDMarshalsAsItsOpaqueString(t *testing.T) {
	data, err := json.Marshal(provider.MustID("task-1"))
	if err != nil || string(data) != `"task-1"` {
		t.Fatalf("Marshal = %s, %v", data, err)
	}
}

func TestLargeScreenshotDoesNotCountAgainstTextLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.png")
	if err := os.WriteFile(path, make([]byte, 2<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	attachment, err := provider.NewLocalAttachment(path, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if err := (provider.Input{Text: "inspect", Attachments: []provider.LocalAttachment{attachment}}).Validate(); err != nil {
		t.Fatalf("large screenshot rejected: %v", err)
	}
}

func TestFakeRejectsScriptLargerThanItsBound(t *testing.T) {
	events := make([]provider.Event, 33)
	p := fake.New(workmodel.CodexSubscription, provider.MustID("session-1"), events)
	_, _, err := p.Start(context.Background(), provider.StartRequest{TaskID: provider.MustID("task-1"), Input: provider.Input{Text: "work"}})
	if !errors.Is(err, fake.ErrScriptTooLarge) {
		t.Fatalf("error = %v, want ErrScriptTooLarge", err)
	}
}
