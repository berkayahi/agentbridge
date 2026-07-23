package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/berkayahi/agentbridge/internal/security"
	"github.com/berkayahi/agentbridge/internal/store"
	"github.com/berkayahi/agentbridge/internal/task"
	"github.com/berkayahi/agentbridge/internal/telegram"
)

const (
	directMessageRunes = 3_500
	directListLimit    = 10
	directLogLimit     = 20
)

var directRedactor = security.NewRedactor(security.Config{MaxPayloadRunes: directMessageRunes})

func (a *App) handleDirectCommand(ctx context.Context, update telegram.Update, command telegram.Command) error {
	if update.Message == nil {
		return errors.New("app: direct command message is missing")
	}

	var (
		text string
		err  error
	)
	switch command.Kind {
	case telegram.KindHelp:
		text = "AgentBridge commands\n/codex <task>\n/claude <task>\n/usage or /codex usage\n/status\n/tasks\n/sessions\n/health\n/logs <task_id>\n/diff <task_id>\n/cancel <task_id>\n/retry <task_id>"
	case telegram.KindStatus:
		text, err = a.statusText(ctx)
	case telegram.KindTasks:
		text, err = a.tasksText(ctx)
	case telegram.KindSessions:
		text, err = a.sessionsText(ctx)
	case telegram.KindDiff:
		text, err = a.diffText(ctx, command.TaskID)
	case telegram.KindLogs:
		text, err = a.logsText(ctx, command.TaskID)
	case telegram.KindHealth:
		text, err = a.healthText(ctx)
	case telegram.KindRetry:
		var retryID string
		retryID, err = a.retryTask(ctx, command.TaskID)
		text = fmt.Sprintf("Task queued for retry: %s (previous attempt: %s)", retryID, command.TaskID)
	default:
		return errors.New("app: unsupported direct command")
	}
	if err != nil {
		return err
	}
	_, err = a.deps.Messenger.Send(ctx, telegram.Message{ChatID: update.Message.Chat.ID, Text: directRedactor.RedactString(text)})
	return err
}

func (a *App) continueTask(ctx context.Context, id, input string, chatID int64) error {
	value, err := a.deps.Store.Task(ctx, id)
	if err != nil {
		return err
	}
	if value.TelegramChatID != chatID {
		return errors.New("app: task belongs to another chat")
	}
	return a.ContinueTask(ctx, id, input)
}

func (a *App) statusText(ctx context.Context) (string, error) {
	tasks, err := a.deps.Store.ListTasks(ctx, store.ListFilter{})
	if err != nil {
		return "", err
	}
	var active, queued, attention, completed int
	for _, value := range tasks {
		switch value.State {
		case task.Queued:
			queued++
		case task.Failed, task.Paused, task.AwaitingAuth, task.AwaitingApproval:
			attention++
		case task.Completed, task.Canceled:
			completed++
		default:
			active++
		}
	}
	return fmt.Sprintf("AgentBridge status\nActive: %d\nQueued: %d\nFailed/paused: %d\nFinished: %d\nTotal: %d", active, queued, attention, completed, len(tasks)), nil
}

func (a *App) tasksText(ctx context.Context) (string, error) {
	tasks, err := a.deps.Store.ListTasks(ctx, store.ListFilter{Limit: directListLimit})
	if err != nil {
		return "", err
	}
	slices.SortFunc(tasks, func(left, right task.Task) int {
		if compared := right.CreatedAt.Compare(left.CreatedAt); compared != 0 {
			return compared
		}
		return strings.Compare(right.ID, left.ID)
	})
	if len(tasks) > directListLimit {
		tasks = tasks[:directListLimit]
	}
	lines := []string{"Recent tasks"}
	for _, value := range tasks {
		lines = append(lines, fmt.Sprintf("%s | %s | %s | %s", value.ID, value.State, value.Provider, value.Title))
	}
	if len(tasks) == 0 {
		lines = append(lines, "No tasks recorded.")
	}
	return strings.Join(lines, "\n"), nil
}

func (a *App) sessionsText(ctx context.Context) (string, error) {
	sessions, err := a.deps.Store.ResumableSessions(ctx)
	if err != nil {
		return "", err
	}
	slices.SortFunc(sessions, func(left, right task.Session) int {
		if compared := right.UpdatedAt.Compare(left.UpdatedAt); compared != 0 {
			return compared
		}
		return strings.Compare(right.ID, left.ID)
	})
	if len(sessions) > directListLimit {
		sessions = sessions[:directListLimit]
	}
	lines := []string{"Resumable sessions"}
	for _, value := range sessions {
		lines = append(lines, fmt.Sprintf("%s | %s | %s | task %s", value.ID, value.Provider, value.Status, value.TaskID))
	}
	if len(sessions) == 0 {
		lines = append(lines, "No resumable sessions.")
	}
	return strings.Join(lines, "\n"), nil
}

