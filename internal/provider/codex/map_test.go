package codex

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/provider"
)

// Codex reports a transient stream failure it is about to retry itself as an
// error notification carrying willRetry. Treating that as the turn's failure
// kills a bee over a hiccup and reports a failure that never happened — one was
// watched dying to "Reconnecting... 2/5" with her work intact. The hiccup still
// belongs in the flight log, so it is reported as liveness, not as an error.
func TestRetryableProviderErrorKeepsTheBeeAlive(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		params string
		want   provider.EventType
	}{
		{"retrying", `{"threadId":"thread-1","error":{"message":"Reconnecting... 2/5","codexErrorInfo":{"responseStreamConnectionFailed":{}}},"willRetry":true}`, provider.EventHeartbeat},
		{"giving up", `{"threadId":"thread-1","error":{"message":"stream is gone","codexErrorInfo":{"responseStreamConnectionFailed":{}}},"willRetry":false}`, provider.EventError},
		{"auth outranks the retry", `{"threadId":"thread-1","error":{"message":"login required","codexErrorInfo":"unauthorized"},"willRetry":true}`, provider.EventAuthRequired},
		{"usage limit outranks the retry", `{"threadId":"thread-1","error":{"message":"slow down","codexErrorInfo":"usageLimitExceeded"},"willRetry":true}`, provider.EventRateLimited},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			event, ok := mapNotification(ServerMessage{Method: "error", Params: json.RawMessage(testCase.params)}, provider.MustID("task-1"), time.Unix(1, 0).UTC())
			if !ok {
				t.Fatal("notification was dropped")
			}
			if event.Type != testCase.want {
				t.Fatalf("type = %q, want %q", event.Type, testCase.want)
			}
			if event.Message == "" {
				t.Fatal("the keeper must still see what happened")
			}
		})
	}
}

// K7: a streaming delta is a fragment of an in-progress message, never the
// whole thing. Mapping it to EventAssistantMessage — the pre-fix behavior —
// turns one spoken sentence into as many "complete messages" as it has
// tokens, and events already sitting in the spool carry EventAssistantMessage
// to mean exactly one whole message, so the delta needs its own type instead
// of redefining that one.
func TestAgentMessageDeltaIsAFragmentNotAWholeMessage(t *testing.T) {
	event, ok := mapNotification(ServerMessage{Method: "item/agentMessage/delta", Params: json.RawMessage(`{"threadId":"thread-1","delta":"partial answer"}`)}, provider.MustID("task-1"), time.Unix(1, 0).UTC())
	if !ok {
		t.Fatal("delta notification was dropped")
	}
	if event.Type != provider.EventAssistantMessageDelta {
		t.Fatalf("type = %q, want %q", event.Type, provider.EventAssistantMessageDelta)
	}
	if event.Message != "partial answer" {
		t.Fatalf("message = %q, want the delta text", event.Message)
	}
}

// K8: a completed agentMessage item carries the assistant's full text (see
// the codex-app-server AgentMessageThreadItem schema: required id, text,
// type). Falling into the tool default branch — the pre-fix behavior — turns
// it into an EventToolEnded with an empty Tool name, because a message item
// has no Name field, and drops the text on the floor.
func TestCompletedAgentMessageItemEmitsTheWholeText(t *testing.T) {
	params := json.RawMessage(`{"threadId":"thread-1","item":{"id":"item-1","type":"agentMessage","text":"the whole answer"}}`)
	event, ok := mapNotification(ServerMessage{Method: "item/completed", Params: params}, provider.MustID("task-1"), time.Unix(1, 0).UTC())
	if !ok {
		t.Fatal("completed agentMessage notification was dropped")
	}
	if event.Type != provider.EventAssistantMessage {
		t.Fatalf("type = %q, want %q", event.Type, provider.EventAssistantMessage)
	}
	if event.Message != "the whole answer" {
		t.Fatalf("message = %q, want the item's full text", event.Message)
	}
	if event.Tool != "" {
		t.Fatalf("tool = %q, want empty: an assistant message is not a tool", event.Tool)
	}
}

