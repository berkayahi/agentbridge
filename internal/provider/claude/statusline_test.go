package claude

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/controlsocket"
)

func TestStatuslineExtractsOnlySafeUsageSubset(t *testing.T) {
	input := `{"session_id":"session-1","model":{"display_name":"Claude"},"rate_limits":{"five_hour":{"used_percentage":12,"resets_at":"2026-07-14T12:00:00Z"},"seven_day":{"used_percentage":34,"resets_at":"2026-07-20T12:00:00Z"}},"transcript_path":"/secret/path","api_key":"must-not-pass"}`
	var got controlsocket.Request
	caller := statusCallerFunc(func(_ context.Context, request controlsocket.Request, result any) error {
		got = request
		return nil
	})
	err := CaptureStatusline(context.Background(), strings.NewReader(input), caller, StatuslineScope{TaskID: "task-1", Provider: "claude", Capability: []byte("cap")}, func() time.Time { return time.Unix(1, 0).UTC() })
	if err != nil {
		t.Fatal(err)
	}
	if got.Tool != "claude_statusline" || strings.Contains(string(got.Params), "secret") || strings.Contains(string(got.Params), "api_key") {
		t.Fatalf("request = %#v", got)
	}
	var snapshot UsageSnapshot
	if err := json.Unmarshal(got.Params, &snapshot); err != nil || snapshot.FiveHour == nil || snapshot.SevenDay == nil {
		t.Fatalf("snapshot = %#v, err = %v", snapshot, err)
	}
}

type statusCallerFunc func(context.Context, controlsocket.Request, any) error

func (f statusCallerFunc) Call(ctx context.Context, request controlsocket.Request, result any) error {
	return f(ctx, request, result)
}
