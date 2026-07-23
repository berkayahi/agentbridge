package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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

func TestUpdateTrustDocumentsRejectURLsAndWritableFiles(t *testing.T) {
	if _, err := ReadTrustRootFile("https://updates.example/root.json"); !errors.Is(err, ErrUntrustedDocument) {
		t.Fatalf("URL trust root error = %v, want ErrUntrustedDocument", err)
	}
	path := filepath.Join(t.TempDir(), "root.json")
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	document, err := json.Marshal(map[string]any{
		"threshold": 1,
		"keys":      map[string]string{"release-1": base64.StdEncoding.EncodeToString(public)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, document, 0o664); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o664); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTrustRootFile(path); !errors.Is(err, ErrUntrustedDocument) {
		t.Fatalf("writable trust root error = %v, want ErrUntrustedDocument", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTrustRootFile(path); err != nil {
		t.Fatalf("protected trust root error = %v", err)
	}
}

func TestFloorCannotBeRewrittenAtSameMetadataVersion(t *testing.T) {
	store := &MemoryFloorStore{}
	first := Floor{MetadataVersion: 1, ProductVersion: "2.0.1", BuildTag: "v2.0.1", ArtifactDigest: hex.EncodeToString(make([]byte, 32)), UpdatedAt: time.Unix(4_000, 0).UTC()}
	if err := store.Save(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.BuildTag = "v2.0.1-replaced"
	if err := store.Save(context.Background(), second); !errors.Is(err, ErrRollback) {
		t.Fatalf("same-version floor error = %v, want ErrRollback", err)
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
