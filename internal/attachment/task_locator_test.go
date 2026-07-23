package attachment

import (
	"context"
	"errors"
	"testing"

	"github.com/berkayahi/agentbridge/internal/store"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

func TestStoreTaskLocatorResolvesExplicitCaptionAndStatusReplyWithinChat(t *testing.T) {
	lookup := &fakeTaskLookup{tasks: []workmodel.Task{
		{ID: "task-1", TelegramChatID: 100, TelegramMessageID: 77, State: workmodel.Running},
		{ID: "task-2", TelegramChatID: 200, TelegramMessageID: 77, State: workmodel.Running},
	}}
	locator := NewStoreTaskLocator(lookup)

	got, err := locator.TaskForCaption(context.Background(), 100, "Please inspect #task-1")
	if err != nil || got != "task-1" {
		t.Fatalf("caption task = %q, %v", got, err)
	}
	got, err = locator.TaskForStatusMessage(context.Background(), 100, 77)
	if err != nil || got != "task-1" {
		t.Fatalf("status task = %q, %v", got, err)
	}
	got, err = locator.TaskForCaption(context.Background(), 100, "ordinary caption")
	if err != nil || got != "" {
		t.Fatalf("ordinary caption task = %q, %v", got, err)
	}
}

func TestStoreTaskLocatorListsOnlyWorkflowActiveTasksInChat(t *testing.T) {
	lookup := &fakeTaskLookup{tasks: []workmodel.Task{
		{ID: "running", TelegramChatID: 100, State: workmodel.Running},
		{ID: "approval", TelegramChatID: 100, State: workmodel.AwaitingApproval},
		{ID: "paused", TelegramChatID: 100, State: workmodel.Paused},
		{ID: "failed", TelegramChatID: 100, State: workmodel.Failed},
		{ID: "other-chat", TelegramChatID: 200, State: workmodel.Running},
	}}
	got, err := NewStoreTaskLocator(lookup).ActiveTaskIDs(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "running" || got[1] != "approval" {
		t.Fatalf("active tasks = %v", got)
	}
}

func TestStoreTaskLocatorNeverCrossesChatBoundaryOrSilentlyFallsBackFromExplicitRef(t *testing.T) {
	lookup := &fakeTaskLookup{tasks: []workmodel.Task{{ID: "task-1", TelegramChatID: 200, State: workmodel.Running}}}
	locator := NewStoreTaskLocator(lookup)
	if _, err := locator.TaskForCaption(context.Background(), 100, "task:task-1"); !errors.Is(err, ErrTaskReference) {
		t.Fatalf("cross-chat caption error = %v", err)
	}
	if got, err := locator.TaskForStatusMessage(context.Background(), 100, 99); err != nil || got != "" {
		t.Fatalf("unknown status task = %q, %v", got, err)
	}
}

func TestStoreTaskLocatorPropagatesCanceledExplicitLookup(t *testing.T) {
	lookup := &fakeTaskLookup{taskErr: context.Canceled}
	_, err := NewStoreTaskLocator(lookup).TaskForCaption(context.Background(), 100, "#task-1")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled lookup error = %v", err)
	}
}

type fakeTaskLookup struct {
	tasks   []workmodel.Task
	taskErr error
}

func (f *fakeTaskLookup) Task(_ context.Context, id string) (workmodel.Task, error) {
	if f.taskErr != nil {
		return workmodel.Task{}, f.taskErr
	}
	for _, value := range f.tasks {
		if value.ID == id {
			return value, nil
		}
	}
	return workmodel.Task{}, store.ErrNotFound
}

func (f *fakeTaskLookup) NonterminalTasks(context.Context) ([]workmodel.Task, error) {
	return append([]workmodel.Task(nil), f.tasks...), nil
}
