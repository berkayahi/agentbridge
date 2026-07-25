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

// The keeper chooses the model when they send a bee, so the choice has to
// survive as a fact about the task: it is reported back, and a bee resumed
// after a restart must fly the model she left with. A model nobody configured
// is refused outright — silently falling back would fly something the keeper
// did not ask for and report a model that never ran.
func TestTaskCarriesTheChosenModel(t *testing.T) {
	newService := func(t *testing.T) *localcontrol.Service {
		t.Helper()
		data, err := sqlite.OpenV2Runtime(context.Background(), filepath.Join(t.TempDir(), "local.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = data.Close() })
		now := time.Unix(1_700_000_000, 0).UTC()
		service, err := localcontrol.New(localcontrol.Config{
			Store: data, Runtimes: fakeCatalog{}, Executor: &fakeExecutor{},
			Providers: fakeProviderCatalog{providers: []localcontrol.ProviderInfo{
				{ID: "codex", DefaultModel: "gpt-5.6-terra", Models: []string{"gpt-5.6-terra", "gpt-5.7-sol"}, Available: true},
			}},
			Clock: func() time.Time { return now }, NewID: deterministicIDs(),
		})
		if err != nil {
			t.Fatal(err)
		}
		return service
	}
	setup := func(t *testing.T, service *localcontrol.Service) (string, string, string) {
		t.Helper()
		ctx := context.Background()
		project, err := service.CreateProject(ctx, localcontrol.CreateProjectRequest{Name: "Hive", IdempotencyKey: "project"})
		if err != nil {
			t.Fatal(err)
		}
		repository, err := service.RegisterRepository(ctx, localcontrol.RegisterRepositoryRequest{Remote: "origin", IdempotencyKey: "repository"})
		if err != nil {
			t.Fatal(err)
		}
		board, err := service.CreateBoard(ctx, localcontrol.CreateBoardRequest{ProjectID: project.Project.ID, Name: "Comb", IdempotencyKey: "board"})
		if err != nil {
			t.Fatal(err)
		}
		return project.Project.ID, board.Board.ID, repository.Repository.ID
	}

	t.Run("a chosen model is stored and reported", func(t *testing.T) {
		service := newService(t)
		project, board, repository := setup(t, service)
		created, err := service.CreateTask(context.Background(), localcontrol.CreateTaskRequest{
			ProjectID: project, BoardID: board, RepositoryID: repository,
			Provider: workmodel.CodexSubscription, Model: "gpt-5.7-sol", Prompt: "fly with sol", IdempotencyKey: "task",
		})
		if err != nil {
			t.Fatal(err)
		}
		if created.Task.Model != "gpt-5.7-sol" {
			t.Fatalf("created model = %q", created.Task.Model)
		}
		read, err := service.Observe(context.Background(), localcontrol.ObserveRequest{TaskID: created.Task.ID, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if read.Task.Model != "gpt-5.7-sol" {
			t.Fatalf("reread model = %q, want it to survive the store", read.Task.Model)
		}
	})

	t.Run("no choice means the provider default", func(t *testing.T) {
		service := newService(t)
		project, board, repository := setup(t, service)
		created, err := service.CreateTask(context.Background(), localcontrol.CreateTaskRequest{
			ProjectID: project, BoardID: board, RepositoryID: repository,
			Provider: workmodel.CodexSubscription, Prompt: "fly as before", IdempotencyKey: "task",
		})
		if err != nil {
			t.Fatal(err)
		}
		if created.Task.Model != "" {
			t.Fatalf("model = %q, want empty so the provider default applies", created.Task.Model)
		}
	})

	t.Run("an unconfigured model is refused", func(t *testing.T) {
		service := newService(t)
		project, board, repository := setup(t, service)
		_, err := service.CreateTask(context.Background(), localcontrol.CreateTaskRequest{
			ProjectID: project, BoardID: board, RepositoryID: repository,
			Provider: workmodel.CodexSubscription, Model: "gpt-9-imaginary", Prompt: "fly with a dream", IdempotencyKey: "task",
		})
		if !errors.Is(err, localcontrol.ErrInvalidRequest) {
			t.Fatalf("err = %v, want ErrInvalidRequest", err)
		}
	})
}
