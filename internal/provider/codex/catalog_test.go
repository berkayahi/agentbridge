package codex

import (
	"context"
	"testing"

	"github.com/berkayahi/agentbridge/internal/provider"
)

func TestExecutionCatalogPreservesPerModelEffortsAndClassifiesUltra(t *testing.T) {
	rpc := newFakeRPC()
	rpc.call = func(method string, _ any, result any) error {
		if method != "model/list" {
			t.Fatalf("method = %q", method)
		}
		setJSON(result, map[string]any{
			"data": []any{
				map[string]any{
					"id": "gpt-5.6-sol", "model": "gpt-5.6-sol", "displayName": "GPT-5.6-Sol",
					"description": "Frontier model", "isDefault": true, "hidden": false,
					"defaultReasoningEffort": "low",
					"supportedReasoningEfforts": []any{
						map[string]any{"reasoningEffort": "max", "description": "Maximum reasoning"},
						map[string]any{"reasoningEffort": "ultra", "description": "Automatic task delegation"},
					},
				},
				map[string]any{
					"id": "gpt-5.6-luna", "model": "gpt-5.6-luna", "displayName": "GPT-5.6-Luna",
					"description": "Fast model", "isDefault": false, "hidden": false,
					"defaultReasoningEffort": "medium",
					"supportedReasoningEfforts": []any{
						map[string]any{"reasoningEffort": "low", "description": "Lighter reasoning"},
						map[string]any{"reasoningEffort": "max", "description": "Maximum reasoning"},
					},
				},
			},
			"nextCursor": nil,
		})
		return nil
	}
	adapter := NewAdapter(rpc, AdapterConfig{})
	t.Cleanup(adapter.Close)

	catalog, err := adapter.ExecutionCatalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if catalog.DefaultModel != "gpt-5.6-sol" || len(catalog.Models) != 2 {
		t.Fatalf("catalog = %#v", catalog)
	}
	sol := catalog.Models[0]
	if sol.ReasoningEfforts[1].ID != "ultra" || sol.ReasoningEfforts[1].Kind != provider.ReasoningEffortOrchestration {
		t.Fatalf("Sol efforts = %#v", sol.ReasoningEfforts)
	}
	luna := catalog.Models[1]
	for _, effort := range luna.ReasoningEfforts {
		if effort.ID == "ultra" {
			t.Fatalf("Luna advertised unsupported Ultra: %#v", luna)
		}
		if effort.Kind != provider.ReasoningEffortStandard {
			t.Fatalf("Luna effort kind = %#v", effort)
		}
	}
}

func TestInitializeAppServerUsesProtocolHandshake(t *testing.T) {
	rpc := newFakeRPC()
	var called, notified bool
	rpc.call = func(method string, params any, result any) error {
		called = method == "initialize"
		value := jsonValue(params)
		client := value["clientInfo"].(map[string]any)
		if client["name"] != "agentbridge" {
			t.Fatalf("client info = %#v", client)
		}
		setJSON(result, map[string]any{"userAgent": "test"})
		return nil
	}
	rpc.notify = func(method string, _ any) error {
		notified = method == "initialized"
		return nil
	}
	if err := InitializeAppServer(context.Background(), rpc); err != nil {
		t.Fatal(err)
	}
	if !called || !notified {
		t.Fatalf("handshake called=%v notified=%v", called, notified)
	}
}
