package codex

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

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
		AnalysisIsolation: testAnalysisIsolation(),
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

func TestAnalyzeReadOnlyDeclinesApprovalWithoutPersistingOrEmittingApprovalEvent(t *testing.T) {
	workspace := t.TempDir()
	rpc := newFakeRPC()
	approvalSaves := 0
	rpc.call = func(method string, params any, result any) error {
		switch method {
		case "thread/start":
			setJSON(result, map[string]any{"thread": map[string]any{"id": "analysis-approval-thread"}})
		case "turn/start":
			setJSON(result, map[string]any{"turn": map[string]any{"id": "analysis-approval-turn"}})
			rpc.requests <- ServerMessage{ID: "analysis-approval-request", Method: "item/commandExecution/requestApproval", Params: json.RawMessage(`{"threadId":"analysis-approval-thread","command":"write file"}`)}
		case "turn/interrupt":
			setJSON(result, map[string]any{})
		}
		return nil
	}
	adapter := NewAdapter(rpc, AdapterConfig{
		AnalysisIsolation: testAnalysisIsolation(),
		Approvals:         approvalSinkFunc(func(context.Context, ApprovalRequest) error { approvalSaves++; return nil }),
	})
	t.Cleanup(adapter.Close)
	policy := provider.NewReadOnlyAnalysisPolicy(workspace)
	_, err := adapter.AnalyzeReadOnly(context.Background(), provider.AnalysisRequest{
		TaskID: provider.MustID("analysis-approval-task"), Input: provider.Input{Text: "inspect"},
		WorkingDirectory: workspace, Policy: policy,
	})
	if !errors.Is(err, provider.ErrAnalysisApprovalDeclined) {
		t.Fatalf("error = %v, want approval decline", err)
	}
	if approvalSaves != 0 {
		t.Fatalf("approval sink saves = %d, want 0", approvalSaves)
	}
	select {
	case response := <-rpc.responses:
		if response.Decision != "decline" {
			t.Fatalf("provider decision = %q, want decline", response.Decision)
		}
	case <-time.After(time.Second):
		t.Fatal("analysis approval was not declined")
	}
}

func TestAnalyzeReadOnlyCancellationInterruptsAndCleansUpTurn(t *testing.T) {
	workspace := t.TempDir()
	rpc := newFakeRPC()
	interrupts := make(chan struct{}, 1)
	turnStarted := make(chan struct{})
	rpc.call = func(method string, _ any, result any) error {
		switch method {
		case "thread/start":
			setJSON(result, map[string]any{"thread": map[string]any{"id": "analysis-cancel-thread"}})
		case "turn/start":
			setJSON(result, map[string]any{"turn": map[string]any{"id": "analysis-cancel-turn"}})
			close(turnStarted)
		case "turn/interrupt":
			interrupts <- struct{}{}
		}
		return nil
	}
	adapter := NewAdapter(rpc, AdapterConfig{AnalysisIsolation: testAnalysisIsolation()})
	t.Cleanup(adapter.Close)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := adapter.AnalyzeReadOnly(ctx, provider.AnalysisRequest{
			TaskID: provider.MustID("analysis-cancel-task"), Input: provider.Input{Text: "inspect"},
			WorkingDirectory: workspace, Policy: provider.NewReadOnlyAnalysisPolicy(workspace),
		})
		done <- err
	}()
	select {
	case <-turnStarted:
	case <-time.After(time.Second):
		t.Fatal("analysis turn did not start")
	}
	cancel()
	select {
	case <-interrupts:
	case <-time.After(time.Second):
		t.Fatal("canceled analysis did not interrupt the active turn")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled analysis error = %v", err)
	}
	if err := adapter.Interrupt(context.Background(), provider.Session{ThreadID: "analysis-cancel-thread"}); err == nil {
		t.Fatal("canceled analysis session was not cleaned up")
	}
}

func testAnalysisIsolation() provider.AnalysisIsolationAttestation {
	return provider.AnalysisIsolationAttestation{
		Mechanism: "test-sandbox", FilesystemReadsWorkspaceOnly: true, HostEnvironmentExcluded: true,
		NetworkDenied: true, ProductionDataDenied: true, DestructiveActionsDenied: true,
	}
}
