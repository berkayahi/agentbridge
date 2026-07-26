package localcontrol

import (
	"context"
	"errors"
	"testing"
	"time"
)

type countingUsage struct {
	calls int
	view  []ProviderUsageView
	err   error
}

func (c *countingUsage) ProviderUsage(context.Context) ([]ProviderUsageView, error) {
	c.calls++
	return c.view, c.err
}

// Asking a runtime what a subscription has left is not free: one adapter answers
// from memory, the other makes two live RPC calls against a running session. The
// surface polls, so concurrent or repeated asks must not multiply those calls.
func TestUsageIsCachedSoAskingOftenCostsOnce(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	source := &countingUsage{view: []ProviderUsageView{{Provider: "codex", Reported: true}}}
	service := &Service{clock: func() time.Time { return now }, usage: source}

	for range 5 {
		if _, err := service.Usage(context.Background()); err != nil {
			t.Fatalf("usage: %v", err)
		}
	}
	if source.calls != 1 {
		t.Fatalf("five asks inside the window must cost one read, got %d", source.calls)
	}

	now = now.Add(usageTTL + time.Second)
	if _, err := service.Usage(context.Background()); err != nil {
		t.Fatalf("usage after expiry: %v", err)
	}
	if source.calls != 2 {
		t.Fatalf("the window has to expire, got %d reads", source.calls)
	}
}

// A stale answer labelled with when it was observed beats no answer, but only
// once there is something to be stale about.
func TestUsageFailureKeepsTheLastAnswerAndOtherwiseReports(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	source := &countingUsage{err: errors.New("engine asleep")}
	service := &Service{clock: func() time.Time { return now }, usage: source}

	if _, err := service.Usage(context.Background()); err == nil {
		t.Fatal("with nothing cached, a failure must be reported rather than answered emptily")
	}

	source.err = nil
	source.view = []ProviderUsageView{{Provider: "codex", Reported: true}}
	if _, err := service.Usage(context.Background()); err != nil {
		t.Fatalf("usage: %v", err)
	}

	now = now.Add(usageTTL + time.Second)
	source.err = errors.New("engine asleep")
	response, err := service.Usage(context.Background())
	if err != nil {
		t.Fatalf("a previous answer must survive a later failure: %v", err)
	}
	if len(response.Providers) != 1 {
		t.Fatalf("the last answer must come back: %+v", response)
	}
}

func TestUsageWithoutASourceIsNotConfigured(t *testing.T) {
	service := &Service{clock: time.Now}
	if _, err := service.Usage(context.Background()); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want not configured", err)
	}
}
