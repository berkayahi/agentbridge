// Package verify executes immutable repository verification commands in order.
package verify

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	bridgeprocess "github.com/berkayahi/agentbridge/internal/process"
	"github.com/berkayahi/agentbridge/internal/security"
)

var (
	ErrFailed          = errors.New("verification failed")
	ErrUnsafeDirectory = errors.New("verification directory escapes worktree")
)

type Command struct {
	Argv    []string
	Dir     string
	Timeout time.Duration
}
type CommandResult struct {
	Argv     []string
	Dir      string
	ExitCode int
}
type Event struct {
	Command   int
	Stream    bridgeprocess.Stream
	Text      string
	Truncated bool
}
type Report struct {
	Commands []CommandResult
	Events   []Event
	Summary  string
}
type Runner struct {
	Supervisor bridgeprocess.Supervisor
	Redactor   *security.Redactor
	OnEvent    func(Event)
}

func (r Runner) Run(ctx context.Context, worktree string, commands []Command) (Report, error) {
	root, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		return Report{}, fmt.Errorf("resolve worktree: %w", err)
	}
	redactor := r.Redactor
	if redactor == nil {
		redactor = security.NewRedactor(security.Config{})
	}
	supervisor := r.Supervisor
	if supervisor.AllowedEnvironment == nil {
		supervisor.AllowedEnvironment = allowedEnvironment()
	}
	report := Report{}
	var summary strings.Builder
	for index, command := range commands {
		if len(command.Argv) == 0 {
			return report, fmt.Errorf("%w: command %d has empty argv", ErrFailed, index+1)
		}
		dir, err := resolveDirectory(root, command.Dir)
		if err != nil {
			return report, err
		}
		timeout := command.Timeout
		if timeout <= 0 {
			timeout = 15 * time.Minute
		}
		commandCtx, cancel := context.WithTimeout(ctx, timeout)
		result, runErr := supervisor.Run(commandCtx, bridgeprocess.Command{Argv: command.Argv, Dir: dir, Env: currentEnvironment(supervisor.AllowedEnvironment)})
		ctxErr := commandCtx.Err()
		cancel()
		report.Commands = append(report.Commands, CommandResult{Argv: append([]string(nil), command.Argv...), Dir: dir, ExitCode: result.ExitCode})
		for _, output := range result.Events {
			event := Event{Command: index, Stream: output.Stream, Text: redactor.RedactString(output.Line), Truncated: output.Truncated}
			report.Events = append(report.Events, event)
			if r.OnEvent != nil {
				r.OnEvent(event)
			}
			summary.WriteString(event.Text)
			summary.WriteByte('\n')
		}
		report.Summary = redactor.RedactString(summary.String())
		if runErr != nil || result.Class != bridgeprocess.ExitSuccess {
			if ctxErr != nil {
				return report, fmt.Errorf("%w: command %d (%s): %w", ErrFailed, index+1, safeArgv(command.Argv), ctxErr)
			}
			return report, fmt.Errorf("%w: command %d (%s), exit %d", ErrFailed, index+1, safeArgv(command.Argv), result.ExitCode)
		}
	}
	return report, nil
}

func resolveDirectory(root, relative string) (string, error) {
	if relative == "" || relative == "." {
		return root, nil
	}
	if filepath.IsAbs(relative) {
		return "", ErrUnsafeDirectory
	}
	clean := filepath.Clean(relative)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", ErrUnsafeDirectory
	}
	candidate, err := filepath.EvalSymlinks(filepath.Join(root, clean))
	if err != nil {
		return "", fmt.Errorf("resolve verification directory: %w", err)
	}
	rel, err := filepath.Rel(root, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", ErrUnsafeDirectory
	}
	return candidate, nil
}
func safeArgv(argv []string) string {
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		quoted[i] = strconv.Quote(arg)
	}
	return strings.Join(quoted, " ")
}
func allowedEnvironment() map[string]struct{} {
	return map[string]struct{}{"HOME": {}, "PATH": {}, "TMPDIR": {}, "GOCACHE": {}, "GOMODCACHE": {}, "GOPATH": {}, "CGO_ENABLED": {}, "SYSTEMROOT": {}}
}
func currentEnvironment(allowed map[string]struct{}) map[string]string {
	values := make(map[string]string)
	for key := range allowed {
		if value, ok := os.LookupEnv(key); ok {
			values[key] = value
		}
	}
	return values
}
