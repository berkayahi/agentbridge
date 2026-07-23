package kernel

import "time"

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
	Payload         []byte
	CreatedAt       time.Time
}
