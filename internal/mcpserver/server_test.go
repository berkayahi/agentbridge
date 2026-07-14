package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/controlsocket"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestServerExposesExactlyFourBoundedTools(t *testing.T) {
	caller := &fakeCaller{}
	server := New(caller, Scope{TaskID: "task-1", Provider: "claude", Capability: []byte("capability"), ApprovalTimeout: time.Second})
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx := context.Background()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "dev"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()

	listed, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	want := []string{"get_task_context", "notify_telegram", "request_telegram_approval", "send_artifact"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}

	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "notify_telegram", Arguments: map[string]any{"message": "done"}})
	if err != nil || result.IsError {
		t.Fatalf("CallTool error = %v, result = %#v", err, result)
	}
	if caller.last.Tool != "notify_telegram" || caller.last.TaskID != "task-1" {
		t.Fatalf("request = %#v", caller.last)
	}
	result, err = clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "notify_telegram", Arguments: map[string]any{"message": "done", "unknown": true}})
	if err == nil && !result.IsError {
		t.Fatal("unknown field was accepted")
	}
}

func TestApprovalTimeoutReturnsDenyAndCancellationPropagates(t *testing.T) {
	caller := &fakeCaller{call: func(ctx context.Context, _ controlsocket.Request, result any) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	server := New(caller, Scope{TaskID: "task-1", Provider: "claude", Capability: []byte("capability"), ApprovalTimeout: 5 * time.Millisecond})
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx := context.Background()
	ss, _ := server.Connect(ctx, serverTransport, nil)
	defer ss.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "dev"}, nil)
	cs, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	result, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "request_telegram_approval", Arguments: map[string]any{"kind": "command", "summary": "run tests"}})
	if err != nil {
		t.Fatal(err)
	}
	output, ok := result.StructuredContent.(map[string]any)
	if !ok || output["approved"] != false {
		t.Fatalf("output = %#v, want deny", result.StructuredContent)
	}
}

func TestCapabilityReaderPrefersInheritedFDAndTestEnvRequiresOptIn(t *testing.T) {
	capability, err := ReadCapability(3, func(int) ([]byte, error) { return []byte("from-fd\n"), nil }, func(string) string { return "" })
	if err != nil || string(capability) != "from-fd" {
		t.Fatalf("capability = %q, err = %v", capability, err)
	}
	_, err = ReadCapability(3, func(int) ([]byte, error) { return nil, errors.New("closed") }, func(name string) string {
		if name == testCapabilityEnv {
			return "secret"
		}
		return ""
	})
	if err == nil {
		t.Fatal("test env fallback accepted without explicit opt-in")
	}
}

type fakeCaller struct {
	last controlsocket.Request
	call func(context.Context, controlsocket.Request, any) error
}

func (f *fakeCaller) Call(ctx context.Context, request controlsocket.Request, result any) error {
	f.last = request
	if f.call != nil {
		return f.call(ctx, request, result)
	}
	if result != nil {
		data, _ := json.Marshal(map[string]any{"ok": true, "approved": true})
		_ = json.Unmarshal(data, result)
	}
	return nil
}