// K8: item/started fires before an agentMessage item has any text, so there
// is nothing yet to report. The pre-fix behavior reported an EventToolStarted
// with an empty Tool name instead of staying silent.
func TestStartedAgentMessageItemEmitsNothing(t *testing.T) {
	params := json.RawMessage(`{"threadId":"thread-1","item":{"id":"item-1","type":"agentMessage"}}`)
	_, ok := mapNotification(ServerMessage{Method: "item/started", Params: params}, provider.MustID("task-1"), time.Unix(1, 0).UTC())
	if ok {
		t.Fatal("item/started for an agentMessage should not produce an event; it has no text yet")
	}
}

// K9: the assistant_message contract promises Codex emits every delta as the
// fragment type and exactly one whole message when the item completes. This
// exercises both halves together so a future change to one cannot silently
// violate the other.
func TestAgentMessageContractDeltaThenOneWholeMessage(t *testing.T) {
	now := time.Unix(1, 0).UTC()
	taskID := provider.MustID("task-1")
	for _, chunk := range []string{"Hel", "lo, ", "world"} {
		event, ok := mapNotification(ServerMessage{Method: "item/agentMessage/delta", Params: json.RawMessage(`{"threadId":"thread-1","delta":"` + chunk + `"}`)}, taskID, now)
		if !ok || event.Type != provider.EventAssistantMessageDelta {
			t.Fatalf("delta event = %#v, ok = %v", event, ok)
		}
	}
	completed, ok := mapNotification(ServerMessage{Method: "item/completed", Params: json.RawMessage(`{"threadId":"thread-1","item":{"id":"item-1","type":"agentMessage","text":"Hello, world"}}`)}, taskID, now)
	if !ok {
		t.Fatal("completed notification was dropped")
	}
	if completed.Type != provider.EventAssistantMessage || completed.Message != "Hello, world" {
		t.Fatalf("completed event = %#v", completed)
	}
}

// Codex reports what each turn cost, per turn. The notification was not in the
// mapping switch at all, so EventUsage was a declared type the engine never
// emitted: a keeper could see an allowance nearly gone and never learn which bee
// spent it.
func TestThreadTokenUsageIsReportedPerTurn(t *testing.T) {
	params := []byte(`{
		"threadId": "thread-1",
		"turnId": "turn-7",
		"tokenUsage": {
			"inputTokens": 1200,
			"cachedInputTokens": 800,
			"outputTokens": 340,
			"reasoningOutputTokens": 120,
			"totalTokens": 1660
		}
	}`)
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)

	event, ok := mapNotification(ServerMessage{Method: "thread/tokenUsage/updated", Params: params}, provider.MustID("task-1"), now)
	if !ok {
		t.Fatal("a token report must not be dropped")
	}
	if event.Type != provider.EventUsage {
		t.Fatalf("type = %q, want usage", event.Type)
	}
	if event.Usage == nil || event.Usage.Tokens == nil {
		t.Fatalf("the tokens themselves must travel: %+v", event.Usage)
	}
	if event.Usage.TurnID != "turn-7" {
		t.Fatalf("a cost has to be attributable to a turn, got %q", event.Usage.TurnID)
	}
	tokens := event.Usage.Tokens
	if tokens.Input != 1200 || tokens.CachedInput != 800 || tokens.Output != 340 ||
		tokens.ReasoningOutput != 120 || tokens.Total != 1660 {
		t.Fatalf("token counts must be reported exactly: %+v", tokens)
	}
	if event.Usage.Provider == "" {
		t.Fatal("the provider that reported the cost must travel with it")
	}
}

func TestAMalformedTokenReportIsDroppedRatherThanGuessed(t *testing.T) {
	if _, ok := mapNotification(ServerMessage{
		Method: "thread/tokenUsage/updated",
		Params: []byte(`{"tokenUsage": "not an object"}`),
	}, provider.MustID("task-1"), time.Now()); ok {
		t.Fatal("an unreadable token report must not become an event with invented zeros")
	}
}
