package session

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSessionBindsOneActiveTask(t *testing.T) {
	session, err := New(NewInput{
		ID:           "session-1",
		RuntimeID:    "codex",
		RepositoryID: "repository-1",
		CreatedAt:    time.Unix(1_000, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	bound, err := session.BindTask("task-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bound.BindTask("task-2"); !errors.Is(err, ErrTaskAlreadyBound) {
		t.Fatalf("second task binding error = %v", err)
	}
	if _, err := bound.ReleaseTask("task-2"); !errors.Is(err, ErrTaskMismatch) {
		t.Fatalf("wrong task release error = %v", err)
	}
	released, err := bound.ReleaseTask("task-1")
	if err != nil {
		t.Fatal(err)
	}
	if released.ActiveTaskID() != "" {
		t.Fatalf("active task after release = %q", released.ActiveTaskID())
	}
	if _, err := released.ReleaseTask(""); !errors.Is(err, ErrTaskMismatch) {
		t.Fatalf("release empty task error = %v", err)
	}
}

func FuzzSessionRejectsInvalidIdentifiers(f *testing.F) {
	f.Add("")
	f.Add(strings.Repeat("x", 129))
	f.Add("session-Δ")
	f.Fuzz(func(t *testing.T, id string) {
		_, err := New(NewInput{
			ID:           id,
			RuntimeID:    "codex",
			RepositoryID: "repository-1",
			CreatedAt:    time.Unix(1_000, 0).UTC(),
		})
		if id == "" || len(id) > 128 || !asciiIdentifier(id) {
			if err == nil {
				t.Fatalf("New accepted invalid identifier %q", id)
			}
		}
	})
}

func asciiIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for _, runeValue := range value {
		if !((runeValue >= 'a' && runeValue <= 'z') ||
			(runeValue >= 'A' && runeValue <= 'Z') ||
			(runeValue >= '0' && runeValue <= '9') ||
			runeValue == '-' || runeValue == '_') {
			return false
		}
	}
	return true
}
