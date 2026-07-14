package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/store"
	storesqlite "github.com/berkayahi/agentbridge/internal/store/sqlite"
	"github.com/berkayahi/agentbridge/internal/task"
	_ "modernc.org/sqlite"
)

func TestOpenMigratesAndReopensWithPragmas(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agentbridge.db")
	db := openStore(t, path)
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	db = openStore(t, path)
	t.Cleanup(func() { _ = db.Close() })

	check, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open pragma check: %v", err)
	}
	defer check.Close()
	var journalMode string
	if err := check.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("read journal mode: %v", err)
	}
	if journalMode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", journalMode)
	}

	badAttachment := task.Attachment{ID: "missing-a", TaskID: "missing", CreatedAt: time.Now()}
	if err := db.SaveAttachment(ctx, badAttachment); err == nil {
		t.Fatal("SaveAttachment() without parent task succeeded; foreign keys are disabled")
	}
}

func TestCreateTaskEventsAndDuplicateProviderEvent(t *testing.T) {
	ctx := context.Background()
	db := openStore(t, filepath.Join(t.TempDir(), "tasks.db"))
	t.Cleanup(func() { _ = db.Close() })

	created := time.Date(2026, time.July, 14, 5, 0, 0, 123, time.FixedZone("TRT", 3*60*60))
	want := newTask("task-1", task.Queued, created)
	initial := newEvent("event-1", want.ID, task.EventTaskCreated, "provider-1", created)
	if err := db.CreateTask(ctx, want, initial); err != nil {
		t.Fatalf("CreateTask(): %v", err)
	}

	got, err := db.Task(ctx, want.ID)
	if err != nil {
		t.Fatalf("Task(): %v", err)
	}
	if got.ID != want.ID || got.State != task.Queued || got.Provider != task.ProviderCodex {
		t.Fatalf("Task() = %#v", got)
	}
	if got.CreatedAt.Location() != time.UTC || !got.CreatedAt.Equal(created) {
		t.Fatalf("CreatedAt = %v, want same instant in UTC", got.CreatedAt)
	}

	second := newEvent("event-2", want.ID, task.EventProviderMessage, "provider-2", created.Add(time.Second))
	if err := db.AppendEvent(ctx, second); err != nil {
		t.Fatalf("AppendEvent(): %v", err)
	}
	events, err := db.Events(ctx, want.ID)
	if err != nil {
		t.Fatalf("Events(): %v", err)
	}
	if len(events) != 2 || events[0].ID != initial.ID || events[1].ID != second.ID {
		t.Fatalf("Events() order = %#v", events)
	}

	duplicate := newEvent("event-3", want.ID, task.EventProviderMessage, "provider-2", created.Add(2*time.Second))
	if err := db.AppendEvent(ctx, duplicate); !errors.Is(err, store.ErrDuplicateEvent) {
		t.Fatalf("AppendEvent(duplicate) = %v, want ErrDuplicateEvent", err)
	}
}

