package localcontrol_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/localcontrol"
	"github.com/berkayahi/agentbridge/internal/runtime"
	"github.com/berkayahi/agentbridge/internal/store/sqlite"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

func TestLocalCreateStartObserveApproveVerifyCommit(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "local.db")
	data, err := sqlite.OpenV2Runtime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = data.Close() }()

	now := time.Unix(1_700_000_000, 0).UTC()
	executor := &fakeExecutor{}
	service, err := localcontrol.New(localcontrol.Config{
		Store: data, Runtimes: fakeCatalog{}, Executor: executor,
		Verifier: fakeVerifier{}, Committer: fakeCommitter{}, Clock: func() time.Time { return now },
		NewID: deterministicIDs(),
	})
	if err != nil {
		t.Fatal(err)
	}
	project, err := service.CreateProject(ctx, localcontrol.CreateProjectRequest{Name: "Local project", IdempotencyKey: "project-key"})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := service.RegisterRepository(ctx, localcontrol.RegisterRepositoryRequest{Remote: "origin", IdempotencyKey: "repository-key"})
	if err != nil {
		t.Fatal(err)
	}
	board, err := service.CreateBoard(ctx, localcontrol.CreateBoardRequest{ProjectID: project.Project.ID, Name: "Main", IdempotencyKey: "board-key"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := service.CreateTask(ctx, localcontrol.CreateTaskRequest{
		ProjectID: project.Project.ID, BoardID: board.Board.ID, RepositoryID: repository.Repository.ID,
		Provider: workmodel.CodexSubscription, Title: "Ship local control", Prompt: "run the local slice", IdempotencyKey: "task-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	if task.Task.Revision != 1 || task.Task.State != workmodel.Queued || task.Task.ExecutionID == "" || task.Task.SessionID == "" {
		t.Fatalf("created task = %#v", task.Task)
	}
	duplicateTask, err := service.CreateTask(ctx, localcontrol.CreateTaskRequest{
		ProjectID: project.Project.ID, BoardID: board.Board.ID, RepositoryID: repository.Repository.ID,
		Provider: workmodel.CodexSubscription, Title: "Ship local control", Prompt: "run the local slice", IdempotencyKey: "task-key",
	})
	if err != nil || duplicateTask.Task.ID != task.Task.ID || duplicateTask.Task.ExecutionID != task.Task.ExecutionID || duplicateTask.Task.SessionID != task.Task.SessionID {
		t.Fatalf("idempotent task creation = %#v err=%v, want the original canonical lineage", duplicateTask, err)
	}
	updated, err := service.UpdateTask(ctx, localcontrol.UpdateTaskRequest{
		TaskID: task.Task.ID, Revision: task.Task.Revision, Title: "Ship the edited local control", Prompt: "run the edited local slice", IdempotencyKey: "update-key",
	})
	if err != nil || updated.Task.Revision != 2 || updated.Task.Title != "Ship the edited local control" || updated.Event.Type != "task_updated" {
		t.Fatalf("updated task = %#v err=%v", updated, err)
	}
	if _, err := service.UpdateTask(ctx, localcontrol.UpdateTaskRequest{
		TaskID: task.Task.ID, Revision: task.Task.Revision, Title: "stale", Prompt: "stale", IdempotencyKey: "stale-update",
	}); !errors.Is(err, localcontrol.ErrStaleRevision) {
		t.Fatalf("stale update error = %v, want ErrStaleRevision", err)
	}
	task.Task = updated.Task

	started, err := service.Start(ctx, localcontrol.StartRequest{TaskID: task.Task.ID, Revision: task.Task.Revision, IdempotencyKey: "start-key"})
	if err != nil {
		t.Fatal(err)
	}
	if started.Task.State != workmodel.Running || len(executor.started) != 1 {
		t.Fatalf("started task = %#v executor=%#v", started.Task, executor)
	}
	if _, err := service.Start(ctx, localcontrol.StartRequest{TaskID: task.Task.ID, Revision: task.Task.Revision, IdempotencyKey: "stale-start"}); !errors.Is(err, localcontrol.ErrStaleRevision) {
		t.Fatalf("stale start error = %v, want ErrStaleRevision", err)
	}
	if err := data.Close(); err != nil {
		t.Fatal(err)
	}
	data, err = sqlite.OpenV2Runtime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	restartIDs := deterministicIDs()
	service, err = localcontrol.New(localcontrol.Config{
		Store: data, Runtimes: fakeCatalog{}, Executor: executor,
		Verifier: fakeVerifier{}, Committer: fakeCommitter{}, Clock: func() time.Time { return now },
		NewID: func(prefix string) string { return "restart-" + restartIDs(prefix) },
	})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := service.Start(ctx, localcontrol.StartRequest{TaskID: task.Task.ID, Revision: task.Task.Revision, IdempotencyKey: "start-key"})
	if err != nil || replayed.Event.ID != started.Event.ID || len(executor.started) != 1 {
		t.Fatalf("replayed start = %#v err=%v executor=%#v", replayed, err, executor)
	}

	observed, err := service.Observe(ctx, localcontrol.ObserveRequest{TaskID: task.Task.ID, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(observed.Events) < 3 || observed.NextCursor == 0 {
		t.Fatalf("observed = %#v", observed)
	}
	observedAgain, err := service.Observe(ctx, localcontrol.ObserveRequest{TaskID: task.Task.ID, AfterCursor: observed.Events[0].Cursor, Limit: 20})
	if err != nil || len(observedAgain.Events) >= len(observed.Events) {
		t.Fatalf("cursor observe = %#v err=%v", observedAgain, err)
	}

	approvalNow := now.Add(time.Second)
	if err := data.UpsertApproval(ctx, workmodel.Approval{ID: "approval-1", TaskID: task.Task.ID, Kind: "command", Status: workmodel.ApprovalPending, RequestPayload: []byte(`{"summary":"go test"}`), RequestedAt: approvalNow, ExpiresAt: timePtr(approvalNow.Add(time.Minute))}); err != nil {
		t.Fatal(err)
	}
	current, err := service.Observe(ctx, localcontrol.ObserveRequest{TaskID: task.Task.ID})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := service.Approve(ctx, localcontrol.ApproveRequest{TaskID: task.Task.ID, ApprovalID: "approval-1", Revision: current.Task.Revision, UserID: "local-operator", Allow: true, IdempotencyKey: "approve-key"})
	if err != nil {
		t.Fatal(err)
	}
	if approved.Task.State != workmodel.Running || !executor.approved {
		t.Fatalf("approved task = %#v executor=%#v", approved.Task, executor)
	}

	verified, err := service.Verify(ctx, localcontrol.VerifyRequest{TaskID: task.Task.ID, Revision: approved.Task.Revision, IdempotencyKey: "verify-key"})
	if err != nil || !verified.Receipt.Passed || verified.Task.State != workmodel.Verifying {
		t.Fatalf("verified = %#v err=%v", verified, err)
	}
	committed, err := service.Commit(ctx, localcontrol.CommitRequest{TaskID: task.Task.ID, Revision: verified.Task.Revision, IdempotencyKey: "commit-key"})
	if err != nil {
		t.Fatal(err)
	}
	if committed.Task.State != workmodel.Completed || committed.Receipt.CommitSHA == "" || committed.Task.CommitSHA == "" {
		t.Fatalf("committed = %#v", committed)
	}

	duplicate, err := service.Commit(ctx, localcontrol.CommitRequest{TaskID: task.Task.ID, Revision: verified.Task.Revision, IdempotencyKey: "commit-key"})
	if err != nil || duplicate.Receipt.ID != committed.Receipt.ID {
		t.Fatalf("duplicate commit = %#v err=%v", duplicate, err)
	}
	if err := data.Close(); err != nil {
		t.Fatal(err)
	}
	data, err = sqlite.OpenV2Runtime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	postRestartIDs := deterministicIDs()
	restarted, err := localcontrol.New(localcontrol.Config{
		Store: data, Runtimes: fakeCatalog{}, Executor: executor,
		Verifier: fakeVerifier{}, Committer: fakeCommitter{}, Clock: func() time.Time { return now },
		NewID: func(prefix string) string { return "post-restart-" + postRestartIDs(prefix) },
	})
	if err != nil {
		t.Fatal(err)
	}
	replayedCommit, err := restarted.Commit(ctx, localcontrol.CommitRequest{TaskID: task.Task.ID, Revision: verified.Task.Revision, IdempotencyKey: "commit-key"})
	if err != nil || replayedCommit.Receipt.ID != committed.Receipt.ID {
		t.Fatalf("replayed commit = %#v err=%v", replayedCommit, err)
	}
}

func TestLocalCancelIsDurableBeforeExecutorInterruption(t *testing.T) {
	ctx := context.Background()
	data, err := sqlite.OpenV2Runtime(ctx, filepath.Join(t.TempDir(), "cancel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	service, err := localcontrol.New(localcontrol.Config{Store: data, Runtimes: fakeCatalog{}, Executor: &fakeExecutor{}, NewID: deterministicIDs()})
	if err != nil {
		t.Fatal(err)
	}
	project, _ := service.CreateProject(ctx, localcontrol.CreateProjectRequest{Name: "p", IdempotencyKey: "p"})
	repo, _ := service.RegisterRepository(ctx, localcontrol.RegisterRepositoryRequest{Remote: "origin", IdempotencyKey: "r"})
	board, _ := service.CreateBoard(ctx, localcontrol.CreateBoardRequest{ProjectID: project.Project.ID, Name: "b", IdempotencyKey: "b"})
	task, err := service.CreateTask(ctx, localcontrol.CreateTaskRequest{ProjectID: project.Project.ID, BoardID: board.Board.ID, RepositoryID: repo.Repository.ID, Provider: workmodel.CodexSubscription, Prompt: "cancel", IdempotencyKey: "t"})
	if err != nil {
		t.Fatal(err)
	}
	started, err := service.Start(ctx, localcontrol.StartRequest{TaskID: task.Task.ID, Revision: task.Task.Revision, IdempotencyKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	canceled, err := service.Cancel(ctx, localcontrol.CancelRequest{TaskID: task.Task.ID, Revision: started.Task.Revision, IdempotencyKey: "c"})
	if err != nil || canceled.Task.State != workmodel.Canceled {
		t.Fatalf("canceled = %#v err=%v", canceled, err)
	}
}

func TestLocalAuthorityRejectsStandaloneOwnedTask(t *testing.T) {
	ctx := context.Background()
	data, err := sqlite.OpenV2Runtime(ctx, filepath.Join(t.TempDir(), "standalone-owner.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	now := time.Unix(1_700_000_100, 0).UTC()
	if err := data.EnsureRepositoryBinding(ctx, "standalone-repo", "origin"); err != nil {
		t.Fatal(err)
	}
	task := workmodel.Task{
		ID: "standalone-owned", RepoProfileID: "standalone-repo", Title: "Standalone", Prompt: "telegram task",
		State: workmodel.Queued, Provider: workmodel.CodexSubscription, CreatedAt: now, UpdatedAt: now,
	}
	if err := data.CreateTask(ctx, task, workmodel.Event{ID: "standalone-created", TaskID: task.ID, Type: workmodel.EventTaskCreated, Visibility: workmodel.VisibilityUser, Payload: []byte(`{"state":"queued"}`), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	service, err := localcontrol.New(localcontrol.Config{Store: data, Runtimes: fakeCatalog{}, NewID: deterministicIDs()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Observe(ctx, localcontrol.ObserveRequest{TaskID: task.ID}); !errors.Is(err, localcontrol.ErrTaskOwnedByAnotherController) {
		t.Fatalf("observe standalone-owned task = %v, want ErrTaskOwnedByAnotherController", err)
	}
}

type fakeCatalog struct{}

func (fakeCatalog) Get(string) (runtime.Adapter, error) { return nil, nil }

type fakeExecutor struct {
	started      []string
	approved     bool
	approvedUser string
}

func (f *fakeExecutor) Start(_ context.Context, task localcontrol.TaskView, _ localcontrol.StartRequest) error {
	f.started = append(f.started, task.ID)
	return nil
}
func (*fakeExecutor) Resume(context.Context, localcontrol.TaskView, localcontrol.ResumeRequest) error {
	return nil
}
func (f *fakeExecutor) Approve(_ context.Context, _ localcontrol.TaskView, _ string, userID string, _ bool) error {
	f.approved = true
	f.approvedUser = userID
	return nil
}
func (*fakeExecutor) Cancel(context.Context, localcontrol.TaskView) error { return nil }

type fakeVerifier struct{}

func (fakeVerifier) Verify(_ context.Context, _ localcontrol.TaskView) (localcontrol.VerificationReceipt, error) {
	return localcontrol.VerificationReceipt{ID: "verification-1", Passed: true, Summary: "verified"}, nil
}

type fakeCommitter struct{}

func (fakeCommitter) Commit(_ context.Context, _ localcontrol.TaskView) (localcontrol.CommitReceipt, error) {
	return localcontrol.CommitReceipt{ID: "commit-1", CommitSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RemoteRef: "refs/heads/task/local", ObservedAt: time.Unix(1_700_000_010, 0).UTC()}, nil
}

func deterministicIDs() func(string) string {
	counts := make(map[string]int)
	return func(prefix string) string {
		counts[prefix]++
		return prefix + "-" + string(rune('a'+counts[prefix]-1))
	}
}

func timePtr(value time.Time) *time.Time { return &value }
