package localcontrol

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/berkayahi/agentbridge/internal/kernel"
)

// LocalProviderEventAuthority appends a provider observation to a local task's
// event log exactly once. It is an optional store capability: an authority
// without it cannot make locally executed work observable.
type LocalProviderEventAuthority interface {
	AppendLocalProviderEvent(ctx context.Context, taskID, eventID, eventType string, payload []byte, createdAt time.Time) (Event, error)
}

// ProjectProviderEvent records a provider observation on the local task so
// GET /v1/tasks/{id}/events reflects the work in flight. Streaming observations
// carry no state transition and take no command lock: they are evidence being
// made visible, not a decision. The local event type deliberately mirrors the
// paired-device carrier so one client renderer serves both paths.
func (s *Service) ProjectProviderEvent(ctx context.Context, taskID string, event kernel.Event) error {
	if s == nil || s.store == nil {
		return ErrNotConfigured
	}
	authority, ok := s.store.(LocalProviderEventAuthority)
	if !ok {
		return ErrNotConfigured
	}
	taskID = strings.TrimSpace(taskID)
	eventID := strings.TrimSpace(event.ID)
	if taskID == "" || eventID == "" {
		return ErrInvalidRequest
	}
	providerType := strings.TrimPrefix(string(event.Type), "provider_")
	payload, err := json.Marshal(map[string]any{
		"event_id":   eventID,
		"event_type": providerType,
		"payload":    json.RawMessage(providerEventPayload(event.Payload)),
	})
	if err != nil {
		return err
	}
	created := event.CreatedAt
	if created.IsZero() {
		created = s.clock().UTC()
	}
	_, err = authority.AppendLocalProviderEvent(ctx, taskID, eventID, "provider_event", payload, created.UTC())
	return err
}

func providerEventPayload(payload []byte) []byte {
	if len(payload) == 0 || !json.Valid(payload) {
		return []byte("{}")
	}
	return payload
}

// LocalProgress projects a provider event onto a local task so the authority's
// event log reflects what the runtime is actually doing.
type LocalProgress interface {
	ProjectProviderEvent(ctx context.Context, taskID string, event kernel.Event) error
}

// LocalObservationSink makes a locally executed task observable. Provider
// events are durable evidence in the execution journal; the local task log is a
// projection of that evidence. The ordering here is the contract: evidence
// commits first and only then is anything projected, so the authority can never
// show progress the journal does not record.
//
// This is the local-target counterpart of the paired-device observation path,
// which ingests the same provider events after they cross the signed link.
type LocalObservationSink struct {
	inner    kernel.EventSink
	progress LocalProgress
	taskID   string
}

func NewLocalObservationSink(inner kernel.EventSink, progress LocalProgress, taskID string) *LocalObservationSink {
	return &LocalObservationSink{inner: inner, progress: progress, taskID: taskID}
}

func (s *LocalObservationSink) Append(ctx context.Context, event kernel.Event) error {
	if s == nil {
		return nil
	}
	if s.inner != nil {
		if err := s.inner.Append(ctx, event); err != nil {
			return err
		}
	}
	if s.progress == nil || s.taskID == "" {
		return nil
	}
	return s.progress.ProjectProviderEvent(ctx, s.taskID, event)
}
