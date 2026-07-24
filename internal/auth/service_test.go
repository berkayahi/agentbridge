package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/workmodel"
)

func TestHealthClassifiesSubscriptionStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		provider workmodel.Provider
		output   string
		err      error
		want     HealthKind
	}{
		{name: "codex healthy", provider: workmodel.CodexSubscription, output: "Logged in using ChatGPT", want: HealthHealthy},
		{name: "claude healthy", provider: workmodel.ClaudeSubscription, output: `{"loggedIn":true,"subscriptionType":"max"}`, want: HealthHealthy},
		{name: "expired", provider: workmodel.CodexSubscription, output: "Your session has expired; run codex login", err: errors.New("exit 1"), want: HealthExpired},
		{name: "claude logged out JSON", provider: workmodel.ClaudeSubscription, output: `{"loggedIn": false}`, want: HealthExpired},
		{name: "command missing", provider: workmodel.ClaudeSubscription, err: ErrCommandMissing, want: HealthCommandMissing},
		{name: "timeout", provider: workmodel.CodexSubscription, err: context.DeadlineExceeded, want: HealthTimeout},
		{name: "unauthorized", provider: workmodel.ClaudeSubscription, output: "HTTP 401 unauthorized token=do-not-copy", err: errors.New("exit 1"), want: HealthUnauthorized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			commands := &fakeCommands{responses: []commandResponse{{output: []byte(tt.output), err: tt.err}}}
			svc := newTestService(t, Options{Commands: commands})
			health := svc.Health(context.Background(), tt.provider)
			if health.Kind != tt.want {
				t.Fatalf("kind = %q, want %q", health.Kind, tt.want)
			}
			if strings.Contains(health.Message, "do-not-copy") {
				t.Fatal("health message leaked command output")
			}
			call := commands.lastCall()
			wantArgs := map[workmodel.Provider][]string{
				workmodel.CodexSubscription:  {"login", "status"},
				workmodel.ClaudeSubscription: {"auth", "status", "--json"},
			}[tt.provider]
			if fmt.Sprint(call.args) != fmt.Sprint(wantArgs) {
				t.Fatalf("args = %v, want %v", call.args, wantArgs)
			}
		})
	}
}

func TestHealthUsesContextResultWhenKilledCommandMasksDeadline(t *testing.T) {
	t.Parallel()
	svc := newTestService(t, Options{Commands: deadlineMaskingCommands{}, CheckTimeout: time.Millisecond})
	health := svc.Health(context.Background(), workmodel.CodexSubscription)
	if health.Kind != HealthTimeout {
		t.Fatalf("kind = %q, want %q", health.Kind, HealthTimeout)
	}
}

func TestUnhealthyCheckMovesAllRunningProviderTasksToAwaitingAuth(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	tasks := &fakeTaskStore{tasks: []workmodel.Task{
		{ID: "codex-1", Provider: workmodel.CodexSubscription, State: workmodel.Running},
		{ID: "codex-2", Provider: workmodel.CodexSubscription, State: workmodel.Running},
		{ID: "claude-1", Provider: workmodel.ClaudeSubscription, State: workmodel.Running},
		{ID: "paused", Provider: workmodel.CodexSubscription, State: workmodel.Paused},
	}}
	incidents := &fakeIncidents{}
	notifier := &fakeNotifier{}
	svc := newTestService(t, Options{
		Commands:  &fakeCommands{responses: []commandResponse{{output: []byte("401 bearer secret-oauth-token"), err: errors.New("exit 1")}}},
		Tasks:     tasks,
		Incidents: incidents,
		Notifier:  notifier,
		Now:       func() time.Time { return now },
		NewID:     sequenceIDs("incident", "event-1", "event-2"),
	})

	incident, err := svc.CheckProvider(context.Background(), workmodel.CodexSubscription)
	if err != nil {
		t.Fatal(err)
	}
	if incident.Kind != HealthUnauthorized || len(incident.TaskIDs) != 2 {
		t.Fatalf("incident = %#v", incident)
	}
	if got := tasks.states(); got["codex-1"] != workmodel.AwaitingAuth || got["codex-2"] != workmodel.AwaitingAuth {
		t.Fatalf("states = %v", got)
	}
	if got := tasks.states(); got["claude-1"] != workmodel.Running || got["paused"] != workmodel.Paused {
		t.Fatalf("unaffected states changed: %v", got)
	}
	assertNoSecret(t, "secret-oauth-token", incidents.text(), notifier.text(), tasks.text())
}

