package codex

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/berkayahi/agentbridge/internal/provider"
)

// A session works in an isolated worktree, but a repository's own toolchain
// writes outside it — Go's build and module caches live in the home directory.
// Without those roots declared, every `go test` a session runs escalates and
// stops at an approval door, which trains an operator to approve blindly. The
// roots are configured per repository and passed through; when none are
// configured nothing is sent and the host's own policy still decides.
func TestWritableRootsArePassedOnlyWhenConfigured(t *testing.T) {
	spawn := func(t *testing.T, writable []string) map[string]any {
		t.Helper()
		var policy map[string]any
		rpc := newFakeRPC()
		rpc.call = func(method string, params any, result any) error {
			switch method {
			case "thread/start":
				setJSON(result, map[string]any{"thread": map[string]any{"id": "thread-1"}})
			case "turn/start":
				if raw, ok := jsonValue(params)["sandboxPolicy"]; ok {
					encoded, _ := json.Marshal(raw)
					_ = json.Unmarshal(encoded, &policy)
				}
				setJSON(result, map[string]any{"turn": map[string]any{"id": "turn-1"}})
			}
			return nil
		}
		adapter := NewAdapter(rpc, AdapterConfig{Sessions: sessionSinkFunc(func(context.Context, provider.Session) error { return nil })})
		t.Cleanup(adapter.Close)
		if _, _, err := adapter.Start(context.Background(), provider.StartRequest{
			TaskID: provider.MustID("task-1"), Input: provider.Input{Text: "work"}, WritablePaths: writable,
		}); err != nil {
			t.Fatal(err)
		}
		return policy
	}

	t.Run("configured roots are declared", func(t *testing.T) {
		policy := spawn(t, []string{"/Users/keeper/Library/Caches/go-build", "/Users/keeper/go/pkg/mod"})
		if policy == nil {
			t.Fatal("no sandbox policy was sent")
		}
		if policy["type"] != "workspaceWrite" {
			t.Fatalf("policy type = %#v", policy["type"])
		}
		roots, ok := policy["writableRoots"].([]any)
		if !ok || len(roots) != 2 || roots[0] != "/Users/keeper/Library/Caches/go-build" {
			t.Fatalf("writable roots = %#v", policy["writableRoots"])
		}
		// Silence would mean denied, and a session that cannot reach the module
		// proxy would stop at a door for every dependency instead.
		if policy["networkAccess"] != true {
			t.Fatalf("network access = %#v, want it kept", policy["networkAccess"])
		}
	})

	t.Run("without configured roots the host decides", func(t *testing.T) {
		if policy := spawn(t, nil); policy != nil {
			t.Fatalf("policy = %#v, want nothing sent", policy)
		}
	})
}
