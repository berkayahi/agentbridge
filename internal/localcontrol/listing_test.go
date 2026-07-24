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

// A hive must be discoverable after a restart: a client that did not create a
// bee still has to find her, or the board can only ever show this session.
func TestListingSurvivesARestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "local.db")
	data, err := sqlite.OpenV2Runtime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	newService := func(store *sqlite.RuntimeStore, prefix string) *localcontrol.Service {
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
	service := newService(data, "")
	project, err := service.CreateProject(ctx, localcontrol.CreateProjectRequest{Name: "Hive", IdempotencyKey: "project-key"})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := service.RegisterRepository(ctx, localcontrol.RegisterRepositoryRequest{Remote: "origin", IdempotencyKey: "repository-key"})
	if err != nil {
		t.Fatal(err)
	}
	board, err := service.CreateBoard(ctx, localcontrol.CreateBoardRequest{ProjectID: project.Project.ID, Name: "Comb", IdempotencyKey: "board-key"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := service.CreateTask(ctx, localcontrol.CreateTaskRequest{
		ProjectID: project.Project.ID, BoardID: board.Board.ID, RepositoryID: repository.Repository.ID,
		Provider: workmodel.Provider("codex"), Prompt: "align the widget", IdempotencyKey: "task-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := data.Close(); err != nil {
		t.Fatal(err)
	}

	// A fresh process, as if the daemon restarted.
	reopened, err := sqlite.OpenV2Runtime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	restarted := newService(reopened, "restart-")

	projects, err := restarted.ListProjects(ctx)
	if err != nil || len(projects.Projects) != 1 || projects.Projects[0].ID != project.Project.ID {
		t.Fatalf("projects = %#v err=%v", projects, err)
	}
	boards, err := restarted.ListBoards(ctx, project.Project.ID)
	if err != nil || len(boards.Boards) != 1 || boards.Boards[0].ID != board.Board.ID {
		t.Fatalf("boards = %#v err=%v", boards, err)
	}
	tasks, err := restarted.ListTasks(ctx, localcontrol.TaskFilter{})
	if err != nil || len(tasks.Tasks) != 1 || tasks.Tasks[0].ID != task.Task.ID {
		t.Fatalf("tasks = %#v err=%v", tasks, err)
	}
	if tasks.Tasks[0].BoardID != board.Board.ID || tasks.Tasks[0].TargetDeviceID != localcontrol.LocalDeviceID {
		t.Fatalf("task view lost its board or device: %#v", tasks.Tasks[0])
	}

	// Filters must actually filter, or a board would show every bee in every column.
	byBoard, err := restarted.ListTasks(ctx, localcontrol.TaskFilter{BoardID: board.Board.ID})
	if err != nil || len(byBoard.Tasks) != 1 {
		t.Fatalf("by board = %#v err=%v", byBoard, err)
	}
	byOtherState, err := restarted.ListTasks(ctx, localcontrol.TaskFilter{States: []workmodel.State{workmodel.Completed}})
	if err != nil || len(byOtherState.Tasks) != 0 {
		t.Fatalf("by state = %#v err=%v", byOtherState, err)
	}
	byOtherDevice, err := restarted.ListTasks(ctx, localcontrol.TaskFilter{TargetDeviceID: "build-pi"})
	if err != nil || len(byOtherDevice.Tasks) != 0 {
		t.Fatalf("by device = %#v err=%v", byOtherDevice, err)
	}

	// The hive feed is the only way project and board events are readable at
	// all: those rows carry no task id.
	hive, err := restarted.ObserveHive(ctx, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, event := range hive.Events {
		seen[event.Type] = true
	}
	for _, want := range []string{"project_created", "repository_registered", "board_created", "task_created"} {
		if !seen[want] {
			t.Fatalf("hive feed is missing %q: %#v", want, seen)
		}
	}
	if hive.NextCursor == 0 {
		t.Fatal("hive feed reported no cursor to resume from")
	}

	// Resuming from the cursor must not repeat what the client already has.
	rest, err := restarted.ObserveHive(ctx, hive.NextCursor, 50)
	if err != nil || len(rest.Events) != 0 {
		t.Fatalf("resumed feed = %#v err=%v", rest, err)
	}
}
