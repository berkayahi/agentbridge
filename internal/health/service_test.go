package health

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestReadinessDoesNotExposeCheckErrors(t *testing.T) {
	secret := "oauth-token-that-must-not-escape"
	service, err := New(func() time.Time { return time.Unix(100, 0).UTC() }, map[string]Check{
		"database": func(context.Context) error { return errors.New("connection failed: " + secret) },
	})
	if err != nil {
		t.Fatal(err)
	}
	report := service.Readiness(context.Background())
	if report.Status != StatusNotReady || len(report.Checks) != 1 || report.Checks[0].Detail != "unavailable" {
		t.Fatalf("readiness = %#v", report)
	}
	if strings.Contains(report.Checks[0].Detail, secret) {
		t.Fatal("readiness leaked check error")
	}
}

func TestReadinessBoundsContextAwareChecks(t *testing.T) {
	service, err := NewWithTimeout(time.Now, map[string]Check{
		"spool": func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}, 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	report := service.Readiness(context.Background())
	if report.Status != StatusNotReady || report.Checks[0].Detail != "timeout" {
		t.Fatalf("readiness = %#v", report)
	}
	if time.Since(started) > 500*time.Millisecond {
		t.Fatal("readiness exceeded its bounded timeout")
	}
}
