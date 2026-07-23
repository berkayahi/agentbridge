package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/approval"
	"github.com/berkayahi/agentbridge/internal/config"
	"github.com/berkayahi/agentbridge/internal/controlsocket"
	"github.com/berkayahi/agentbridge/internal/provider"
	"github.com/berkayahi/agentbridge/internal/provider/codex"
	"github.com/berkayahi/agentbridge/internal/security"
	"github.com/berkayahi/agentbridge/internal/store/sqlite"
	"github.com/berkayahi/agentbridge/internal/task"
	"github.com/berkayahi/agentbridge/internal/telegram"
)

func TestDeriveRuntimePathsRequiresAbsoluteDataDirectory(t *testing.T) {
	for _, value := range []string{"", ".", "relative/data"} {
		if _, err := deriveRuntimePaths(value); err == nil {
			t.Fatalf("deriveRuntimePaths(%q) succeeded", value)
		}
	}
}

func TestCodexApprovalSinkRedactsBeforeFirstDurableWrite(t *testing.T) {
	const secret = "recognizable-bot-token-literal"
	data, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "agentbridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = data.Close() })
	now := time.Unix(100, 0).UTC()
	if err := data.CreateTask(context.Background(), task.Task{
		ID: "task-redacted", RepoProfileID: "sample", Prompt: "test", State: task.Queued,
		Provider: task.ProviderCodex, TelegramChatID: 100, CreatedAt: now, UpdatedAt: now,
	}, task.Event{ID: "event-redacted", TaskID: "task-redacted", Type: task.EventTaskCreated, Visibility: task.VisibilityUser, Payload: []byte(`{}`), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	sink := approvalSink{store: data, redactor: security.NewRedactor(security.Config{Secrets: []string{secret}})}
	if err := sink.SaveApproval(context.Background(), codex.ApprovalRequest{
		ID: provider.MustID("approval-redacted"), TaskID: provider.MustID("task-redacted"),
		Kind: "command", Summary: "execute " + secret, CreatedAt: now, ExpiresAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	values, err := data.PendingApprovals(context.Background())
	if err != nil || len(values) != 1 {
		t.Fatalf("approvals = %#v, err = %v", values, err)
	}
	if got := string(values[0].RequestPayload); strings.Contains(got, secret) || !strings.Contains(got, "REDACTED") {
		t.Fatalf("durable approval payload = %s", got)
	}
}

func TestPrepareRuntimePathsCreatesOnlyOwnerAccessibleDirectories(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	paths, err := deriveRuntimePaths(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.prepare(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.data, paths.attachments, paths.worktrees, paths.runtime, paths.claudeConfig, paths.mcpConfig} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("mode %s = %s, want owner-only directory", path, info.Mode())
		}
	}
	if got, want := paths.database, filepath.Join(root, "agentbridge.db"); got != want {
		t.Fatalf("database=%q, want %q", got, want)
	}
	if got, want := paths.controlSocket, filepath.Join(root, "run", "control.sock"); got != want {
		t.Fatalf("controlSocket=%q, want %q", got, want)
	}
}

func TestPrepareRuntimePathsRejectsSymlinkDataDirectory(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "state")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	paths, err := deriveRuntimePaths(link)
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.prepare(); err == nil {
		t.Fatal("prepare accepted symlink data directory")
	}
}

func TestSubscriptionEnvironmentSharesClaudeConfigWithoutProviderSecrets(t *testing.T) {
	got := subscriptionEnvironment([]string{
		"PATH=/usr/bin", "HOME=/home/service", "LANG=C.UTF-8",
		"OPENAI_API_KEY=forbidden", "ANTHROPIC_API_KEY=forbidden",
		"ANTHROPIC_AUTH_TOKEN=forbidden", "CLAUDE_CODE_OAUTH_TOKEN=forbidden",
		"CLAUDE_CONFIG_DIR=/wrong", "AGENTBRIDGE_TASK_ID=stale",
	}, "/state/claude")
	joined := strings.Join(got, "\n")
	for _, forbidden := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "/wrong", "AGENTBRIDGE_TASK_ID"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("environment retained %q: %q", forbidden, joined)
		}
	}
	for _, want := range []string{"PATH=/usr/bin", "HOME=/home/service", "LANG=C.UTF-8", "CLAUDE_CONFIG_DIR=/state/claude"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("environment missing %q: %q", want, joined)
		}
	}
}

