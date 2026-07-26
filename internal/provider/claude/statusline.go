package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/berkayahi/agentbridge/internal/controlsocket"
	"github.com/berkayahi/agentbridge/internal/provider"
	"github.com/berkayahi/agentbridge/internal/workmodel"
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
	path   string
}

// NewUsageCache restores the last safe status-line observation when a durable
// path is supplied. The snapshot contains allowance windows only; credentials,
// transcript paths, and session identifiers are never written.
func NewUsageCache(paths ...string) *UsageCache {
	cache := &UsageCache{}
	if len(paths) != 1 || !filepath.IsAbs(paths[0]) {
		return cache
	}
	cache.path = filepath.Clean(paths[0])
	info, err := os.Lstat(cache.path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxStatuslineBytes {
		return cache
	}
	body, err := os.ReadFile(cache.path)
	if err != nil {
		return cache
	}
	var snapshot UsageSnapshot
	if json.Unmarshal(body, &snapshot) == nil && durableSnapshot(snapshot, time.Now().UTC()) {
		cache.latest = snapshot
	}
	return cache
}

func (c *UsageCache) Update(snapshot UsageSnapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.latest = snapshot
	c.persist(snapshot)
}
func (c *UsageCache) Snapshot() UsageSnapshot { c.mu.RLock(); defer c.mu.RUnlock(); return c.latest }
func (c *UsageCache) ProviderUsage() (provider.Usage, error) {
	snapshot := c.Snapshot()
	if snapshot.ObservedAt.IsZero() {
		return provider.Usage{}, ErrUsageUnavailable
	}
	usage := provider.Usage{Provider: workmodel.ClaudeSubscription, ObservedAt: snapshot.ObservedAt.UTC()}
	if snapshot.FiveHour != nil {
		usage.Windows = append(usage.Windows, provider.UsageWindow{Name: "five_hour", UsedPercent: snapshot.FiveHour.UsedPercent, ResetsAt: snapshot.FiveHour.ResetsAt.UTC()})
	}
	if snapshot.SevenDay != nil {
		usage.Windows = append(usage.Windows, provider.UsageWindow{Name: "seven_day", UsedPercent: snapshot.SevenDay.UsedPercent, ResetsAt: snapshot.SevenDay.ResetsAt.UTC()})
	}
	return usage, nil
}

func durableSnapshot(snapshot UsageSnapshot, now time.Time) bool {
	if snapshot.ObservedAt.IsZero() || snapshot.ObservedAt.After(now.Add(5*time.Minute)) ||
		now.Sub(snapshot.ObservedAt) > 8*24*time.Hour {
		return false
	}
	return snapshot.FiveHour != nil || snapshot.SevenDay != nil
}

func (c *UsageCache) persist(snapshot UsageSnapshot) {
	if c.path == "" || !durableSnapshot(snapshot, time.Now().UTC()) {
		return
	}
	snapshot.SessionID = ""
	body, err := json.Marshal(snapshot)
	if err != nil || len(body) > maxStatuslineBytes {
		return
	}
	directory := filepath.Dir(c.path)
	file, err := os.CreateTemp(directory, ".claude-usage-*")
	if err != nil {
		return
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if file.Chmod(0o600) != nil {
		_ = file.Close()
		return
	}
	if _, err = file.Write(body); err != nil || file.Sync() != nil || file.Close() != nil {
		_ = file.Close()
		return
	}
	_ = os.Rename(temporary, c.path)
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
	data, err := io.ReadAll(io.LimitReader(reader, maxStatuslineBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxStatuslineBytes {
		return errors.New("status-line input too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
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
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("status-line input must contain one JSON object")
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
