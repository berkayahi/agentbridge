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
	cmd.Env = gitEnvironment(r.Environment)
	var stdout, stderr boundedBuffer
	stdout.limit, stderr.limit = limit, limit
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	result := RunResult{Stdout: r.redact(stdout.String()), Stderr: r.redact(stderr.String()), Summary: r.summary(args)}
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