func TestClaudeSubscriptionAuthCheckerUsesNonTurnStatusCommand(t *testing.T) {
	runner := &authCommandStub{output: []byte(`{"loggedIn":true,"subscriptionType":"max"}`)}
	checkedAt := time.Unix(500, 0).UTC()
	checker := claudeSubscriptionAuthChecker(runner, func() time.Time { return checkedAt })
	status, err := checker(context.Background())
	if err != nil || !status.Authenticated || status.CheckedAt != checkedAt {
		t.Fatalf("status=%#v err=%v", status, err)
	}
	if runner.name != "claude" || strings.Join(runner.args, " ") != "auth status --json" {
		t.Fatalf("command=%q args=%v", runner.name, runner.args)
	}

	runner.output, runner.err = []byte(`{"loggedIn":false}`), errors.New("exit 1")
	status, err = checker(context.Background())
	if err != nil || status.Authenticated {
		t.Fatalf("logged-out status=%#v err=%v", status, err)
	}
}

func TestHealthAdapterReportsProviderAuthenticationWithoutErrorDetails(t *testing.T) {
	const secret = "provider-command-secret"
	checkedAt := time.Unix(600, 0).UTC()
	health, err := (healthAdapter{
		store: healthTaskStoreStub{},
		providers: map[task.Provider]provider.Provider{
			task.ProviderClaude: healthProviderStub{status: provider.AuthStatus{CheckedAt: checkedAt}, err: errors.New(secret)},
		},
	}).Health(context.Background())
	if err != nil || health.Status != "degraded" {
		t.Fatalf("health=%#v err=%v", health, err)
	}
	component, ok := health.Components["claude_auth"].(map[string]any)
	if !ok || component["authenticated"] != false || component["status"] != "unavailable" || component["checked_at"] != checkedAt {
		t.Fatalf("component=%#v", health.Components["claude_auth"])
	}
	if strings.Contains(fmt.Sprint(health.Components), secret) {
		t.Fatalf("health leaked provider error: %#v", health.Components)
	}
}

