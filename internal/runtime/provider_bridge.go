package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

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

// deltaFlushInterval and deltaFlushBytes bound how long a streaming fragment can
// sit unwritten and how much can accumulate before it is.
//
// Only fragments are batched. A completed assistant message is authoritative —
// Codex's own schema says a completed item may not match the concatenation of
// its deltas — so the fragments are a live preview of what she is saying, not
// the record of what she said. Every other event, including every approval and
// every commit, keeps its own durable append: those are the ones a keeper acts
// on and the ones a crash must not lose.
//
// Without this, one chatty bee wrote a durable row per token: thousands of
// appends for a single turn, which is also why an event page of 200 could be
// nothing but half-words.
const (
	deltaFlushInterval = 400 * time.Millisecond
	deltaFlushBytes    = 4096
)

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
	// The fragment currently being accumulated, if any.
	var pending *provider.Event
	var pendingText strings.Builder
	flushTimer := time.NewTimer(deltaFlushInterval)
	defer flushTimer.Stop()
	stopTimer := func() {
		if !flushTimer.Stop() {
			select {
			case <-flushTimer.C:
			default:
			}
		}
	}
	stopTimer()

	// appendEvent is the one durable write path, so a batched fragment and an
	// ordinary event cannot drift apart in how they are recorded.
	appendEvent := func(value provider.Event) error {
		payload, err := json.Marshal(value)
		if err != nil {
			failures++
			if failures >= relayFailureLimit {
				return fmt.Errorf("relay provider events: %w", err)
			}
			return nil
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
			return nil
		}
		failures = 0
		return nil
	}

	flushPending := func() error {
		if pending == nil {
			return nil
		}
		batched := *pending
		batched.Message = pendingText.String()
		pending = nil
		pendingText.Reset()
		stopTimer()
		return appendEvent(batched)
	}

	for {
		select {
		case <-ctx.Done():
			// A fragment already spoken is still worth having, so it is written
			// before the relay lets go.
			_ = flushPending()
			return ctx.Err()
		case <-flushTimer.C:
			if err := flushPending(); err != nil {
				return err
			}
		case value, ok := <-source:
			if !ok {
				if err := flushPending(); err != nil {
					return err
				}
				return nil
			}
			if value.Type == provider.EventAssistantMessageDelta {
				if pending == nil {
					held := value
					pending = &held
					pendingText.Reset()
					stopTimer()
					flushTimer.Reset(deltaFlushInterval)
				}
				pendingText.WriteString(value.Message)
				if pendingText.Len() >= deltaFlushBytes {
					if err := flushPending(); err != nil {
						return err
					}
				}
				continue
			}
			// Anything that is not a fragment ends the fragment it interrupted,
			// so the order a keeper reads is the order things happened.
			if err := flushPending(); err != nil {
				return err
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
			_ = payload
			if err := appendEvent(value); err != nil {
				return err
			}
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
