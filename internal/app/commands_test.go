package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/provider"
	providerfake "github.com/berkayahi/agentbridge/internal/provider/fake"
	"github.com/berkayahi/agentbridge/internal/store"
	"github.com/berkayahi/agentbridge/internal/task"
)

func TestDirectReadCommandsUseDurableStateWithoutStartingModelTurns(t *testing.T) {
	p := &observedProvider{Provider: providerfake.New(task.ProviderCodex, provider.MustID("unused-session"), nil)}
	fixture := newFixtureWithProvider(t, p)
	created := time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC)
	failed := task.Task{
		ID: "task-failed", RepoProfileID: "sample", Title: "Fix navigation", Prompt: "secret prompt",
		State: task.Failed, Provider: task.ProviderCodex, TelegramChatID: 100,
		CommitSHA: "0123456789abcdef", PushRef: "refs/heads/staging", CreatedAt: created, UpdatedAt: created.Add(time.Minute),
	}
	createStoredTask(t, fixture.store, failed)
	fixture.store.events[failed.ID] = append(fixture.store.events[failed.ID],
		task.Event{ID: "diff", TaskID: failed.ID, Type: task.EventDiffSummary, Visibility: task.VisibilityUser, Payload: []byte(`{"summary":"button aligned"}`), CreatedAt: created.Add(time.Minute)},
		task.Event{ID: "hidden", TaskID: failed.ID, Type: task.EventProviderMessage, Visibility: task.VisibilityInternal, Payload: []byte(`{"message":"must not leak"}`), CreatedAt: created.Add(2 * time.Minute)},
		task.Event{ID: "log", TaskID: failed.ID, Type: task.EventFailure, Visibility: task.VisibilityUser, Payload: []byte(`{"token":"12345678:ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmno"}`), CreatedAt: created.Add(3 * time.Minute)},
	)
	fixture.store.sessions["session-1"] = task.Session{ID: "session-1", TaskID: failed.ID, Provider: task.ProviderCodex, Status: "paused", Resumable: true, UpdatedAt: created}
	fixture.start(t)

	tests := []struct {
		command string
		want    []string
		absent  []string
	}{
		{command: "/status", want: []string{"AgentBridge status", "Failed/paused: 1"}, absent: []string{"secret prompt"}},
		{command: "/tasks", want: []string{"task-failed", "failed", "Fix navigation"}, absent: []string{"secret prompt"}},
		{command: "/sessions", want: []string{"session-1", "codex", "paused"}},
		{command: "/diff task-failed", want: []string{"Diff for task-failed", "button aligned", "0123456789abcdef", "refs/heads/staging"}, absent: []string{"must not leak"}},
		{command: "/logs task-failed", want: []string{"Logs for task-failed", "diff_summary", "failure", "[REDACTED:telegram-token]"}, absent: []string{"must not leak", "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmno"}},
		{command: "/health", want: []string{"AgentBridge health", "Store: ok", "codex: authenticated"}},
	}
	for i, test := range tests {
		t.Run(strings.Fields(test.command)[0], func(t *testing.T) {
			before := len(fixture.messenger.messages())
			if _, err := fixture.app.HandleUpdate(context.Background(), promptUpdate(int64(50+i), test.command)); err != nil {
				t.Fatal(err)
			}
			messages := fixture.messenger.messages()
			if len(messages) != before+1 {
				t.Fatalf("message count = %d, want %d", len(messages), before+1)
			}
			got := messages[len(messages)-1].Text
			for _, want := range test.want {
				if !strings.Contains(got, want) {
					t.Errorf("message %q does not contain %q", got, want)
				}
			}
			for _, absent := range test.absent {
				if strings.Contains(got, absent) {
					t.Errorf("message %q contains %q", got, absent)
				}
			}
			if len([]rune(got)) > 3_500 {
				t.Fatalf("message runes = %d", len([]rune(got)))
			}
		})
	}
	if p.starts != 0 || p.resumes != 0 {
		t.Fatalf("direct commands invoked provider: starts=%d resumes=%d", p.starts, p.resumes)
	}
}

