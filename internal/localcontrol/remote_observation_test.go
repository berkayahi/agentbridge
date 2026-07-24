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

func TestRemoteObservationIngestsEvidenceAndRealApprovalsIdempotently(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_800_000_100, 0).UTC()
	data, err := sqlite.OpenV2Runtime(ctx, filepath.Join(t.TempDir(), "remote-observation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	if err := data.CreateDevice(ctx, localcontrol.Device{
		ID: "pi-observe", Name: "Observation Pi", Kind: localcontrol.DeviceKindRaspberryPi,
		Fingerprint: "pi-observe-fingerprint", State: localcontrol.DeviceStatePaired,
		ConnectionEpoch: 1, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}, nil); err != nil {
		t.Fatal(err)
	}

	approvalTime := now.Add(time.Second)
	remote := &observingDeviceRuntime{queueDeviceRuntime: queueDeviceRuntime{}, observation: localcontrol.DeviceObservation{
		Cursor: 1,
		Events: []localcontrol.DeviceEvent{{
			Cursor: 1, ID: "provider-event-42", TaskID: "task-a", Type: "provider_message",
			Payload: json.RawMessage(`{"summary":"remote evidence"}`), CreatedAt: approvalTime,
		}},
		Approvals: []localcontrol.ApprovalView{{
			ID: "provider-approval-42", TaskID: "task-a", Kind: "command", Status: string(workmodel.ApprovalPending),
			RequestPayload: json.RawMessage(`{"summary":"remote approval"}`), RequestedAt: approvalTime,
			ExpiresAt: timePtr(approvalTime.Add(time.Hour)),
		}},
	}}
	service, err := localcontrol.New(localcontrol.Config{
		Store: data, Runtimes: fakeCatalog{}, Executor: &fakeExecutor{}, Verifier: fakeVerifier{}, Committer: fakeCommitter{},
		RemoteDevices: map[string]localcontrol.DeviceRuntime{"pi-observe": remote}, Clock: func() time.Time { return now }, NewID: deterministicIDs(),
	})
	if err != nil {
		t.Fatal(err)
	}
	project, err := service.CreateProject(ctx, localcontrol.CreateProjectRequest{Name: "Remote observation", IdempotencyKey: "remote-project"})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := service.RegisterRepository(ctx, localcontrol.RegisterRepositoryRequest{Remote: "origin", IdempotencyKey: "remote-repository"})
	if err != nil {
		t.Fatal(err)
	}
	board, err := service.CreateBoard(ctx, localcontrol.CreateBoardRequest{ProjectID: project.Project.ID, Name: "Remote", IdempotencyKey: "remote-board"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := service.CreateTask(ctx, localcontrol.CreateTaskRequest{
		ProjectID: project.Project.ID, BoardID: board.Board.ID, RepositoryID: repository.Repository.ID,
		TargetDeviceID: "pi-observe", Provider: workmodel.CodexSubscription, Prompt: "observe the remote task", IdempotencyKey: "remote-task",
	})
	if err != nil {
		t.Fatal(err)
	}
	remote.observation.Events[0].TaskID = task.Task.ID
	remote.observation.Approvals[0].TaskID = task.Task.ID

	first, err := service.Observe(ctx, localcontrol.ObserveRequest{TaskID: task.Task.ID, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Events) != 2 {
		t.Fatalf("first observation events = %#v, want task creation plus device event", first.Events)
	}
	var deviceEvent localcontrol.Event
	for _, event := range first.Events {
		if event.Type == "device_event" {
			deviceEvent = event
		}
	}
	if deviceEvent.ID == "" || deviceEvent.Cursor == 0 || !json.Valid(deviceEvent.Payload) {
		t.Fatalf("device event = %#v", deviceEvent)
	}
	approvals, err := service.PendingApprovals(ctx, task.Task.ID)
	if err != nil || len(approvals.Approvals) != 1 || approvals.Approvals[0].ID != "provider-approval-42" {
		t.Fatalf("ingested approvals = %#v err=%v", approvals, err)
	}

	second, err := service.Observe(ctx, localcontrol.ObserveRequest{TaskID: task.Task.ID, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Events) != len(first.Events) || remote.observations != 2 || len(remote.after) != 2 || remote.after[0] != 0 || remote.after[1] != 1 {
		t.Fatalf("duplicate observation = events=%d/%d remote_calls=%d afters=%v", len(second.Events), len(first.Events), remote.observations, remote.after)
	}

	resolved := workmodel.Approval{
		ID: "provider-approval-42", TaskID: task.Task.ID, Kind: "command", Status: workmodel.ApprovalApproved,
		RequestPayload: json.RawMessage(`{"summary":"remote approval"}`), RequestedAt: approvalTime,
		DecisionPayload: json.RawMessage(`{"allow":true}`), ResolvedAt: timePtr(now.Add(2 * time.Second)),
	}
	if err := data.UpsertApproval(ctx, resolved); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Observe(ctx, localcontrol.ObserveRequest{TaskID: task.Task.ID, Limit: 50}); err != nil {
		t.Fatal(err)
	}
	remaining, err := service.PendingApprovals(ctx, task.Task.ID)
	if err != nil || len(remaining.Approvals) != 0 {
		t.Fatalf("resolved remote approval was resurrected: %#v err=%v", remaining, err)
	}

	remote.observation.Events[0].Payload = json.RawMessage(`{"summary":"tampered remote evidence"}`)
	if _, err := service.Observe(ctx, localcontrol.ObserveRequest{TaskID: task.Task.ID, Limit: 50}); !errors.Is(err, localcontrol.ErrIdempotencyConflict) {
		t.Fatalf("stable remote event payload collision = %v, want ErrIdempotencyConflict", err)
	}
}

type observingDeviceRuntime struct {
	queueDeviceRuntime
	observation  localcontrol.DeviceObservation
	observations int
	after        []uint64
}

func (r *observingDeviceRuntime) Observe(_ context.Context, _ localcontrol.TaskView, after uint64) (localcontrol.DeviceObservation, error) {
	r.observations++
	r.after = append(r.after, after)
	return r.observation, nil
}
