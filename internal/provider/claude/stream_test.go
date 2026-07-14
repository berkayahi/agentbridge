package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/provider"
)

func TestCommandArgumentsAndEnvironmentUseSubscriptionStreamJSON(t *testing.T) {
	args := CommandArgs("/runtime/task-mcp.json", "session-1", "opus")
	want := []string{"-p", "--verbose", "--input-format", "stream-json", "--output-format", "stream-json", "--permission-prompt-tool", "mcp__agentbridge__request_telegram_approval", "--mcp-config", "/runtime/task-mcp.json", "--model", "opus", "--resume", "session-1"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
	env := ChildEnvironment([]string{
		"PATH=/bin",
		"OPENAI_API_KEY=bad",
		"ANTHROPIC_API_KEY=bad",
		"ANTHROPIC_AUTH_TOKEN=bad",
		"CLAUDE_CODE_OAUTH_TOKEN=bad",
		"AGENTBRIDGE_CONTROL_SOCKET=/wrong",
		"AGENTBRIDGE_TASK_ID=wrong",
		"AGENTBRIDGE_PROVIDER=wrong",
		"AGENTBRIDGE_CAPABILITY=must-not-survive",
	}, "/runtime/claude", "task-1", "/run/agentbridge/control.sock")
	joined := strings.Join(env, "\n")
	for _, forbidden := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "AGENTBRIDGE_CAPABILITY"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("environment retained %s", forbidden)
		}
	}
	for _, required := range []string{
		"CLAUDE_CONFIG_DIR=/runtime/claude",
		"AGENTBRIDGE_CONTROL_SOCKET=/run/agentbridge/control.sock",
		"AGENTBRIDGE_TASK_ID=task-1",
		"AGENTBRIDGE_PROVIDER=claude",
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("environment = %q, want %q", joined, required)
		}
	}
	if strings.Contains(strings.Join(args, "\n"), "capability") {
		t.Fatalf("environment = %q", joined)
	}
}

