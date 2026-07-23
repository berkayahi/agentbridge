package standalone

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/kernel"
	"github.com/berkayahi/agentbridge/internal/store/sqlite"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

func TestKernelControllerPersistsStartIntentThroughV2Store(t *testing.T) {
	ctx := context.Background()
	data, err := sqlite.OpenV2Runtime(ctx, filepath.Join(t.TempDir(), "agentbridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	if err := data.EnsureRepositoryBinding(ctx, "repo-1", "origin"); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	if err := data.CreateTask(ctx, workmodel.Task{
		ID: "task-1", RepoProfileID: "repo-1", Title: "Start", Prompt: "run", State: workmodel.Queued,
		Provider: workmodel.CodexSubscription, CreatedAt: now, UpdatedAt: now,
	}, workmodel.Event{ID: "created", TaskID: "task-1", Type: workmodel.EventTaskCreated, Visibility: workmodel.VisibilityUser, Payload: []byte(`{}`), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	bridgeKernel, err := kernel.New(kernel.Config{Work: data, Owner: "standalone-test"})
	if err != nil {
		t.Fatal(err)
	}
	controller := NewKernelController(bridgeKernel)
	if err := controller.Start(ctx, kernel.StartExecution{
		CommandID: "command-1", ExecutionID: "task-1-execution", TaskID: "task-1", SessionID: "task-1-session",
		RepositoryID: "repo-1", RuntimeID: "codex", PolicySnapshot: []byte("{}"), FencingEpoch: 1,
		Input: kernel.Input{Text: "run"}, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	intent, err := data.Repositories().Intents.Get(ctx, "command-1")
	if err != nil {
		t.Fatal(err)
	}
	if intent.State != "pending" {
		t.Fatalf("intent state = %q, want pending", intent.State)
	}
	events, err := data.Repositories().Events.List(ctx, "task-1-execution")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].ID != "command-1-event" || events[1].Type != string(kernel.EventIntentAccepted) {
		t.Fatalf("events = %#v", events)
	}
}