func TestServeDaemonLoadsCredentialAndPreparedPathsBeforeComposition(t *testing.T) {
	clearProviderAPIKeyEnvironment(t)
	root := t.TempDir()
	credentials := filepath.Join(root, "credentials")
	if err := os.Mkdir(credentials, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credentials, "telegram_bot_token"), []byte("123:secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREDENTIALS_DIRECTORY", credentials)
	t.Setenv("AGENTBRIDGE_DATA_DIR", filepath.Join(root, "state"))
	configPath := filepath.Join(root, "config.yaml")
	writeServeConfig(t, configPath)
	called := false
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := serveDaemonWithBuilder(ctx, configPath, func(_ context.Context, cfg config.Config, paths runtimePaths, token config.Credential, environment []string) (daemonRuntime, error) {
		called = true
		if token.Value() != "123:secret" || cfg.Server.Listen != "127.0.0.1:8787" {
			t.Fatalf("composition inputs were not loaded")
		}
		if _, err := os.Stat(paths.attachments); err != nil {
			t.Fatalf("runtime paths not prepared: %v", err)
		}
		if !strings.Contains(strings.Join(environment, "\n"), "CLAUDE_CONFIG_DIR="+paths.claudeConfig) {
			t.Fatal("shared Claude config missing from provider environment")
		}
		return &fakeDaemonRuntime{running: make(chan struct{}), stopped: make(chan struct{})}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("daemon builder was not called")
	}
}

func writeServeConfig(t *testing.T, path string) {
	t.Helper()
	value := `server:
  listen: 127.0.0.1:8787
  allowed_tailscale_identities: [operator@example.invalid]
telegram:
  private_chat_only: true
  allowed_user_ids: [42]
  paired_chat_id: 42
providers:
  codex: {executable: /usr/local/bin/codex, model: gpt-5.6-terra}
repositories:
  sample:
    checkout_path: /srv/sample
    remote: origin
    base_ref: refs/heads/staging
    verification: [{argv: [go, test, ./...], dir: .}]
    delivery: {enabled: false}
`
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRunDaemonLifecycleCancelsRunAndShutsDown(t *testing.T) {
	runtime := &fakeDaemonRuntime{running: make(chan struct{}), stopped: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runDaemonLifecycle(ctx, runtime) }()
	select {
	case <-runtime.running:
	case <-time.After(time.Second):
		t.Fatal("runtime did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runDaemonLifecycle: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runtime did not stop")
	}
	if runtime.calls() != "start,run,shutdown" {
		t.Fatalf("calls=%q", runtime.calls())
	}
}

func TestRunDaemonLifecyclePropagatesServiceFailureAfterShutdown(t *testing.T) {
	want := errors.New("listener failed")
	runtime := &fakeDaemonRuntime{runErr: want, running: make(chan struct{}), stopped: make(chan struct{})}
	if err := runDaemonLifecycle(context.Background(), runtime); !errors.Is(err, want) {
		t.Fatalf("error=%v, want listener failure", err)
	}
	if runtime.calls() != "start,run,shutdown" {
		t.Fatalf("calls=%q", runtime.calls())
	}
}

func TestRunDaemonLifecycleShutsDownAfterStartupFailure(t *testing.T) {
	want := errors.New("startup failed")
	runtime := &fakeDaemonRuntime{startErr: want, running: make(chan struct{}), stopped: make(chan struct{})}
	if err := runDaemonLifecycle(context.Background(), runtime); !errors.Is(err, want) {
		t.Fatalf("error=%v, want startup failure", err)
	}
	if runtime.calls() != "start,shutdown" {
		t.Fatalf("calls=%q", runtime.calls())
	}
}

func TestLiveDaemonRoutesTelegramUpdatesAndOwnsEveryGoroutine(t *testing.T) {
	application := &fakeLiveApplication{handled: make(chan telegram.Update, 1)}
	source := &fakeLiveTelegram{updates: make(chan telegram.Update, 1)}
	httpServer := &fakeLiveHTTP{stopped: make(chan struct{})}
	source.updates <- telegram.Update{ID: 44}
	runtime := newLiveDaemon(application, source, httpServer, "127.0.0.1:8787")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runDaemonLifecycle(ctx, runtime) }()
	select {
	case update := <-application.handled:
		if update.ID != 44 {
			t.Fatalf("update=%d", update.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("Telegram update was not routed")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("live daemon leaked a goroutine")
	}
	if !application.started || !application.stopped || !source.stopped {
		t.Fatalf("lifecycle app(start=%v stop=%v) telegram(stop=%v)", application.started, application.stopped, source.stopped)
	}
}

func TestControlHandlerSendsOnlyTaskWorktreeArtifacts(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "result.txt")
	if err := os.WriteFile(inside, []byte("result"), 0o600); err != nil {
		t.Fatal(err)
	}
	messenger := &artifactMessenger{}
	handler := controlHandler{store: controlTaskStore{value: task.Task{ID: "task-1", Provider: task.ProviderClaude, WorktreePath: root, TelegramChatID: 42}}, messenger: messenger}
	params := []byte(`{"path":"` + inside + `","name":"result.txt"}`)
	result, err := handler.Handle(context.Background(), controlsocket.Request{TaskID: "task-1", Provider: "claude", Tool: "send_artifact", Params: params})
	if err != nil || result == nil || messenger.contents != "result" || messenger.name != "result.txt" || messenger.chatID != 42 {
		t.Fatalf("result=%#v err=%v messenger=%#v", result, err, messenger)
	}

	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{outside, filepath.Join(root, "escape.txt")} {
		if candidate != outside {
			if err := os.Symlink(outside, candidate); err != nil {
				t.Fatal(err)
			}
		}
		params = []byte(`{"path":"` + candidate + `","name":"secret.txt"}`)
		if _, err := handler.Handle(context.Background(), controlsocket.Request{TaskID: "task-1", Provider: "claude", Tool: "send_artifact", Params: params}); err == nil {
			t.Fatalf("unsafe artifact %q was sent", candidate)
		}
	}
}

func TestControlHandlerRedactsConfiguredCredentialsBeforeTelegram(t *testing.T) {
	const secret = "recognizable-bot-token-literal"
	messenger := &artifactMessenger{sent: make(chan telegram.Message, 1)}
	handler := controlHandler{
		store:     controlTaskStore{value: task.Task{ID: "task-1", Provider: task.ProviderClaude, TelegramChatID: 42}},
		messenger: messenger,
		redactor:  security.NewRedactor(security.Config{Secrets: []string{secret}}),
	}
	_, err := handler.Handle(context.Background(), controlsocket.Request{
		TaskID: "task-1", Provider: "claude", Tool: "notify_telegram",
		Params: []byte(`{"message":"credential ` + secret + `"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case message := <-messenger.sent:
		if strings.Contains(message.Text, secret) || !strings.Contains(message.Text, "REDACTED") {
			t.Fatalf("Telegram message was not redacted: %q", message.Text)
		}
	case <-time.After(time.Second):
		t.Fatal("Telegram notification was not sent")
	}
}

func TestControlHandlerApprovalWaitsForSignedTelegramDecision(t *testing.T) {
	store := &approvalStore{}
	messenger := &artifactMessenger{sent: make(chan telegram.Message, 1)}
	signer := telegram.NewCallbackSigner([]byte("0123456789abcdef0123456789abcdef"), nil)
	broker, err := approval.New(approval.Config{
		Store: store, Messenger: messenger, Signer: signer, NewID: func() string { return "approval-1" },
		AuthorizeUser: func(value string) bool { return value == "42" },
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := controlHandler{
		store:     controlTaskStore{value: task.Task{ID: "task-1", Provider: task.ProviderClaude, TelegramChatID: 99}},
		messenger: messenger, approvals: broker,
	}
	result := make(chan any, 1)
	errorsCh := make(chan error, 1)
	go func() {
		value, handleErr := handler.Handle(context.Background(), controlsocket.Request{
			TaskID: "task-1", Provider: "claude", Tool: "request_telegram_approval",
			Params: []byte(`{"kind":"shell","summary":"Run verified command"}`),
		})
		result <- value
		errorsCh <- handleErr
	}()
	select {
	case message := <-messenger.sent:
		if message.ChatID != 99 || len(message.InlineKeyboard) == 0 {
			t.Fatalf("approval message=%#v", message)
		}
	case <-time.After(time.Second):
		t.Fatal("approval message was not sent")
	}
	if err := broker.HandleDecision(context.Background(), "task-1", "approval-1", "42", true); err != nil {
		t.Fatal(err)
	}
	if err := <-errorsCh; err != nil {
		t.Fatal(err)
	}
	approved, ok := (<-result).(approval.Result)
	if !ok || !approved.Approved {
		t.Fatalf("result=%#v", approved)
	}
}

func TestProcTaskInspectorFindsOnlyExactLiveTaskWorktreeEvidence(t *testing.T) {
	procRoot := t.TempDir()
	worktree := t.TempDir()
	nested := filepath.Join(worktree, "frontend")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	writeProcEvidence(t, procRoot, "101", nested, "task-1", "claude")
	inspector := procTaskInspector{root: procRoot, platform: "linux", maxEntries: 32}
	running, err := inspector.Running(context.Background(), "task-1", task.ProviderClaude, worktree)
	if err != nil || !running {
		t.Fatalf("running=%v err=%v", running, err)
	}
	// Codex tool children may not carry task markers; the per-task worktree cwd
	// is therefore sufficient orphan evidence.
	running, err = inspector.Running(context.Background(), "task-2", task.ProviderCodex, worktree)
	if err != nil || !running {
		t.Fatalf("cwd-only evidence running=%v err=%v", running, err)
	}
	otherWorktree := t.TempDir()
	running, err = inspector.Running(context.Background(), "task-1", task.ProviderClaude, otherWorktree)
	if err != nil || running {
		t.Fatalf("wrong worktree running=%v err=%v", running, err)
	}
}

func TestProcTaskInspectorFailsClosedWithoutReliableEvidenceSource(t *testing.T) {
	inspector := procTaskInspector{root: filepath.Join(t.TempDir(), "missing"), platform: "linux", maxEntries: 32}
	if running, err := inspector.Running(context.Background(), "task-1", task.ProviderClaude, t.TempDir()); err == nil || running {
		t.Fatalf("missing proc source running=%v err=%v, want conservative error", running, err)
	}
	unsupported := procTaskInspector{root: "/proc", platform: "darwin", maxEntries: 32}
	if running, err := unsupported.Running(context.Background(), "task-1", task.ProviderClaude, t.TempDir()); err != nil || !running {
		t.Fatalf("unsupported platform running=%v err=%v, want conservative process evidence", running, err)
	}
}

func writeProcEvidence(t *testing.T, root, pid, cwd, taskID, providerName string) {
	t.Helper()
	dir := filepath.Join(root, pid)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	environment := "PATH=/usr/bin\x00AGENTBRIDGE_TASK_ID=" + taskID + "\x00AGENTBRIDGE_PROVIDER=" + providerName + "\x00"
	if err := os.WriteFile(filepath.Join(dir, "environ"), []byte(environment), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cwd, filepath.Join(dir, "cwd")); err != nil {
		t.Fatal(err)
	}
}

type fakeDaemonRuntime struct {
	mu       sync.Mutex
	order    []string
	runErr   error
	startErr error
	running  chan struct{}
	stopped  chan struct{}
}

type fakeLiveApplication struct {
	started bool
	stopped bool
	handled chan telegram.Update
}

func (f *fakeLiveApplication) Start(context.Context) error { f.started = true; return nil }
func (f *fakeLiveApplication) HandleUpdate(_ context.Context, update telegram.Update) (string, error) {
	f.handled <- update
	return "", nil
}
func (f *fakeLiveApplication) Shutdown(context.Context) error { f.stopped = true; return nil }

type fakeLiveTelegram struct {
	updates chan telegram.Update
	stopped bool
}

func (f *fakeLiveTelegram) Run(ctx context.Context) { <-ctx.Done(); f.stopped = true }
func (f *fakeLiveTelegram) Next(ctx context.Context) (telegram.Update, error) {
	select {
	case update := <-f.updates:
		return update, nil
	case <-ctx.Done():
		return telegram.Update{}, ctx.Err()
	}
}

type fakeLiveHTTP struct{ stopped chan struct{} }

func (f *fakeLiveHTTP) Listen(string) error { <-f.stopped; return nil }
func (f *fakeLiveHTTP) ShutdownWithContext(context.Context) error {
	select {
	case <-f.stopped:
	default:
		close(f.stopped)
	}
	return nil
}

type controlTaskStore struct{ value task.Task }

func (s controlTaskStore) Task(_ context.Context, id string) (task.Task, error) {
	if id != s.value.ID {
		return task.Task{}, errors.New("not found")
	}
	return s.value, nil
}

type authCommandStub struct {
	output []byte
	err    error
	name   string
	args   []string
}

func (s *authCommandStub) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	s.name, s.args = name, append([]string(nil), args...)
	return append([]byte(nil), s.output...), s.err
}

type healthTaskStoreStub struct{}

func (healthTaskStoreStub) NonterminalTasks(context.Context) ([]task.Task, error) { return nil, nil }

type healthProviderStub struct {
	provider.Provider
	status provider.AuthStatus
	err    error
}

func (p healthProviderStub) AuthStatus(context.Context) (provider.AuthStatus, error) {
	return p.status, p.err
}

type artifactMessenger struct {
	chatID   int64
	name     string
	contents string
	sent     chan telegram.Message
}

func (m *artifactMessenger) Send(_ context.Context, message telegram.Message) (telegram.MessageRef, error) {
	if m.sent != nil {
		m.sent <- message
	}
	return telegram.MessageRef{}, nil
}
func (*artifactMessenger) Edit(context.Context, telegram.MessageRef, telegram.Message) error {
	return nil
}
func (*artifactMessenger) AnswerCallback(context.Context, string, string) error { return nil }
func (m *artifactMessenger) SendDocument(_ context.Context, document telegram.Document) error {
	data, err := io.ReadAll(document.Data)
	if err != nil {
		return err
	}
	m.chatID, m.name, m.contents = document.ChatID, document.Filename, string(data)
	return nil
}

type approvalStore struct {
	mu     sync.Mutex
	values []task.Approval
}

func (s *approvalStore) UpsertApproval(_ context.Context, value task.Approval) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values = append(s.values, value)
	return nil
}

func (*approvalStore) AppendEvent(context.Context, task.Event) error { return nil }

func (*approvalStore) Events(context.Context, string) ([]task.Event, error) { return nil, nil }

func (f *fakeDaemonRuntime) Start(context.Context) error {
	f.record("start")
	return f.startErr
}

func (f *fakeDaemonRuntime) Run(ctx context.Context) error {
	f.record("run")
	close(f.running)
	if f.runErr != nil {
		return f.runErr
	}
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeDaemonRuntime) Shutdown(context.Context) error {
	f.record("shutdown")
	close(f.stopped)
	return nil
}

func (f *fakeDaemonRuntime) record(value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.order = append(f.order, value)
}

func (f *fakeDaemonRuntime) calls() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.order, ",")
}