func TestRuntime401UsesSameAuthTransition(t *testing.T) {
	t.Parallel()
	tasks := &fakeTaskStore{tasks: []workmodel.Task{{ID: "task-1", Provider: workmodel.ClaudeSubscription, State: workmodel.Running}}}
	svc := newTestService(t, Options{Tasks: tasks, NewID: sequenceIDs("incident", "event")})

	incident, err := svc.HandleProviderError(context.Background(), workmodel.ClaudeSubscription, errors.New("request failed: 401 oauth-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if incident.Kind != HealthUnauthorized || tasks.states()["task-1"] != workmodel.AwaitingAuth {
		t.Fatalf("incident = %#v; states = %v", incident, tasks.states())
	}
	assertNoSecret(t, "oauth-secret", tasks.text())
}

func TestRepeatedUnhealthyCheckKeepsOneOpenIncident(t *testing.T) {
	t.Parallel()
	tasks := &fakeTaskStore{tasks: []workmodel.Task{{ID: "task-1", Provider: workmodel.CodexSubscription, State: workmodel.Running}}}
	incidents := &fakeIncidents{}
	svc := newTestService(t, Options{
		Commands: &fakeCommands{responses: []commandResponse{
			{output: []byte("session expired"), err: errors.New("exit 1")},
			{output: []byte("session expired"), err: errors.New("exit 1")},
		}},
		Tasks: tasks, Incidents: incidents, NewID: sequenceIDs("event", "incident", "incident-retry"),
	})
	first, err := svc.CheckProvider(context.Background(), workmodel.CodexSubscription)
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.CheckProvider(context.Background(), workmodel.CodexSubscription)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("incident IDs = %q and %q", first.ID, second.ID)
	}
	incidents.mu.Lock()
	defer incidents.mu.Unlock()
	if len(incidents.values) != 1 {
		t.Fatalf("saved incidents = %d", len(incidents.values))
	}
}

func TestPeriodicCheckRetriesIncidentPersistenceWithoutRetransition(t *testing.T) {
	t.Parallel()
	tasks := &fakeTaskStore{tasks: []workmodel.Task{{ID: "task-1", Provider: workmodel.CodexSubscription, State: workmodel.Running}}}
	incidents := &fakeIncidents{fail: 1}
	svc := newTestService(t, Options{
		Commands: &fakeCommands{responses: []commandResponse{
			{output: []byte("session expired"), err: errors.New("exit 1")},
			{output: []byte("session expired"), err: errors.New("exit 1")},
		}},
		Tasks: tasks, Incidents: incidents, NewID: sequenceIDs("event", "incident", "incident-retry"),
	})
	if _, err := svc.CheckProvider(context.Background(), workmodel.CodexSubscription); err == nil {
		t.Fatal("first persistence failure returned nil")
	}
	incident, err := svc.CheckProvider(context.Background(), workmodel.CodexSubscription)
	if err != nil {
		t.Fatal(err)
	}
	if incident.ID != "incident-retry" || tasks.states()["task-1"] != workmodel.AwaitingAuth {
		t.Fatalf("incident = %#v; states = %v", incident, tasks.states())
	}
	incidents.mu.Lock()
	defer incidents.mu.Unlock()
	if len(incidents.values) != 1 {
		t.Fatalf("persisted incidents = %d", len(incidents.values))
	}
}

