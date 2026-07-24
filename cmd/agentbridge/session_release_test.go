package main

import (
	"context"
	"errors"
	"testing"

	"github.com/berkayahi/agentbridge/internal/kernel"
	bridgeRuntime "github.com/berkayahi/agentbridge/internal/runtime"
)

type countingSink struct {
	appended int
	err      error
}

func (s *countingSink) Append(context.Context, kernel.Event) error {
	s.appended++
	return s.err
}

// A finished bee's session must be released. Holding it leaked one entry per
// flight for the life of the process — the one thing that grows with use.
func TestSessionIsReleasedWhenTheTurnEnds(t *testing.T) {
	executor := &localRuntimeExecutor{sessions: map[string]bridgeRuntime.Session{
		"task-1": {ID: "session-1", TaskID: "task-1"},
		"task-2": {ID: "session-2", TaskID: "task-2"},
	}}
	if executor.HeldSessions() != 2 {
		t.Fatalf("held = %d", executor.HeldSessions())
	}
	inner := &countingSink{}
	sink := sessionReleasingSink{inner: inner, executor: executor, taskID: "task-1"}

	// Streaming observations must not release anything: she is still working.
	if err := sink.Append(context.Background(), kernel.Event{Type: "provider_assistant_message"}); err != nil {
		t.Fatal(err)
	}
	if executor.HeldSessions() != 2 {
		t.Fatalf("a streaming event released a session: held = %d", executor.HeldSessions())
	}

	if err := sink.Append(context.Background(), kernel.Event{Type: "provider_completed"}); err != nil {
		t.Fatal(err)
	}
	if executor.HeldSessions() != 1 {
		t.Fatalf("held after completion = %d, want 1", executor.HeldSessions())
	}
	if _, held := executor.sessions["task-1"]; held {
		t.Fatal("the finished task still holds a session")
	}
	if _, held := executor.sessions["task-2"]; !held {
		t.Fatal("another bee's session was released")
	}
	if inner.appended != 2 {
		t.Fatalf("evidence appended = %d, want 2", inner.appended)
	}
}

// An errored turn is also over, so it must not hold a session either.
func TestSessionIsReleasedWhenTheTurnErrors(t *testing.T) {
	executor := &localRuntimeExecutor{sessions: map[string]bridgeRuntime.Session{"task-1": {ID: "session-1"}}}
	sink := sessionReleasingSink{inner: &countingSink{}, executor: executor, taskID: "task-1"}
	if err := sink.Append(context.Background(), kernel.Event{Type: "provider_error"}); err != nil {
		t.Fatal(err)
	}
	if executor.HeldSessions() != 0 {
		t.Fatalf("held = %d, want 0", executor.HeldSessions())
	}
}

// Evidence comes first: a durable failure is reported, and the session is still
// released because the provider's turn really did end.
func TestSessionReleaseReportsADurableFailure(t *testing.T) {
	want := errors.New("durable append failed")
	executor := &localRuntimeExecutor{sessions: map[string]bridgeRuntime.Session{"task-1": {ID: "session-1"}}}
	sink := sessionReleasingSink{inner: &countingSink{err: want}, executor: executor, taskID: "task-1"}
	if err := sink.Append(context.Background(), kernel.Event{Type: "provider_completed"}); !errors.Is(err, want) {
		t.Fatalf("err = %v, want the durable failure", err)
	}
	if executor.HeldSessions() != 0 {
		t.Fatalf("held = %d, want 0", executor.HeldSessions())
	}
}
