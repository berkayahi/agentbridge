package provider

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/berkayahi/agentbridge/internal/spool"
)

type EventType string

const (
	// EventAssistantMessage carries one complete assistant message: the full
	// text a consumer can display or store as-is. A provider that streams
	// tokens must assemble them into this type on completion; it must never
	// map an individual token or chunk to EventAssistantMessage, because
	// events already stored in the spool carry this type meaning "whole
	// message" and consumers rely on that meaning to render or replay history.
	EventAssistantMessage EventType = "assistant_message"
	// EventAssistantMessageDelta carries one fragment of an in-progress
	// assistant message — a partial token or chunk, not yet the full text.
	// Consumers that want the running text must concatenate deltas
	// themselves; the durable record of what was actually said is the single
	// EventAssistantMessage emitted when the message completes, not the sum
	// of its deltas. A provider with no streaming notion (one message per
	// turn) simply never emits this type.
	EventAssistantMessageDelta EventType = "assistant_message_delta"
	EventCommandStarted        EventType = "command_started"
	EventCommandEnded          EventType = "command_ended"
	EventFileStarted           EventType = "file_started"
	EventFileEnded             EventType = "file_ended"
	EventToolStarted           EventType = "tool_started"
	EventToolEnded             EventType = "tool_ended"
	EventApprovalRequired      EventType = "approval_required"
	EventApprovalExpired       EventType = "approval_expired"
	EventAuthRequired          EventType = "auth_required"
	EventRateLimited           EventType = "rate_limited"
	EventUsage                 EventType = "usage"
	EventHeartbeat             EventType = "heartbeat"
	EventError                 EventType = "error"
	EventCompleted             EventType = "completed"
)

// Event contains observable provider output only. Hidden reasoning is neither
// requested from providers nor represented by this contract.
// The field names are tagged. They were untagged for a long time, which put Go
// field names on the wire — Message, Tool, Path — and the only reader is Kovan's
// surface. That reader now accepts either casing, so new events carry proper
// names while events already in the spool keep theirs. The history is mixed and
// that is simply true: rewriting durable evidence to make a casing uniform would
// trade the one thing the hive treats as sacred for tidiness.
type Event struct {
	ID        ID         `json:"id"`
	TaskID    ID         `json:"task_id"`
	RequestID ID         `json:"request_id,omitempty"`
	Type      EventType  `json:"type"`
	Message   string     `json:"message,omitempty"`
	Tool      string     `json:"tool,omitempty"`
	Path      string     `json:"path,omitempty"`
	ExitCode  *int       `json:"exit_code,omitempty"`
	Usage     *Usage     `json:"usage,omitempty"`
	ResetAt   *time.Time `json:"reset_at,omitempty"`
	Lane      spool.Lane `json:"lane,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// SpoolSink is the provider-facing durable event boundary. Implementations
// must return only after the event is committed to the local spool.
type SpoolSink interface {
	Append(context.Context, spool.Event) (spool.AppendResult, error)
}

// SpoolEvent converts observable provider output into the transport-neutral
// event shape. The event payload is the provider envelope, never hidden model
// reasoning or local credentials.
func (e Event) SpoolEvent(executionID string) (spool.Event, error) {
	payload, err := json.Marshal(e)
	if err != nil {
		return spool.Event{}, err
	}
	lane := e.Lane
	if !lane.Valid() {
		lane = providerLane(e.Type)
	}
	created := e.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	return spool.Event{ExecutionID: executionID, Lane: lane, Type: "provider_" + string(e.Type), ProviderEventID: e.ID.String(), Payload: payload, CreatedAt: created}, nil
}

// PersistEvent provides a small adapter for provider consumers that still
// receive the native channel contract. Callers can safely acknowledge the
// provider event only after this function returns nil.
func PersistEvent(ctx context.Context, sink SpoolSink, executionID string, event Event) (spool.AppendResult, error) {
	if sink == nil {
		return spool.AppendResult{}, errors.New("provider: durable event sink is nil")
	}
	value, err := event.SpoolEvent(executionID)
	if err != nil {
		return spool.AppendResult{}, err
	}
	return sink.Append(ctx, value)
}

func providerLane(kind EventType) spool.Lane {
	switch kind {
	case EventApprovalRequired, EventApprovalExpired, EventAuthRequired, EventRateLimited, EventError, EventCompleted, EventCommandEnded:
		return spool.LaneCritical
	case EventHeartbeat:
		return spool.LaneDiagnostic
	default:
		return spool.LaneStructured
	}
}