func TestMonitorChecksPeriodicallyUntilCanceled(t *testing.T) {
	t.Parallel()
	commands := &fakeCommands{}
	svc := newTestService(t, Options{Commands: commands})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- svc.Monitor(ctx, 5*time.Millisecond, workmodel.CodexSubscription, workmodel.ClaudeSubscription)
	}()
	deadline := time.Now().Add(time.Second)
	for commands.callCount() < 4 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("monitor error = %v", err)
	}
	if got := commands.callCount(); got < 4 {
		t.Fatalf("command calls = %d", got)
	}
}

func TestHealthyChecksResolveAnIncidentExactlyOnce(t *testing.T) {
	t.Parallel()
	opened := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	incidents := &fakeIncidents{values: []Incident{{
		ID: "incident", Provider: workmodel.CodexSubscription, Kind: HealthExpired,
		Status: IncidentOpen, OpenedAt: opened,
	}}}
	checks := 0
	svc := newTestService(t, Options{
		Incidents: incidents,
		Now: func() time.Time {
			checks++
			return opened.Add(time.Duration(checks) * time.Minute)
		},
	})
	if _, err := svc.CheckProvider(context.Background(), workmodel.CodexSubscription); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CheckProvider(context.Background(), workmodel.CodexSubscription); err != nil {
		t.Fatal(err)
	}
	incidents.mu.Lock()
	defer incidents.mu.Unlock()
	if incidents.saves != 1 {
		t.Fatalf("resolution saves = %d", incidents.saves)
	}
	if incidents.values[0].ResolvedAt == nil || !incidents.values[0].ResolvedAt.Equal(opened.Add(time.Minute)) {
		t.Fatalf("resolved at = %v", incidents.values[0].ResolvedAt)
	}
}

func TestAuthIncidentSuspendsLiveProviderBeforeDurableTransition(t *testing.T) {
	tasks := &fakeTaskStore{tasks: []workmodel.Task{{ID: "task-1", Provider: workmodel.CodexSubscription, State: workmodel.Running}}}
	suspender := &fakeSuspender{}
	svc := newTestService(t, Options{
		Commands: &fakeCommands{responses: []commandResponse{{output: []byte("login required"), err: errors.New("exit 1")}}},
		Tasks:    tasks, Suspender: suspender, NewID: sequenceIDs("event", "incident"),
	})
	if _, err := svc.CheckProvider(context.Background(), workmodel.CodexSubscription); err != nil {
		t.Fatal(err)
	}
	if !suspender.called || suspender.provider != workmodel.CodexSubscription {
		t.Fatalf("suspender = %#v", suspender)
	}
	if got := tasks.states()["task-1"]; got != workmodel.AwaitingAuth {
		t.Fatalf("state = %s", got)
	}
}

