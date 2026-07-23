package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestInstallerVerifiesStagedDigestAndRecordsFloor(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "agentbridge")
	staged := filepath.Join(dir, "agentbridge.staged")
	if err := os.WriteFile(target, []byte("old"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("new"), 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(4_000, 0).UTC()
	metadata := testMetadata(staged, now)
	floor := &MemoryFloorStore{}
	installer := Installer{Target: target, Verify: func(context.Context, Metadata) error { return nil }, Health: func(context.Context, string) error { return nil }, Floor: floor}
	if err := installer.Install(context.Background(), metadata, staged, now); err != nil {
		t.Fatal(err)
	}
	value, err := os.ReadFile(target)
	if err != nil || string(value) != "new" {
		t.Fatalf("installed target = %q err=%v", value, err)
	}
	stored, err := floor.Load(context.Background())
	if err != nil || stored.MetadataVersion != metadata.Version {
		t.Fatalf("floor = %#v err=%v", stored, err)
	}
}

func TestInstallerRollsBackWhenFloorPersistenceFails(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "agentbridge")
	staged := filepath.Join(dir, "agentbridge.staged")
	if err := os.WriteFile(target, []byte("old"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("new"), 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(4_000, 0).UTC()
	metadata := testMetadata(staged, now)
	installer := Installer{Target: target, Verify: func(context.Context, Metadata) error { return nil }, Health: func(context.Context, string) error { return nil }, Floor: failingFloorStore{err: errors.New("floor unavailable")}}
	if err := installer.Install(context.Background(), metadata, staged, now); err == nil {
		t.Fatal("install succeeded despite floor failure")
	}
	value, err := os.ReadFile(target)
	if err != nil || string(value) != "old" {
		t.Fatalf("rolled-back target = %q err=%v", value, err)
	}
}

func TestFileFloorStoreRoundTripsOwnerOnlyState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update-floor.json")
	store, err := NewFileFloorStore(path)
	if err != nil {
		t.Fatal(err)
	}
	want := Floor{MetadataVersion: 3, ProductVersion: "2.0.1", BuildTag: "v2.0.1", ArtifactDigest: hex.EncodeToString(make([]byte, 32)), UpdatedAt: time.Unix(4_000, 0).UTC()}
	if err := store.Save(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(context.Background())
	if err != nil || got.MetadataVersion != want.MetadataVersion || got.ArtifactDigest != want.ArtifactDigest {
		t.Fatalf("floor = %#v err=%v", got, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("floor permissions = %v err=%v", info.Mode().Perm(), err)
	}
}

func testMetadata(path string, now time.Time) Metadata {
	value, _ := os.ReadFile(path)
	digest := sha256.Sum256(value)
	return Metadata{Version: 1, ExpiresAt: now.Add(time.Hour), Identity: BinaryIdentity{
		ProductVersion: "2.0.1", BuildTag: "v2.0.1", SourceCommit: hex.EncodeToString(make([]byte, 20)), ArtifactDigest: hex.EncodeToString(digest[:]), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
	}, SignerIDs: []string{"release-1"}, Signatures: map[string][]byte{"release-1": []byte("signature")}}
}

type failingFloorStore struct{ err error }

func (failingFloorStore) Load(context.Context) (Floor, error) { return Floor{}, nil }
func (f failingFloorStore) Save(context.Context, Floor) error { return f.err }
