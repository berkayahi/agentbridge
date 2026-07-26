// Package process starts and terminates child processes without invoking a shell.
package process

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"sync"
	"time"

	"github.com/berkayahi/agentbridge/internal/egressguard"
	"github.com/berkayahi/agentbridge/internal/isolation"
)

var ErrStart = errors.New("process: start")

type Stream string

const (
	Stdout Stream = "stdout"
	Stderr Stream = "stderr"
)

type ExitClass string

const (
	ExitSuccess  ExitClass = "success"
	ExitFailed   ExitClass = "failed"
	ExitCanceled ExitClass = "canceled"
)

type Command struct {
	Argv        []string
	Dir         string
	Env         map[string]string
	Isolation   *isolation.Policy
	EgressGuard *egressguard.Guard
}
type Event struct {
	Stream      Stream
	Line        string
	Truncated   bool
	Quarantined bool
}
type Result struct {
	Class    ExitClass
	ExitCode int
	Events   []Event
}

type Supervisor struct {
	AllowedEnvironment map[string]struct{}
	MaxLineBytes       int
	MaxEvents          int
	InterruptGrace     time.Duration
}

func (s Supervisor) Run(ctx context.Context, command Command) (Result, error) {
	if len(command.Argv) == 0 || command.Argv[0] == "" {
		return Result{}, fmt.Errorf("%w: argv must not be empty", ErrStart)
	}
	maxLine := s.MaxLineBytes
	if maxLine <= 0 {
		maxLine = 64 << 10
	}
	maxEvents := s.MaxEvents
	if maxEvents <= 0 {
		maxEvents = 1000
	}
	grace := s.InterruptGrace
	if grace <= 0 {
		grace = 5 * time.Second
	}
	cmd := exec.Command(command.Argv[0], command.Argv[1:]...)
	cmd.Dir = command.Dir
	cmd.Env = s.environment(command.Env)
	configureProcessGroup(cmd)
	if command.Isolation != nil {
		if err := isolation.PrepareCommand(cmd, *command.Isolation); err != nil {
			return Result{}, fmt.Errorf("%w: isolation: %v", ErrStart, err)
		}
	}
	var mu sync.Mutex
	events := make([]Event, 0)
	appendEvent := func(event Event) {
		if command.EgressGuard != nil {
			data, err := command.EgressGuard.Check(egressguard.ClassTerminalOutput, []byte(event.Line))
			event.Line = string(data)
			event.Quarantined = err != nil
		}
		mu.Lock()
		defer mu.Unlock()
		if len(events) < maxEvents {
			events = append(events, event)
		}
	}
	stdout := &lineWriter{stream: Stdout, limit: maxLine, emit: appendEvent}
	stderr := &lineWriter{stream: Stderr, limit: maxLine, emit: appendEvent}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrStart, err)
	}
	if command.Isolation != nil {
		if err := isolation.ApplyStartedProcess(cmd.Process, *command.Isolation); err != nil {
			_ = killProcessGroup(cmd.Process)
			_ = cmd.Wait()
			return Result{}, fmt.Errorf("%w: isolation limits: %v", ErrStart, err)
		}
	}

	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	canceled := false
	var waitErr error
	select {
	case waitErr = <-wait:
	case <-ctx.Done():
		canceled = true
		_ = interruptProcessGroup(cmd.Process)
		timer := time.NewTimer(grace)
		select {
		case waitErr = <-wait:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
			_ = killProcessGroup(cmd.Process)
			waitErr = <-wait
		}
	}
	stdout.Flush()
	stderr.Flush()
	mu.Lock()
	resultEvents := append([]Event(nil), events...)
	mu.Unlock()
	result := Result{Class: ExitSuccess, ExitCode: 0, Events: resultEvents}
	if canceled {
		result.Class = ExitCanceled
		result.ExitCode = exitCode(waitErr)
		return result, nil
	}
	if waitErr != nil {
		result.Class = ExitFailed
		result.ExitCode = exitCode(waitErr)
	}
	return result, nil
}

func (s Supervisor) environment(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		if _, ok := s.AllowedEnvironment[key]; ok {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	filtered := make([]string, 0, len(keys))
	for _, key := range keys {
		filtered = append(filtered, key+"="+values[key])
	}
	allowed := s.AllowedEnvironment
	if allowed == nil {
		allowed = map[string]struct{}{}
	}
	return isolation.FilterEnvironment(filtered, isolation.EnvironmentPolicy{Allowed: allowed})
}

type lineWriter struct {
	stream    Stream
	limit     int
	emit      func(Event)
	line      []byte
	truncated bool
}

func (w *lineWriter) Write(data []byte) (int, error) {
	for _, b := range data {
		if b == '\n' {
			w.Flush()
			continue
		}
		if len(w.line) < w.limit {
			w.line = append(w.line, b)
		} else {
			w.truncated = true
		}
	}
	return len(data), nil
}

func (w *lineWriter) Flush() {
	if len(w.line) > 0 || w.truncated {
		w.emit(Event{Stream: w.stream, Line: string(w.line), Truncated: w.truncated})
	}
	w.line = w.line[:0]
	w.truncated = false
}

func exitCode(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}