func TestRetryRequeuesOnlyFailedOrPausedTask(t *testing.T) {
	fixture := newFixture(t, []provider.Event{{ID: provider.MustID("done"), Type: provider.EventCompleted}})
	value := task.Task{ID: "task-retry", RepoProfileID: "sample", Title: "Retry task", Prompt: "finish work", State: task.Failed, Provider: task.ProviderCodex, TelegramChatID: 100, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	createStoredTask(t, fixture.store, value)
	fixture.start(t)

	if _, err := fixture.app.HandleUpdate(context.Background(), promptUpdate(71, "/retry task-retry")); err != nil {
		t.Fatal(err)
	}
	retryID := onlyOtherTaskID(t, fixture.store, value.ID)
	if got := fixture.wait(t, retryID); got.State != task.Completed {
		t.Fatalf("state = %s", got.State)
	}
	previous, err := fixture.store.Task(context.Background(), value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if previous.State != task.Failed {
		t.Fatalf("previous attempt state = %s", previous.State)
	}
	events, err := fixture.store.Events(context.Background(), retryID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || !strings.Contains(string(events[0].Payload), `"retry_of":"task-retry"`) {
		t.Fatalf("retry lineage events = %#v", events)
	}
	messages := fixture.messenger.messages()
	if got := messages[len(messages)-1].Text; !strings.Contains(got, retryID) || !strings.Contains(got, value.ID) {
		t.Fatalf("retry confirmation = %q", got)
	}

	completed := task.Task{ID: "task-complete", RepoProfileID: "sample", Title: "Done", Prompt: "done", State: task.Completed, Provider: task.ProviderCodex, TelegramChatID: 100, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	createStoredTask(t, fixture.store, completed)
	if _, err := fixture.app.HandleUpdate(context.Background(), promptUpdate(72, "/retry task-complete")); err == nil {
		t.Fatal("retry completed task succeeded")
	}
}

func TestDirectCommandPropagatesUnknownTask(t *testing.T) {
	fixture := newFixture(t, nil)
	fixture.start(t)
	if _, err := fixture.app.HandleUpdate(context.Background(), promptUpdate(80, "/logs missing")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("error = %v", err)
	}
}

func TestProviderUsageRendersWindowsResetTimesAndCredits(t *testing.T) {
	credits := 12.5
	p := &usageProvider{
		Provider: providerfake.New(task.ProviderCodex, provider.MustID("unused-session"), nil),
		usage: provider.Usage{
			Provider:   task.ProviderCodex,
			ObservedAt: time.Date(2026, 7, 14, 10, 30, 0, 0, time.UTC),
			Windows: []provider.UsageWindow{{
				Name: "five_hour", UsedPercent: 37.5,
				ResetsAt: time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC),
			}},
			Credits: &credits,
		},
	}
	fixture := newFixtureWithProvider(t, p)
	fixture.start(t)

	if _, err := fixture.app.HandleUpdate(context.Background(), promptUpdate(90, "/codex usage")); err != nil {
		t.Fatal(err)
	}
	got := fixture.messenger.messages()[0].Text
	for _, want := range []string{"codex usage", "five_hour: 37.5% used", "resets 2026-07-14T12:00:00Z", "Credits: 12.50", "Observed: 2026-07-14T10:30:00Z"} {
		if !strings.Contains(got, want) {
			t.Errorf("usage message %q does not contain %q", got, want)
		}
	}
}

func TestProviderUsageAlwaysRepliesWhenAuthenticationIsRequired(t *testing.T) {
	p := &usageProvider{
		Provider: providerfake.New(task.ProviderCodex, provider.MustID("unused-session"), nil),
		usageErr: errors.New("sensitive CLI failure"),
		auth:     provider.AuthStatus{Authenticated: false},
	}
	fixture := newFixtureWithProvider(t, p)
	fixture.start(t)

	if _, err := fixture.app.HandleUpdate(context.Background(), promptUpdate(91, "/codex usage")); err != nil {
		t.Fatal(err)
	}
	got := fixture.messenger.messages()[0].Text
	if !strings.Contains(got, "codex usage unavailable: authentication required") {
		t.Fatalf("usage message = %q", got)
	}
	if strings.Contains(got, "sensitive CLI failure") {
		t.Fatalf("usage leaked provider error: %q", got)
	}
}

type observedProvider struct {
	provider.Provider
	starts  int
	resumes int
}

type usageProvider struct {
	provider.Provider
	usage    provider.Usage
	usageErr error
	auth     provider.AuthStatus
	authErr  error
}

func (p *usageProvider) Usage(context.Context) (provider.Usage, error) { return p.usage, p.usageErr }
func (p *usageProvider) AuthStatus(context.Context) (provider.AuthStatus, error) {
	return p.auth, p.authErr
}

func (p *observedProvider) Start(ctx context.Context, request provider.StartRequest) (provider.Session, <-chan provider.Event, error) {
	p.starts++
	return p.Provider.Start(ctx, request)
}

func (p *observedProvider) Resume(ctx context.Context, request provider.ResumeRequest) (provider.Session, <-chan provider.Event, error) {
	p.resumes++
	return p.Provider.Resume(ctx, request)
}

func createStoredTask(t *testing.T, values *memoryStore, value task.Task) {
	t.Helper()
	event := task.Event{ID: "created-" + value.ID, TaskID: value.ID, Type: task.EventTaskCreated, Visibility: task.VisibilityUser, Payload: []byte(`{"created":true}`), CreatedAt: value.CreatedAt}
	if err := values.CreateTask(context.Background(), value, event); err != nil {
		t.Fatal(err)
	}
}

func onlyOtherTaskID(t *testing.T, values *memoryStore, previous string) string {
	t.Helper()
	tasks, err := values.ListTasks(context.Background(), store.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range tasks {
		if value.ID != previous {
			return value.ID
		}
	}
	t.Fatal("fresh retry task was not created")
	return ""
}
