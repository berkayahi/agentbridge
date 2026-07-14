package claude

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/berkayahi/agentbridge/internal/controlsocket"
	"github.com/berkayahi/agentbridge/internal/provider"
	"github.com/berkayahi/agentbridge/internal/task"
)

const maxStatuslineBytes = 64 * 1024

type UsageWindow struct {
	UsedPercent float64   `json:"used_percent"`
	ResetsAt    time.Time `json:"resets_at"`
}

type UsageSnapshot struct {
	SessionID  string       `json:"session_id"`
	FiveHour   *UsageWindow `json:"five_hour,omitempty"`
	SevenDay   *UsageWindow `json:"seven_day,omitempty"`
	ObservedAt time.Time    `json:"observed_at"`
}

type UsageCache struct {
	mu     sync.RWMutex
	latest UsageSnapshot
}

func NewUsageCache() *UsageCache                    { return &UsageCache{} }
func (c *UsageCache) Update(snapshot UsageSnapshot) { c.mu.Lock(); c.latest = snapshot; c.mu.Unlock() }
func (c *UsageCache) Snapshot() UsageSnapshot       { c.mu.RLock(); defer c.mu.RUnlock(); return c.latest }
func (c *UsageCache) ProviderUsage() (provider.Usage, error) {
	snapshot := c.Snapshot()
	if snapshot.ObservedAt.IsZero() {
		return provider.Usage{}, ErrUsageUnavailable
	}
	usage := provider.Usage{Provider: task.ProviderClaude, ObservedAt: snapshot.ObservedAt.UTC()}
	if snapshot.FiveHour != nil {
		usage.Windows = append(usage.Windows, provider.UsageWindow{Name: "five_hour", UsedPercent: snapshot.FiveHour.UsedPercent, ResetsAt: snapshot.FiveHour.ResetsAt.UTC()})
	}
	if snapshot.SevenDay != nil {
		usage.Windows = append(usage.Windows, provider.UsageWindow{Name: "seven_day", UsedPercent: snapshot.SevenDay.UsedPercent, ResetsAt: snapshot.SevenDay.ResetsAt.UTC()})
	}
	return usage, nil
}

type StatuslineScope struct {
	TaskID     string
	Provider   string
	Capability []byte
}

type StatuslineCaller interface {
	Call(context.Context, controlsocket.Request, any) error
}

func CaptureStatusline(ctx context.Context, reader io.Reader, caller StatuslineCaller, scope StatuslineScope, now func() time.Time) error {
	if now == nil {
		now = time.Now
	}
	decoder := json.NewDecoder(io.LimitReader(reader, maxStatuslineBytes+1))
	var input struct {
		SessionID  string `json:"session_id"`
		RateLimits struct {
			FiveHour statusWindow `json:"five_hour"`
			SevenDay statusWindow `json:"seven_day"`
		} `json:"rate_limits"`
	}
	if err := decoder.Decode(&input); err != nil {
		return err
	}
	snapshot := UsageSnapshot{SessionID: input.SessionID, ObservedAt: now().UTC(), FiveHour: input.RateLimits.FiveHour.window(), SevenDay: input.RateLimits.SevenDay.window()}
	params, _ := json.Marshal(snapshot)
	request := controlsocket.Request{TaskID: scope.TaskID, Provider: scope.Provider, Capability: append([]byte(nil), scope.Capability...), Tool: "claude_statusline", Params: params}
	return caller.Call(ctx, request, nil)
}

type statusWindow struct {
	UsedPercentage float64 `json:"used_percentage"`
	ResetsAt       string  `json:"resets_at"`
}

func (w statusWindow) window() *UsageWindow {
	if w.UsedPercentage == 0 && w.ResetsAt == "" {
		return nil
	}
	reset, err := time.Parse(time.RFC3339, w.ResetsAt)
	if err != nil {
		return &UsageWindow{UsedPercent: w.UsedPercentage}
	}
	return &UsageWindow{UsedPercent: w.UsedPercentage, ResetsAt: reset.UTC()}
}
