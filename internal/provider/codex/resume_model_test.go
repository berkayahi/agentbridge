package codex

import (
	"context"
	"testing"

	"github.com/berkayahi/agentbridge/internal/provider"
)

// A bee resumed after a restart must fly the model she left with. Codex takes a
// per-turn model override, so a resume that omits it silently drops the keeper's
// choice back to whatever the provider defaults to — and the card would still
// claim the chosen one.
func TestResumeKeepsTheChosenModel(t *testing.T) {
	var resumed any
	rpc := newFakeRPC()
	rpc.call = func(method string, params any, result any) error {
		switch method {
		case "thread/resume":
			setJSON(result, map[string]any{"thread": map[string]any{"id": "thread-1"}})
		case "turn/start":
			resumed = jsonValue(params)["model"]
			setJSON(result, map[string]any{"turn": map[string]any{"id": "turn-1"}})
		}
		return nil
	}
	adapter := NewAdapter(rpc, AdapterConfig{Sessions: sessionSinkFunc(func(context.Context, provider.Session) error { return nil })})
	t.Cleanup(adapter.Close)
	if _, _, err := adapter.Resume(context.Background(), provider.ResumeRequest{
		TaskID:  provider.MustID("task-1"),
		Session: provider.Session{ID: provider.MustID("session-1"), TaskID: provider.MustID("task-1"), ThreadID: "thread-1"},
		Input:   provider.Input{Text: "continue"},
		Model:   "gpt-5.7-sol",
	}); err != nil {
		t.Fatal(err)
	}
	if resumed != "gpt-5.7-sol" {
		t.Fatalf("resumed model = %#v, want the model she left with", resumed)
	}
}
