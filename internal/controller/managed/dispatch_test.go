package managed

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/kernel"
	managedprotocol "github.com/berkayahi/agentbridge/internal/managed"
)

func TestDispatchBindsCommandToVerifiedFrameIdentity(t *testing.T) {
	executor := &recordingExecutor{}
	controller := NewWithExecutor(executor)
	payload, err := json.Marshal(commandPayload{
		Kind: "start", TaskID: "task-1", RuntimeID: "runtime-1", RepositoryID: "repo-1", Model: "gpt-5.6-terra",
		Input: "run the task", FencingEpoch: 3, PolicySnapshot: []byte("policy"), ExpiresAt: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	frame := managedprotocol.Frame{
		PayloadType: "command", CommandID: "command-1", ExecutionID: "execution-1", SessionID: "session-1", Payload: payload,
	}
	if err := controller.Dispatch(context.Background(), frame); err != nil {
		t.Fatal(err)
	}
	if executor.start.CommandID != frame.CommandID || executor.start.ExecutionID != frame.ExecutionID || executor.start.SessionID != frame.SessionID || executor.start.TaskID != "task-1" {
		t.Fatalf("start command = %#v, frame = %#v", executor.start, frame)
	}
}

func TestDispatchRejectsPayloadIdentitySubstitution(t *testing.T) {
	executor := &recordingExecutor{}
	controller := NewWithExecutor(executor)
	payload, err := json.Marshal(commandPayload{Kind: "cancel", CommandID: "attacker-command", TaskID: "task-1", RuntimeID: "runtime-1"})
	if err != nil {
		t.Fatal(err)
	}
	err = controller.Dispatch(context.Background(), managedprotocol.Frame{
		PayloadType: "command", CommandID: "command-1", ExecutionID: "execution-1", Payload: payload,
	})
	if err == nil {
		t.Fatal("command identity substitution was accepted")
	}
	if executor.cancel.CommandID != "" {
		t.Fatal("kernel executor received rejected command")
	}
}

type recordingExecutor struct {
	start  kernel.StartExecution
	resume kernel.ResumeExecution
	steer  kernel.SteerExecution
	cancel kernel.CancelExecution
	close  kernel.CloseExecution
	fork   kernel.ForkExecution
}

func (r *recordingExecutor) Start(_ context.Context, command kernel.StartExecution) error {
	r.start = command
	return nil
}
func (r *recordingExecutor) Resume(_ context.Context, command kernel.ResumeExecution) error {
	r.resume = command
	return nil
}
func (r *recordingExecutor) Steer(_ context.Context, command kernel.SteerExecution) error {
	r.steer = command
	return nil
}
func (r *recordingExecutor) Cancel(_ context.Context, command kernel.CancelExecution) error {
	r.cancel = command
	return nil
}
func (r *recordingExecutor) Close(_ context.Context, command kernel.CloseExecution) error {
	r.close = command
	return nil
}
func (r *recordingExecutor) Fork(_ context.Context, command kernel.ForkExecution) error {
	r.fork = command
	return nil
}
