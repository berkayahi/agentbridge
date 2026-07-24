package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/kernel"
	"github.com/berkayahi/agentbridge/internal/provider"
)

type recordingEventSink struct {
	events []kernel.Event
}

func (s *recordingEventSink) Append(_ context.Context, value kernel.Event) error {
	s.events = append(s.events, value)
	return nil
}

func TestRelayProviderEventsAssignsStableIDWhenProviderOmitsOne(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	source := make(chan provider.Event, 1)
	source <- provider.Event{Type: provider.EventToolStarted, TaskID: provider.MustID("task-1"), Tool: "shell", CreatedAt: now}
	close(source)
	sink := new(recordingEventSink)

	RelayProviderEvents(context.Background(), "execution-1", source, sink)
	if len(sink.events) != 1 {
		t.Fatalf("durable events = %d, want 1", len(sink.events))
	}
	first := sink.events[0]
	if first.ID == "" || first.ProviderEventID != first.ID {
		t.Fatalf("fallback event identity = id=%q provider_id=%q", first.ID, first.ProviderEventID)
	}
	if first.ID != durableProviderEventID("execution-1", provider.Event{Type: provider.EventToolStarted, TaskID: provider.MustID("task-1"), Tool: "shell", CreatedAt: now}) {
		t.Fatalf("fallback event ID = %q, want deterministic ID", first.ID)
	}
}

// A durable failure must be reported, not discarded: the event log is what the
// hive shows a keeper, so a relay that stops silently leaves a bee looking
// frozen with nobody told why.
func TestRelayReportsAPersistentDurableFailure(t *testing.T) {
	source := make(chan provider.Event, 8)
	for i := 0; i < relayFailureLimit; i++ {
		source <- provider.Event{Type: provider.EventAssistantMessage, Message: "working"}
	}
	close(source)
	failing := &failingSink{err: errors.New("disk full")}
	if err := RelayProviderEvents(context.Background(), "execution-1", source, failing); err == nil {
		t.Fatal("a persistent durable failure must be reported")
	}
}

// One transient failure must not end a flight's observability.
func TestRelaySurvivesATransientDurableFailure(t *testing.T) {
	source := make(chan provider.Event, 4)
	source <- provider.Event{Type: provider.EventAssistantMessage, Message: "one"}
	source <- provider.Event{Type: provider.EventAssistantMessage, Message: "two"}
	close(source)
	flaky := &failingSink{err: errors.New("transient"), failFirst: 1}
	if err := RelayProviderEvents(context.Background(), "execution-1", source, flaky); err != nil {
		t.Fatalf("err = %v, want the relay to carry on", err)
	}
	if flaky.appended != 1 {
		t.Fatalf("appended = %d, want the second event to land", flaky.appended)
	}
}

// The relay returns when the provider closes her channel, so a caller can
// release the session instead of parking a goroutine for the whole process.
func TestRelayReturnsWhenTheProviderCloses(t *testing.T) {
	source := make(chan provider.Event)
	close(source)
	if err := RelayProviderEvents(context.Background(), "execution-1", source, &failingSink{}); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestRelayRefusesWithoutASink(t *testing.T) {
	source := make(chan provider.Event)
	close(source)
	if err := RelayProviderEvents(context.Background(), "execution-1", source, nil); err == nil {
		t.Fatal("a relay without a durable sink must be refused")
	}
}

type failingSink struct {
	err       error
	failFirst int
	calls     int
	appended  int
}

func (s *failingSink) Append(context.Context, kernel.Event) error {
	s.calls++
	if s.err != nil && (s.failFirst == 0 || s.calls <= s.failFirst) {
		return s.err
	}
	s.appended++
	return nil
}
