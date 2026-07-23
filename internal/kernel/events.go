package kernel

import (
	"context"
	"errors"
	"time"

	"github.com/berkayahi/agentbridge/internal/spool"
)

type EventType string

const (
	EventIntentAccepted       EventType = "intent_accepted"
	EventIntentCompleted      EventType = "intent_completed"
	EventReconciliationNeeded EventType = "reconciliation_required"
	EventCancellationFenced   EventType = "cancellation_fenced"
)

type Event struct {
	ID              string
	ExecutionID     string
	Type            EventType
	Visibility      string
	ProviderEventID string
	Lane            spool.Lane
	Payload         []byte
	CreatedAt       time.Time
}

// SpoolAppender is the durable backend used at the kernel event boundary.
type SpoolAppender interface {
	Append(context.Context, spool.Event) (spool.AppendResult, error)
}

// SpoolEventSink adapts the spool's result-bearing append contract to the
// kernel EventSink interface without acknowledging before SQLite commits.
type SpoolEventSink struct{ sink SpoolAppender }

func NewSpoolEventSink(sink SpoolAppender) SpoolEventSink {
	return SpoolEventSink{sink: sink}
}

func (s SpoolEventSink) Append(ctx context.Context, value Event) error {
	if s.sink == nil {
		return errors.New("kernel: durable event sink is nil")
	}
	event := spool.Event{
		ExecutionID:     value.ExecutionID,
		Lane:            value.Lane,
		Type:            string(value.Type),
		ProviderEventID: value.ProviderEventID,
		Payload:         append([]byte(nil), value.Payload...),
		CreatedAt:       value.CreatedAt,
	}
	if event.ProviderEventID == "" {
		event.ProviderEventID = value.ID
	}
	if !event.Lane.Valid() {
		event.Lane = spool.LaneForType(event.Type)
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	_, err := s.sink.Append(ctx, event)
	return err
}
