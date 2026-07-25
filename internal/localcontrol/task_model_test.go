package localcontrol_test

import (
	"context"
	"encoding/json"
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
				{
					ID: "codex", DefaultModel: "gpt-5.6-terra",
					Models: []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"},
					ModelProfiles: []localcontrol.ProviderModel{
						{ID: "gpt-5.6-sol", SupportedReasoningEfforts: []localcontrol.ProviderReasoningEffort{
							{ID: "max", Kind: "reasoning"}, {ID: "ultra", Kind: "orchestration"},
						}},
						{ID: "gpt-5.6-terra", SupportedReasoningEfforts: []localcontrol.ProviderReasoningEffort{
							{ID: "medium", Kind: "reasoning"}, {ID: "ultra", Kind: "orchestration"},
						}},
						{ID: "gpt-5.6-luna", SupportedReasoningEfforts: []localcontrol.ProviderReasoningEffort{
							{ID: "medium", Kind: "reasoning"}, {ID: "max", Kind: "reasoning"},
						}},
					},
					Available: true,
				},
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
			Provider: workmodel.CodexSubscription, Model: "gpt-5.6-sol", Prompt: "run with sol", IdempotencyKey: "task",
		})
		if err != nil {
			t.Fatal(err)
		}
		if created.Task.Model != "gpt-5.6-sol" {
			t.Fatalf("created model = %q", created.Task.Model)
		}
		read, err := service.Observe(context.Background(), localcontrol.ObserveRequest{TaskID: created.Task.ID, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if read.Task.Model != "gpt-5.6-sol" {
			t.Fatalf("reread model = %q, want it to survive the store", read.Task.Model)
		}
	})

	t.Run("execution profile is stored, reported, and included in events", func(t *testing.T) {
		service := newService(t)
		project, board, repository := setup(t, service)
		profile := workmodel.ExecutionProfile{Model: "gpt-5.6-sol", ReasoningEffort: "ultra"}
		created, err := service.CreateTask(context.Background(), localcontrol.CreateTaskRequest{
			ProjectID: project, BoardID: board, RepositoryID: repository,
			Provider: workmodel.CodexSubscription, ExecutionProfile: profile,
			Prompt: "run with automatic delegation", IdempotencyKey: "profile-task",
		})
		if err != nil {
			t.Fatal(err)
		}
		if created.Task.ExecutionProfile != profile || created.Task.Model != profile.Model {
			t.Fatalf("created task = %#v", created.Task)
		}
		observed, err := service.Observe(context.Background(), localcontrol.ObserveRequest{TaskID: created.Task.ID, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if observed.Task.ExecutionProfile != profile {
			t.Fatalf("observed profile = %#v", observed.Task.ExecutionProfile)
		}
		var eventPayload struct {
			ExecutionProfile workmodel.ExecutionProfile `json:"execution_profile"`
		}
		if len(observed.Events) == 0 || json.Unmarshal(observed.Events[0].Payload, &eventPayload) != nil || eventPayload.ExecutionProfile != profile {
			t.Fatalf("task_created events = %#v", observed.Events)
		}
	})

	t.Run("unsupported model and effort combination is refused", func(t *testing.T) {
		service := newService(t)
		project, board, repository := setup(t, service)
		_, err := service.CreateTask(context.Background(), localcontrol.CreateTaskRequest{
			ProjectID: project, BoardID: board, RepositoryID: repository,
			Provider:         workmodel.CodexSubscription,
			ExecutionProfile: workmodel.ExecutionProfile{Model: "gpt-5.6-luna", ReasoningEffort: "ultra"},
			Prompt:           "invalid combination", IdempotencyKey: "invalid-profile",
		})
		if !errors.Is(err, localcontrol.ErrInvalidRequest) {
			t.Fatalf("err = %v, want ErrInvalidRequest", err)
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