func (a *App) diffText(ctx context.Context, id string) (string, error) {
	value, err := a.deps.Store.Task(ctx, id)
	if err != nil {
		return "", err
	}
	events, err := a.deps.Store.Events(ctx, id)
	if err != nil {
		return "", err
	}
	lines := []string{"Diff for " + id}
	for _, event := range events {
		if event.Visibility != task.VisibilityUser || !diffEvent(event.Type) {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s | %s", event.Type, safePayload(event.Payload)))
	}
	if value.CommitSHA != "" {
		lines = append(lines, "Commit: "+value.CommitSHA)
	}
	if value.PushRef != "" {
		lines = append(lines, "Ref: "+value.PushRef)
	}
	if value.DeploymentURL != "" {
		lines = append(lines, "Deployment: "+value.DeploymentURL)
	}
	if len(lines) == 1 {
		lines = append(lines, "No durable diff or delivery result yet.")
	}
	return strings.Join(lines, "\n"), nil
}

func diffEvent(value task.EventType) bool {
	switch value {
	case task.EventDiffSummary, task.EventVerification, task.EventCommitCreated, task.EventPushCompleted, task.EventDeployment:
		return true
	default:
		return false
	}
}

func (a *App) logsText(ctx context.Context, id string) (string, error) {
	if _, err := a.deps.Store.Task(ctx, id); err != nil {
		return "", err
	}
	events, err := a.deps.Store.Events(ctx, id)
	if err != nil {
		return "", err
	}
	visible := make([]task.Event, 0, len(events))
	for _, event := range events {
		if event.Visibility == task.VisibilityUser {
			visible = append(visible, event)
		}
	}
	if len(visible) > directLogLimit {
		visible = visible[len(visible)-directLogLimit:]
	}
	lines := []string{"Logs for " + id}
	for _, event := range visible {
		lines = append(lines, fmt.Sprintf("%s | %s | %s", event.CreatedAt.UTC().Format(time.RFC3339), event.Type, safePayload(event.Payload)))
	}
	if len(visible) == 0 {
		lines = append(lines, "No user-visible events.")
	}
	return strings.Join(lines, "\n"), nil
}

func (a *App) healthText(ctx context.Context) (string, error) {
	if _, err := a.deps.Store.ListTasks(ctx, store.ListFilter{Limit: 1}); err != nil {
		return "", err
	}
	names := make([]task.Provider, 0, len(a.deps.Providers))
	for name := range a.deps.Providers {
		names = append(names, name)
	}
	slices.Sort(names)
	lines := []string{"AgentBridge health", "Store: ok"}
	for _, name := range names {
		status, err := a.deps.Providers[name].AuthStatus(ctx)
		if err != nil {
			lines = append(lines, fmt.Sprintf("%s: unavailable", name))
			continue
		}
		state := "authentication required"
		if status.Authenticated {
			state = "authenticated"
		}
		lines = append(lines, fmt.Sprintf("%s: %s", name, state))
	}
	return strings.Join(lines, "\n"), nil
}

func (a *App) retryTask(ctx context.Context, id string) (string, error) {
	previous, err := a.deps.Store.Task(ctx, id)
	if err != nil {
		return "", err
	}
	if previous.State != task.Failed && previous.State != task.Paused {
		return "", fmt.Errorf("app: task %s in state %s cannot be retried: %w", id, previous.State, store.ErrInvalidTransition)
	}
	at := a.deps.Clock().UTC()
	retry := task.Task{
		ID:             a.nextID(),
		RepoProfileID:  previous.RepoProfileID,
		Title:          previous.Title,
		Prompt:         previous.Prompt,
		State:          task.Queued,
		Provider:       previous.Provider,
		TelegramChatID: previous.TelegramChatID,
		CreatedAt:      at,
		UpdatedAt:      at,
	}
	event := a.event(retry.ID, task.EventTaskCreated, task.VisibilityUser, map[string]any{"retry_of": previous.ID})
	if err := a.deps.Store.CreateTask(ctx, retry, event); err != nil {
		return "", err
	}
	if err := a.publish(ctx, event); err != nil {
		a.deps.Logger.Warn("could not publish retry event", "task", retry.ID)
	}
	if err := a.project(ctx, retry, "queued as a fresh retry attempt", true); err != nil {
		return "", err
	}
	if err := a.enqueue(queuedTask{id: retry.ID}); err != nil {
		return "", err
	}
	return retry.ID, nil
}

func safePayload(payload []byte) string {
	if len(payload) == 0 {
		return "{}"
	}
	return string(directRedactor.RedactBytes(payload))
}
