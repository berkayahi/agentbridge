package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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

func TestStatuslineRejectsOversizedOrTrailingInput(t *testing.T) {
	caller := statusCallerFunc(func(context.Context, controlsocket.Request, any) error { return nil })
	for _, input := range []string{
		`{"session_id":"s"} {"second":true}`,
		`{"session_id":"` + strings.Repeat("x", maxStatuslineBytes) + `"}`,
	} {
		if err := CaptureStatusline(context.Background(), strings.NewReader(input), caller, StatuslineScope{}, time.Now); err == nil {
			t.Fatalf("invalid input accepted")
		}
	}
}

func TestUsageCacheSurvivesRestartWithoutPersistingSessionIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude-usage.json")
	cache := NewUsageCache(path)
	cache.Update(UsageSnapshot{
		SessionID:  "private-session",
		ObservedAt: time.Now().UTC(),
		FiveHour:   &UsageWindow{UsedPercent: 42, ResetsAt: time.Now().Add(time.Hour).UTC()},
	})

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "private-session") {
		t.Fatal("the persisted allowance disclosed a provider session identifier")
	}
	restarted := NewUsageCache(path)
	usage, err := restarted.ProviderUsage()
	if err != nil || len(usage.Windows) != 1 || usage.Windows[0].UsedPercent != 42 {
		t.Fatalf("restored usage = %#v, err = %v", usage, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("persisted mode = %o", info.Mode().Perm())
	}
}

type statusCallerFunc func(context.Context, controlsocket.Request, any) error

func (f statusCallerFunc) Call(ctx context.Context, request controlsocket.Request, result any) error {
	return f(ctx, request, result)
}
