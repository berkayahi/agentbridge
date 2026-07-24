package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

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

// relayFailureLimit bounds how many consecutive durable failures the relay
// tolerates before it gives up. One transient error must not end a flight's
// observability; a persistent one must not be swallowed forever.
const relayFailureLimit = 5

// RelayProviderEvents turns provider presentation events into durable kernel
// events. A critical event is never acknowledged by this bridge itself.
//
// It returns when the provider closes its channel or the context ends, so a
// caller can release the session instead of leaving a goroutine parked for the
// life of the process. A durable failure is returned rather than discarded:
// the event log is what the hive shows a keeper, so silently stopping would
// leave a bee looking frozen with nobody told why.
func RelayProviderEvents(ctx context.Context, executionID string, source <-chan provider.Event, sink kernel.EventSink) error {
	if sink == nil {
		return errors.New("relay provider events: no durable sink")
	}
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case value, ok := <-source:
			if !ok {
				return nil
			}
			payload, err := json.Marshal(value)
			if err != nil {
				// One unencodable event must not end the relay: skip it and
				// keep the rest of the flight observable.
				failures++
				if failures >= relayFailureLimit {
					return fmt.Errorf("relay provider events: %w", err)
				}
				continue
			}
			eventID := durableProviderEventID(executionID, value)
			if err := sink.Append(ctx, kernel.Event{
				ID: eventID, ExecutionID: executionID, Type: kernel.EventType("provider_" + string(value.Type)),
				Visibility: "user", ProviderEventID: eventID, Payload: payload, CreatedAt: value.CreatedAt,
			}); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				failures++
				if failures >= relayFailureLimit {
					return fmt.Errorf("relay provider events: %w", err)
				}
				continue
			}
			failures = 0
		}
	}
}

// RelayProviderEventsLogged runs the relay and reports a failure once at the
// process edge. Adapters use this so a lost event stream is visible in the
// journal instead of vanishing inside a bare goroutine.
func RelayProviderEventsLogged(ctx context.Context, executionID string, source <-chan provider.Event, sink kernel.EventSink, done func()) {
	if done != nil {
		defer done()
	}
	if err := RelayProviderEvents(ctx, executionID, source, sink); err != nil && !errors.Is(err, context.Canceled) {
		slog.Default().Warn("provider event relay stopped", "execution_id", executionID, "error_type", fmt.Sprintf("%T", err))
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
