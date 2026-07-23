package execution

import (
	"errors"
	"testing"
	"time"
)

func TestExecutionTransitionsAreOneWayAfterTerminalState(t *testing.T) {
	execution, err := New(NewInput{
		ID:                   "execution-1",
		LocalTaskID:          "task-1",
		SessionID:            "session-1",
		RuntimeID:            "codex",
		RepositoryID:         "repository-1",
		CommandID:            "command-1",
		FencingEpoch:         1,
		MaxTransientAttempts: 1,
		PolicySnapshot:       []byte(`{"retry":"bounded"}`),
		CreatedAt:            time.Unix(1_000, 0).UTC(),
		UpdatedAt:            time.Unix(1_000, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := execution.Transition(StateAccepted, time.Unix(1_001, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	running, err := accepted.Transition(StateRunning, time.Unix(1_002, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	canceling, err := running.Transition(StateCancelRequested, time.Unix(1_003, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	canceled, err := canceling.Transition(StateCanceled, time.Unix(1_004, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := canceled.Transition(StateRunning, time.Unix(1_005, 0).UTC()); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("terminal transition error = %v", err)
	}
}

func TestExecutionTransientRetryRequiresSafetyProofAndNewFence(t *testing.T) {
	execution := newExecution(t)
	if _, err := execution.TransientRetry(TransientRetryCommand{
		ID:           "command-2",
		EvidenceID:   "evidence-1",
		Basis:        RetryBasisNoUncertainExternalEffect,
		FencingEpoch: 2,
		IssuedAt:     time.Unix(1_001, 0).UTC(),
	}); !errors.Is(err, ErrUnsafeRetry) {
		t.Fatalf("queued retry error = %v", err)
	}
	accepted, err := execution.Transition(StateAccepted, time.Unix(1_001, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	execution, err = accepted.Transition(StateRunning, time.Unix(1_002, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := execution.TransientRetry(TransientRetryCommand{}); !errors.Is(err, ErrUnsafeRetry) {
		t.Fatalf("unsafe retry error = %v", err)
	}
	if _, err := execution.TransientRetry(TransientRetryCommand{
		ID:           "command-1",
		EvidenceID:   "evidence-1",
		Basis:        RetryBasisNoUncertainExternalEffect,
		FencingEpoch: 2,
		IssuedAt:     time.Unix(1_003, 0).UTC(),
	}); !errors.Is(err, ErrUnsafeRetry) {
		t.Fatalf("replayed command error = %v", err)
	}
	retried, err := execution.TransientRetry(TransientRetryCommand{
		ID:           "command-2",
		EvidenceID:   "evidence-1",
		Basis:        RetryBasisNoUncertainExternalEffect,
		FencingEpoch: 2,
		IssuedAt:     time.Unix(1_003, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if retried.Attempt() != 1 || retried.FencingEpoch() != 2 || retried.CommandID() != "command-2" || !retried.UpdatedAt().Equal(time.Unix(1_003, 0).UTC()) {
		t.Fatalf("retry = attempt %d, fence %d, command %q, updated %s", retried.Attempt(), retried.FencingEpoch(), retried.CommandID(), retried.UpdatedAt())
	}
	if _, err := retried.TransientRetry(TransientRetryCommand{
		ID:           "command-3",
		EvidenceID:   "evidence-2",
		Basis:        RetryBasisNoUncertainExternalEffect,
		FencingEpoch: 3,
		IssuedAt:     time.Unix(1_004, 0).UTC(),
	}); !errors.Is(err, ErrUnsafeRetry) {
		t.Fatalf("unbounded retry error = %v", err)
	}
}

func TestExecutionTerminalFailureCreatesSuccessorWithLineage(t *testing.T) {
	execution := newExecution(t)
	accepted, err := execution.Transition(StateAccepted, time.Unix(1_001, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	running, err := accepted.Transition(StateRunning, time.Unix(1_002, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	failed, err := running.Transition(StateFailed, time.Unix(1_003, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	successor, err := failed.Successor("execution-2", "command-2", 2, time.Unix(1_004, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if successor.RetryOfExecutionID() != failed.ID() || successor.Attempt() != 0 || successor.State() != StateQueued {
		t.Fatalf("successor = %#v", successor)
	}
	if _, err := failed.Successor("execution-early", "command-2", 2, time.Unix(1_002, 0).UTC()); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("early successor error = %v", err)
	}
	if _, err := execution.Successor("execution-3", "command-3", 2, time.Unix(1_004, 0).UTC()); !errors.Is(err, ErrNotTerminalFailure) {
		t.Fatalf("nonterminal successor error = %v", err)
	}
}

func FuzzExecutionTransition(f *testing.F) {
	f.Add(string(StateQueued), string(StateAccepted))
	f.Add(string(StateCanceled), string(StateRunning))
	f.Add("unknown", string(StateRunning))
	f.Fuzz(func(t *testing.T, fromText, toText string) {
		from, fromErr := ParseState(fromText)
		to, toErr := ParseState(toText)
		if fromErr != nil || toErr != nil {
			return
		}
		_ = CanTransition(from, to)
	})
}

func newExecution(t *testing.T) Execution {
	t.Helper()
	execution, err := New(NewInput{
		ID:                   "execution-1",
		LocalTaskID:          "task-1",
		SessionID:            "session-1",
		RuntimeID:            "codex",
		RepositoryID:         "repository-1",
		CommandID:            "command-1",
		FencingEpoch:         1,
		MaxTransientAttempts: 1,
		PolicySnapshot:       []byte(`{"retry":"bounded"}`),
		CreatedAt:            time.Unix(1_000, 0).UTC(),
		UpdatedAt:            time.Unix(1_000, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return execution
}
