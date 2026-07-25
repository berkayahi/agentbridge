package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/provider"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

func TestProcessAdapterCarriesExecutionProfileThroughStartAndResume(t *testing.T) {
	if os.Getenv("GO_WANT_CODEX_PROFILE_HELPER") == "1" {
		runExecutionProfileHelper()
		os.Exit(0)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	process, err := StartProcess(ctx, ProcessConfig{
		Executable: os.Args[0],
		Args:       []string{"-test.run=TestProcessAdapterCarriesExecutionProfileThroughStartAndResume", "--"},
		Env:        append(os.Environ(), "GO_WANT_CODEX_PROFILE_HELPER=1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer process.Close()
	if err := InitializeAppServer(ctx, process.Client); err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(process.Client, AdapterConfig{})
	t.Cleanup(adapter.Close)

	sol := workmodel.ExecutionProfile{Model: "gpt-5.6-sol", ReasoningEffort: "ultra"}
	session, _, err := adapter.Start(ctx, provider.StartRequest{
		TaskID: provider.MustID("task-1"), Input: provider.Input{Text: "start"}, ExecutionProfile: sol,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := adapter.Resume(ctx, provider.ResumeRequest{
		TaskID: provider.MustID("task-1"), Session: session,
		Input: provider.Input{Text: "resume"}, ExecutionProfile: sol,
	}); err != nil {
		t.Fatal(err)
	}
}

func runExecutionProfileHelper() {
	scanner := bufio.NewScanner(os.Stdin)
	turn := 0
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil || len(request.ID) == 0 {
			continue
		}
		result := any(map[string]any{})
		switch request.Method {
		case "initialize":
			result = map[string]any{"userAgent": "profile-helper"}
		case "thread/start":
			if request.Params["model"] != "gpt-5.6-sol" {
				writeProfileHelperError(request.ID, "thread/start model")
				continue
			}
			result = map[string]any{"thread": map[string]any{"id": "thread-1"}}
		case "thread/resume":
			if request.Params["model"] != "gpt-5.6-sol" {
				writeProfileHelperError(request.ID, "thread/resume model")
				continue
			}
			result = map[string]any{"thread": map[string]any{"id": "thread-1"}}
		case "turn/start":
			turn++
			wantModel, wantEffort := "gpt-5.6-sol", "ultra"
			if request.Params["model"] != wantModel || request.Params["effort"] != wantEffort {
				writeProfileHelperError(request.ID, "turn/start profile")
				continue
			}
			result = map[string]any{"turn": map[string]any{"id": fmt.Sprintf("turn-%d", turn)}}
		default:
			writeProfileHelperError(request.ID, "unexpected method")
			continue
		}
		response, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(request.ID), "result": result})
		fmt.Println(string(response))
	}
}

func writeProfileHelperError(id json.RawMessage, message string) {
	response, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": json.RawMessage(id),
		"error": map[string]any{"code": -32602, "message": message},
	})
	fmt.Println(string(response))
}
