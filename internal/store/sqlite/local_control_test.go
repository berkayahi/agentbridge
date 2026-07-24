package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/localcontrol"
	"github.com/berkayahi/agentbridge/internal/store"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

func TestRuntimeStoreCreateTaskAtomicallyRollsBackOnIdempotencyConflict(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(2_100, 0).UTC()
	data, err := OpenV2Runtime(ctx, filepath.Join(t.TempDir(), "atomic-task.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()

	project := localcontrol.Project{ID: "project-atomic", Name: "Atomic", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := data.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	board := localcontrol.Board{ID: "board-atomic", ProjectID: project.ID, Name: "Build", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := data.CreateBoard(ctx, board); err != nil {
		t.Fatal(err)
	}
	repository := localcontrol.Repository{ID: "repository-atomic", Remote: "origin", CreatedAt: now}
	if err := data.CreateRepository(ctx, repository); err != nil {
		t.Fatal(err)
	}
	if err := data.SaveIdempotency(ctx, localcontrol.IdempotencyRecord{
		Key: "atomic-task-key", Operation: "different-operation", RequestHash: "different-request",
		ResponseBytes: []byte(`{}`), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	task := workmodel.Task{
		ID: "task-atomic", RepoProfileID: repository.ID, Title: "Atomic task", Prompt: "prove rollback",
		Provider: workmodel.CodexSubscription, State: workmodel.Queued, CreatedAt: now, UpdatedAt: now,
	}
	_, err = data.CreateTaskAtomically(ctx, localcontrol.AtomicTaskCreation{
		ProjectID: project.ID, BoardID: board.ID, TargetDeviceID: localcontrol.LocalDeviceID,
		Task: task,
		InitialEvent: workmodel.Event{
			ID: "runtime-atomic", TaskID: task.ID, Type: workmodel.EventTaskCreated,
			Visibility: workmodel.VisibilityUser, Payload: []byte(`{"state":"queued"}`), CreatedAt: now,
		},
		LocalEvent: localcontrol.Event{
			ID: "local-atomic", ResourceType: "task", ResourceID: task.ID, TaskID: task.ID,
			Revision: 1, Type: "task_created", Payload: []byte(`{"state":"queued"}`), CreatedAt: now,
		},
		Idempotency: localcontrol.IdempotencyRecord{
			Key: "atomic-task-key", Operation: "create_task", RequestHash: "create-request",
			ResponseBytes: []byte(`{"task":{"id":"task-atomic"}}`), CreatedAt: now,
		},
	})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("atomic task conflict = %v, want store.ErrConflict", err)
	}
	if _, err := data.Task(ctx, task.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("rolled-back task lookup = %v, want store.ErrNotFound", err)
	}
	events, err := data.ListLocalEvents(ctx, task.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("rolled-back local events = %#v, want none", events)
	}
	record, err := data.LoadIdempotency(ctx, "atomic-task-key")
	if err != nil {
		t.Fatal(err)
	}
	if record.Operation != "different-operation" || record.RequestHash != "different-request" {
		t.Fatalf("existing idempotency record changed by rollback = %#v", record)
	}
}

func TestRuntimeStoreLocalTaskUsesLocalControllerOwnership(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(2_200, 0).UTC()
	data, err := OpenV2Runtime(ctx, filepath.Join(t.TempDir(), "local-owner.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	project := localcontrol.Project{ID: "project-owner", Name: "Owner", Revision: 1, CreatedAt: now, UpdatedAt: now}
	board := localcontrol.Board{ID: "board-owner", ProjectID: project.ID, Name: "Build", Revision: 1, CreatedAt: now, UpdatedAt: now}
	repository := localcontrol.Repository{ID: "repository-owner", Remote: "origin", CreatedAt: now}
	for _, create := range []func() error{
		func() error { return data.CreateProject(ctx, project) },
		func() error { return data.CreateBoard(ctx, board) },
		func() error { return data.CreateRepository(ctx, repository) },
	} {
		if err := create(); err != nil {
			t.Fatal(err)
		}
	}
	task := workmodel.Task{ID: "local-owner-task", RepoProfileID: repository.ID, Title: "Local owner", Prompt: "run locally", Provider: workmodel.CodexSubscription, CreatedAt: now, UpdatedAt: now}
	if _, err := data.CreateTaskInContext(ctx, project.ID, board.ID, localcontrol.LocalDeviceID, task,
		workmodel.Event{ID: "runtime-owner", TaskID: task.ID, Type: workmodel.EventTaskCreated, Visibility: workmodel.VisibilityUser, Payload: []byte(`{"state":"queued"}`), CreatedAt: now},
		localcontrol.Event{ID: "local-owner", ResourceType: "task", ResourceID: task.ID, TaskID: task.ID, Revision: 1, Type: "task_created", Payload: []byte(`{"state":"queued"}`), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	var owner string
	if err := data.db.QueryRowContext(ctx, `SELECT controller_owner FROM local_tasks WHERE id = ?`, task.ID).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != string(workmodel.TaskControllerLocal) {
		t.Fatalf("local task controller owner = %q, want %q", owner, workmodel.TaskControllerLocal)
	}
}
