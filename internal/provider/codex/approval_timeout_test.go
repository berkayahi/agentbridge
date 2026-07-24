package codex

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/provider"
)

func TestApprovalTimeoutEmitsCorrelatedDurableLifecycleEvent(t *testing.T) {
	rpc := newFakeRPC()
	adapter := NewAdapter(rpc, AdapterConfig{
		Approvals:       approvalSinkFunc(func(context.Context, ApprovalRequest) error { return nil }),
		ApprovalUser:    func(provider.ID) string { return "operator" },
		ApprovalTimeout: 5 * time.Millisecond,
		Now:             func() time.Time { return time.Unix(100, 0).UTC() },
	})
	t.Cleanup(adapter.Close)
	taskID := provider.MustID("task-timeout")
	adapter.registerSession(provider.Session{ID: provider.MustID("session-timeout"), TaskID: taskID, ThreadID: "thread-timeout"})
	events := adapter.eventsFor("thread-timeout")

	go func() {
		rpc.requests <- ServerMessage{ID: "approval-timeout", Method: "item/commandExecution/requestApproval", Params: json.RawMessage(`{"threadId":"thread-timeout","turnId":"turn-timeout","itemId":"item-timeout","command":"go test ./..."}`)}
	}()
	var requestID provider.ID
	select {
	case got := <-events:
		if got.Type != provider.EventApprovalRequired {
			t.Fatalf("first event = %#v", got)
		}
		requestID = got.RequestID
	case <-time.After(time.Second):
		t.Fatal("approval request was not emitted")
	}
	select {
	case got := <-rpc.responses:
		if got.Decision != "decline" {
			t.Fatalf("timeout response = %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("approval timeout did not fail closed")
	}
	select {
	case got := <-events:
		if got.Type != provider.EventType("approval_expired") || got.RequestID != requestID || got.TaskID != taskID {
			t.Fatalf("timeout event = %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("approval timeout lifecycle event was not emitted")
	}
}
