package standalone

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/workmodel"
)

func TestReconcileQueuesOnlySafeResumptions(t *testing.T) {
	fixture := newFixture(t, nil)
	queued := seededTask("queued", workmodel.Queued)
	running := seededTask("running", workmodel.Running)
	running.WorktreePath, running.BaseSHA, running.ProviderSessionID = "/work/running", "base", "session"
	unsafe := seededTask("pushing", workmodel.Pushing)
	unsafe.WorktreePath, unsafe.BaseSHA = "/work/pushing", "base"
	for _, value := range []workmodel.Task{queued, running, unsafe} {
		if err := fixture.store.CreateTask(context.Background(), value, workmodel.Event{ID: value.ID + "-created", TaskID: value.ID, Type: workmodel.EventTaskCreated, Visibility: workmodel.VisibilityUser, CreatedAt: value.CreatedAt}); err != nil {
			t.Fatal(err)
		}
	}
	fixture.store.sessions["session"] = workmodel.Session{ID: "session", TaskID: running.ID, Provider: running.Provider, ProviderSessionID: "session", Resumable: true}
	fixture.workspace.result = Workspace{BaseSHA: "base", Path: "/work/running"}
	fixture.start(t)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		value, _ := fixture.store.Task(context.Background(), unsafe.ID)
		if value.State == workmodel.Paused {
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
	value := seededTask("running", workmodel.Running)
	value.WorktreePath, value.BaseSHA, value.ProviderSessionID = "/work/running", "base", "session"
	if err := fixture.store.CreateTask(context.Background(), value, workmodel.Event{ID: "created", TaskID: value.ID, Type: workmodel.EventTaskCreated, Visibility: workmodel.VisibilityUser, CreatedAt: value.CreatedAt}); err != nil {
		t.Fatal(err)
	}
	fixture.store.sessions["session"] = workmodel.Session{ID: "session", TaskID: value.ID, ProviderSessionID: "session", Resumable: true}
	fixture.workspace.result = Workspace{}
	fixture.start(t)
	got := fixture.wait(t, value.ID)
	if got.State != workmodel.Paused || got.FailureReason == "" {
		t.Fatalf("task = %#v", got)
	}
}

func TestLocalControllerTaskIsLeftAloneByReconcile(t *testing.T) {
	fixture := newFixture(t, nil)
	value := seededTask("local-owner", workmodel.Queued)
	value.ControllerOwner = workmodel.TaskControllerLocal
	if err := fixture.store.CreateTask(context.Background(), value, workmodel.Event{ID: "created", TaskID: value.ID, Type: workmodel.EventTaskCreated, Visibility: workmodel.VisibilityUser, CreatedAt: value.CreatedAt}); err != nil {
		t.Fatal(err)
	}
	fixture.start(t)
	time.Sleep(20 * time.Millisecond)
	got, err := fixture.store.Task(context.Background(), value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != workmodel.Queued {
		t.Fatalf("locally owned task state = %s, want queued", got.State)
	}
}

func TestStartDoesNotDeadlockWhenReconciliationExceedsQueueCapacity(t *testing.T) {
	fixture := newFixture(t, nil)
	fixture.app.config.QueueSize = 1
	for index := range 8 {
		value := seededTask(fmt.Sprintf("queued-%d", index), workmodel.Queued)
		if err := fixture.store.CreateTask(context.Background(), value, workmodel.Event{ID: value.ID + "-created", TaskID: value.ID, Type: workmodel.EventTaskCreated, Visibility: workmodel.VisibilityUser, CreatedAt: value.CreatedAt}); err != nil {
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

func seededTask(id string, state workmodel.State) workmodel.Task {
	at := time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC)
	return workmodel.Task{ID: id, RepoProfileID: "sample", Title: id, Prompt: id, State: state, Provider: workmodel.CodexSubscription, TelegramChatID: 100, CreatedAt: at, UpdatedAt: at}
}
