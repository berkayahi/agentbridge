package gitoperation

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestOperationKeepsGitIntentImmutable(t *testing.T) {
	sha := strings.Repeat("a", 40)
	operation, err := New(NewInput{
		ID:             "git-operation-1",
		ExecutionID:    "execution-1",
		Kind:           KindPush,
		TargetRef:      "refs/heads/agentbridge/task-1",
		ExpectedOldSHA: sha,
		IdempotencyKey: "command-1",
		CreatedAt:      time.Unix(1_000, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	running, err := operation.Transition(StateRunning)
	if err != nil {
		t.Fatal(err)
	}
	if running.Kind() != KindPush || running.TargetRef() != operation.TargetRef() || running.ExpectedOldSHA() != sha || running.IdempotencyKey() != "command-1" {
		t.Fatalf("operation intent changed: %#v", running)
	}
	completed, err := running.Transition(StateSucceeded)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := completed.Transition(StateRunning); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("terminal transition error = %v", err)
	}
	for _, ref := range []string{"refs/heads/", "refs//heads/main", "refs/heads/main.lock", "refs/heads/main\nbad"} {
		if _, err := New(NewInput{
			ID:             "git-operation-2",
			ExecutionID:    "execution-1",
			Kind:           KindPush,
			TargetRef:      ref,
			ExpectedOldSHA: sha,
			IdempotencyKey: "command-2",
			CreatedAt:      time.Unix(1_000, 0).UTC(),
		}); err == nil {
			t.Fatalf("New accepted invalid ref %q", ref)
		}
	}
}
