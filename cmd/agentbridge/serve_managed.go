package main

import (
	"context"
	"errors"
	"fmt"
	mathrand "math/rand"
	"path/filepath"
	"time"

	"github.com/berkayahi/agentbridge/internal/config"
	managedcontroller "github.com/berkayahi/agentbridge/internal/controller/managed"
	"github.com/berkayahi/agentbridge/internal/deviceidentity"
	"github.com/berkayahi/agentbridge/internal/kernel"
	"github.com/berkayahi/agentbridge/internal/managed"
	"github.com/berkayahi/agentbridge/internal/store/sqlite"
)

// managedDaemon is intentionally smaller than the standalone composition.
// Managed mode receives canonical work from the platform; local surfaces do
// not create competing tasks. The kernel, signed connector, and spool are the
// complete managed authority boundary for this process.
type managedDaemon struct {
	store  *sqlite.Store
	client *managed.Client
}

func (d *managedDaemon) Start(context.Context) error { return nil }

func (d *managedDaemon) Run(ctx context.Context) error {
	if d == nil || d.client == nil {
		return errors.New("managed daemon is unavailable")
	}
	return d.client.Run(ctx)
}

func (d *managedDaemon) Shutdown(context.Context) error {
	if d == nil || d.store == nil {
		return nil
	}
	return d.store.Close()
}

func buildManagedDaemon(ctx context.Context, cfg config.Config, paths runtimePaths) (daemonRuntime, error) {
	data, err := sqlite.OpenV2WithRuntimeLock(ctx, paths.database)
	if err != nil {
		return nil, err
	}
	fail := func(cause error) (daemonRuntime, error) {
		_ = data.Close()
		return nil, cause
	}

	identityPath := cfg.Managed.IdentityPath
	if identityPath == "" {
		identityPath = filepath.Join(paths.data, "device-key.json")
	}
	recordPath := cfg.Managed.RecordPath
	if recordPath == "" {
		recordPath = filepath.Join(paths.data, "enrollment.json")
	}
	statePath := cfg.Managed.StatePath
	if statePath == "" {
		statePath = filepath.Join(paths.data, "managed-state.json")
	}
	identity, err := deviceidentity.Load(identityPath)
	if err != nil {
		return fail(fmt.Errorf("load managed device identity: %w", err))
	}
	record, err := deviceidentity.LoadRecord(recordPath)
	if err != nil {
		return fail(fmt.Errorf("load managed enrollment: %w", err))
	}
	if record.OrganizationID != cfg.Managed.OrganizationID || record.DeviceID != cfg.Managed.DeviceID || record.Fingerprint != identity.Fingerprint() || record.Revoked || record.Quarantined {
		return fail(errors.New("managed enrollment does not match configured trusted device"))
	}
	state, err := managed.NewFileStateStore(statePath)
	if err != nil {
		return fail(err)
	}
	spoolBridge, err := managed.NewSpoolBridge(data.Spool(), identity, cfg.Managed.OrganizationID, cfg.Managed.DeviceID, 24*time.Hour, time.Now)
	if err != nil {
		return fail(err)
	}
	bridgeKernel, err := kernel.New(kernel.Config{Work: data, Owner: "managed-" + cfg.Managed.DeviceID, IntentTTL: 24 * time.Hour})
	if err != nil {
		return fail(fmt.Errorf("initialize managed kernel: %w", err))
	}
	controller := managedcontroller.New(bridgeKernel)
	randomSource := mathrand.New(mathrand.NewSource(time.Now().UnixNano()))
	client, err := managed.NewPersistentClient(managed.PersistentClientConfig{
		State: state, WebSocket: managed.WebSocketConfig{URL: cfg.Managed.GatewayURL}, Identity: identity,
		Enrollment: &record, OrganizationID: cfg.Managed.OrganizationID, DeviceID: cfg.Managed.DeviceID,
		Capabilities: []string{"commands", "events", "offline-replay"}, Dispatch: controller.Dispatcher(),
		Backoff: managed.Backoff{Base: time.Second, Max: time.Minute, Rand: randomSource}, Clock: time.Now,
		Spool: spoolBridge, SpoolBatchSize: 128,
	})
	if err != nil {
		return fail(fmt.Errorf("initialize managed connector: %w", err))
	}
	return &managedDaemon{store: data, client: client}, nil
}