func TestTransitionIsAtomic(t *testing.T) {
	ctx := context.Background()
	db := openStore(t, filepath.Join(t.TempDir(), "transition.db"))
	t.Cleanup(func() { _ = db.Close() })
	created := time.Now().UTC()
	seedTask(t, db, newTask("task-1", task.Queued, created))

	event := newEvent("event-2", "task-1", task.EventStateTransitioned, "", created.Add(time.Second))
	if err := db.Transition(ctx, "task-1", task.Preparing, event); err != nil {
		t.Fatalf("Transition(valid): %v", err)
	}
	invalid := newEvent("event-3", "task-1", task.EventStateTransitioned, "", created.Add(2*time.Second))
	if err := db.Transition(ctx, "task-1", task.Pushing, invalid); !errors.Is(err, store.ErrInvalidTransition) {
		t.Fatalf("Transition(invalid) = %v, want ErrInvalidTransition", err)
	}

	got, err := db.Task(ctx, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != task.Preparing {
		t.Fatalf("state = %q, want preparing", got.State)
	}
	events, err := db.Events(ctx, "task-1")
	if err != nil || len(events) != 2 {
		t.Fatalf("events after rollback = %d, %v; want 2", len(events), err)
	}
}

func TestTransitionPersistsLifecycleTimestamps(t *testing.T) {
	ctx := context.Background()
	db := openStore(t, filepath.Join(t.TempDir(), "lifecycle-times.db"))
	t.Cleanup(func() { _ = db.Close() })
	created := time.Date(2026, time.July, 14, 8, 0, 0, 0, time.UTC)
	seedTask(t, db, newTask("task-1", task.Queued, created))

	transitions := []struct {
		to task.State
		at time.Time
	}{
		{to: task.Preparing, at: created.Add(time.Minute)},
		{to: task.Running, at: created.Add(2 * time.Minute)},
		{to: task.Verifying, at: created.Add(3 * time.Minute)},
		{to: task.Committing, at: created.Add(4 * time.Minute)},
		{to: task.Pushing, at: created.Add(5 * time.Minute)},
		{to: task.Completed, at: created.Add(6 * time.Minute)},
	}
	for index, transition := range transitions {
		event := newEvent(fmt.Sprintf("transition-%d", index), "task-1", task.EventStateTransitioned, "", transition.at)
		if err := db.Transition(ctx, "task-1", transition.to, event); err != nil {
			t.Fatalf("Transition(%s): %v", transition.to, err)
		}
	}

	got, err := db.Task(ctx, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.StartedAt == nil || !got.StartedAt.Equal(created.Add(2*time.Minute)) {
		t.Fatalf("StartedAt = %v, want running transition time", got.StartedAt)
	}
	if got.FinishedAt == nil || !got.FinishedAt.Equal(created.Add(6*time.Minute)) {
		t.Fatalf("FinishedAt = %v, want completion transition time", got.FinishedAt)
	}
	if elapsed := got.Elapsed(created.Add(time.Hour)); elapsed != 4*time.Minute {
		t.Fatalf("Elapsed() = %v, want 4m", elapsed)
	}
}

func TestEventOrderingHandlesFractionalSeconds(t *testing.T) {
	ctx := context.Background()
	db := openStore(t, filepath.Join(t.TempDir(), "fractional-order.db"))
	t.Cleanup(func() { _ = db.Close() })
	base := time.Date(2026, time.July, 14, 8, 0, 0, 0, time.UTC)
	value := newTask("task-1", task.Queued, base)
	seedTask(t, db, value)
	later := newEvent("later", value.ID, task.EventProviderMessage, "later", base.Add(100*time.Millisecond))
	if err := db.AppendEvent(ctx, later); err != nil {
		t.Fatal(err)
	}

	events, err := db.Events(ctx, value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].CreatedAt.After(events[1].CreatedAt) {
		t.Fatalf("Events() are not chronological: %#v", events)
	}
}

func TestConcurrentTransitionReturnsStableConflict(t *testing.T) {
	ctx := context.Background()
	db := openStore(t, filepath.Join(t.TempDir(), "concurrent-transition.db"))
	t.Cleanup(func() { _ = db.Close() })
	created := time.Now().UTC()
	seedTask(t, db, newTask("task-1", task.Queued, created))

	start := make(chan struct{})
	errorsCh := make(chan error, 2)
	for i := range 2 {
		go func(index int) {
			<-start
			event := newEvent(fmt.Sprintf("event-%d", index), "task-1", task.EventStateTransitioned, "", created.Add(time.Duration(index+1)*time.Nanosecond))
			errorsCh <- db.Transition(ctx, "task-1", task.Preparing, event)
		}(i)
	}
	close(start)
	first, second := <-errorsCh, <-errorsCh
	if first != nil && second != nil {
		t.Fatalf("both transitions failed: %v, %v", first, second)
	}
	loser := first
	if loser == nil {
		loser = second
	}
	if !errors.Is(loser, store.ErrConflict) {
		t.Fatalf("losing Transition() = %v, want ErrConflict", loser)
	}
}

func TestCreateTaskAndInitialEventAreAtomic(t *testing.T) {
	ctx := context.Background()
	db := openStore(t, filepath.Join(t.TempDir(), "create-atomic.db"))
	t.Cleanup(func() { _ = db.Close() })
	value := newTask("task-1", task.Queued, time.Now())
	invalidEvent := newEvent("event-1", value.ID, task.EventTaskCreated, "", value.CreatedAt)
	invalidEvent.Visibility = "invalid"

	if err := db.CreateTask(ctx, value, invalidEvent); err == nil {
		t.Fatal("CreateTask() succeeded with an invalid initial event")
	}
	if _, err := db.Task(ctx, value.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Task() after rolled-back creation = %v, want ErrNotFound", err)
	}
}

func TestRelatedRecordsAndRestartQueries(t *testing.T) {
	ctx := context.Background()
	db := openStore(t, filepath.Join(t.TempDir(), "restart.db"))
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now().UTC()
	seedTask(t, db, newTask("active", task.Running, now))
	seedTask(t, db, newTask("done", task.Completed, now))

	session := task.Session{ID: "session-1", TaskID: "active", Provider: task.ProviderClaude, ProviderSessionID: "claude-1", Status: "active", Resumable: true, CreatedAt: now, UpdatedAt: now}
	if err := db.UpsertSession(ctx, session); err != nil {
		t.Fatalf("UpsertSession(): %v", err)
	}
	session.Status = "paused"
	session.UpdatedAt = now.Add(time.Minute)
	if err := db.UpsertSession(ctx, session); err != nil {
		t.Fatalf("UpsertSession(update): %v", err)
	}

	expires := now.Add(time.Hour)
	approval := task.Approval{ID: "approval-1", TaskID: "active", Kind: "shell", Status: task.ApprovalPending, RequestPayload: []byte(`{"command":"git status"}`), RequestedAt: now, ExpiresAt: &expires}
	if err := db.UpsertApproval(ctx, approval); err != nil {
		t.Fatalf("UpsertApproval(): %v", err)
	}
	attachment := task.Attachment{ID: "attachment-1", TaskID: "active", Kind: "image", Name: "screen.png", MediaType: "image/png", StoragePath: "attachments/screen.png", SizeBytes: 42, SHA256: "abc123", CreatedAt: now}
	if err := db.SaveAttachment(ctx, attachment); err != nil {
		t.Fatalf("SaveAttachment(): %v", err)
	}

	nonterminal, err := db.NonterminalTasks(ctx)
	if err != nil || len(nonterminal) != 1 || nonterminal[0].ID != "active" {
		t.Fatalf("NonterminalTasks() = %#v, %v", nonterminal, err)
	}
	resumable, err := db.ResumableSessions(ctx)
	if err != nil || len(resumable) != 1 || resumable[0].Status != "paused" {
		t.Fatalf("ResumableSessions() = %#v, %v", resumable, err)
	}
	pending, err := db.PendingApprovals(ctx)
	if err != nil || len(pending) != 1 || pending[0].ID != approval.ID {
		t.Fatalf("PendingApprovals() = %#v, %v", pending, err)
	}
	attachments, err := db.Attachments(ctx, "active")
	if err != nil || len(attachments) != 1 || attachments[0].Name != attachment.Name || attachments[0].SHA256 != attachment.SHA256 {
		t.Fatalf("Attachments() = %#v, %v", attachments, err)
	}
}

func TestTaskProjectionsPersistOrchestrationResults(t *testing.T) {
	ctx := context.Background()
	db := openStore(t, filepath.Join(t.TempDir(), "projections.db"))
	t.Cleanup(func() { _ = db.Close() })
	now := time.Date(2026, time.July, 14, 12, 0, 0, 123, time.UTC)
	seedTask(t, db, newTask("task-1", task.Running, now))

	if err := db.SaveWorkspace(ctx, "task-1", "base-456", "/tmp/worktree-456"); err != nil {
		t.Fatalf("SaveWorkspace(): %v", err)
	}
	if err := db.SaveTelegramMessage(ctx, "task-1", 9876); err != nil {
		t.Fatalf("SaveTelegramMessage(): %v", err)
	}
	session := task.Session{
		ID:                "session-1",
		TaskID:            "task-1",
		Provider:          task.ProviderCodex,
		ProviderSessionID: "provider-session-1",
		ProviderThreadID:  "thread-1",
		Status:            "active",
		Resumable:         true,
		CreatedAt:         now.Add(time.Second),
		UpdatedAt:         now.Add(2 * time.Second),
	}
	if err := db.SaveProviderSession(ctx, "task-1", session); err != nil {
		t.Fatalf("SaveProviderSession(): %v", err)
	}
	if err := db.SaveDelivery(ctx, "task-1", "commit-789", "refs/heads/staging", "https://staging.example.test"); err != nil {
		t.Fatalf("SaveDelivery(): %v", err)
	}
	if err := db.SaveFailure(ctx, "task-1", "Authorization: Bearer secret-token\nexport OPENAI_API_KEY=also-secret"); err != nil {
		t.Fatalf("SaveFailure(): %v", err)
	}

	got, err := db.Task(ctx, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseSHA != "base-456" || got.WorktreePath != "/tmp/worktree-456" {
		t.Fatalf("workspace projection = %q, %q", got.BaseSHA, got.WorktreePath)
	}
	if got.TelegramMessageID != 9876 {
		t.Fatalf("TelegramMessageID = %d", got.TelegramMessageID)
	}
	if got.ProviderSessionID != session.ProviderSessionID || got.ProviderThreadID != session.ProviderThreadID {
		t.Fatalf("provider projection = %q, %q", got.ProviderSessionID, got.ProviderThreadID)
	}
	if got.CommitSHA != "commit-789" || got.PushRef != "refs/heads/staging" || got.DeploymentURL != "https://staging.example.test" {
		t.Fatalf("delivery projection = %q, %q, %q", got.CommitSHA, got.PushRef, got.DeploymentURL)
	}
	if strings.Contains(got.FailureReason, "secret-token") || strings.Contains(got.FailureReason, "also-secret") || !strings.Contains(got.FailureReason, "[REDACTED:") {
		t.Fatalf("failure reason was not redacted: %q", got.FailureReason)
	}
	if got.UpdatedAt.Location() != time.UTC || got.UpdatedAt.Before(now) {
		t.Fatalf("UpdatedAt = %v, want a current UTC timestamp", got.UpdatedAt)
	}
	resumable, err := db.ResumableSessions(ctx)
	if err != nil || len(resumable) != 1 || resumable[0].ID != session.ID {
		t.Fatalf("ResumableSessions() = %#v, %v", resumable, err)
	}
}

func TestTaskProjectionRejectsMissingTaskAndMismatchedSession(t *testing.T) {
	ctx := context.Background()
	db := openStore(t, filepath.Join(t.TempDir(), "projection-errors.db"))
	t.Cleanup(func() { _ = db.Close() })

	for name, call := range map[string]func() error{
		"workspace": func() error { return db.SaveWorkspace(ctx, "missing", "base", "/tmp/worktree") },
		"message":   func() error { return db.SaveTelegramMessage(ctx, "missing", 1) },
		"delivery":  func() error { return db.SaveDelivery(ctx, "missing", "commit", "refs/heads/staging", "") },
		"failure":   func() error { return db.SaveFailure(ctx, "missing", "failed") },
	} {
		if err := call(); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%s error = %v, want ErrNotFound", name, err)
		}
	}

	now := time.Now().UTC()
	seedTask(t, db, newTask("task-1", task.Running, now))
	session := task.Session{ID: "session-1", TaskID: "another-task", Provider: task.ProviderCodex, CreatedAt: now, UpdatedAt: now}
	if err := db.SaveProviderSession(ctx, "task-1", session); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("SaveProviderSession(mismatch) = %v, want ErrConflict", err)
	}
	if sessions, err := db.ResumableSessions(ctx); err != nil || len(sessions) != 0 {
		t.Fatalf("mismatched session persisted: %#v, %v", sessions, err)
	}
}

func TestLeaseOwnershipExpiryAndRelease(t *testing.T) {
	ctx := context.Background()
	db := openStore(t, filepath.Join(t.TempDir(), "leases.db"))
	t.Cleanup(func() { _ = db.Close() })

	acquired, err := db.AcquireLease(ctx, "repo-1", "worker-a", time.Hour)
	if err != nil || !acquired {
		t.Fatalf("AcquireLease(first) = %v, %v", acquired, err)
	}
	acquired, err = db.AcquireLease(ctx, "repo-1", "worker-b", time.Hour)
	if err != nil || acquired {
		t.Fatalf("AcquireLease(contended) = %v, %v", acquired, err)
	}
	if err := db.HeartbeatLease(ctx, "repo-1", "worker-b", time.Hour); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("HeartbeatLease(wrong owner) = %v, want ErrConflict", err)
	}
	if err := db.ReleaseLease(ctx, "repo-1", "worker-a"); err != nil {
		t.Fatalf("ReleaseLease(): %v", err)
	}
	acquired, err = db.AcquireLease(ctx, "repo-1", "worker-b", -time.Second)
	if err != nil || !acquired {
		t.Fatalf("AcquireLease(expiring) = %v, %v", acquired, err)
	}
	expired, err := db.ExpiredLeases(ctx)
	if err != nil || len(expired) != 1 || expired[0].OwnerID != "worker-b" {
		t.Fatalf("ExpiredLeases() = %#v, %v", expired, err)
	}
}

