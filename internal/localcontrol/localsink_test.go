package localcontrol_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/kernel"
	"github.com/berkayahi/agentbridge/internal/localcontrol"
)

type recordingSink struct {
	appended []kernel.Event
	err      error
}

func (s *recordingSink) Append(_ context.Context, event kernel.Event) error {
	if s.err != nil {
		return s.err
	}
	s.appended = append(s.appended, event)
	return nil
}

type recordingProgress struct {
	taskIDs []string
	events  []kernel.Event
	err     error
}

func (p *recordingProgress) ProjectProviderEvent(_ context.Context, taskID string, event kernel.Event) error {
	if p.err != nil {
		return p.err
	}
	p.taskIDs = append(p.taskIDs, taskID)
	p.events = append(p.events, event)
	return nil
}

func providerEvent(id, eventType string) kernel.Event {
	payload, _ := json.Marshal(map[string]string{"message": "aligning the widget"})
	return kernel.Event{
		ID: id, ExecutionID: "task-1-execution", Type: kernel.EventType(eventType),
		Visibility: "user", ProviderEventID: id, Payload: payload,
		CreatedAt: time.Unix(1_700_000_100, 0).UTC(),
	}
}

// Evidence is the record; the task-visible projection is derived from it. The
// durable sink must therefore commit before anything is projected.
func TestLocalObservationSinkCommitsEvidenceBeforeProjecting(t *testing.T) {
	inner := &recordingSink{}
	progress := &recordingProgress{}
	sink := localcontrol.NewLocalObservationSink(inner, progress, "task-1")
	if err := sink.Append(context.Background(), providerEvent("provider-1", "provider_assistant_message")); err != nil {
		t.Fatal(err)
	}
	if len(inner.appended) != 1 || len(progress.events) != 1 {
		t.Fatalf("inner=%d projected=%d", len(inner.appended), len(progress.events))
	}
	if progress.taskIDs[0] != "task-1" {
		t.Fatalf("projected task = %q", progress.taskIDs[0])
	}
}

// If the evidence write fails there is nothing to project, and the projection
// must not run: a board that shows work the journal does not record is a lie.
func TestLocalObservationSinkDoesNotProjectWithoutEvidence(t *testing.T) {
	want := errors.New("durable append failed")
	inner := &recordingSink{err: want}
	progress := &recordingProgress{}
	sink := localcontrol.NewLocalObservationSink(inner, progress, "task-1")
	if err := sink.Append(context.Background(), providerEvent("provider-1", "provider_assistant_message")); !errors.Is(err, want) {
		t.Fatalf("err = %v, want the durable failure", err)
	}
	if len(progress.events) != 0 {
		t.Fatalf("projected %d events without evidence", len(progress.events))
	}
}

// A projection failure must surface so the relay can report it rather than
// leaving the task silently unobservable.
func TestLocalObservationSinkSurfacesProjectionFailure(t *testing.T) {
	want := errors.New("projection failed")
	sink := localcontrol.NewLocalObservationSink(&recordingSink{}, &recordingProgress{err: want}, "task-1")
	if err := sink.Append(context.Background(), providerEvent("provider-1", "provider_tool_started")); !errors.Is(err, want) {
		t.Fatalf("err = %v, want the projection failure", err)
	}
}

// Without a task the sink is still a valid durable sink: the paired-device and
// standalone paths keep their existing behaviour.
func TestLocalObservationSinkWithoutTaskOnlyRecordsEvidence(t *testing.T) {
	inner := &recordingSink{}
	progress := &recordingProgress{}
	sink := localcontrol.NewLocalObservationSink(inner, progress, "")
	if err := sink.Append(context.Background(), providerEvent("provider-1", "provider_completed")); err != nil {
		t.Fatal(err)
	}
	if len(inner.appended) != 1 || len(progress.events) != 0 {
		t.Fatalf("inner=%d projected=%d", len(inner.appended), len(progress.events))
	}
}
