package telegram

import (
	"errors"
	"regexp"
	"strings"

	"github.com/berkayahi/agentbridge/internal/task"
)

var taskIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

type Kind string

const (
	KindPrompt   Kind = "prompt"
	KindUsage    Kind = "usage"
	KindStatus   Kind = "status"
	KindTasks    Kind = "tasks"
	KindSessions Kind = "sessions"
	KindDiff     Kind = "diff"
	KindLogs     Kind = "logs"
	KindCancel   Kind = "cancel"
	KindRetry    Kind = "retry"
	KindHealth   Kind = "health"
	KindHelp     Kind = "help"
	KindApprove  Kind = "approve"
	KindReject   Kind = "reject"
)

type Command struct {
	Kind       Kind
	Provider   task.Provider
	Argument   string
	TaskID     string
	ApprovalID string
	CallbackID string
}

// ParseUpdate converts either a slash command or a signed callback into one
// bounded command representation for the application dispatcher.
func ParseUpdate(update Update, botUsername string, signer *CallbackSigner) (Command, error) {
	if update.Message != nil && update.Callback != nil {
		return Command{}, errors.New("telegram: ambiguous update")
	}
	if update.Message != nil {
		input := update.Message.Text
		if strings.TrimSpace(input) == "" {
			input = update.Message.Caption
		}
		return ParseCommand(input, botUsername)
	}
	if update.Callback == nil || signer == nil || strings.TrimSpace(update.Callback.ID) == "" {
		return Command{}, errors.New("telegram: unsupported update")
	}
	action, err := signer.Verify(update.Callback.Data)
	if err != nil {
		return Command{}, err
	}
	kind := KindApprove
	if action.Action == "reject" {
		kind = KindReject
	} else if action.Action != "approve" {
		return Command{}, errors.New("telegram: unsupported callback action")
	}
	return Command{Kind: kind, TaskID: action.TaskID, ApprovalID: action.ApprovalID, CallbackID: update.Callback.ID}, nil
}

var directKinds = map[string]Kind{
	"usage": KindUsage, "status": KindStatus, "tasks": KindTasks,
	"sessions": KindSessions, "diff": KindDiff, "logs": KindLogs,
	"cancel": KindCancel, "retry": KindRetry, "health": KindHealth, "help": KindHelp, "start": KindHelp,
}

func ParseCommand(input, botUsername string) (Command, error) {
	input = strings.TrimSpace(input)
	if !strings.HasPrefix(input, "/") {
		return Command{}, errors.New("telegram: command must start with slash")
	}
	nameEnd := strings.IndexAny(input, " \t\r\n")
	name, argument := input[1:], ""
	if nameEnd >= 0 {
		name = input[1:nameEnd]
		argument = strings.TrimSpace(input[nameEnd:])
	}
	parts := strings.Split(name, "@")
	if len(parts) > 2 || len(parts) == 2 && !strings.EqualFold(parts[1], strings.TrimPrefix(botUsername, "@")) {
		return Command{}, errors.New("telegram: command is for another bot")
	}
	name = strings.ToLower(parts[0])
	if name == "codex" || name == "claude" {
		if argument == "" {
			return Command{}, errors.New("telegram: provider prompt is required")
		}
		if strings.EqualFold(argument, "usage") {
			return Command{Kind: KindUsage, Provider: task.Provider(name)}, nil
		}
		return Command{Kind: KindPrompt, Provider: task.Provider(name), Argument: argument}, nil
	}
	kind, ok := directKinds[name]
	if !ok {
		return Command{}, errors.New("telegram: unsupported command")
	}
	requiresTask := kind == KindDiff || kind == KindLogs || kind == KindCancel || kind == KindRetry
	if requiresTask {
		if !taskIDPattern.MatchString(argument) {
			return Command{}, errors.New("telegram: valid task ID is required")
		}
		return Command{Kind: kind, TaskID: argument}, nil
	}
	if argument != "" {
		return Command{}, errors.New("telegram: command takes no argument")
	}
	return Command{Kind: kind}, nil
}
