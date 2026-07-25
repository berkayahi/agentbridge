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
