package process

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestSupervisorSeparatesAndBoundsOutput(t *testing.T) {
	s := Supervisor{MaxLineBytes: 8, MaxEvents: 10, InterruptGrace: 10 * time.Millisecond}
	result, err := s.Run(context.Background(), Command{Argv: []string{"/bin/sh", "-c", "printf 'out-long-line'; printf 'err-line' >&2"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 2 {
		t.Fatalf("events = %#v", result.Events)
	}
	byStream := map[Stream]Event{}
	for _, event := range result.Events {
		byStream[event.Stream] = event
	}
	if _, ok := byStream[Stderr]; !ok {
		t.Fatalf("streams = %#v", result.Events)
	}
	if !byStream[Stdout].Truncated || len(byStream[Stdout].Line) != 8 {
		t.Fatalf("unbounded event = %#v", byStream[Stdout])
	}
}

func TestSupervisorClassifiesExitAndStartErrors(t *testing.T) {
	s := Supervisor{}
	result, err := s.Run(context.Background(), Command{Argv: []string{"/bin/sh", "-c", "exit 7"}})
	if err != nil || result.Class != ExitFailed || result.ExitCode != 7 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	_, err = s.Run(context.Background(), Command{Argv: []string{"/definitely/missing"}})
	if err == nil || !errors.Is(err, ErrStart) {
		t.Fatalf("err = %v", err)
	}
	if _, err := s.Run(context.Background(), Command{Argv: nil}); err == nil {
		t.Fatal("empty argv accepted")
	}
}

func TestSupervisorCancellationTerminatesProcess(t *testing.T) {
	s := Supervisor{InterruptGrace: 10 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	result, err := s.Run(ctx, Command{Argv: []string{"/bin/sh", "-c", "trap '' INT; sleep 10"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Class != ExitCanceled {
		t.Fatalf("class = %s", result.Class)
	}
}

func TestSupervisorUsesExplicitEnvironmentAllowlist(t *testing.T) {
	s := Supervisor{AllowedEnvironment: map[string]struct{}{"VISIBLE": {}}}
	result, err := s.Run(context.Background(), Command{Argv: []string{"/usr/bin/env"}, Env: map[string]string{"VISIBLE": "yes", "SECRET": "no"}})
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	for _, event := range result.Events {
		output.WriteString(event.Line)
	}
	if !strings.Contains(output.String(), "VISIBLE=yes") || strings.Contains(output.String(), "SECRET=no") {
		t.Fatalf("output = %q", output.String())
	}
	if _, err := exec.LookPath("env"); err != nil {
		t.Fatal(err)
	}
}
