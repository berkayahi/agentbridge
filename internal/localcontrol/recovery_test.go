package localcontrol_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/localcontrol"
	"github.com/berkayahi/agentbridge/internal/store/sqlite"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

// A bee whose provider session died with the daemon must not be reported as
// still working. Pausing her is recoverable; leaving her running is a lie the
// board would repeat forever.
func TestRecoveryPausesAStrandedBee(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "local.db")
	data, err := sqlite.OpenV2Runtime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	build := func(store *sqlite.RuntimeStore, prefix string) *localcontrol.Service {
		ids := deterministicIDs()
		service, err := localcontrol.New(localcontrol.Config{
			Store: store, Runtimes: fakeCatalog{}, Executor: &fakeExecutor{},
			Verifier: fakeVerifier{}, Committer: fakeCommitter{},
			Clock: func() time.Time { return now },
			NewID: func(kind string) string { return prefix + ids(kind) },
		})
		if err != nil {
			t.Fatal(err)
		}
		return service
	}
	service := build(data, "")
	project, _ := service.CreateProject(ctx, localcontrol.CreateProjectRequest{Name: "Hive", IdempotencyKey: "p"})
	repository, _ := service.RegisterRepository(ctx, localcontrol.RegisterRepositoryRequest{Remote: "origin", IdempotencyKey: "r"})
	board, _ := service.CreateBoard(ctx, localcontrol.CreateBoardRequest{ProjectID: project.Project.ID, Name: "Comb", IdempotencyKey: "b"})
	task, err := service.CreateTask(ctx, localcontrol.CreateTaskRequest{
		ProjectID: project.Project.ID, BoardID: board.Board.ID, RepositoryID: repository.Repository.ID,
		Provider: workmodel.Provider("codex"), Prompt: "align the widget", IdempotencyKey: "t",
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := service.Start(ctx, localcontrol.StartRequest{TaskID: task.Task.ID, Revision: task.Task.Revision, IdempotencyKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if started.Task.State != workmodel.Running {
		t.Fatalf("state before restart = %s", started.Task.State)
	}
	if err := data.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := sqlite.OpenV2Runtime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	restarted := build(reopened, "restart-")
	if err := restarted.RecoverLocalTasks(ctx); err != nil {
		t.Fatal(err)
	}
	observed, err := restarted.Observe(ctx, localcontrol.ObserveRequest{TaskID: task.Task.ID, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if observed.Task.State != workmodel.Paused {
		t.Fatalf("state after recovery = %s, want paused", observed.Task.State)
	}
	explained := false
	for _, event := range observed.Events {
		if event.Type == "local_session_lost" {
			explained = true
		}
	}
	if !explained {
		t.Fatal("recovery left no event explaining the gap")
	}

	// Recovery must be safe to run twice: a second start must not pause a bee
	// that is legitimately flying again.
	resumed, err := restarted.Start(ctx, localcontrol.StartRequest{
		TaskID: task.Task.ID, Revision: observed.Task.Revision, IdempotencyKey: "s2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Task.State != workmodel.Running {
		t.Fatalf("state after resume = %s", resumed.Task.State)
	}
}

// A hive with nothing stranded must do nothing at all.
func TestRecoveryLeavesACalmHiveAlone(t *testing.T) {
	ctx := context.Background()
	data, err := sqlite.OpenV2Runtime(ctx, filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = data.Close() }()
	service, err := localcontrol.New(localcontrol.Config{
		Store: data, Runtimes: fakeCatalog{}, Executor: &fakeExecutor{},
		Clock: func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }, NewID: deterministicIDs(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.RecoverLocalTasks(ctx); err != nil {
		t.Fatal(err)
	}
}

// A bee already home with a checked receipt must survive a restart untouched:
// her worktree and receipt are durable and the keeper's tap is still valid, so
// pausing her would make the work happen twice.
func TestRecoveryLeavesAHomeBeeReadyToLand(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "local.db")
	data, err := sqlite.OpenV2Runtime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	build := func(store *sqlite.RuntimeStore, prefix string) *localcontrol.Service {
		ids := deterministicIDs()
		service, err := localcontrol.New(localcontrol.Config{
			Store: store, Runtimes: fakeCatalog{}, Executor: &fakeExecutor{},
			Verifier: fakeVerifier{}, Committer: fakeCommitter{},
			Clock: func() time.Time { return now },
			NewID: func(kind string) string { return prefix + ids(kind) },
		})
		if err != nil {
			t.Fatal(err)
		}
		return service
	}
	service := build(data, "")
	project, _ := service.CreateProject(ctx, localcontrol.CreateProjectRequest{Name: "Hive", IdempotencyKey: "p"})
	repository, _ := service.RegisterRepository(ctx, localcontrol.RegisterRepositoryRequest{Remote: "origin", IdempotencyKey: "r"})
	board, _ := service.CreateBoard(ctx, localcontrol.CreateBoardRequest{ProjectID: project.Project.ID, Name: "Comb", IdempotencyKey: "b"})
	task, _ := service.CreateTask(ctx, localcontrol.CreateTaskRequest{
		ProjectID: project.Project.ID, BoardID: board.Board.ID, RepositoryID: repository.Repository.ID,
		Provider: workmodel.Provider("codex"), Prompt: "align the widget", IdempotencyKey: "t",
	})
	started, _ := service.Start(ctx, localcontrol.StartRequest{TaskID: task.Task.ID, Revision: task.Task.Revision, IdempotencyKey: "s"})
	verified, err := service.Verify(ctx, localcontrol.VerifyRequest{
		TaskID: task.Task.ID, Revision: started.Task.Revision, IdempotencyKey: "v",
	})
	if err != nil {
		t.Fatal(err)
	}
	if verified.Task.State != workmodel.Verifying {
		t.Fatalf("state before restart = %s", verified.Task.State)
	}
	if err := data.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.OpenV2Runtime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	restarted := build(reopened, "restart-")
	if err := restarted.RecoverLocalTasks(ctx); err != nil {
		t.Fatal(err)
	}
	observed, err := restarted.Observe(ctx, localcontrol.ObserveRequest{TaskID: task.Task.ID, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if observed.Task.State != workmodel.Verifying {
		t.Fatalf("state after recovery = %s, want verifying — her tap must survive", observed.Task.State)
	}
}
