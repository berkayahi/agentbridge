package runtime

import (
	"context"
	"encoding/json"
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

// Fragments are batched and nothing else is. A fragment is a live preview of what
// she is saying — Codex's own schema warns a completed item may not match the
// concatenation of its deltas — while an approval or a commit is something a
// keeper acts on and a crash must not lose.
func TestOnlyStreamingFragmentsAreBatched(t *testing.T) {
	source := make(chan provider.Event, 16)
	sink := new(recordingEventSink)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- RelayProviderEvents(ctx, "execution-1", source, sink) }()

	for _, fragment := range []string{"The build ", "identity is ", "reconciled."} {
		source <- provider.Event{Type: provider.EventAssistantMessageDelta, Message: fragment}
	}
	// An approval ends the fragment it interrupted, so the order a keeper reads
	// is the order things happened.
	source <- provider.Event{Type: provider.EventApprovalRequired, Message: "May I write outside the worktree?"}
	close(source)

	if err := <-done; err != nil {
		t.Fatalf("relay: %v", err)
	}

	events := sink.events
	if len(events) != 2 {
		t.Fatalf("three fragments and one approval must be two durable events, got %d: %+v", len(events), events)
	}
	if got := events[0].Type; got != kernel.EventType("provider_assistant_message_delta") {
		t.Fatalf("first event type = %q", got)
	}
	var first struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(events[0].Payload, &first); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if first.Message != "The build identity is reconciled." {
		t.Fatalf("the batch must carry the joined text, got %q", first.Message)
	}
	if events[1].Type != kernel.EventType("provider_approval_required") {
		t.Fatalf("the approval must be its own durable event, got %q", events[1].Type)
	}
}

func TestAFragmentIsNotLostWhenTheProviderStops(t *testing.T) {
	source := make(chan provider.Event, 4)
	sink := new(recordingEventSink)
	done := make(chan error, 1)
	go func() { done <- RelayProviderEvents(context.Background(), "execution-1", source, sink) }()

	source <- provider.Event{Type: provider.EventAssistantMessageDelta, Message: "half a thought"}
	close(source)
	if err := <-done; err != nil {
		t.Fatalf("relay: %v", err)
	}
	if len(sink.events) != 1 {
		t.Fatalf("a fragment already spoken is still worth having: %+v", sink.events)
	}
}
