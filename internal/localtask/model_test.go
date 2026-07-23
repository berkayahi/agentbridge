package localtask

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLocalTaskAllowsOnlyOneActiveExecution(t *testing.T) {
	task, err := New(NewInput{
		ID:        "task-1",
		Title:     "Repair the device",
		Prompt:    "Repair the device safely.",
		CreatedAt: time.Unix(1_000, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}

	active, err := task.BeginExecution("execution-1")
	if err != nil {
		t.Fatal(err)
	}
	if active.ActiveExecutionID() != "execution-1" {
		t.Fatalf("active execution = %q", active.ActiveExecutionID())
	}
	if _, err := active.BeginExecution("execution-2"); !errors.Is(err, ErrActiveExecution) {
		t.Fatalf("second active execution error = %v", err)
	}
	if _, err := active.FinishExecution("execution-2"); !errors.Is(err, ErrExecutionMismatch) {
		t.Fatalf("finish wrong execution error = %v", err)
	}
	finished, err := active.FinishExecution("execution-1")
	if err != nil {
		t.Fatal(err)
	}
	if finished.ActiveExecutionID() != "" {
		t.Fatalf("active execution after finish = %q", finished.ActiveExecutionID())
	}
	if _, err := finished.BeginExecution("execution-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := finished.FinishExecution(""); !errors.Is(err, ErrExecutionMismatch) {
		t.Fatalf("finish empty execution error = %v", err)
	}
}

func FuzzLocalTaskRejectsInvalidIdentifiers(f *testing.F) {
	f.Add("")
	f.Add(strings.Repeat("x", 129))
	f.Add("task-Δ")
	f.Fuzz(func(t *testing.T, id string) {
		_, err := New(NewInput{
			ID:        id,
			Title:     "Title",
			Prompt:    "Prompt",
			CreatedAt: time.Unix(1_000, 0).UTC(),
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
