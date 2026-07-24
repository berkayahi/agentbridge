package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
			eventID := durableProviderEventID(executionID, value)
			_ = sink.Append(ctx, kernel.Event{ID: eventID, ExecutionID: executionID, Type: kernel.EventType("provider_" + string(value.Type)), Visibility: "user", ProviderEventID: eventID, Payload: payload, CreatedAt: value.CreatedAt})
		}
	}
}

func durableProviderEventID(executionID string, value provider.Event) string {
	if value.ID.Valid() {
		return value.ID.String()
	}
	payload, err := json.Marshal(value)
	if err != nil {
		payload = []byte(string(value.Type) + "\x00" + value.Message + "\x00" + value.Tool + "\x00" + value.Path)
	}
	digestInput := make([]byte, 0, len(executionID)+1+len(payload))
	digestInput = append(digestInput, executionID...)
	digestInput = append(digestInput, 0)
	digestInput = append(digestInput, payload...)
	digest := sha256.Sum256(digestInput)
	return "provider-" + hex.EncodeToString(digest[:16])
}
