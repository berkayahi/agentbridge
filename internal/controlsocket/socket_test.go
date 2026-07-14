package controlsocket

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUnixSocketAuthenticatesEveryTaskScopedRequest(t *testing.T) {
	path := shortSocketPath(t)
	handler := HandlerFunc(func(_ context.Context, request Request) (any, error) {
		return map[string]any{"tool": request.Tool}, nil
	})
	server := NewServer(path, handler)
	server.Grant("task-1", "claude", []byte("0123456789abcdef0123456789abcdef"))
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("socket permissions = %o, want 600", got)
	}

	client := Client{Path: path}
	request := Request{TaskID: "task-1", Provider: "claude", Capability: []byte("0123456789abcdef0123456789abcdef"), Tool: "notify_telegram", Params: json.RawMessage(`{"message":"safe"}`)}
	var result map[string]any
	if err := client.Call(context.Background(), request, &result); err != nil {
		t.Fatal(err)
	}
	if result["tool"] != "notify_telegram" {
		t.Fatalf("result = %#v", result)
	}

	for _, mutate := range []func(*Request){
		func(r *Request) { r.TaskID = "other" },
		func(r *Request) { r.Provider = "codex" },
		func(r *Request) { r.Capability = []byte("wrong") },
	} {
		bad := request
		mutate(&bad)
		if err := client.Call(context.Background(), bad, nil); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("error = %v, want ErrUnauthorized", err)
		}
	}
}

func TestSocketRejectsOversizedRequestsAndUnavailableDaemon(t *testing.T) {
	missing := Client{Path: filepath.Join(t.TempDir(), "missing.sock")}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := missing.Call(ctx, Request{Tool: "get_task_context"}, nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable", err)
	}

	path := shortSocketPath(t)
	server := NewServer(path, HandlerFunc(func(context.Context, Request) (any, error) { return nil, nil }))
	server.Grant("task-1", "claude", []byte("0123456789abcdef0123456789abcdef"))
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client := Client{Path: path}
	err := client.Call(context.Background(), Request{TaskID: "task-1", Provider: "claude", Capability: []byte("0123456789abcdef0123456789abcdef"), Tool: "notify_telegram", Params: json.RawMessage(`{"message":"` + strings.Repeat("x", MaxMessageBytes) + `"}`)}, nil)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("error = %v, want ErrTooLarge", err)
	}
}

func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ab-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "control.sock")
}