func TestRecoverySuccessRequiresExplicitResumeForAffectedTasks(t *testing.T) {
	t.Parallel()
	tasks := &fakeTaskStore{tasks: []workmodel.Task{
		{ID: "task-1", Provider: workmodel.CodexSubscription, State: workmodel.Running, BaseSHA: "abc", WorktreePath: "/tmp/w1", ProviderSessionID: "session-1"},
		{ID: "task-2", Provider: workmodel.CodexSubscription, State: workmodel.Running, BaseSHA: "def", WorktreePath: "/tmp/w2", ProviderSessionID: "session-2"},
	}}
	commands := &fakeCommands{responses: []commandResponse{
		{output: []byte("session expired"), err: errors.New("exit 1")},
		{output: []byte("Logged in using ChatGPT")},
	}}
	resumer := &fakeResumer{onResume: func(value workmodel.Task) error {
		if got := tasks.states()[value.ID]; got != workmodel.Running {
			return fmt.Errorf("task state during resume = %s, want running", got)
		}
		return nil
	}}
	login := &fakePTY{output: "Open https://auth.openai.com/device and enter ABCD-EFGH\n", release: closedChannel()}
	svc := newTestService(t, Options{
		Commands: commands, Tasks: tasks, Resumer: resumer, PTY: login,
		Authorizer: fakeAuthorizer{"tailscale-user": true},
		NewID:      sequenceIDs("event-1", "event-2", "incident", "recovery", "resume-event-1", "resume-event-2"),
	})
	if _, err := svc.CheckProvider(context.Background(), workmodel.CodexSubscription); err != nil {
		t.Fatal(err)
	}

	recoveryID, err := svc.StartRecovery(context.Background(), "tailscale-user", workmodel.CodexSubscription)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.WaitRecovery(context.Background(), recoveryID); err != nil {
		t.Fatal(err)
	}
	view, err := svc.Recovery(context.Background(), "tailscale-user", recoveryID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != RecoverySucceeded || view.Transcript != "" {
		t.Fatalf("view = %#v; finished transcript must be erased", view)
	}
	if got := tasks.states(); got["task-1"] != workmodel.AwaitingAuth || got["task-2"] != workmodel.AwaitingAuth {
		t.Fatalf("states after login = %v", got)
	}
	if len(resumer.validated) != 0 || len(resumer.resumed) != 0 {
		t.Fatalf("login resumed tasks implicitly: validated = %v, resumed = %v", resumer.validated, resumer.resumed)
	}
	for _, taskID := range []string{"task-1", "task-2"} {
		if err := svc.ResumeTask(context.Background(), "tailscale-user", taskID); err != nil {
			t.Fatalf("explicit resume %s: %v", taskID, err)
		}
	}
	if got := tasks.states(); got["task-1"] != workmodel.Running || got["task-2"] != workmodel.Running {
		t.Fatalf("states after explicit resume = %v", got)
	}
	if fmt.Sprint(resumer.validated) != "[task-1 task-2]" || fmt.Sprint(resumer.resumed) != "[task-1 task-2]" {
		t.Fatalf("validated = %v, resumed = %v", resumer.validated, resumer.resumed)
	}
	if call := login.lastCall(); call.name != "codex" || fmt.Sprint(call.args) != "[login --device-auth]" {
		t.Fatalf("login call = %#v", call)
	}
}

func TestRecoveryValidationFailurePausesTaskWithSafeReason(t *testing.T) {
	t.Parallel()
	tasks := &fakeTaskStore{tasks: []workmodel.Task{{ID: "task-1", Provider: workmodel.ClaudeSubscription, State: workmodel.Running}}}
	commands := &fakeCommands{responses: []commandResponse{
		{output: []byte(`{"loggedIn":false}`), err: errors.New("exit 1")},
		{output: []byte(`{"loggedIn":true}`)},
	}}
	resumer := &fakeResumer{validateErr: errors.New("worktree includes secret-path")}
	svc := newTestService(t, Options{
		Commands: commands, Tasks: tasks, Resumer: resumer,
		PTY: &fakePTY{release: closedChannel()}, Authorizer: fakeAuthorizer{"operator": true},
		NewID: sequenceIDs("incident", "event-1", "recovery", "event-2"),
	})
	if _, err := svc.CheckProvider(context.Background(), workmodel.ClaudeSubscription); err != nil {
		t.Fatal(err)
	}
	id, err := svc.StartRecovery(context.Background(), "operator", workmodel.ClaudeSubscription)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.WaitRecovery(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if got := tasks.states()["task-1"]; got != workmodel.AwaitingAuth {
		t.Fatalf("state after login = %q", got)
	}
	if err := svc.ResumeTask(context.Background(), "operator", "task-1"); err == nil {
		t.Fatal("explicit resume unexpectedly succeeded for invalid worktree")
	}
	if got := tasks.states()["task-1"]; got != workmodel.Paused {
		t.Fatalf("state = %q", got)
	}
	assertNoSecret(t, "secret-path", tasks.text())
}

func TestFailedRecoveryPausesAffectedTasks(t *testing.T) {
	t.Parallel()
	tasks := &fakeTaskStore{tasks: []workmodel.Task{{ID: "task-1", Provider: workmodel.CodexSubscription, State: workmodel.Running}}}
	svc := newTestService(t, Options{
		Commands: &fakeCommands{responses: []commandResponse{{output: []byte("session expired"), err: errors.New("exit 1")}}},
		Tasks:    tasks, PTY: &fakePTY{err: errors.New("login failed"), release: closedChannel()},
		Authorizer: fakeAuthorizer{"operator": true}, NewID: sequenceIDs("event-1", "incident", "recovery", "event-2"),
	})
	if _, err := svc.CheckProvider(context.Background(), workmodel.CodexSubscription); err != nil {
		t.Fatal(err)
	}
	id, err := svc.StartRecovery(context.Background(), "operator", workmodel.CodexSubscription)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.WaitRecovery(context.Background(), id); err == nil {
		t.Fatal("failed recovery returned nil")
	}
	if got := tasks.states()["task-1"]; got != workmodel.Paused {
		t.Fatalf("state = %q, want %q", got, workmodel.Paused)
	}
}

func TestRecoveryRehydratesDurableAwaitingAuthTasksAfterRestart(t *testing.T) {
	t.Parallel()
	tasks := &fakeTaskStore{tasks: []workmodel.Task{{
		ID: "task-1", Provider: workmodel.CodexSubscription, State: workmodel.AwaitingAuth,
		BaseSHA: "base", WorktreePath: "/tmp/worktree", ProviderSessionID: "session",
	}}}
	incidents := &fakeIncidents{values: []Incident{{
		ID: "incident", Provider: workmodel.CodexSubscription, Kind: HealthExpired,
		Status: IncidentOpen, TaskIDs: []string{"task-1"}, OpenedAt: time.Now().UTC(),
	}}}
	resumer := &fakeResumer{}
	svc := newTestService(t, Options{
		Commands: &fakeCommands{responses: []commandResponse{
			{output: []byte("session expired"), err: errors.New("exit 1")},
			{output: []byte("Logged in using ChatGPT")},
		}},
		Tasks: tasks, Incidents: incidents, Resumer: resumer, PTY: &fakePTY{release: closedChannel()},
		Authorizer: fakeAuthorizer{"operator": true}, NewID: sequenceIDs("recovery", "event"),
	})
	periodicIncident, err := svc.CheckProvider(context.Background(), workmodel.CodexSubscription)
	if err != nil {
		t.Fatal(err)
	}
	if periodicIncident.ID != "incident" {
		t.Fatalf("periodic check created incident %q", periodicIncident.ID)
	}
	id, err := svc.StartRecovery(context.Background(), "operator", workmodel.CodexSubscription)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.WaitRecovery(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if got := tasks.states()["task-1"]; got != workmodel.AwaitingAuth {
		t.Fatalf("state after login = %q", got)
	}
	if len(resumer.resumed) != 0 {
		t.Fatalf("resumed implicitly = %v", resumer.resumed)
	}
	if err := svc.ResumeTask(context.Background(), "operator", "task-1"); err != nil {
		t.Fatalf("explicit resume: %v", err)
	}
	if got := tasks.states()["task-1"]; got != workmodel.Running {
		t.Fatalf("state after explicit resume = %q", got)
	}
	if fmt.Sprint(resumer.resumed) != "[task-1]" {
		t.Fatalf("resumed = %v", resumer.resumed)
	}
	incidents.mu.Lock()
	defer incidents.mu.Unlock()
	if len(incidents.values) != 1 || incidents.values[0].Status != IncidentResolved {
		t.Fatalf("incidents = %#v", incidents.values)
	}
}

func TestRecoveryRequiresAuthorizationAndSupportsCancel(t *testing.T) {
	t.Parallel()
	login := &fakePTY{started: make(chan struct{}), release: make(chan struct{})}
	svc := newTestService(t, Options{PTY: login, Authorizer: fakeAuthorizer{"operator": true}, NewID: sequenceIDs("recovery")})
	if _, err := svc.StartRecovery(context.Background(), "intruder", workmodel.CodexSubscription); !errors.Is(err, ErrForbidden) {
		t.Fatalf("start error = %v", err)
	}
	id, err := svc.StartRecovery(context.Background(), "operator", workmodel.ClaudeSubscription)
	if err != nil {
		t.Fatal(err)
	}
	<-login.started
	if err := svc.SubmitCode(context.Background(), "operator", id, "erase-me"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Recovery(context.Background(), "intruder", id); !errors.Is(err, ErrForbidden) {
		t.Fatalf("view error = %v", err)
	}
	if err := svc.CancelRecovery(context.Background(), "operator", id); err != nil {
		t.Fatal(err)
	}
	if err := svc.WaitRecovery(context.Background(), id); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait error = %v", err)
	}
	view, err := svc.Recovery(context.Background(), "operator", id)
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != RecoveryCanceled || view.Transcript != "" {
		t.Fatalf("view = %#v", view)
	}
	session, err := svc.recovery(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(session.input) != 0 {
		t.Fatal("canceled recovery retained callback input")
	}
}

func TestOnlyOneRecoveryMayRunPerProvider(t *testing.T) {
	t.Parallel()
	login := &fakePTY{started: make(chan struct{}), release: make(chan struct{})}
	svc := newTestService(t, Options{PTY: login, Authorizer: fakeAuthorizer{"operator": true}, NewID: sequenceIDs("recovery")})
	id, err := svc.StartRecovery(context.Background(), "operator", workmodel.CodexSubscription)
	if err != nil {
		t.Fatal(err)
	}
	<-login.started
	if _, err := svc.StartRecovery(context.Background(), "operator", workmodel.CodexSubscription); !errors.Is(err, ErrRecoveryActive) {
		t.Fatalf("second start error = %v", err)
	}
	if err := svc.CancelRecovery(context.Background(), "operator", id); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryRemainsActiveThroughTerminalTaskCleanup(t *testing.T) {
	t.Parallel()
	base := &fakeTaskStore{tasks: []workmodel.Task{{ID: "task-1", Provider: workmodel.CodexSubscription, State: workmodel.AwaitingAuth}}}
	tasks := &blockingTaskStore{fakeTaskStore: base, started: make(chan struct{}), release: make(chan struct{})}
	svc := newTestService(t, Options{
		Tasks: tasks, PTY: &fakePTY{err: errors.New("login failed"), release: closedChannel()},
		Authorizer: fakeAuthorizer{"operator": true}, NewID: sequenceIDs("recovery", "pause-event"),
	})
	id, err := svc.StartRecovery(context.Background(), "operator", workmodel.CodexSubscription)
	if err != nil {
		t.Fatal(err)
	}
	<-tasks.started
	if _, err := svc.StartRecovery(context.Background(), "operator", workmodel.CodexSubscription); !errors.Is(err, ErrRecoveryActive) {
		t.Fatalf("start during cleanup error = %v", err)
	}
	close(tasks.release)
	if err := svc.WaitRecovery(context.Background(), id); err == nil {
		t.Fatal("failed recovery returned nil")
	}
}

func TestAuthorizedRecoveryAcceptsOneBoundedCallbackCode(t *testing.T) {
	t.Parallel()
	login := &fakePTY{started: make(chan struct{}), receiveInput: true, release: closedChannel()}
	svc := newTestService(t, Options{PTY: login, Authorizer: fakeAuthorizer{"operator": true}, NewID: sequenceIDs("recovery")})
	id, err := svc.StartRecovery(context.Background(), "operator", workmodel.ClaudeSubscription)
	if err != nil {
		t.Fatal(err)
	}
	<-login.started
	if err := svc.SubmitCode(context.Background(), "intruder", id, "callback-code"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("intruder submit error = %v", err)
	}
	if err := svc.SubmitCode(context.Background(), "operator", id, "callback-code"); err != nil {
		t.Fatal(err)
	}
	if err := svc.SubmitCode(context.Background(), "operator", id, "second-code"); !errors.Is(err, ErrCodeSubmitted) {
		t.Fatalf("second submit error = %v", err)
	}
	if err := svc.WaitRecovery(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if got := login.inputText(); got != "callback-code\n" {
		t.Fatalf("PTY input = %q", got)
	}
}

func TestRecoveryExpiresAndErasesTranscript(t *testing.T) {
	t.Parallel()
	login := &fakePTY{output: "Open https://auth.openai.com/device and enter CODE-SECRET", started: make(chan struct{}), release: make(chan struct{})}
	svc := newTestService(t, Options{
		PTY: login, Authorizer: fakeAuthorizer{"operator": true}, RecoveryTTL: 20 * time.Millisecond,
		NewID: sequenceIDs("recovery"),
	})
	id, err := svc.StartRecovery(context.Background(), "operator", workmodel.CodexSubscription)
	if err != nil {
		t.Fatal(err)
	}
	<-login.started
	view, err := svc.Recovery(context.Background(), "operator", id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(view.Transcript, "CODE-SECRET") {
		t.Fatalf("live transcript = %q", view.Transcript)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := svc.WaitRecovery(ctx, id); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait error = %v", err)
	}
	view, err = svc.Recovery(context.Background(), "operator", id)
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != RecoveryExpired || view.Transcript != "" {
		t.Fatalf("expired view = %#v", view)
	}
}

type commandResponse struct {
	output []byte
	err    error
}

type deadlineMaskingCommands struct{}

func (deadlineMaskingCommands) Run(ctx context.Context, _ string, _ ...string) ([]byte, error) {
	<-ctx.Done()
	return nil, errors.New("signal: killed")
}

type commandCall struct {
	name string
	args []string
}

type fakeCommands struct {
	mu        sync.Mutex
	responses []commandResponse
	calls     []commandCall
}

func (f *fakeCommands) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, commandCall{name: name, args: append([]string(nil), args...)})
	if len(f.responses) == 0 {
		if name == "claude" {
			return []byte(`{"loggedIn":true}`), nil
		}
		return []byte("Logged in using ChatGPT"), nil
	}
	result := f.responses[0]
	f.responses = f.responses[1:]
	return append([]byte(nil), result.output...), result.err
}

func (f *fakeCommands) lastCall() commandCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

func (f *fakeCommands) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type fakeTaskStore struct {
	mu          sync.Mutex
	tasks       []workmodel.Task
	transitions []workmodel.Event
}

type blockingTaskStore struct {
	*fakeTaskStore
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingTaskStore) Transition(ctx context.Context, id string, state workmodel.State, event workmodel.Event) error {
	if state == workmodel.Paused {
		b.once.Do(func() { close(b.started) })
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-b.release:
		}
	}
	return b.fakeTaskStore.Transition(ctx, id, state, event)
}

func (f *fakeTaskStore) NonterminalTasks(context.Context) ([]workmodel.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]workmodel.Task(nil), f.tasks...), nil
}

func (f *fakeTaskStore) Transition(_ context.Context, id string, state workmodel.State, event workmodel.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.tasks {
		if f.tasks[i].ID == id {
			if !workmodel.CanTransition(f.tasks[i].State, state) {
				return errors.New("invalid transition")
			}
			f.tasks[i].State = state
			f.transitions = append(f.transitions, event)
			return nil
		}
	}
	return errors.New("not found")
}

func (f *fakeTaskStore) states() map[string]workmodel.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]workmodel.State, len(f.tasks))
	for _, value := range f.tasks {
		out[value.ID] = value.State
	}
	return out
}

