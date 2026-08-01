package git

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/berkayahi/agentbridge/internal/security"
)

type RunResult struct{ Stdout, Stderr, Summary string }

type Runner struct {
	Executable     string
	MaxOutputBytes int
	Redactor       *security.Redactor
	Environment    []string
}

func (r Runner) Run(ctx context.Context, dir string, args ...string) (RunResult, error) {
	return r.run(ctx, dir, gitEnvironment(r.Environment), args...)
}

// RunWithEnvironment runs Git with additional, call-scoped environment
// settings. The settings replace matching entries in the runner's normal safe
// environment instead of bypassing that environment.
func (r Runner) RunWithEnvironment(ctx context.Context, dir string, environment []string, args ...string) (RunResult, error) {
	return r.run(ctx, dir, mergeEnvironment(gitEnvironment(r.Environment), environment), args...)
}

// RunWithEnvironmentUnredacted is restricted to an in-process consumer that
// must inspect bytes before deciding whether they are safe to retain. Stdout
// is never included in an error; stderr remains redacted.
func (r Runner) RunWithEnvironmentUnredacted(ctx context.Context, dir string, environment []string, args ...string) (RunResult, error) {
	return r.runUnredacted(ctx, dir, mergeEnvironment(gitEnvironment(r.Environment), environment), args...)
}

func (r Runner) run(ctx context.Context, dir string, environment []string, args ...string) (RunResult, error) {
	return r.runWithRedaction(ctx, dir, environment, args...)
}

func (r Runner) runUnredacted(ctx context.Context, dir string, environment []string, args ...string) (RunResult, error) {
	return r.runCommand(ctx, dir, environment, args, false)
}

func (r Runner) runWithRedaction(ctx context.Context, dir string, environment []string, args ...string) (RunResult, error) {
	return r.runCommand(ctx, dir, environment, args, true)
}

func (r Runner) runCommand(ctx context.Context, dir string, environment []string, args []string, redactStdout bool) (RunResult, error) {
	if err := ctx.Err(); err != nil {
		return RunResult{}, err
	}
	executable := r.Executable
	if executable == "" {
		executable = "git"
	}
	limit := r.MaxOutputBytes
	if limit <= 0 {
		limit = 64 << 10
	}
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Dir = dir
	cmd.Env = environment
	var stdout, stderr boundedBuffer
	stdout.limit, stderr.limit = limit, limit
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	result := RunResult{Stdout: stdout.String(), Stderr: r.redact(stderr.String()), Summary: r.summary(args)}
	if redactStdout {
		result.Stdout = r.redact(result.Stdout)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, ctxErr
	}
	if err != nil {
		return result, fmt.Errorf("git %s: %w: %s", result.Summary, err, result.Stderr)
	}
	return result, nil
}

func (r Runner) redact(value string) string {
	redactor := r.Redactor
	if redactor == nil {
		redactor = security.NewRedactor(security.Config{})
	}
	return redactor.RedactString(value)
}
func (r Runner) summary(args []string) string {
	parts := make([]string, len(args))
	for i, arg := range args {
		parts[i] = strconv.Quote(r.redact(arg))
	}
	return strings.Join(parts, " ")
}

func gitEnvironment(extra []string) []string {
	allowed := map[string]bool{"HOME": true, "PATH": true, "TMPDIR": true, "SSH_AUTH_SOCK": true, "SSH_AGENT_PID": true, "USER": true, "LOGNAME": true, "SystemRoot": true}
	env := []string{"GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}
	if len(extra) > 0 {
		env = append([]string(nil), extra...)
	}
	if len(extra) > 0 {
		return env
	}
	for _, value := range os.Environ() {
		key, _, ok := strings.Cut(value, "=")
		if ok && allowed[key] {
			env = append(env, value)
		}
	}
	return env
}

func mergeEnvironment(base, overrides []string) []string {
	result := append([]string(nil), base...)
	for _, override := range overrides {
		key, _, ok := strings.Cut(override, "=")
		if !ok || key == "" {
			continue
		}
		filtered := result[:0]
		for _, value := range result {
			existingKey, _, existingOK := strings.Cut(value, "=")
			if !existingOK || existingKey != key {
				filtered = append(filtered, value)
			}
		}
		result = append(filtered, override)
	}
	return result
}

type boundedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.buf.Len()
	if remaining > 0 {
		if remaining < len(p) {
			_, _ = b.buf.Write(p[:remaining])
			b.truncated = true
		} else {
			_, _ = b.buf.Write(p)
		}
	} else if len(p) > 0 {
		b.truncated = true
	}
	return n, nil
}
func (b *boundedBuffer) String() string {
	value := b.buf.String()
	if b.truncated {
		value += "…[TRUNCATED]"
	}
	return value
}
