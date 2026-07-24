package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/workmodel"
)

func TestRuntimeStorePersistsStandaloneTaskInV2Lineage(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agentbridge.db")
	data, err := OpenV2Runtime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Unix(1_700_000_000, 0).UTC()
	if err := data.EnsureRepositoryBinding(ctx, "repo-1", "origin"); err != nil {
		t.Fatal(err)
	}
	value := workmodel.Task{
		ID: "task-1", RepoProfileID: "repo-1", Title: "Run local task", Prompt: "inspect the repository",
		State: workmodel.Queued, Provider: workmodel.CodexSubscription,
		TelegramChatID: 42, CreatedAt: now, UpdatedAt: now,
	}
	created := workmodel.Event{
		ID: "event-1", TaskID: value.ID, Type: workmodel.EventTaskCreated,
		Visibility: workmodel.VisibilityUser, Payload: []byte(`{"title":"Run local task"}`), CreatedAt: now,
	}
	if err := data.CreateTask(ctx, value, created); err != nil {
		t.Fatal(err)
	}

	if err := data.Transition(ctx, value.ID, workmodel.Preparing, workmodel.Event{
		ID: "event-2", TaskID: value.ID, Type: workmodel.EventStateTransitioned,
		Visibility: workmodel.VisibilityUser, Payload: []byte(`{"state":"preparing"}`), CreatedAt: now.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := data.Transition(ctx, value.ID, workmodel.Running, workmodel.Event{
		ID: "event-3", TaskID: value.ID, Type: workmodel.EventStateTransitioned,
		Visibility: workmodel.VisibilityUser, Payload: []byte(`{"state":"running"}`), CreatedAt: now.Add(2 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := data.SaveWorkspace(ctx, value.ID, "base-sha", "/tmp/worktree/task-1"); err != nil {
		t.Fatal(err)
	}
	if err := data.SaveTelegramMessage(ctx, value.ID, 99); err != nil {
		t.Fatal(err)
	}

	got, err := data.Task(ctx, value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != workmodel.Running || got.Revision != 4 || got.RepoProfileID != value.RepoProfileID || got.Provider != value.Provider || got.ControllerOwner != workmodel.TaskControllerStandalone || got.TelegramChatID != 42 || got.TelegramMessageID != 99 || got.BaseSHA != "base-sha" {
		t.Fatalf("task projection = %#v", got)
	}
	events, err := data.Events(ctx, value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0].ID != created.ID || events[2].Type != workmodel.EventStateTransitioned {
		t.Fatalf("events = %#v", events)
	}

	var legacyTables, v2Ledgers int
	if err := data.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'tasks'").Scan(&legacyTables); err != nil {
		t.Fatal(err)
	}
	if err := data.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM migration_ledger").Scan(&v2Ledgers); err != nil {
		t.Fatal(err)
	}
	if legacyTables != 0 || v2Ledgers != 9 {
		t.Fatalf("legacy tables=%d v2 ledgers=%d", legacyTables, v2Ledgers)
	}
	if err := data.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenV2Runtime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got, err := reopened.Task(ctx, value.ID); err != nil || got.State != workmodel.Running {
		t.Fatalf("reopened task = %#v, err = %v", got, err)
	}
}

func TestRuntimeStoreUpsertSessionReusesCanonicalActiveTaskSession(t *testing.T) {
	ctx := context.Background()
	data, err := OpenV2Runtime(ctx, filepath.Join(t.TempDir(), "provider-session.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()

	now := time.Unix(1_700_000_100, 0).UTC()
	if err := data.EnsureRepositoryBinding(ctx, "repo-1", "origin"); err != nil {
		t.Fatal(err)
	}
	value := workmodel.Task{ID: "task-1", RepoProfileID: "repo-1", Title: "Provider session", Prompt: "start", State: workmodel.Queued, Provider: workmodel.CodexSubscription, CreatedAt: now, UpdatedAt: now}
	if err := data.CreateTask(ctx, value, workmodel.Event{ID: "event-1", TaskID: value.ID, Type: workmodel.EventTaskCreated, Visibility: workmodel.VisibilityUser, Payload: []byte(`{}`), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}

	providerSession := workmodel.Session{
		ID: "thread-1", TaskID: value.ID, Provider: value.Provider,
		ProviderSessionID: "thread-1", ProviderThreadID: "thread-1", Status: "running", Resumable: true,
		CreatedAt: now.Add(time.Second), UpdatedAt: now.Add(time.Second),
	}
	if err := data.UpsertSession(ctx, providerSession); err != nil {
		t.Fatal(err)
	}

	var count int
	if err := data.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE local_task_id = ?`, value.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("session rows for task = %d, want canonical row only", count)
	}
	var id, activeTask, providerSessionID, providerThreadID, status string
	if err := data.db.QueryRowContext(ctx, `SELECT id, active_local_task_id, provider_session_id, provider_thread_id, status FROM sessions WHERE local_task_id = ?`, value.ID).Scan(&id, &activeTask, &providerSessionID, &providerThreadID, &status); err != nil {
		t.Fatal(err)
	}
	if id != value.ID+"-session" || activeTask != value.ID || providerSessionID != "thread-1" || providerThreadID != "thread-1" || status != "running" {
		t.Fatalf("canonical session = id=%q active=%q provider=%q thread=%q status=%q", id, activeTask, providerSessionID, providerThreadID, status)
	}
}