func (f *fakeTaskStore) text() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fmt.Sprint(f.transitions)
}

type fakeIncidents struct {
	mu     sync.Mutex
	values []Incident
	fail   int
	saves  int
}

func (f *fakeIncidents) OpenIncident(_ context.Context, provider workmodel.Provider) (Incident, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.values) - 1; i >= 0; i-- {
		if f.values[i].Provider == provider && f.values[i].Status == IncidentOpen {
			return f.values[i], nil
		}
	}
	return Incident{}, ErrIncidentNotFound
}

func (f *fakeIncidents) SaveIncident(_ context.Context, value Incident) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail > 0 {
		f.fail--
		return errors.New("incident persistence failed")
	}
	f.saves++
	for i := range f.values {
		if f.values[i].ID == value.ID {
			f.values[i] = value
			return nil
		}
	}
	f.values = append(f.values, value)
	return nil
}

func (f *fakeIncidents) text() string { f.mu.Lock(); defer f.mu.Unlock(); return fmt.Sprint(f.values) }

type fakeNotifier struct {
	mu     sync.Mutex
	values []IncidentSummary
}

func (f *fakeNotifier) AuthIncident(_ context.Context, value IncidentSummary) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values = append(f.values, value)
	return nil
}

func (f *fakeNotifier) text() string { f.mu.Lock(); defer f.mu.Unlock(); return fmt.Sprint(f.values) }