func TestErrorsFiltersAndCanceledContext(t *testing.T) {
	ctx := context.Background()
	db := openStore(t, filepath.Join(t.TempDir(), "errors.db"))
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Task(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Task(missing) = %v, want ErrNotFound", err)
	}
	now := time.Now().UTC()
	one := newTask("one", task.Queued, now)
	one.RepoProfileID = "repo-a"
	two := newTask("two", task.Running, now.Add(time.Second))
	two.RepoProfileID = "repo-b"
	seedTask(t, db, one)
	seedTask(t, db, two)
	got, err := db.ListTasks(ctx, store.ListFilter{RepoProfileID: "repo-a", States: []task.State{task.Queued}, Limit: 10})
	if err != nil || len(got) != 1 || got[0].ID != "one" {
		t.Fatalf("ListTasks() = %#v, %v", got, err)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := db.Task(canceled, "one"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Task(canceled) = %v, want context.Canceled", err)
	}
}

func TestOpenUpgradesLegacyAttachmentChecksumSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`CREATE TABLE attachments (id TEXT PRIMARY KEY, task_id TEXT NOT NULL, kind TEXT NOT NULL, name TEXT NOT NULL, media_type TEXT NOT NULL, storage_path TEXT NOT NULL, size_bytes INTEGER NOT NULL, created_at TEXT NOT NULL)`)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := storesqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("upgrade legacy schema: %v", err)
	}
	if err := upgraded.Close(); err != nil {
		t.Fatal(err)
	}
	check, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	rows, err := check.Query(`PRAGMA table_info(attachments)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid, notnull, pk int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notnull, &defaultValue, &pk); err != nil {
			t.Fatal(err)
		}
		if name == "sha256" {
			found = true
		}
	}
	if !found {
		t.Fatal("sha256 column was not migrated")
	}
}

func openStore(t *testing.T, path string) *storesqlite.Store {
	t.Helper()
	db, err := storesqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	return db
}

func newTask(id string, state task.State, created time.Time) task.Task {
	return task.Task{ID: id, RepoProfileID: "repo-1", Title: "Görev", Prompt: "Do work", State: state, Provider: task.ProviderCodex, TelegramChatID: 42, TelegramMessageID: 7, BaseSHA: "abc123", WorktreePath: "/tmp/worktree", CreatedAt: created, UpdatedAt: created}
}

func newEvent(id, taskID string, eventType task.EventType, providerID string, created time.Time) task.Event {
	return task.Event{ID: id, TaskID: taskID, Type: eventType, Visibility: task.VisibilityInternal, ProviderEventID: providerID, Payload: []byte(`{"redacted":true}`), CreatedAt: created}
}

type taskStore interface {
	CreateTask(context.Context, task.Task, task.Event) error
}

func seedTask(t *testing.T, db taskStore, value task.Task) {
	t.Helper()
	event := newEvent("created-"+value.ID, value.ID, task.EventTaskCreated, "", value.CreatedAt)
	if err := db.CreateTask(context.Background(), value, event); err != nil {
		t.Fatalf("seed task %s: %v", value.ID, err)
	}
}
