package localcontrol_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/berkayahi/agentbridge/internal/localcontrol"
)

func TestLinkedRuntimeRejectsLateDeviceReply(t *testing.T) {
	runtime, err := localcontrol.NewLinkedRuntime(linkFunc(func(_ context.Context, command localcontrol.DeviceCommand) (localcontrol.DeviceReply, error) {
		return localcontrol.DeviceReply{MessageID: 1, DeviceID: command.DeviceID, ConnectionEpoch: command.ConnectionEpoch - 1, Accepted: true}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	err = runtime.Start(context.Background(), localcontrol.TaskView{ID: "task-1", ExecutionID: "execution-1", SessionID: "session-1", TargetDeviceID: "pi-1", TargetEpoch: 4}, localcontrol.StartRequest{IdempotencyKey: "start-1"})
	if !errors.Is(err, localcontrol.ErrDeviceFence) {
		t.Fatalf("late device reply = %v, want ErrDeviceFence", err)
	}
}

func TestLinkedRuntimePreservesTypedDeviceRejection(t *testing.T) {
	runtime, err := localcontrol.NewLinkedRuntime(linkFunc(func(_ context.Context, command localcontrol.DeviceCommand) (localcontrol.DeviceReply, error) {
		return localcontrol.DeviceReply{MessageID: 1, DeviceID: command.DeviceID, ConnectionEpoch: command.ConnectionEpoch, Error: "provider start failed"}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	err = runtime.Start(context.Background(), localcontrol.TaskView{
		ID: "task-1", ExecutionID: "execution-1", SessionID: "session-1", Provider: "codex",
		RepositoryID: "repo-1", RuntimeID: "codex", Title: "task", Prompt: "run", TargetDeviceID: "pi-1", TargetEpoch: 1, Revision: 1,
	}, localcontrol.StartRequest{IdempotencyKey: "start-1"})
	if !errors.Is(err, localcontrol.ErrDeviceLinkRejected) || !strings.Contains(err.Error(), "provider start failed") {
		t.Fatalf("typed device rejection = %v", err)
	}
}

func TestLinkedRuntimeCarriesOnlyTypedCommandFields(t *testing.T) {
	var received localcontrol.DeviceCommand
	runtime, err := localcontrol.NewLinkedRuntime(linkFunc(func(_ context.Context, command localcontrol.DeviceCommand) (localcontrol.DeviceReply, error) {
		received = command
		return localcontrol.DeviceReply{MessageID: 1, DeviceID: command.DeviceID, ConnectionEpoch: command.ConnectionEpoch, Accepted: true}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Start(context.Background(), localcontrol.TaskView{ID: "task-1", ExecutionID: "execution-1", SessionID: "session-1", RepositoryID: "repo-1", RepositoryRemote: "origin", TargetDeviceID: "pi-1", TargetEpoch: 2}, localcontrol.StartRequest{Input: "run", IdempotencyKey: "start-1"}); err != nil {
		t.Fatal(err)
	}
	if received.Operation != "start" || received.DeviceID != "pi-1" || received.ConnectionEpoch != 2 || received.ID != "start-1" || received.RepositoryID != "repo-1" || received.RepositoryRemote != "origin" || string(received.Payload) == "" {
		t.Fatalf("device command = %#v", received)
	}
}

type linkFunc func(context.Context, localcontrol.DeviceCommand) (localcontrol.DeviceReply, error)

func (f linkFunc) Execute(ctx context.Context, command localcontrol.DeviceCommand) (localcontrol.DeviceReply, error) {
	return f(ctx, command)
}
