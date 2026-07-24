package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/localcontrol"
	"github.com/berkayahi/agentbridge/internal/store"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

func TestDeviceCommandQueuePersistsAndFencesIdempotency(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_800_000_100, 0).UTC()
	path := filepath.Join(t.TempDir(), "device-commands.db")
	data, err := OpenV2Runtime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := data.CreateDevice(ctx, localcontrol.Device{
		ID: "pi-commands", Name: "Commands Pi", Kind: localcontrol.DeviceKindRaspberryPi,
		Fingerprint: "pi-commands-fingerprint", State: localcontrol.DeviceStatePaired,
		ConnectionEpoch: 1, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}, nil); err != nil {
		data.Close()
		t.Fatal(err)
	}
	if err := data.EnsureRepositoryBinding(ctx, "origin", "origin"); err != nil {
		data.Close()
		t.Fatal(err)
	}
	task := workmodel.Task{ID: "command-task", RepoProfileID: "origin", Title: "Commands", Prompt: "run", State: workmodel.Queued, Provider: workmodel.CodexSubscription, CreatedAt: now, UpdatedAt: now}
	if err := data.CreateTask(ctx, task, workmodel.Event{ID: "command-task-event", TaskID: task.ID, Type: workmodel.EventTaskCreated, Visibility: workmodel.VisibilityUser, Payload: []byte(`{"state":"queued"}`), CreatedAt: now}); err != nil {
		data.Close()
		t.Fatal(err)
	}
	record := localcontrol.DeviceCommandRecord{
		ID: "command-key", TaskID: task.ID, DeviceID: "pi-commands", AssignmentEpoch: 1,
		Operation: "start", RequestHash: "request-hash", RequestPayload: []byte(`{"task_id":"command-task"}`),
		Revision: 1, State: localcontrol.DeviceCommandPending, CreatedAt: now, UpdatedAt: now,
	}
	if err := data.EnqueueDeviceCommand(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := data.EnqueueDeviceCommand(ctx, record); err != nil {
		t.Fatalf("idempotent enqueue = %v", err)
	}
	conflict := record
	conflict.RequestHash = "different"
	if err := data.EnqueueDeviceCommand(ctx, conflict); !errors.Is(err, localcontrol.ErrIdempotencyConflict) {
		t.Fatalf("conflicting enqueue = %v, want ErrIdempotencyConflict", err)
	}
	claimed, err := data.ClaimDeviceCommand(ctx, record.ID, now.Add(time.Second))
	if err != nil || !claimed {
		t.Fatalf("claim = %v err=%v", claimed, err)
	}
	if err := data.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenV2Runtime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	pending, err := reopened.ListPendingDeviceCommands(ctx, record.DeviceID, 10)
	if err != nil || len(pending) != 1 || pending[0].Attempts != 1 || pending[0].State != localcontrol.DeviceCommandInFlight {
		t.Fatalf("persisted pending commands = %#v err=%v", pending, err)
	}
	if err := reopened.CompleteDeviceCommand(ctx, record.ID, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	completed, err := reopened.GetDeviceCommand(ctx, record.ID)
	if err != nil || completed.State != localcontrol.DeviceCommandCompleted {
		t.Fatalf("completed command = %#v err=%v", completed, err)
	}
	if err := reopened.FailDeviceCommand(ctx, record.ID, "late failure", now.Add(3*time.Second)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("failing completed command = %v, want store.ErrNotFound", err)
	}
	stillCompleted, err := reopened.GetDeviceCommand(ctx, record.ID)
	if err != nil || stillCompleted.State != localcontrol.DeviceCommandCompleted {
		t.Fatalf("completed command after late failure = %#v err=%v", stillCompleted, err)
	}
	if _, err := reopened.GetDeviceCommand(ctx, "missing-command"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing command = %v, want store.ErrNotFound", err)
	}
}