type fakeResumer struct {
	mu          sync.Mutex
	validated   []string
	resumed     []string
	validateErr error
	resumeErr   error
	onResume    func(workmodel.Task) error
}

type fakeSuspender struct {
	called   bool
	provider workmodel.Provider
}

func (f *fakeSuspender) SuspendProvider(_ context.Context, provider workmodel.Provider) error {
	f.called, f.provider = true, provider
	return nil
}

func (f *fakeResumer) ValidateResume(_ context.Context, value workmodel.Task) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.validated = append(f.validated, value.ID)
	return f.validateErr
}

func (f *fakeResumer) ResumeTask(_ context.Context, value workmodel.Task) error {
	f.mu.Lock()
	f.resumed = append(f.resumed, value.ID)
	onResume, resumeErr := f.onResume, f.resumeErr
	f.mu.Unlock()
	if onResume != nil {
		return onResume(value)
	}
	return resumeErr
}

type fakeAuthorizer map[string]bool

func (f fakeAuthorizer) AuthorizeRecovery(_ context.Context, principal string) error {
	if !f[principal] {
		return ErrForbidden
	}
	return nil
}

type fakePTY struct {
	mu           sync.Mutex
	output       string
	err          error
	started      chan struct{}
	release      chan struct{}
	calls        []commandCall
	receiveInput bool
	input        string
}

