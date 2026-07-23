package kernel

import (
	"context"

	"github.com/berkayahi/agentbridge/internal/events"
	"github.com/berkayahi/agentbridge/internal/store"
)

// EventSink acknowledges only after the event is durably committed.
type EventSink interface {
	Append(context.Context, Event) error
}

type DurableEventSink struct{ work store.UnitOfWork }

func NewDurableEventSink(work store.UnitOfWork) DurableEventSink { return DurableEventSink{work: work} }

func (s DurableEventSink) Append(ctx context.Context, value Event) error {
	return s.work.Within(ctx, func(repos store.Repositories) error {
		return repos.Events.Append(ctx, events.Record{ID: value.ID, ExecutionID: value.ExecutionID, Type: string(value.Type), Visibility: value.Visibility, ProviderEventID: value.ProviderEventID, Payload: append([]byte(nil), value.Payload...), CreatedAt: value.CreatedAt})
	})
}
