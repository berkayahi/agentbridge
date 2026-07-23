package runtime

import (
	"context"
	"encoding/json"

	"github.com/berkayahi/agentbridge/internal/kernel"
	"github.com/berkayahi/agentbridge/internal/provider"
)

func ProviderInput(input kernel.Input) provider.Input { return provider.Input{Text: input.Text} }

func ProviderSession(value Session) (provider.Session, bool) {
	native, ok := value.Native.(provider.Session)
	return native, ok
}

func RuntimeSession(value provider.Session, runtimeID string) Session {
	return Session{ID: value.ID.String(), TaskID: value.TaskID.String(), ExternalID: value.ExternalID, ThreadID: value.ThreadID, RuntimeID: runtimeID, Native: value}
}

// RelayProviderEvents turns provider presentation events into durable kernel
// events. A critical event is never acknowledged by this bridge itself.
func RelayProviderEvents(ctx context.Context, executionID string, source <-chan provider.Event, sink kernel.EventSink) {
	for {
		select {
		case <-ctx.Done():
			return
		case value, ok := <-source:
			if !ok {
				return
			}
			payload, err := json.Marshal(value)
			if err != nil || sink == nil {
				return
			}
			_ = sink.Append(ctx, kernel.Event{ID: value.ID.String(), ExecutionID: executionID, Type: kernel.EventType("provider_" + string(value.Type)), Visibility: "user", ProviderEventID: value.ID.String(), Payload: payload, CreatedAt: value.CreatedAt})
		}
	}
}
