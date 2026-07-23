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
	EventAssistantMessage EventType = "assistant_message"
	EventCommandStarted   EventType = "command_started"
	EventCommandEnded     EventType = "command_ended"
	EventFileStarted      EventType = "file_started"
	EventFileEnded        EventType = "file_ended"
	EventToolStarted      EventType = "tool_started"
	EventToolEnded        EventType = "tool_ended"
	EventApprovalRequired EventType = "approval_required"
	EventApprovalExpired  EventType = "approval_expired"
	EventAuthRequired     EventType = "auth_required"
	EventRateLimited      EventType = "rate_limited"
	EventUsage            EventType = "usage"
	EventHeartbeat        EventType = "heartbeat"
	EventError            EventType = "error"
	EventCompleted        EventType = "completed"
)

// Event contains observable provider output only. Hidden reasoning is neither
// requested from providers nor represented by this contract.
type Event struct {
	ID        ID
	TaskID    ID
	RequestID ID
	Type      EventType
	Message   string
	Tool      string
	Path      string
	ExitCode  *int
	Usage     *Usage
	ResetAt   *time.Time
	Lane      spool.Lane
	CreatedAt time.Time
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