func (f *fakePTY) Run(ctx context.Context, name string, args []string, input <-chan []byte, output func([]byte)) error {
	f.mu.Lock()
	f.calls = append(f.calls, commandCall{name: name, args: append([]string(nil), args...)})
	started := f.started
	release := f.release
	text := f.output
	err := f.err
	receiveInput := f.receiveInput
	f.mu.Unlock()
	if text != "" {
		output([]byte(text))
	}
	if started != nil {
		close(started)
	}
	if receiveInput {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case value := <-input:
			f.mu.Lock()
			f.input = string(value)
			f.mu.Unlock()
		}
	}
	if release != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
		}
	}
	return err
}

func (f *fakePTY) lastCall() commandCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

func (f *fakePTY) inputText() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.input
}

func newTestService(t *testing.T, options Options) *Service {
	t.Helper()
	if options.Commands == nil {
		options.Commands = &fakeCommands{}
	}
	if options.Tasks == nil {
		options.Tasks = &fakeTaskStore{}
	}
	if options.Incidents == nil {
		options.Incidents = &fakeIncidents{}
	}
	if options.Notifier == nil {
		options.Notifier = &fakeNotifier{}
	}
	if options.Resumer == nil {
		options.Resumer = &fakeResumer{}
	}
	if options.PTY == nil {
		options.PTY = &fakePTY{release: closedChannel()}
	}
	if options.Authorizer == nil {
		options.Authorizer = fakeAuthorizer{"operator": true}
	}
	options.Logger = slog.New(slog.NewTextHandler(&strings.Builder{}, nil))
	if options.NewID == nil {
		options.NewID = sequenceIDs("id-1", "id-2", "id-3", "id-4", "id-5", "id-6", "id-7", "id-8")
	}
	svc, err := NewService(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	return svc
}

func sequenceIDs(ids ...string) func() string {
	var mu sync.Mutex
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		if len(ids) == 0 {
			panic("out of ids")
		}
		id := ids[0]
		ids = ids[1:]
		return id
	}
}

func closedChannel() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func assertNoSecret(t *testing.T, secret string, values ...string) {
	t.Helper()
	for _, value := range values {
		if strings.Contains(value, secret) {
			t.Fatalf("secret %q found in %q", secret, value)
		}
	}
}
