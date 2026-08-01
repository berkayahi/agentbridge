package codex

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/berkayahi/agentbridge/internal/provider"
)

func TestAnalyzeReadOnlyUsesExplicitSandboxAndDoesNotPersistSyntheticSession(t *testing.T) {
	workspace := t.TempDir()
	rpc := newFakeRPC()
	persistErr := errors.New("synthetic session must not be persisted")
	rpc.call = func(method string, params any, result any) error {
		switch method {
		case "thread/start":
			setJSON(result, map[string]any{"thread": map[string]any{"id": "analysis-thread"}})
		case "turn/start":
			values := jsonValue(params)
			policy, ok := values["sandboxPolicy"].(map[string]any)
			if !ok || policy["networkAccess"] != false {
				t.Fatalf("sandbox policy = %#v", values["sandboxPolicy"])
			}
			roots, ok := policy["writableRoots"].([]any)
			if !ok || len(roots) != 1 || roots[0] != workspace {
				t.Fatalf("sandbox roots = %#v", policy["writableRoots"])
			}
			rpc.notifications <- ServerMessage{Method: "item/completed", Params: json.RawMessage(`{"threadId":"analysis-thread","item":{"type":"agentMessage","text":"{\"role\":\"cartographer\",\"findings\":[],\"capabilities\":[],\"assumptions\":[],\"conflicts\":[],\"unknowns\":[]}"}}`)}
			rpc.notifications <- ServerMessage{Method: "turn/completed", Params: json.RawMessage(`{"threadId":"analysis-thread"}`)}
			setJSON(result, map[string]any{"turn": map[string]any{"id": "analysis-turn"}})
		}
		return nil
	}
	adapter := NewAdapter(rpc, AdapterConfig{
		AnalysisIsolation: true,
		Sessions:          sessionSinkFunc(func(context.Context, provider.Session) error { return persistErr }),
	})
	t.Cleanup(adapter.Close)
	policy := provider.NewReadOnlyAnalysisPolicy(workspace)
	result, err := adapter.AnalyzeReadOnly(context.Background(), provider.AnalysisRequest{
		TaskID: provider.MustID("synthetic-analysis-task"), Input: provider.Input{Text: "inspect"},
		WorkingDirectory: workspace, Model: "fixture-model", Policy: policy,
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Output) == "" || result.ProviderID != adapter.Name() {
		t.Fatalf("analysis result = %#v", result)
	}
	if filepath.Clean(workspace) != workspace {
		t.Fatal("test workspace was not clean")
	}
}

func TestAnalyzeReadOnlyRejectsMissingIsolationCapability(t *testing.T) {
	policy := provider.NewReadOnlyAnalysisPolicy(t.TempDir())
	adapter := NewAdapter(newFakeRPC(), AdapterConfig{})
	t.Cleanup(adapter.Close)
	_, err := adapter.AnalyzeReadOnly(context.Background(), provider.AnalysisRequest{
		TaskID: provider.MustID("analysis-task"), Input: provider.Input{Text: "inspect"},
		WorkingDirectory: policy.WorkspacePath, Policy: policy,
	})
	if !errors.Is(err, provider.ErrAnalysisUnavailable) {
		t.Fatalf("error = %v, want unavailable", err)
	}
}
