package claude

import (
	"context"
	"testing"

	"github.com/berkayahi/agentbridge/internal/provider"
)

type recordingSpawner struct {
	model  string
	runner *stubRunner
}

func (s *recordingSpawner) Spawn(_ context.Context, cfg ProcessConfig) (Runner, error) {
	s.model = cfg.Model
	s.runner = &stubRunner{events: make(chan provider.Event)}
	return s.runner, nil
}

type stubRunner struct {
	events chan provider.Event
}

func (r *stubRunner) SessionID() string                          { return "session-1" }
func (r *stubRunner) Events() <-chan provider.Event              { return r.events }
func (r *stubRunner) Send(context.Context, provider.Input) error { return nil }
func (r *stubRunner) Close() error                               { close(r.events); return nil }

// The keeper chooses the model when they send a bee, and the same model has to
// come back with her after a restart. This adapter used to take its model from
// configuration only, so every Claude bee flew the configured default no matter
// what was chosen.
func TestSpawnUsesTheChosenModelOnStartAndResume(t *testing.T) {
	for _, testCase := range []struct {
		name string
		fly  func(*Adapter) error
		want string
	}{
		{"start with a choice", func(a *Adapter) error {
			_, _, err := a.Start(context.Background(), provider.StartRequest{TaskID: provider.MustID("task-1"), Input: provider.Input{Text: "work"}, Model: "sonnet"})
			return err
		}, "sonnet"},
		{"start without a choice keeps the configured default", func(a *Adapter) error {
			_, _, err := a.Start(context.Background(), provider.StartRequest{TaskID: provider.MustID("task-1"), Input: provider.Input{Text: "work"}})
			return err
		}, "opus"},
		{"resume keeps the model she left with", func(a *Adapter) error {
			_, _, err := a.Resume(context.Background(), provider.ResumeRequest{
				TaskID:  provider.MustID("task-1"),
				Session: provider.Session{ID: provider.MustID("session-1"), TaskID: provider.MustID("task-1"), ExternalID: "session-1"},
				Input:   provider.Input{Text: "continue"}, Model: "sonnet",
			})
			return err
		}, "sonnet"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			spawner := &recordingSpawner{}
			adapter := NewAdapter(AdapterConfig{Spawn: spawner, Process: ProcessConfig{Model: "opus", ControlSocket: "/tmp/control.sock", Capability: []byte("capability")}})
			if err := testCase.fly(adapter); err != nil {
				t.Fatal(err)
			}
			if spawner.model != testCase.want {
				t.Fatalf("spawned model = %q, want %q", spawner.model, testCase.want)
			}
		})
	}
}
