package localcontrol_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/localcontrol"
	"github.com/berkayahi/agentbridge/internal/store/sqlite"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

func TestRemoteCommandQueuesOnDisconnectAndReplaysAfterReconnect(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0).UTC()
	data, err := sqlite.OpenV2Runtime(ctx, filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	if err := data.CreateDevice(ctx, localcontrol.Device{
		ID: "pi-queue", Name: "Queue Pi", Kind: localcontrol.DeviceKindRaspberryPi,
		Fingerprint: "pi-queue-fingerprint", State: localcontrol.DeviceStatePaired,
		ConnectionEpoch: 1, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}, nil); err != nil {
		t.Fatal(err)
	}
	remote := &queueDeviceRuntime{unavailable: true}
	service, err := localcontrol.New(localcontrol.Config{
		Store: data, Runtimes: fakeCatalog{}, Executor: &fakeExecutor{}, Verifier: fakeVerifier{}, Committer: fakeCommitter{},
		RemoteDevices: map[string]localcontrol.DeviceRuntime{"pi-queue": remote}, Clock: func() time.Time { return now }, NewID: deterministicIDs(),
	})
	if err != nil {
		t.Fatal(err)
	}
	project, err := service.CreateProject(ctx, localcontrol.CreateProjectRequest{Name: "Queue project", IdempotencyKey: "queue-project"})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := service.RegisterRepository(ctx, localcontrol.RegisterRepositoryRequest{Remote: "origin", IdempotencyKey: "queue-repository"})
	if err != nil {
		t.Fatal(err)
	}
	board, err := service.CreateBoard(ctx, localcontrol.CreateBoardRequest{ProjectID: project.Project.ID, Name: "Queue", IdempotencyKey: "queue-board"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := service.CreateTask(ctx, localcontrol.CreateTaskRequest{
		ProjectID: project.Project.ID, BoardID: board.Board.ID, RepositoryID: repository.Repository.ID,
		TargetDeviceID: "pi-queue", Provider: workmodel.CodexSubscription, Prompt: "queue this task", IdempotencyKey: "queue-task",
	})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := service.Start(ctx, localcontrol.StartRequest{TaskID: task.Task.ID, Revision: task.Task.Revision, IdempotencyKey: "queue-start"})
	if err != nil || !queued.Queued || queued.Task.State != workmodel.Paused {
		t.Fatalf("queued start = %#v err=%v", queued, err)
	}
	requeued, err := service.Start(ctx, localcontrol.StartRequest{TaskID: task.Task.ID, Revision: task.Task.Revision, IdempotencyKey: "queue-start"})
	if err != nil || !requeued.Queued || requeued.Event.ID != queued.Event.ID || remote.calls != 1 {
		t.Fatalf("queued start replay = %#v err=%v calls=%d", requeued, err, remote.calls)
	}
	pending, err := data.ListPendingDeviceCommands(ctx, "pi-queue", 10)
	if err != nil || len(pending) != 1 || pending[0].State != localcontrol.DeviceCommandPending {
		t.Fatalf("pending commands = %#v err=%v", pending, err)
	}
	if remote.calls != 1 {
		t.Fatalf("remote calls after disconnect = %d, want 1", remote.calls)
	}

	replayed, err := service.ReplayDeviceCommands(ctx, localcontrol.ReplayDeviceCommandsRequest{DeviceID: "pi-queue"})
	if err != nil || replayed.Replayed != 1 || len(replayed.Pending) != 0 {
		record, recordErr := data.GetDeviceCommand(ctx, "queue-start")
		observed, observeErr := service.Observe(ctx, localcontrol.ObserveRequest{TaskID: task.Task.ID, Limit: 50})
		t.Fatalf("replayed commands = %#v err=%v record=%#v recordErr=%v task=%#v observeErr=%v", replayed, err, record, recordErr, observed.Task, observeErr)
	}
	completedReplay, err := service.Start(ctx, localcontrol.StartRequest{TaskID: task.Task.ID, Revision: task.Task.Revision, IdempotencyKey: "queue-start"})
	if err != nil || completedReplay.Queued || completedReplay.Task.State != workmodel.Running {
		t.Fatalf("completed queue replay = %#v err=%v", completedReplay, err)
	}
	observed, err := service.Observe(ctx, localcontrol.ObserveRequest{TaskID: task.Task.ID, Limit: 50})
	if err != nil || observed.Task.State != workmodel.Running {
		t.Fatalf("replayed task = %#v err=%v", observed.Task, err)
	}
}

func TestRemoteStartReplayRecoversPreparedTaskAfterControllerRestart(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_800_000_500, 0).UTC()
	data, err := sqlite.OpenV2Runtime(ctx, filepath.Join(t.TempDir(), "prepared-replay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	if err := data.CreateDevice(ctx, localcontrol.Device{
		ID: "pi-prepared", Name: "Prepared Pi", Kind: localcontrol.DeviceKindRaspberryPi,
		Fingerprint: "pi-prepared-fingerprint", State: localcontrol.DeviceStatePaired,
		ConnectionEpoch: 1, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}, nil); err != nil {
		t.Fatal(err)
	}
	remote := &queueDeviceRuntime{unavailable: true}
	service, err := localcontrol.New(localcontrol.Config{
		Store: data, Runtimes: fakeCatalog{}, Executor: &fakeExecutor{}, Verifier: fakeVerifier{}, Committer: fakeCommitter{},
		RemoteDevices: map[string]localcontrol.DeviceRuntime{"pi-prepared": remote}, Clock: func() time.Time { return now }, NewID: deterministicIDs(),
	})
	if err != nil {
		t.Fatal(err)
	}
	project, err := service.CreateProject(ctx, localcontrol.CreateProjectRequest{Name: "Prepared replay", IdempotencyKey: "prepared-project"})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := service.RegisterRepository(ctx, localcontrol.RegisterRepositoryRequest{Remote: "origin", IdempotencyKey: "prepared-repository"})
	if err != nil {
		t.Fatal(err)
	}
	board, err := service.CreateBoard(ctx, localcontrol.CreateBoardRequest{ProjectID: project.Project.ID, Name: "Recovery", IdempotencyKey: "prepared-board"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := service.CreateTask(ctx, localcontrol.CreateTaskRequest{
		ProjectID: project.Project.ID, BoardID: board.Board.ID, RepositoryID: repository.Repository.ID,
		TargetDeviceID: "pi-prepared", Provider: workmodel.CodexSubscription, Prompt: "recover prepared start", IdempotencyKey: "prepared-task",
	})
	if err != nil {
		t.Fatal(err)
	}
	deferred, err := service.Start(ctx, localcontrol.StartRequest{TaskID: task.Task.ID, Revision: task.Task.Revision, Input: "recover prepared start", IdempotencyKey: "prepared-start"})
	if err != nil || !deferred.Queued || deferred.Task.State != workmodel.Paused {
		t.Fatalf("initial deferred start = %#v err=%v", deferred, err)
	}
	queued, err := data.TransitionAtRevision(ctx, task.Task.ID, deferred.Task.Revision, workmodel.Queued,
		workmodel.Event{ID: "prepared-requeue", TaskID: task.Task.ID, Type: workmodel.EventStateTransitioned, Visibility: workmodel.VisibilityInternal, Payload: []byte(`{}`), CreatedAt: now},
		localcontrol.Event{ID: "prepared-requeue-local", ResourceType: "task", ResourceID: task.Task.ID, TaskID: task.Task.ID, Revision: deferred.Task.Revision + 1, Type: "prepared_requeue", Payload: []byte(`{}`), CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	_, err = data.TransitionAtRevision(ctx, task.Task.ID, queued.Revision, workmodel.Preparing,
		workmodel.Event{ID: "prepared-accepted", TaskID: task.Task.ID, Type: workmodel.EventStateTransitioned, Visibility: workmodel.VisibilityInternal, Payload: []byte(`{}`), CreatedAt: now},
		localcontrol.Event{ID: "prepared-accepted-local", ResourceType: "task", ResourceID: task.Task.ID, TaskID: task.Task.ID, Revision: queued.Revision + 1, Type: "prepared_accepted", Payload: []byte(`{}`), CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := data.ClaimDeviceCommand(ctx, "prepared-start", now); err != nil || !claimed {
		t.Fatalf("claim prepared command = %v err=%v", claimed, err)
	}
	replayed, err := service.ReplayDeviceCommands(ctx, localcontrol.ReplayDeviceCommandsRequest{DeviceID: "pi-prepared"})
	if err != nil || replayed.Replayed != 1 || len(replayed.Pending) != 0 {
		t.Fatalf("prepared replay = %#v err=%v", replayed, err)
	}
	observed, err := service.Observe(ctx, localcontrol.ObserveRequest{TaskID: task.Task.ID, Limit: 50})
	if err != nil || observed.Task.State != workmodel.Running || remote.calls != 2 {
		t.Fatalf("prepared replay task = %#v err=%v calls=%d", observed.Task, err, remote.calls)
	}
}

func TestRemoteApprovalReplayCompletesDurableDecisionAfterControllerRestart(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_800_000_700, 0).UTC()
	data, err := sqlite.OpenV2Runtime(ctx, filepath.Join(t.TempDir(), "approval-replay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	if err := data.CreateDevice(ctx, localcontrol.Device{
		ID: "pi-approval", Name: "Approval Pi", Kind: localcontrol.DeviceKindRaspberryPi,
		Fingerprint: "pi-approval-fingerprint", State: localcontrol.DeviceStatePaired,
		ConnectionEpoch: 1, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}, nil); err != nil {
		t.Fatal(err)
	}
	remote := &queueDeviceRuntime{}
	service, err := localcontrol.New(localcontrol.Config{
		Store: data, Runtimes: fakeCatalog{}, Executor: &fakeExecutor{}, Verifier: fakeVerifier{}, Committer: fakeCommitter{},
		RemoteDevices: map[string]localcontrol.DeviceRuntime{"pi-approval": remote}, Clock: func() time.Time { return now }, NewID: deterministicIDs(),
	})
	if err != nil {
		t.Fatal(err)
	}
	project, err := service.CreateProject(ctx, localcontrol.CreateProjectRequest{Name: "Approval replay", IdempotencyKey: "approval-project"})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := service.RegisterRepository(ctx, localcontrol.RegisterRepositoryRequest{Remote: "origin", IdempotencyKey: "approval-repository"})
	if err != nil {
		t.Fatal(err)
	}
	board, err := service.CreateBoard(ctx, localcontrol.CreateBoardRequest{ProjectID: project.Project.ID, Name: "Approvals", IdempotencyKey: "approval-board"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := service.CreateTask(ctx, localcontrol.CreateTaskRequest{
		ProjectID: project.Project.ID, BoardID: board.Board.ID, RepositoryID: repository.Repository.ID,
		TargetDeviceID: "pi-approval", Provider: workmodel.CodexSubscription, Prompt: "recover approval", IdempotencyKey: "approval-task",
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := service.Start(ctx, localcontrol.StartRequest{TaskID: task.Task.ID, Revision: task.Task.Revision, IdempotencyKey: "approval-start"})
	if err != nil || started.Task.State != workmodel.Running {
		t.Fatalf("approval start = %#v err=%v", started, err)
	}
	approvalID := "approval-replay"
	if err := data.UpsertApproval(ctx, workmodel.Approval{ID: approvalID, TaskID: task.Task.ID, Kind: "command", Status: workmodel.ApprovalPending, RequestPayload: []byte(`{"summary":"recover approval"}`), RequestedAt: now, ExpiresAt: timePtr(now.Add(time.Hour))}); err != nil {
		t.Fatal(err)
	}
	awaiting, err := data.TransitionAtRevision(ctx, task.Task.ID, started.Task.Revision, workmodel.AwaitingApproval,
		workmodel.Event{ID: "approval-requested", TaskID: task.Task.ID, Type: workmodel.EventApprovalRequested, Visibility: workmodel.VisibilityUser, Payload: []byte(`{}`), CreatedAt: now},
		localcontrol.Event{ID: "approval-requested-local", ResourceType: "task", ResourceID: task.Task.ID, TaskID: task.Task.ID, Revision: started.Task.Revision + 1, Type: "approval_requested", Payload: []byte(`{}`), CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	resolved := now.Add(time.Second)
	if err := data.UpsertApproval(ctx, workmodel.Approval{ID: approvalID, TaskID: task.Task.ID, Kind: "command", Status: workmodel.ApprovalApproved, RequestPayload: []byte(`{"summary":"recover approval"}`), DecisionPayload: []byte(`{"allow":true,"user_id":"desktop"}`), RequestedAt: now, ResolvedAt: &resolved, ExpiresAt: timePtr(now.Add(time.Hour))}); err != nil {
		t.Fatal(err)
	}
	request := localcontrol.ApproveRequest{TaskID: task.Task.ID, ApprovalID: approvalID, Revision: awaiting.Revision, UserID: "desktop", Allow: true, IdempotencyKey: "approval-replay-command"}
	hashPayload := struct {
		TaskID     string `json:"task_id"`
		ApprovalID string `json:"approval_id"`
		Revision   int64  `json:"revision"`
		UserID     string `json:"user_id"`
		Allow      bool   `json:"allow"`
	}{request.TaskID, request.ApprovalID, request.Revision, request.UserID, request.Allow}
	encodedPayload, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	encodedHashPayload, err := json.Marshal(map[string]any{
		"task_id": hashPayload.TaskID, "approval_id": hashPayload.ApprovalID,
		"user_id": hashPayload.UserID, "allow": hashPayload.Allow,
	})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(append([]byte("approve\x00"), encodedHashPayload...))
	if err := data.EnqueueDeviceCommand(ctx, localcontrol.DeviceCommandRecord{
		ID: request.IdempotencyKey, TaskID: request.TaskID, DeviceID: "pi-approval", AssignmentEpoch: 1,
		Operation: "approve", RequestHash: hex.EncodeToString(digest[:]), RequestPayload: encodedPayload,
		Revision: request.Revision, State: localcontrol.DeviceCommandPending, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if claimed, err := data.ClaimDeviceCommand(ctx, request.IdempotencyKey, now); err != nil || !claimed {
		t.Fatalf("claim approval command = %v err=%v", claimed, err)
	}
	replayed, err := service.ReplayDeviceCommands(ctx, localcontrol.ReplayDeviceCommandsRequest{DeviceID: "pi-approval"})
	if err != nil || replayed.Replayed != 1 || len(replayed.Pending) != 0 {
		t.Fatalf("approval replay = %#v err=%v", replayed, err)
	}
	observed, err := service.Observe(ctx, localcontrol.ObserveRequest{TaskID: task.Task.ID, Limit: 50})
	if err != nil || observed.Task.State != workmodel.Running || remote.calls != 2 {
		t.Fatalf("approval replay task = %#v err=%v calls=%d", observed.Task, err, remote.calls)
	}
	approval, err := data.GetApproval(ctx, approvalID)
	if err != nil || approval.Status != workmodel.ApprovalApproved {
		t.Fatalf("replayed approval = %#v err=%v", approval, err)
	}
}

func TestRemoteApprovalReplayCompletesRecordedDecision(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_800_000_800, 0).UTC()
	data, err := sqlite.OpenV2Runtime(ctx, filepath.Join(t.TempDir(), "approval-recorded-replay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	if err := data.CreateDevice(ctx, localcontrol.Device{
		ID: "pi-approval-recorded", Name: "Recorded Approval Pi", Kind: localcontrol.DeviceKindRaspberryPi,
		Fingerprint: "pi-approval-recorded-fingerprint", State: localcontrol.DeviceStatePaired,
		ConnectionEpoch: 1, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}, nil); err != nil {
		t.Fatal(err)
	}
	remote := &queueDeviceRuntime{}
	failingStore := &failCompleteStore{RuntimeStore: data}
	service, err := localcontrol.New(localcontrol.Config{
		Store: failingStore, Runtimes: fakeCatalog{}, Executor: &fakeExecutor{}, Verifier: fakeVerifier{}, Committer: fakeCommitter{},
		RemoteDevices: map[string]localcontrol.DeviceRuntime{"pi-approval-recorded": remote}, Clock: func() time.Time { return now }, NewID: deterministicIDs(),
	})
	if err != nil {
		t.Fatal(err)
	}
	project, err := service.CreateProject(ctx, localcontrol.CreateProjectRequest{Name: "Recorded approval replay", IdempotencyKey: "approval-recorded-project"})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := service.RegisterRepository(ctx, localcontrol.RegisterRepositoryRequest{Remote: "origin", IdempotencyKey: "approval-recorded-repository"})
	if err != nil {
		t.Fatal(err)
	}
	board, err := service.CreateBoard(ctx, localcontrol.CreateBoardRequest{ProjectID: project.Project.ID, Name: "Recovery", IdempotencyKey: "approval-recorded-board"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := service.CreateTask(ctx, localcontrol.CreateTaskRequest{
		ProjectID: project.Project.ID, BoardID: board.Board.ID, RepositoryID: repository.Repository.ID,
		TargetDeviceID: "pi-approval-recorded", Provider: workmodel.CodexSubscription, Prompt: "recover approval decision", IdempotencyKey: "approval-recorded-task",
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := service.Start(ctx, localcontrol.StartRequest{TaskID: task.Task.ID, Revision: task.Task.Revision, IdempotencyKey: "approval-recorded-start"})
	if err != nil {
		t.Fatal(err)
	}
	approvalID := "approval-recorded"
	if err := data.UpsertApproval(ctx, workmodel.Approval{
		ID: approvalID, TaskID: task.Task.ID, Kind: "command", Status: workmodel.ApprovalPending,
		RequestPayload: []byte(`{"summary":"recover decision"}`), RequestedAt: now, ExpiresAt: timePtr(now.Add(time.Hour)),
	}); err != nil {
		t.Fatal(err)
	}
	awaiting, err := data.TransitionAtRevision(ctx, task.Task.ID, started.Task.Revision, workmodel.AwaitingApproval,
		workmodel.Event{ID: "approval-recorded-requested", TaskID: task.Task.ID, Type: workmodel.EventApprovalRequested, Visibility: workmodel.VisibilityUser, Payload: []byte(`{}`), CreatedAt: now},
		localcontrol.Event{ID: "approval-recorded-requested-local", ResourceType: "task", ResourceID: task.Task.ID, TaskID: task.Task.ID, Revision: started.Task.Revision + 1, Type: "approval_requested", Payload: []byte(`{}`), CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	failingStore.failComplete = true
	if _, err := service.Approve(ctx, localcontrol.ApproveRequest{
		TaskID: task.Task.ID, ApprovalID: approvalID, Revision: awaiting.Revision, UserID: "desktop", Allow: true, IdempotencyKey: "approval-recorded-command",
	}); err == nil {
		t.Fatal("approval completed despite simulated command-row failure")
	}
	pending, err := data.ListPendingDeviceCommands(ctx, "pi-approval-recorded", 10)
	if err != nil || len(pending) != 1 || pending[0].State != localcontrol.DeviceCommandInFlight {
		t.Fatalf("recorded approval command before replay = %#v err=%v", pending, err)
	}
	resolvedTask, err := data.Task(ctx, task.Task.ID)
	if err != nil || resolvedTask.State != workmodel.Running {
		t.Fatalf("recorded approval task before replay = %#v err=%v", resolvedTask, err)
	}
	events, err := data.ListLocalEvents(ctx, task.Task.ID, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	resolved := 0
	for _, event := range events {
		if event.Type == "approval_resolved" {
			resolved++
		}
	}
	if resolved != 1 || remote.approvals != 1 {
		t.Fatalf("recorded approval evidence = %d, native approval calls = %d", resolved, remote.approvals)
	}

	restartIDs := deterministicIDs()
	restarted, err := localcontrol.New(localcontrol.Config{
		Store: data, Runtimes: fakeCatalog{}, Executor: &fakeExecutor{}, Verifier: fakeVerifier{}, Committer: fakeCommitter{},
		RemoteDevices: map[string]localcontrol.DeviceRuntime{"pi-approval-recorded": remote}, Clock: func() time.Time { return now },
		NewID: func(prefix string) string { return "restart-" + restartIDs(prefix) },
	})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := restarted.ReplayDeviceCommands(ctx, localcontrol.ReplayDeviceCommandsRequest{DeviceID: "pi-approval-recorded"})
	if err != nil || replayed.Replayed != 1 || len(replayed.Pending) != 0 {
		t.Fatalf("recorded approval replay = %#v err=%v", replayed, err)
	}
	if remote.approvals != 1 {
		t.Fatalf("native approval calls after replay = %d, want exactly one approval effect", remote.approvals)
	}
	events, err = data.ListLocalEvents(ctx, task.Task.ID, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	resolved = 0
	for _, event := range events {
		if event.Type == "approval_resolved" {
			resolved++
		}
	}
	if resolved != 1 {
		t.Fatalf("approval evidence duplicated on replay: %d", resolved)
	}
	command, err := data.GetDeviceCommand(ctx, "approval-recorded-command")
	if err != nil || command.State != localcontrol.DeviceCommandCompleted {
		t.Fatalf("replayed approval command = %#v err=%v", command, err)
	}
}

func TestRemoteCommitReplayCompletesCheckpointedCommand(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_800_000_900, 0).UTC()
	data, err := sqlite.OpenV2Runtime(ctx, filepath.Join(t.TempDir(), "commit-replay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	if err := data.CreateDevice(ctx, localcontrol.Device{
		ID: "pi-commit-replay", Name: "Commit Replay Pi", Kind: localcontrol.DeviceKindRaspberryPi,
		Fingerprint: "pi-commit-replay-fingerprint", State: localcontrol.DeviceStatePaired,
		ConnectionEpoch: 1, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}, nil); err != nil {
		t.Fatal(err)
	}
	remote := &queueDeviceRuntime{}
	failingStore := &failCompleteStore{RuntimeStore: data}
	service, err := localcontrol.New(localcontrol.Config{
		Store: failingStore, Runtimes: fakeCatalog{}, Executor: &fakeExecutor{}, Verifier: fakeVerifier{}, Committer: fakeCommitter{},
		RemoteDevices: map[string]localcontrol.DeviceRuntime{"pi-commit-replay": remote}, Clock: func() time.Time { return now }, NewID: deterministicIDs(),
	})
	if err != nil {
		t.Fatal(err)
	}
	project, err := service.CreateProject(ctx, localcontrol.CreateProjectRequest{Name: "Commit replay", IdempotencyKey: "commit-replay-project"})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := service.RegisterRepository(ctx, localcontrol.RegisterRepositoryRequest{Remote: "origin", IdempotencyKey: "commit-replay-repository"})
	if err != nil {
		t.Fatal(err)
	}
	board, err := service.CreateBoard(ctx, localcontrol.CreateBoardRequest{ProjectID: project.Project.ID, Name: "Recovery", IdempotencyKey: "commit-replay-board"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := service.CreateTask(ctx, localcontrol.CreateTaskRequest{
		ProjectID: project.Project.ID, BoardID: board.Board.ID, RepositoryID: repository.Repository.ID,
		TargetDeviceID: "pi-commit-replay", Provider: workmodel.CodexSubscription, Prompt: "recover commit", IdempotencyKey: "commit-replay-task",
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := service.Start(ctx, localcontrol.StartRequest{TaskID: task.Task.ID, Revision: task.Task.Revision, IdempotencyKey: "commit-replay-start"})
	if err != nil {
		t.Fatal(err)
	}
	verified, err := service.Verify(ctx, localcontrol.VerifyRequest{TaskID: task.Task.ID, Revision: started.Task.Revision, IdempotencyKey: "commit-replay-verify"})
	if err != nil {
		t.Fatal(err)
	}
	failingStore.failComplete = true
	if _, err := service.Commit(ctx, localcontrol.CommitRequest{TaskID: task.Task.ID, Revision: verified.Task.Revision, IdempotencyKey: "commit-replay-command"}); err == nil {
		t.Fatal("commit completed despite simulated command-row failure")
	}
	pending, err := data.ListPendingDeviceCommands(ctx, "pi-commit-replay", 10)
	if err != nil || len(pending) != 1 || pending[0].State != localcontrol.DeviceCommandInFlight {
		t.Fatalf("checkpointed command before replay = %#v err=%v", pending, err)
	}
	checkpointedTask, err := data.Task(ctx, task.Task.ID)
	if err != nil || checkpointedTask.State != workmodel.Committing {
		t.Fatalf("checkpointed task before replay = %#v err=%v", checkpointedTask, err)
	}

	restartIDs := deterministicIDs()
	restarted, err := localcontrol.New(localcontrol.Config{
		Store: data, Runtimes: fakeCatalog{}, Executor: &fakeExecutor{}, Verifier: fakeVerifier{}, Committer: fakeCommitter{},
		RemoteDevices: map[string]localcontrol.DeviceRuntime{"pi-commit-replay": remote}, Clock: func() time.Time { return now },
		NewID: func(prefix string) string { return "restart-" + restartIDs(prefix) },
	})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := restarted.ReplayDeviceCommands(ctx, localcontrol.ReplayDeviceCommandsRequest{DeviceID: "pi-commit-replay"})
	if err != nil || replayed.Replayed != 1 || len(replayed.Pending) != 0 {
		record, recordErr := data.GetDeviceCommand(ctx, "commit-replay-command")
		t.Fatalf("checkpointed commit replay = %#v err=%v record=%#v recordErr=%v", replayed, err, record, recordErr)
	}
	completed, err := data.Task(ctx, task.Task.ID)
	if err != nil || completed.State != workmodel.Completed {
		t.Fatalf("replayed commit task = %#v err=%v", completed, err)
	}
	command, err := data.GetDeviceCommand(ctx, "commit-replay-command")
	if err != nil || command.State != localcontrol.DeviceCommandCompleted {
		t.Fatalf("replayed commit command = %#v err=%v", command, err)
	}
	if remote.commits != 1 {
		t.Fatalf("remote commit calls = %d, want exactly one native effect", remote.commits)
	}
}

func TestRemoteVerifyReplayCompletesRecordedEvidence(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_800_001_000, 0).UTC()
	data, err := sqlite.OpenV2Runtime(ctx, filepath.Join(t.TempDir(), "verify-replay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	if err := data.CreateDevice(ctx, localcontrol.Device{
		ID: "pi-verify-replay", Name: "Verify Replay Pi", Kind: localcontrol.DeviceKindRaspberryPi,
		Fingerprint: "pi-verify-replay-fingerprint", State: localcontrol.DeviceStatePaired,
		ConnectionEpoch: 1, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}, nil); err != nil {
		t.Fatal(err)
	}
	remote := &queueDeviceRuntime{}
	failingStore := &failCompleteStore{RuntimeStore: data}
	service, err := localcontrol.New(localcontrol.Config{
		Store: failingStore, Runtimes: fakeCatalog{}, Executor: &fakeExecutor{}, Verifier: fakeVerifier{}, Committer: fakeCommitter{},
		RemoteDevices: map[string]localcontrol.DeviceRuntime{"pi-verify-replay": remote}, Clock: func() time.Time { return now }, NewID: deterministicIDs(),
	})
	if err != nil {
		t.Fatal(err)
	}
	project, err := service.CreateProject(ctx, localcontrol.CreateProjectRequest{Name: "Verify replay", IdempotencyKey: "verify-replay-project"})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := service.RegisterRepository(ctx, localcontrol.RegisterRepositoryRequest{Remote: "origin", IdempotencyKey: "verify-replay-repository"})
	if err != nil {
		t.Fatal(err)
	}
	board, err := service.CreateBoard(ctx, localcontrol.CreateBoardRequest{ProjectID: project.Project.ID, Name: "Recovery", IdempotencyKey: "verify-replay-board"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := service.CreateTask(ctx, localcontrol.CreateTaskRequest{
		ProjectID: project.Project.ID, BoardID: board.Board.ID, RepositoryID: repository.Repository.ID,
		TargetDeviceID: "pi-verify-replay", Provider: workmodel.CodexSubscription, Prompt: "recover verify", IdempotencyKey: "verify-replay-task",
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := service.Start(ctx, localcontrol.StartRequest{TaskID: task.Task.ID, Revision: task.Task.Revision, IdempotencyKey: "verify-replay-start"})
	if err != nil {
		t.Fatal(err)
	}
	failingStore.failComplete = true
	if _, err := service.Verify(ctx, localcontrol.VerifyRequest{TaskID: task.Task.ID, Revision: started.Task.Revision, IdempotencyKey: "verify-replay-command"}); err == nil {
		t.Fatal("verify completed despite simulated command-row failure")
	}
	pending, err := data.ListPendingDeviceCommands(ctx, "pi-verify-replay", 10)
	if err != nil || len(pending) != 1 || pending[0].State != localcontrol.DeviceCommandInFlight {
		t.Fatalf("recorded verification command before replay = %#v err=%v", pending, err)
	}
	checkpointedTask, err := data.Task(ctx, task.Task.ID)
	if err != nil || checkpointedTask.State != workmodel.Verifying {
		t.Fatalf("recorded verification task before replay = %#v err=%v", checkpointedTask, err)
	}
	events, err := data.ListLocalEvents(ctx, task.Task.ID, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	passed := 0
	for _, event := range events {
		if event.Type == "verification_passed" {
			passed++
		}
	}
	if passed != 1 || remote.verifies != 1 {
		t.Fatalf("recorded verification evidence = %d, native verify calls = %d", passed, remote.verifies)
	}

	restartIDs := deterministicIDs()
	restarted, err := localcontrol.New(localcontrol.Config{
		Store: data, Runtimes: fakeCatalog{}, Executor: &fakeExecutor{}, Verifier: fakeVerifier{}, Committer: fakeCommitter{},
		RemoteDevices: map[string]localcontrol.DeviceRuntime{"pi-verify-replay": remote}, Clock: func() time.Time { return now },
		NewID: func(prefix string) string { return "restart-" + restartIDs(prefix) },
	})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := restarted.ReplayDeviceCommands(ctx, localcontrol.ReplayDeviceCommandsRequest{DeviceID: "pi-verify-replay"})
	if err != nil || replayed.Replayed != 1 || len(replayed.Pending) != 0 {
		t.Fatalf("recorded verification replay = %#v err=%v", replayed, err)
	}
	if remote.verifies != 1 {
		t.Fatalf("native verify calls after replay = %d, want exactly one", remote.verifies)
	}
	events, err = data.ListLocalEvents(ctx, task.Task.ID, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	passed = 0
	for _, event := range events {
		if event.Type == "verification_passed" {
			passed++
		}
	}
	if passed != 1 {
		t.Fatalf("verification evidence duplicated on replay: %d", passed)
	}
	command, err := data.GetDeviceCommand(ctx, "verify-replay-command")
	if err != nil || command.State != localcontrol.DeviceCommandCompleted {
		t.Fatalf("replayed verify command = %#v err=%v", command, err)
	}
}

type failCompleteStore struct {
	*sqlite.RuntimeStore
	failComplete bool
}

func (s *failCompleteStore) CompleteDeviceCommand(ctx context.Context, id string, now time.Time) error {
	if s.failComplete {
		s.failComplete = false
		return errors.New("simulated process crash before command completion")
	}
	return s.RuntimeStore.CompleteDeviceCommand(ctx, id, now)
}

type queueDeviceRuntime struct {
	unavailable bool
	calls       int
	approvals   int
	verifies    int
	commits     int
}

func (r *queueDeviceRuntime) Start(context.Context, localcontrol.TaskView, localcontrol.StartRequest) error {
	r.calls++
	if r.unavailable {
		r.unavailable = false
		return localcontrol.ErrDeviceUnreachable
	}
	return nil
}
func (*queueDeviceRuntime) Resume(context.Context, localcontrol.TaskView, localcontrol.ResumeRequest) error {
	return nil
}

func (r *queueDeviceRuntime) Approve(context.Context, localcontrol.TaskView, string, string, bool) error {
	r.calls++
	r.approvals++
	return nil
}
func (*queueDeviceRuntime) Cancel(context.Context, localcontrol.TaskView) error { return nil }
func (r *queueDeviceRuntime) Verify(context.Context, localcontrol.TaskView) (localcontrol.VerificationReceipt, error) {
	r.verifies++
	return localcontrol.VerificationReceipt{ID: "queue-verification", Passed: true}, nil
}
func (r *queueDeviceRuntime) Commit(context.Context, localcontrol.TaskView) (localcontrol.CommitReceipt, error) {
	r.commits++
	return localcontrol.CommitReceipt{ID: "queue-commit", CommitSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RemoteRef: "refs/heads/queue"}, nil
}
