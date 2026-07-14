package attachment

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/berkayahi/agentbridge/internal/task"
)

var (
	ErrTaskReference = errors.New("attachment: invalid task reference")
	explicitTaskRef  = regexp.MustCompile(`(^|[[:space:]])(task:|#)([A-Za-z0-9][A-Za-z0-9_-]{0,127})([[:space:]]|$)`)
)

type TaskLookup interface {
	Task(context.Context, string) (task.Task, error)
	NonterminalTasks(context.Context) ([]task.Task, error)
}

// StoreTaskLocator associates Telegram files without allowing a caption or
// status reply to cross the originating private chat boundary.
type StoreTaskLocator struct{ tasks TaskLookup }

func NewStoreTaskLocator(tasks TaskLookup) *StoreTaskLocator {
	return &StoreTaskLocator{tasks: tasks}
}

func (l *StoreTaskLocator) TaskForID(ctx context.Context, chatID int64, id string) (string, error) {
	if l == nil || l.tasks == nil {
		return "", ErrTaskReference
	}
	value, err := l.tasks.Task(ctx, id)
	if err != nil || value.TelegramChatID != chatID || !workflowActive(value.State) {
		return "", fmt.Errorf("%w: task is unavailable", ErrTaskReference)
	}
	return value.ID, nil
}

func (l *StoreTaskLocator) TaskForCaption(ctx context.Context, chatID int64, caption string) (string, error) {
	match := explicitTaskRef.FindStringSubmatch(caption)
	if len(match) == 0 {
		return "", nil
	}
	if l == nil || l.tasks == nil {
		return "", ErrTaskReference
	}
	value, err := l.tasks.Task(ctx, match[3])
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "", err
	}
	if err != nil || value.TelegramChatID != chatID || !workflowActive(value.State) {
		return "", fmt.Errorf("%w: task is unavailable", ErrTaskReference)
	}
	return value.ID, nil
}

func (l *StoreTaskLocator) TaskForStatusMessage(ctx context.Context, chatID, messageID int64) (string, error) {
	values, err := l.activeTasks(ctx, chatID)
	if err != nil {
		return "", err
	}
	for _, value := range values {
		if value.TelegramMessageID == messageID {
			return value.ID, nil
		}
	}
	return "", nil
}

func (l *StoreTaskLocator) ActiveTaskIDs(ctx context.Context, chatID int64) ([]string, error) {
	values, err := l.activeTasks(ctx, chatID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(values))
	for _, value := range values {
		ids = append(ids, value.ID)
	}
	return ids, nil
}

func (l *StoreTaskLocator) activeTasks(ctx context.Context, chatID int64) ([]task.Task, error) {
	if l == nil || l.tasks == nil {
		return nil, errors.New("attachment: task lookup is required")
	}
	values, err := l.tasks.NonterminalTasks(ctx)
	if err != nil {
		return nil, fmt.Errorf("list attachment tasks: %w", err)
	}
	active := make([]task.Task, 0, len(values))
	for _, value := range values {
		if value.TelegramChatID == chatID && workflowActive(value.State) {
			active = append(active, value)
		}
	}
	return active, nil
}

func workflowActive(state task.State) bool {
	switch state {
	case task.Queued, task.Preparing, task.Running, task.AwaitingApproval, task.AwaitingAuth, task.Verifying, task.Committing, task.Pushing:
		return true
	default:
		return false
	}
}

var _ TaskLocator = (*StoreTaskLocator)(nil)
