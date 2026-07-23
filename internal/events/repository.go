package events

import (
	"context"
	"time"
)

// Record is the durable, transport-neutral execution event envelope.
type Record struct {
	ID              string
	ExecutionID     string
	Type            string
	Visibility      string
	ProviderEventID string
	Payload         []byte
	CreatedAt       time.Time
}

// Repository persists events before presenting them to live subscribers.
type Repository interface {
	Append(context.Context, Record) error
	List(context.Context, string) ([]Record, error)
}
