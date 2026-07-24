package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/localcontrol"
	"github.com/berkayahi/agentbridge/internal/store/sqlite"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

func TestDeviceExecutionHandlerObservesShadowEventsAndApprovals(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_800_000_200, 0).UTC()
	data, err := sqlite.OpenV2Runtime(ctx, filepath.Join(t.TempDir(), "device-observation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	handler := newDeviceExecutionHandler(data, &localRuntimeExecutor{}, localVerifier{}, localCommitter{})
	command := localcontrol.DeviceCommand{
		ID: "observe-one", Operation: "observe", TaskID: "shadow-task", DeviceID: "pi-agent", ConnectionEpoch: 1,
		RepositoryID: "repo-agent", RepositoryRemote: "origin", Provider: workmodel.CodexSubscription, Title: "Shadow task", Prompt: "observe this task",
		Payload: json.RawMessage(`{"after_cursor":0}`),
	}
	first, err := handler.Handle(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	var firstObservation localcontrol.DeviceObservation
	if err := json.Unmarshal(first.Payload, &firstObservation); err != nil {
		t.Fatal(err)
	}
	if firstObservation.Cursor != 1 || len(firstObservation.Events) != 1 || firstObservation.Events[0].ID != "shadow-task-device-created" {
		t.Fatalf("first device observation = %#v", firstObservation)
	}
	binding, err := data.GetRepository(ctx, "repo-agent")
	if err != nil || binding.Remote != "origin" {
		t.Fatalf("device repository binding = %#v err=%v", binding, err)
	}
	if err := data.UpsertApproval(ctx, workmodel.Approval{
		ID: "shadow-approval", TaskID: "shadow-task", Kind: "command", Status: workmodel.ApprovalPending,
		RequestPayload: []byte(`{"summary":"approve on Pi"}`), RequestedAt: now, ExpiresAt: deviceTimePtr(now.Add(time.Hour)),
	}); err != nil {
		t.Fatal(err)
	}
	command.ID = "observe-two"
	second, err := handler.Handle(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	var secondObservation localcontrol.DeviceObservation
	if err := json.Unmarshal(second.Payload, &secondObservation); err != nil {
		t.Fatal(err)
	}
	if secondObservation.Cursor != 1 || len(secondObservation.Events) != 1 || len(secondObservation.Approvals) != 1 || secondObservation.Approvals[0].ID != "shadow-approval" {
		t.Fatalf("second device observation = %#v", secondObservation)
	}
	command.ID = "observe-three"
	command.Payload = json.RawMessage(`{"after_cursor":1}`)
	third, err := handler.Handle(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	var thirdObservation localcontrol.DeviceObservation
	if err := json.Unmarshal(third.Payload, &thirdObservation); err != nil {
		t.Fatal(err)
	}
	if thirdObservation.Cursor != 1 || len(thirdObservation.Events) != 0 || len(thirdObservation.Approvals) != 1 {
		t.Fatalf("cursor device observation = %#v", thirdObservation)
	}
}

func TestDeviceExecutionHandlerObservationCursorStopsAtReturnedBatch(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_800_000_300, 0).UTC()
	data, err := sqlite.OpenV2Runtime(ctx, filepath.Join(t.TempDir(), "device-observation-bounded.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	handler := newDeviceExecutionHandler(data, &localRuntimeExecutor{}, localVerifier{}, localCommitter{})
	command := localcontrol.DeviceCommand{
		ID: "observe-bounded-one", Operation: "observe", TaskID: "shadow-task-bounded", DeviceID: "pi-agent", ConnectionEpoch: 1,
		RepositoryID: "repo-agent", Provider: workmodel.CodexSubscription, Title: "Bounded task", Prompt: "observe bounded events",
		Payload: json.RawMessage(`{"after_cursor":0}`),
	}
	if _, err := handler.Handle(ctx, command); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 204; index++ {
		if err := data.AppendEvent(ctx, workmodel.Event{
			ID: fmt.Sprintf("bounded-provider-%03d", index), TaskID: command.TaskID,
			Type: workmodel.EventProviderMessage, Visibility: workmodel.VisibilityUser,
			Payload: json.RawMessage(fmt.Sprintf(`{"index":%d}`, index)), CreatedAt: now.Add(time.Duration(index+1) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}

	first, err := handler.Handle(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	var firstObservation localcontrol.DeviceObservation
	if err := json.Unmarshal(first.Payload, &firstObservation); err != nil {
		t.Fatal(err)
	}
	firstCursor, firstLastCursor := uint64(0), uint64(0)
	if len(firstObservation.Events) > 0 {
		firstCursor, firstLastCursor = firstObservation.Events[0].Cursor, firstObservation.Events[len(firstObservation.Events)-1].Cursor
	}
	if firstObservation.Cursor != 200 || len(firstObservation.Events) != 200 || firstCursor != 1 || firstLastCursor != 200 {
		t.Fatalf("first bounded observation = cursor %d events=%d first=%d last=%d", firstObservation.Cursor, len(firstObservation.Events), firstCursor, firstLastCursor)
	}

	command.ID = "observe-bounded-two"
	command.Payload = json.RawMessage(`{"after_cursor":200}`)
	second, err := handler.Handle(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	var secondObservation localcontrol.DeviceObservation
	if err := json.Unmarshal(second.Payload, &secondObservation); err != nil {
		t.Fatal(err)
	}
	secondCursor, secondLastCursor := uint64(0), uint64(0)
	if len(secondObservation.Events) > 0 {
		secondCursor, secondLastCursor = secondObservation.Events[0].Cursor, secondObservation.Events[len(secondObservation.Events)-1].Cursor
	}
	if secondObservation.Cursor != 205 || len(secondObservation.Events) != 5 || secondCursor != 201 || secondLastCursor != 205 {
		t.Fatalf("second bounded observation = cursor %d events=%d first=%d last=%d", secondObservation.Cursor, len(secondObservation.Events), secondCursor, secondLastCursor)
	}
}

func deviceTimePtr(value time.Time) *time.Time { return &value }
