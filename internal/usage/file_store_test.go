package usage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileStoreRoundTripsUsageWindowOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	want := Window{Provider: "codex", DeviceID: "device-1", Runtime: "codex-v2", AccountSafeID: "account-1", ObservedAt: time.Unix(2_000, 0).UTC(), SchemaVersion: 1, Status: StatusAvailable}
	if err := store.Save(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	got, err := store.Latest(context.Background(), want.Provider, want.DeviceID)
	if err != nil || got.Provider != want.Provider || got.ObservedAt != want.ObservedAt {
		t.Fatalf("usage = %#v err=%v", got, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("usage permissions = %v err=%v", info.Mode().Perm(), err)
	}
}

func TestFileStoreRejectsOlderUsageObservation(t *testing.T) {
	store, err := NewFileStore(filepath.Join(t.TempDir(), "usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	newer := Window{Provider: "claude", DeviceID: "device-1", Runtime: "claude-v2", AccountSafeID: "account-1", ObservedAt: time.Unix(2_000, 0).UTC(), SchemaVersion: 1, Status: StatusAvailable}
	older := newer
	older.ObservedAt = newer.ObservedAt.Add(-time.Minute)
	if err := store.Save(context.Background(), newer); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), older); err != ErrStaleObservation {
		t.Fatalf("older save error = %v, want ErrStaleObservation", err)
	}
}
