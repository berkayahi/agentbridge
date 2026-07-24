package localcontrol_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/localcontrol"
	"github.com/berkayahi/agentbridge/internal/store/sqlite"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

const localHelperEnv = "AGENTBRIDGE_LOCAL_CONTROL_HELPER"

func TestLocalProcessRestartRecovery(t *testing.T) {
	if os.Getenv(localHelperEnv) == "1" {
		runLocalControlHelper(t)
		return
	}

	root, err := os.MkdirTemp("/tmp", "ab-local-mvp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	database := filepath.Join(root, "local.db")
	socket := filepath.Join(root, "local.sock")
	secretPath := filepath.Join(root, "local.secret")
	secret := []byte("01234567890123456789012345678901")
	if err := os.WriteFile(secretPath, secret, 0o600); err != nil {
		t.Fatal(err)
	}

	process := startLocalControlHelper(t, database, socket)
	client := newProcessClient(t, socket, secretPath)
	project, err := client.CreateProject(context.Background(), localcontrol.CreateProjectRequest{Name: "Process project", IdempotencyKey: "process-project"})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := client.RegisterRepository(context.Background(), localcontrol.RegisterRepositoryRequest{Remote: "origin", IdempotencyKey: "process-repository"})
	if err != nil {
		t.Fatal(err)
	}
	board, err := client.CreateBoard(context.Background(), localcontrol.CreateBoardRequest{ProjectID: project.Project.ID, Name: "Main", IdempotencyKey: "process-board"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := client.CreateTask(context.Background(), localcontrol.CreateTaskRequest{
		ProjectID: project.Project.ID, BoardID: board.Board.ID, RepositoryID: repository.Repository.ID,
		Provider: workmodel.CodexSubscription, Prompt: "process restart slice", IdempotencyKey: "process-task",
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := client.Start(context.Background(), localcontrol.StartRequest{TaskID: task.Task.ID, Revision: task.Task.Revision, IdempotencyKey: "process-start"})
	if err != nil || started.Task.State != workmodel.Running {
		t.Fatalf("start = %#v err=%v", started, err)
	}
	client.Close()
	stopLocalControlHelper(t, process)

	process = startLocalControlHelper(t, database, socket)
	client = newProcessClient(t, socket, secretPath)
	replayedStart, err := client.Start(context.Background(), localcontrol.StartRequest{TaskID: task.Task.ID, Revision: task.Task.Revision, IdempotencyKey: "process-start"})
	if err != nil || replayedStart.Event.ID != started.Event.ID {
		t.Fatalf("replayed start = %#v err=%v", replayedStart, err)
	}
	approved, err := client.Approve(context.Background(), localcontrol.ApproveRequest{TaskID: task.Task.ID, ApprovalID: "approval-process", Revision: replayedStart.Task.Revision, UserID: "desktop", Allow: true, IdempotencyKey: "process-approve"})
	if err != nil || approved.Task.State != workmodel.Running {
		t.Fatalf("approve = %#v err=%v", approved, err)
	}
	client.Close()
	stopLocalControlHelper(t, process)

	process = startLocalControlHelper(t, database, socket)
	client = newProcessClient(t, socket, secretPath)
	verified, err := client.Verify(context.Background(), localcontrol.VerifyRequest{TaskID: task.Task.ID, Revision: approved.Task.Revision, IdempotencyKey: "process-verify"})
	if err != nil || !verified.Receipt.Passed || verified.Task.State != workmodel.Verifying {
		t.Fatalf("verify = %#v err=%v", verified, err)
	}
	client.Close()
	stopLocalControlHelper(t, process)

	process = startLocalControlHelper(t, database, socket)
	client = newProcessClient(t, socket, secretPath)
	committed, err := client.Commit(context.Background(), localcontrol.CommitRequest{TaskID: task.Task.ID, Revision: verified.Task.Revision, IdempotencyKey: "process-commit"})
	if err != nil || committed.Task.State != workmodel.Completed || committed.Receipt.CommitSHA == "" {
		t.Fatalf("commit = %#v err=%v", committed, err)
	}
	observed, err := client.Observe(context.Background(), localcontrol.ObserveRequest{TaskID: task.Task.ID, Limit: 200})
	if err != nil || len(observed.Events) < 8 || observed.Task.ID != task.Task.ID || observed.Task.ProjectID != project.Project.ID || observed.Task.RepositoryID != repository.Repository.ID {
		t.Fatalf("observed = %#v err=%v", observed, err)
	}
	client.Close()
	stopLocalControlHelper(t, process)
}

func runLocalControlHelper(t *testing.T) {
	t.Helper()
	database := os.Getenv("AGENTBRIDGE_LOCAL_CONTROL_DATABASE")
	socket := os.Getenv("AGENTBRIDGE_LOCAL_CONTROL_SOCKET")
	if database == "" || socket == "" {
		t.Fatal("local control helper paths are missing")
	}
	release, err := sqlite.AcquireDatabaseRuntimeLock(database)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	data, err := sqlite.OpenV2WithRuntimeLock(context.Background(), database)
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	service, err := localcontrol.New(localcontrol.Config{
		Store: data, Runtimes: fakeCatalog{}, Executor: &processExecutor{store: data},
		Verifier: processVerifier{}, Committer: processCommitter{},
	})
	if err != nil {
		t.Fatal(err)
	}
	secret, err := os.ReadFile(os.Getenv("AGENTBRIDGE_LOCAL_CONTROL_SECRET"))
	if err != nil {
		t.Fatal(err)
	}
	handler, err := localcontrol.NewHTTPHandler(service, secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := localcontrol.ServeUnix(context.Background(), socket, handler); err != nil {
		t.Fatal(err)
	}
}

type processExecutor struct{ store *sqlite.RuntimeStore }

func (e *processExecutor) Start(ctx context.Context, view localcontrol.TaskView, _ localcontrol.StartRequest) error {
	now := time.Now().UTC()
	return e.store.UpsertApproval(ctx, workmodel.Approval{ID: "approval-process", TaskID: view.ID, Kind: "fake", Status: workmodel.ApprovalPending, RequestPayload: []byte(`{"summary":"fake approval"}`), RequestedAt: now, ExpiresAt: timePtr(now.Add(time.Hour))})
}
func (*processExecutor) Resume(context.Context, localcontrol.TaskView, localcontrol.ResumeRequest) error {
	return nil
}
func (*processExecutor) Approve(context.Context, localcontrol.TaskView, string, string, bool) error {
	return nil
}
func (*processExecutor) Cancel(context.Context, localcontrol.TaskView) error { return nil }

type processVerifier struct{}

func (processVerifier) Verify(context.Context, localcontrol.TaskView) (localcontrol.VerificationReceipt, error) {
	return localcontrol.VerificationReceipt{ID: "verification-process", Passed: true, Summary: "fake verification", ObservedAt: time.Now().UTC()}, nil
}

type processCommitter struct{}

func (processCommitter) Commit(context.Context, localcontrol.TaskView) (localcontrol.CommitReceipt, error) {
	return localcontrol.CommitReceipt{ID: "commit-process", CommitSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", RemoteRef: "refs/heads/task/local", ObservedAt: time.Now().UTC()}, nil
}

func startLocalControlHelper(t *testing.T, database, socket string) *exec.Cmd {
	t.Helper()
	if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=TestLocalProcessRestartRecovery", "--")
	command.Env = append(os.Environ(), localHelperEnv+"=1", "AGENTBRIDGE_LOCAL_CONTROL_DATABASE="+database, "AGENTBRIDGE_LOCAL_CONTROL_SOCKET="+socket, "AGENTBRIDGE_LOCAL_CONTROL_SECRET="+filepath.Join(filepath.Dir(database), "local.secret"))
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(socket); err == nil {
			return command
		}
		if command.ProcessState != nil {
			t.Fatalf("local control helper exited: %v", command.ProcessState)
		}
		if time.Now().After(deadline) {
			_ = command.Process.Kill()
			_ = command.Wait()
			t.Fatal("local control helper did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func newProcessClient(t *testing.T, socket, secretPath string) *localcontrol.Client {
	t.Helper()
	client, err := localcontrol.NewUnixClient(socket, secretPath)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func stopLocalControlHelper(t *testing.T, command *exec.Cmd) {
	t.Helper()
	if command == nil || command.Process == nil {
		return
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("local control helper exited cleanly after forced restart")
	}
}
