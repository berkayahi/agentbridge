package runtime

import (
	"context"
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
