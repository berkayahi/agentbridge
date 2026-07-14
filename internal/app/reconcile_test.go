package app

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/task"
)

func TestReconcileQueuesOnlySafeResumptions(t *testing.T) {
	fixture := newFixture(t, nil)
	queued := seededTask("queued", task.Queued)
	running := seededTask("running", task.Running)
	running.WorktreePath, running.BaseSHA, running.ProviderSessionID = "/work/running", "base", "session"
	unsafe := seededTask("pushing", task.Pushing)
	unsafe.WorktreePath, unsafe.BaseSHA = "/work/pushing", "base"
	for _, value := range []task.Task{queued, running, unsafe} {
		if err := fixture.store.CreateTask(context.Background(), value, task.Event{ID: value.ID + "-created", TaskID: value.ID, Type: task.EventTaskCreated, Visibility: task.VisibilityUser, CreatedAt: value.CreatedAt}); err != nil {
			t.Fatal(err)
		}
	}
	fixture.store.sessions["session"] = task.Session{ID: "session", TaskID: running.ID, Provider: running.Provider, ProviderSessionID: "session", Resumable: true}
	fixture.workspace.result = Workspace{BaseSHA: "base", Path: "/work/running"}
	fixture.start(t)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		value, _ := fixture.store.Task(context.Background(), unsafe.ID)
		if value.State == task.Paused {
			if value.FailureReason == "" {
				t.Fatal("paused task has no durable explanation")
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("unsafe pushing task was not paused")
}

func TestReconcilePausesRunningTaskWhenWorkspaceInvariantChanged(t *testing.T) {
	fixture := newFixture(t, nil)
	value := seededTask("running", task.Running)
	value.WorktreePath, value.BaseSHA, value.ProviderSessionID = "/work/running", "base", "session"
	if err := fixture.store.CreateTask(context.Background(), value, task.Event{ID: "created", TaskID: value.ID, Type: task.EventTaskCreated, Visibility: task.VisibilityUser, CreatedAt: value.CreatedAt}); err != nil {
		t.Fatal(err)
	}
	fixture.store.sessions["session"] = task.Session{ID: "session", TaskID: value.ID, ProviderSessionID: "session", Resumable: true}
	fixture.workspace.result = Workspace{}
	fixture.start(t)
	got := fixture.wait(t, value.ID)
	if got.State != task.Paused || got.FailureReason == "" {
		t.Fatalf("task = %#v", got)
	}
}

func TestStartDoesNotDeadlockWhenReconciliationExceedsQueueCapacity(t *testing.T) {
	fixture := newFixture(t, nil)
	fixture.app.config.QueueSize = 1
	for index := range 8 {
		value := seededTask(fmt.Sprintf("queued-%d", index), task.Queued)
		if err := fixture.store.CreateTask(context.Background(), value, task.Event{ID: value.ID + "-created", TaskID: value.ID, Type: task.EventTaskCreated, Visibility: task.VisibilityUser, CreatedAt: value.CreatedAt}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- fixture.app.Start(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Start deadlocked while reconciliation filled the queue")
	}
	shutdown, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	_ = fixture.app.Shutdown(shutdown)
}

func TestStartRollsBackWorkersWhenReconciliationFails(t *testing.T) {
	fixture := newFixture(t, nil)
	fixture.store.expiredErr = errors.New("database unavailable")
	if err := fixture.app.Start(context.Background()); err == nil {
		t.Fatal("Start succeeded")
	}
	fixture.store.expiredErr = nil
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := fixture.app.Start(ctx); err != nil {
		t.Fatalf("Start after rollback: %v", err)
	}
	shutdown, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := fixture.app.Shutdown(shutdown); err != nil {
		t.Fatal(err)
	}
}

func seededTask(id string, state task.State) task.Task {
	at := time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC)
	return task.Task{ID: id, RepoProfileID: "sample", Title: id, Prompt: id, State: state, Provider: task.ProviderCodex, TelegramChatID: 100, CreatedAt: at, UpdatedAt: at}
}
