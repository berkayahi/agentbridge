package main

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// A desktop-only controller composes no Telegram adapter and no Tailscale
// dashboard, so Run must supervise the owner-only local API instead of
// refusing to start.
func TestComposedDaemonDesktopOnlyRunWaitsOnLocalAPI(t *testing.T) {
	localDone := make(chan error, 1)
	d := &composedDaemon{
		desktopOnly:  true,
		localHandler: http.NotFoundHandler(),
		localDone:    localDone,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := d.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("desktop-only run error = %v, want context cancellation", err)
	}
}

// The local API is the only transport in this mode, so its unexpected exit must
// fail the daemon lifecycle rather than leave a misleadingly healthy process.
func TestComposedDaemonDesktopOnlyRunPropagatesLocalAPIFailure(t *testing.T) {
	want := errors.New("local api failed")
	localDone := make(chan error, 1)
	localDone <- want
	d := &composedDaemon{
		desktopOnly:  true,
		localHandler: http.NotFoundHandler(),
		localDone:    localDone,
	}
	if err := d.Run(context.Background()); !errors.Is(err, want) {
		t.Fatalf("desktop-only run error = %v, want the local API failure", err)
	}
}

// A clean local-API exit is still unexpected while the daemon is meant to serve.
func TestComposedDaemonDesktopOnlyRunRejectsSilentLocalAPIExit(t *testing.T) {
	localDone := make(chan error, 1)
	localDone <- nil
	d := &composedDaemon{
		desktopOnly:  true,
		localHandler: http.NotFoundHandler(),
		localDone:    localDone,
	}
	if err := d.Run(context.Background()); err == nil {
		t.Fatal("a silent local API exit must fail the daemon lifecycle")
	}
}

// Without the desktop-only marker the standalone controller requirements stay
// exactly as strict as before.
func TestComposedDaemonStillRequiresStandaloneController(t *testing.T) {
	d := &composedDaemon{localHandler: http.NotFoundHandler(), localDone: make(chan error, 1)}
	if err := d.Run(context.Background()); err == nil {
		t.Fatal("a non-desktop daemon without Telegram or dashboard must fail")
	}
}
