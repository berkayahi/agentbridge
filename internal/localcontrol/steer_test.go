package localcontrol_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/localcontrol"
	"github.com/berkayahi/agentbridge/internal/store/sqlite"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

type steeringExecutor struct {
	fakeExecutor
	steered []string
	err     error
}

func (e *steeringExecutor) Steer(_ context.Context, view localcontrol.TaskView, request localcontrol.SteerRequest) error {
	if e.err != nil {
		return e.err
	}
	e.steered = append(e.steered, request.Input)
	return nil
}

func flyingTask(t *testing.T, executor localcontrol.Executor) (*localcontrol.Service, localcontrol.TaskView) {
	t.Helper()
	ctx := context.Background()
	data, err := sqlite.OpenV2Runtime(ctx, filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = data.Close() })
	now := time.Unix(1_700_000_000, 0).UTC()
	service, err := localcontrol.New(localcontrol.Config{
		Store: data, Runtimes: fakeCatalog{}, Executor: executor,
		Verifier: fakeVerifier{}, Committer: fakeCommitter{},
		Clock: func() time.Time { return now }, NewID: deterministicIDs(),
	})
	if err != nil {
		t.Fatal(err)
	}
	project, err := service.CreateProject(ctx, localcontrol.CreateProjectRequest{Name: "Hive", IdempotencyKey: "p"})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := service.RegisterRepository(ctx, localcontrol.RegisterRepositoryRequest{Remote: "origin", IdempotencyKey: "r"})
	if err != nil {
		t.Fatal(err)
	}
	board, err := service.CreateBoard(ctx, localcontrol.CreateBoardRequest{ProjectID: project.Project.ID, Name: "Comb", IdempotencyKey: "b"})
	if err != nil {
		t.Fatal(err)
	}
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
	return service, started.Task
}

// The keeper must be able to say one more thing to a bee already in the air,
// and it must be durable: a steer that leaves no trace makes the flight log
// lie about why she changed course.
func TestSteerReachesTheFlyingBeeAndIsDurable(t *testing.T) {
	executor := &steeringExecutor{}
	service, view := flyingTask(t, executor)
	ctx := context.Background()
	response, err := service.Steer(ctx, localcontrol.SteerRequest{
		TaskID: view.ID, Revision: view.Revision, Input: "keep it smaller", IdempotencyKey: "steer-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(executor.steered) != 1 || executor.steered[0] != "keep it smaller" {
		t.Fatalf("executor saw %#v", executor.steered)
	}
	if response.Event.Type != "steered" {
		t.Fatalf("event = %#v", response.Event)
	}
	observed, err := service.Observe(ctx, localcontrol.ObserveRequest{TaskID: view.ID, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range observed.Events {
		if event.Type == "steered" {
			found = true
		}
	}
	if !found {
		t.Fatal("the steer left no durable event")
	}
}

// Steering is only meaningful while she is flying.
func TestSteerRefusesATaskThatIsNotFlying(t *testing.T) {
	executor := &steeringExecutor{}
	service, view := flyingTask(t, executor)
	ctx := context.Background()
	if _, err := service.Cancel(ctx, localcontrol.CancelRequest{TaskID: view.ID, Revision: view.Revision, IdempotencyKey: "c"}); err != nil {
		t.Fatal(err)
	}
	current, err := service.Observe(ctx, localcontrol.ObserveRequest{TaskID: view.ID, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Steer(ctx, localcontrol.SteerRequest{
		TaskID: view.ID, Revision: current.Task.Revision, Input: "too late", IdempotencyKey: "steer-late",
	}); err == nil {
		t.Fatal("steering a canceled bee must fail")
	}
	if len(executor.steered) != 0 {
		t.Fatalf("executor was called anyway: %#v", executor.steered)
	}
}

// An empty instruction is not a steer.
func TestSteerRequiresAnInstruction(t *testing.T) {
	service, view := flyingTask(t, &steeringExecutor{})
	if _, err := service.Steer(context.Background(), localcontrol.SteerRequest{
		TaskID: view.ID, Revision: view.Revision, Input: "   ", IdempotencyKey: "steer-empty",
	}); !errors.Is(err, localcontrol.ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
}

// A runtime that cannot steer must say so rather than silently accepting.
func TestSteerRequiresASteerableRuntime(t *testing.T) {
	service, view := flyingTask(t, &fakeExecutor{})
	if _, err := service.Steer(context.Background(), localcontrol.SteerRequest{
		TaskID: view.ID, Revision: view.Revision, Input: "keep it smaller", IdempotencyKey: "steer-unsupported",
	}); !errors.Is(err, localcontrol.ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}
