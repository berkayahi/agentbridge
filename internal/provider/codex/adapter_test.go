package codex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/provider"
)

func TestAdapterStartsThreadPersistsSessionBeforeTurnAndUsesLocalImage(t *testing.T) {
	dir := t.TempDir()
	image := filepath.Join(dir, "ekran görüntüsü.png")
	if err := os.WriteFile(image, []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	attachment, err := provider.NewLocalAttachment(image, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	rpc := newFakeRPC()
	rpc.call = func(method string, params any, result any) error {
		order = append(order, method)
		switch method {
		case "thread/start":
			setJSON(result, map[string]any{"thread": map[string]any{"id": "thread-1"}})
		case "turn/start":
			payload := jsonValue(params)
			input := payload["input"].([]any)
			if got := input[1].(map[string]any); got["type"] != "localImage" || got["path"] != image {
				t.Fatalf("image input = %#v", got)
			}
			setJSON(result, map[string]any{"turn": map[string]any{"id": "turn-1"}})
		}
		return nil
	}
	sessions := sessionSinkFunc(func(_ context.Context, session provider.Session) error {
		order = append(order, "persist_session")
		if session.ThreadID != "thread-1" {
			t.Fatalf("session = %#v", session)
		}
		return nil
	})
	adapter := NewAdapter(rpc, AdapterConfig{Sessions: sessions, Now: time.Now})
	t.Cleanup(adapter.Close)

	session, _, err := adapter.Start(context.Background(), provider.StartRequest{
		TaskID:           provider.MustID("task-1"),
		WorkingDirectory: dir,
		Model:            "configured-model",
		Input:            provider.Input{Text: "inspect", Attachments: []provider.LocalAttachment{attachment}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if session.ThreadID != "thread-1" || session.ExternalID != "thread-1" {
		t.Fatalf("session = %#v", session)
	}
	want := []string{"thread/start", "persist_session", "turn/start"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
}

func TestAdapterResumesSteersAndInterruptsExactV2Methods(t *testing.T) {
	rpc := newFakeRPC()
	var methods []string
	rpc.call = func(method string, params any, result any) error {
		methods = append(methods, method)
		switch method {
		case "thread/resume":
			setJSON(result, map[string]any{"thread": map[string]any{"id": "thread-1"}})
		case "turn/start", "turn/steer":
			setJSON(result, map[string]any{"turn": map[string]any{"id": "turn-1"}})
		}
		return nil
	}
	adapter := NewAdapter(rpc, AdapterConfig{Sessions: sessionSinkFunc(func(context.Context, provider.Session) error { return nil })})
	t.Cleanup(adapter.Close)
	session := provider.Session{ID: provider.MustID("session-1"), TaskID: provider.MustID("task-1"), ThreadID: "thread-1"}
	session, _, err := adapter.Resume(context.Background(), provider.ResumeRequest{TaskID: session.TaskID, Session: session, Input: provider.Input{Text: "continue"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.Steer(context.Background(), session, provider.Input{Text: "focus tests"}); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Interrupt(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	want := []string{"thread/resume", "turn/start", "turn/steer", "turn/interrupt"}
	if !reflect.DeepEqual(methods, want) {
		t.Fatalf("methods = %v, want %v", methods, want)
	}
}

func TestApprovalIsPersistedBeforeEventAndDecisionIsCorrelated(t *testing.T) {
	rpc := newFakeRPC()
	var order []string
	var requestID provider.ID
	approvals := approvalSinkFunc(func(_ context.Context, approval ApprovalRequest) error {
		order = append(order, "persist")
		requestID = approval.ID
		return nil
	})
	adapter := NewAdapter(rpc, AdapterConfig{
		Sessions:        sessionSinkFunc(func(context.Context, provider.Session) error { return nil }),
		Approvals:       approvals,
		ApprovalUser:    func(provider.ID) string { return "operator-1" },
		ApprovalTimeout: time.Minute,
		Now:             func() time.Time { return time.Unix(100, 0).UTC() },
	})
	t.Cleanup(adapter.Close)
	taskID := provider.MustID("task-1")
	adapter.registerSession(provider.Session{ID: provider.MustID("session-1"), TaskID: taskID, ThreadID: "thread-1"})
	go func() {
		rpc.requests <- ServerMessage{ID: "approval-1", Method: "item/commandExecution/requestApproval", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","command":"go test ./..."}`)}
	}()
	events := adapter.eventsFor("thread-1")
	select {
	case event := <-events:
		order = append(order, "event")
		if event.Type != provider.EventApprovalRequired {
			t.Fatalf("event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("approval event not emitted")
	}
	if !reflect.DeepEqual(order, []string{"persist", "event"}) {
		t.Fatalf("order = %v", order)
	}

	wrong := provider.ApprovalDecision{RequestID: requestID, TaskID: taskID, UserID: "intruder", Allow: true}
	if err := adapter.ResolveApproval(context.Background(), wrong); !errors.Is(err, ErrApprovalRejected) {
		t.Fatalf("wrong-user error = %v", err)
	}
	response := <-rpc.responses
	if response.Decision != "decline" {
		t.Fatalf("response = %#v, want decline", response)
	}
	if err := adapter.ResolveApproval(context.Background(), wrong); !errors.Is(err, ErrApprovalNotPending) {
		t.Fatalf("duplicate error = %v", err)
	}
}

func TestApprovalIDScopesProviderRequestIDs(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	first, err := approvalID(provider.MustID("task-1"), "0", "touch /tmp/one", now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := approvalID(provider.MustID("task-2"), "0", "touch /tmp/one", now)
	if err != nil {
		t.Fatal(err)
	}
	third, err := approvalID(provider.MustID("task-1"), "0", "touch /tmp/one", now.Add(time.Nanosecond))
	if err != nil {
		t.Fatal(err)
	}
	if first == second || first == third || second == third {
		t.Fatalf("approval IDs collided: first=%s second=%s third=%s", first, second, third)
	}
}

func TestApprovalRejectsWrongTaskAndExpiredDecisionAndTimesOutDeny(t *testing.T) {
	for _, test := range []struct {
		name     string
		decision provider.ApprovalDecision
		now      time.Time
	}{
		{name: "wrong task", decision: provider.ApprovalDecision{RequestID: provider.MustID("approval-1"), TaskID: provider.MustID("other-task"), UserID: "operator"}, now: time.Unix(100, 0).UTC()},
		{name: "expired", decision: provider.ApprovalDecision{RequestID: provider.MustID("approval-1"), TaskID: provider.MustID("task-1"), UserID: "operator"}, now: time.Unix(200, 0).UTC()},
	} {
		t.Run(test.name, func(t *testing.T) {
			rpc := newFakeRPC()
			adapter := NewAdapter(rpc, AdapterConfig{Now: func() time.Time { return test.now }})
			t.Cleanup(adapter.Close)
			adapter.pending["approval-1"] = pendingApproval{rpcID: json.RawMessage(`"approval-1"`), request: ApprovalRequest{
				ID: provider.MustID("approval-1"), TaskID: provider.MustID("task-1"), UserID: "operator", ExpiresAt: time.Unix(150, 0).UTC(),
			}}
			if err := adapter.ResolveApproval(context.Background(), test.decision); !errors.Is(err, ErrApprovalRejected) {
				t.Fatalf("error = %v, want ErrApprovalRejected", err)
			}
			if got := <-rpc.responses; got.Decision != "decline" {
				t.Fatalf("decision = %q, want decline", got.Decision)
			}
		})
	}

	rpc := newFakeRPC()
	adapter := NewAdapter(rpc, AdapterConfig{ApprovalTimeout: 5 * time.Millisecond})
	adapter.pending["approval-timeout"] = pendingApproval{rpcID: json.RawMessage(`"approval-timeout"`)}
	adapter.wg.Add(1)
	go adapter.expireApproval(provider.MustID("approval-timeout"), 5*time.Millisecond)
	select {
	case got := <-rpc.responses:
		if got.Decision != "decline" {
			t.Fatalf("timeout decision = %q", got.Decision)
		}
	case <-time.After(time.Second):
		t.Fatal("approval timeout did not default deny")
	}
	adapter.Close()
}

func TestUsageAndAuthReadAccountWithoutStartingTurn(t *testing.T) {
	rpc := newFakeRPC()
	var methods []string
	rpc.call = func(method string, _ any, result any) error {
		methods = append(methods, method)
		switch method {
		case "account/rateLimits/read":
			setJSON(result, map[string]any{"rateLimits": map[string]any{
				"primary":   map[string]any{"usedPercent": 25, "resetsAt": 200},
				"secondary": map[string]any{"usedPercent": 40, "resetsAt": 300},
			}})
		case "account/usage/read":
			setJSON(result, map[string]any{"summary": map[string]any{"lifetimeTokens": 123}})
		case "account/read":
			setJSON(result, map[string]any{"requiresOpenaiAuth": false, "account": map[string]any{"type": "chatgpt", "email": "operator@example.invalid"}})
		}
		return nil
	}
	adapter := NewAdapter(rpc, AdapterConfig{Now: func() time.Time { return time.Unix(10, 0).UTC() }})
	t.Cleanup(adapter.Close)
	usage, err := adapter.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(usage.Windows) != 2 || usage.Windows[0].ResetsAt.Location() != time.UTC {
		t.Fatalf("usage = %#v", usage)
	}
	auth, err := adapter.AuthStatus(context.Background())
	if err != nil || !auth.Authenticated {
		t.Fatalf("auth = %#v, err = %v", auth, err)
	}
	want := []string{"account/rateLimits/read", "account/usage/read", "account/read"}
	if !reflect.DeepEqual(methods, want) {
		t.Fatalf("methods = %v, want %v", methods, want)
	}
}

func TestVisibleNotificationMappingAndAuthError(t *testing.T) {
	event, ok := mapNotification(ServerMessage{Method: "item/agentMessage/delta", Params: json.RawMessage(`{"threadId":"thread-1","delta":"visible answer"}`)}, provider.MustID("task-1"), time.Unix(1, 0).UTC())
	if !ok || event.Type != provider.EventAssistantMessage || event.Message != "visible answer" {
		t.Fatalf("event = %#v, ok = %v", event, ok)
	}
	event, ok = mapNotification(ServerMessage{Method: "error", Params: json.RawMessage(`{"threadId":"thread-1","error":{"message":"login required","codexErrorInfo":"unauthorized"},"willRetry":false}`)}, provider.MustID("task-1"), time.Unix(1, 0).UTC())
	if !ok || event.Type != provider.EventAuthRequired {
		t.Fatalf("event = %#v, ok = %v", event, ok)
	}
}

type fakeRPC struct {
	call          func(string, any, any) error
	notifications chan ServerMessage
	requests      chan ServerMessage
	responses     chan approvalResponse
}

type approvalResponse struct {
	ID       string
	Decision string
}

func newFakeRPC() *fakeRPC {
	return &fakeRPC{notifications: make(chan ServerMessage, 8), requests: make(chan ServerMessage, 8), responses: make(chan approvalResponse, 8)}
}

func (f *fakeRPC) Call(_ context.Context, method string, params, result any) error {
	if f.call == nil {
		return nil
	}
	return f.call(method, params, result)
}
func (f *fakeRPC) Notify(context.Context, string, any) error { return nil }

func (f *fakeRPC) RespondResult(_ context.Context, id json.RawMessage, result any) error {
	decision := jsonValue(result)["decision"].(string)
	f.responses <- approvalResponse{ID: normalizeID(id), Decision: decision}
	return nil
}
func (f *fakeRPC) Notifications() <-chan ServerMessage { return f.notifications }
func (f *fakeRPC) Requests() <-chan ServerMessage      { return f.requests }

func setJSON(target any, value any) {
	data, _ := json.Marshal(value)
	_ = json.Unmarshal(data, target)
}

func jsonValue(value any) map[string]any {
	data, _ := json.Marshal(value)
	var decoded map[string]any
	_ = json.Unmarshal(data, &decoded)
	return decoded
}

type sessionSinkFunc func(context.Context, provider.Session) error

func (f sessionSinkFunc) SaveSession(ctx context.Context, session provider.Session) error {
	return f(ctx, session)
}

type approvalSinkFunc func(context.Context, ApprovalRequest) error

func (f approvalSinkFunc) SaveApproval(ctx context.Context, approval ApprovalRequest) error {
	return f(ctx, approval)
}

var _ = sync.Mutex{}
