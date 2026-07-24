package localcontrol_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/localcontrol"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

func TestLinkedRuntimeRequestsTypedObservationWithoutQueueing(t *testing.T) {
	link := &observationDeviceLink{}
	runtime, err := localcontrol.NewLinkedRuntime(link)
	if err != nil {
		t.Fatal(err)
	}
	view := localcontrol.TaskView{ID: "task-observe", TargetDeviceID: "pi-observe", TargetEpoch: 7}
	observation, err := runtime.Observe(context.Background(), view, 4)
	if err != nil {
		t.Fatal(err)
	}
	if link.command.Operation != "observe" || link.command.ID == "" || observation.Cursor != 5 || len(observation.Events) != 1 || observation.Events[0].ID != "remote-event" {
		t.Fatalf("command=%#v observation=%#v", link.command, observation)
	}
	var payload struct {
		AfterCursor uint64 `json:"after_cursor"`
	}
	if err := json.Unmarshal(link.command.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.AfterCursor != 4 {
		t.Fatalf("observe payload = %#v", payload)
	}
}

type observationDeviceLink struct {
	command localcontrol.DeviceCommand
}

func (l *observationDeviceLink) Execute(_ context.Context, command localcontrol.DeviceCommand) (localcontrol.DeviceReply, error) {
	l.command = command
	payload, err := json.Marshal(localcontrol.DeviceObservation{
		Cursor:    5,
		Events:    []localcontrol.DeviceEvent{{Cursor: 5, ID: "remote-event", TaskID: command.TaskID, Type: "provider_message", CreatedAt: time.Unix(1, 0).UTC()}},
		Approvals: []localcontrol.ApprovalView{{ID: "remote-approval", TaskID: command.TaskID, Kind: "command", Status: string(workmodel.ApprovalPending)}},
	})
	if err != nil {
		return localcontrol.DeviceReply{}, err
	}
	return localcontrol.DeviceReply{DeviceID: command.DeviceID, ConnectionEpoch: command.ConnectionEpoch, Accepted: true, Payload: payload}, nil
}