func TestProcessHelperInitializesSessionAndStreamsEvents(t *testing.T) {
	if os.Getenv("GO_WANT_CLAUDE_HELPER") == "1" {
		capabilityFile := os.NewFile(3, "agentbridge-capability")
		capability, err := io.ReadAll(capabilityFile)
		if err != nil || string(capability) != "task-capability" {
			fmt.Fprintln(os.Stderr, "capability fd unavailable")
			os.Exit(2)
		}
		if os.Getenv("AGENTBRIDGE_CONTROL_SOCKET") != "/run/agentbridge/control.sock" || os.Getenv("AGENTBRIDGE_TASK_ID") != "task-1" || os.Getenv("AGENTBRIDGE_PROVIDER") != "claude" {
			fmt.Fprintln(os.Stderr, "task scope unavailable")
			os.Exit(2)
		}
		scanner := bufio.NewScanner(os.Stdin)
		if scanner.Scan() {
			var input any
			if json.Unmarshal(scanner.Bytes(), &input) != nil {
				os.Exit(2)
			}
			fmt.Fprintln(os.Stdout, `{"type":"system","subtype":"init","session_id":"helper-session"}`)
			fmt.Fprintln(os.Stdout, `{"type":"result","subtype":"success","is_error":false,"session_id":"helper-session","result":"done"}`)
		}
		for scanner.Scan() {
		}
		os.Exit(0)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p, err := StartProcess(ctx, ProcessConfig{
		Executable:      os.Args[0],
		testArgs:        []string{"-test.run=TestProcessHelperInitializesSessionAndStreamsEvents"},
		Environment:     append(os.Environ(), "GO_WANT_CLAUDE_HELPER=1", "ANTHROPIC_API_KEY=must-be-scrubbed"),
		ClaudeConfigDir: t.TempDir(), MCPConfigPath: "/tmp/test-mcp.json", Model: "opus",
		ControlSocket: "/run/agentbridge/control.sock", Capability: []byte("task-capability"),
		InitialInput: provider.Input{Text: "hello"}, TaskID: provider.MustID("task-1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.SessionID() != "helper-session" {
		t.Fatalf("session = %q", p.SessionID())
	}
	select {
	case event := <-p.Events():
		if event.Type != provider.EventCompleted || event.TaskID != provider.MustID("task-1") {
			t.Fatalf("event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("stream event not received")
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestGeneratedMCPConfigIsOwnerOnlyAndContainsNoCapability(t *testing.T) {
	path, err := WriteMCPConfig(t.TempDir(), "/usr/local/bin/agentbridge")
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	if bytes.Contains(data, []byte("capability")) || !bytes.Contains(data, []byte(`"args":["mcp"]`)) {
		t.Fatalf("config = %s", data)
	}
}

func TestEnsureStatuslineSettingsMergesWithoutClobberingAuthConfiguration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"permissions":{"allow":["Read"]},"theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureStatuslineSettings(dir, "/usr/local/bin/agentbridge"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	if settings["theme"] != "dark" || settings["permissions"] == nil {
		t.Fatalf("existing settings were clobbered: %#v", settings)
	}
	status, _ := settings["statusLine"].(map[string]any)
	if status["type"] != "command" || status["command"] != "/usr/local/bin/agentbridge claude-statusline" {
		t.Fatalf("statusLine=%#v", status)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("settings mode=%v err=%v", info.Mode(), err)
	}
}

func TestParseStreamMapsVisibleEventsAndStructuredFailures(t *testing.T) {
	data, err := os.Open("testdata/session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	parsed := ParseStream(data, 64*1024)
	var types []provider.EventType
	var sessionID string
	for item := range parsed {
		if item.SessionID != "" {
			sessionID = item.SessionID
		}
		if item.Event.Type != "" {
			types = append(types, item.Event.Type)
		}
	}
	if sessionID != "session-example" {
		t.Fatalf("session = %q", sessionID)
	}
	want := []provider.EventType{provider.EventAssistantMessage, provider.EventToolStarted, provider.EventToolEnded, provider.EventRateLimited, provider.EventCompleted}
	if !reflect.DeepEqual(types, want) {
		t.Fatalf("event types = %v, want %v", types, want)
	}

	malformed := ParseStream(strings.NewReader("not-json\n"), 1024)
	if got := <-malformed; got.Event.Type != provider.EventError {
		t.Fatalf("malformed event = %#v", got)
	}
}

func TestInputMessagePreservesUnicodeAttachmentPathsWithoutShell(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ekran görüntüsü final.png")
	if err := os.WriteFile(path, []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	attachment, _ := provider.NewLocalAttachment(path, "image/png")
	line, err := InputLine(provider.Input{Text: "inspect", Attachments: []provider.LocalAttachment{attachment}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(line, []byte(path)) || !bytes.Contains(line, []byte("Inspect each local attachment")) {
		t.Fatalf("input line = %s", line)
	}
	var valid any
	if json.Unmarshal(bytes.TrimSpace(line), &valid) != nil {
		t.Fatal("input is not JSON")
	}
}

func TestAdapterPersistsSessionBeforeReturningAndReadsCachedUsage(t *testing.T) {
	var order []string
	runner := &fakeRunner{sessionID: "session-1", events: make(chan provider.Event, 2)}
	spawner := spawnFunc(func(context.Context, ProcessConfig) (Runner, error) {
		order = append(order, "spawn")
		return runner, nil
	})
	cache := NewUsageCache()
	cache.Update(UsageSnapshot{SessionID: "session-1", ObservedAt: time.Unix(10, 0).UTC(), FiveHour: &UsageWindow{UsedPercent: 20, ResetsAt: time.Unix(100, 0).UTC()}})
	adapter := NewAdapter(AdapterConfig{
		Spawn: spawner, Usage: cache,
		Sessions: sessionSinkFunc(func(_ context.Context, session provider.Session) error {
			order = append(order, "persist")
			if session.ExternalID != "session-1" {
				t.Fatalf("session = %#v", session)
			}
			return nil
		}),
	})
	session, _, err := adapter.Start(context.Background(), provider.StartRequest{TaskID: provider.MustID("task-1"), Input: provider.Input{Text: "work"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"spawn", "persist"}) {
		t.Fatalf("order = %v", order)
	}
	if err := adapter.Steer(context.Background(), session, provider.Input{Text: "continue"}); err != nil || len(runner.sent) != 1 {
		t.Fatalf("steer error = %v, sent = %d", err, len(runner.sent))
	}
	usage, err := adapter.Usage(context.Background())
	if err != nil || len(usage.Windows) != 1 || usage.ObservedAt != time.Unix(10, 0).UTC() {
		t.Fatalf("usage = %#v, err = %v", usage, err)
	}
}

func TestAdapterUsesDistinctTaskScopesAndRevokesTerminalCapability(t *testing.T) {
	var configs []ProcessConfig
	revoked := make(chan string, 2)
	spawner := spawnFunc(func(_ context.Context, cfg ProcessConfig) (Runner, error) {
		cfg.Capability = append([]byte(nil), cfg.Capability...)
		configs = append(configs, cfg)
		events := make(chan provider.Event, 1)
		events <- provider.Event{Type: provider.EventCompleted}
		return &fakeRunner{sessionID: "session-" + cfg.TaskID.String(), events: events}, nil
	})
	adapter := NewAdapter(AdapterConfig{Spawn: spawner, Scope: func(id provider.ID) (TaskScope, error) {
		return TaskScope{ControlSocket: "/run/agentbridge/control.sock", Capability: []byte("cap-" + id.String()), Revoke: func() { revoked <- id.String() }}, nil
	}})
	for _, taskID := range []string{"task-1", "task-2"} {
		_, events, err := adapter.Start(context.Background(), provider.StartRequest{TaskID: provider.MustID(taskID), Input: provider.Input{Text: "work"}})
		if err != nil {
			t.Fatal(err)
		}
		if got := <-events; got.Type != provider.EventCompleted {
			t.Fatalf("event = %#v", got)
		}
	}
	if string(configs[0].Capability) == string(configs[1].Capability) || string(configs[0].Capability) != "cap-task-1" || string(configs[1].Capability) != "cap-task-2" {
		t.Fatalf("capabilities = %q %q", configs[0].Capability, configs[1].Capability)
	}
	seen := map[string]bool{}
	for range 2 {
		select {
		case id := <-revoked:
			seen[id] = true
		case <-time.After(time.Second):
			t.Fatal("capability was not revoked")
		}
	}
	if !seen["task-1"] || !seen["task-2"] {
		t.Fatalf("revoked = %v", seen)
	}
}

func TestAgentSDKAllowanceExhaustionMapsToPausedRateLimit(t *testing.T) {
	parsed := ParseStream(strings.NewReader(`{"type":"result","subtype":"error_max_usage","is_error":true,"result":"Agent SDK monthly limit reached","session_id":"session-1"}`+"\n"), 1024)
	got := <-parsed
	if got.Event.Type != provider.EventRateLimited || !got.Paused {
		t.Fatalf("parsed = %#v", got)
	}
}

type fakeRunner struct {
	sessionID string
	events    chan provider.Event
	sent      []provider.Input
}

func (r *fakeRunner) SessionID() string             { return r.sessionID }
func (r *fakeRunner) Events() <-chan provider.Event { return r.events }
func (r *fakeRunner) Send(_ context.Context, input provider.Input) error {
	r.sent = append(r.sent, input)
	return nil
}
func (r *fakeRunner) Close() error { return nil }

type spawnFunc func(context.Context, ProcessConfig) (Runner, error)

func (f spawnFunc) Spawn(ctx context.Context, cfg ProcessConfig) (Runner, error) { return f(ctx, cfg) }

type sessionSinkFunc func(context.Context, provider.Session) error

func (f sessionSinkFunc) SaveSession(ctx context.Context, session provider.Session) error {
	return f(ctx, session)
}
