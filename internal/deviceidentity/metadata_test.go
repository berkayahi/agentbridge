package deviceidentity

import (
	"context"
	"crypto/ed25519"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestEnrollmentRecordRoundTripsPublicIdentityFacts(t *testing.T) {
	path := t.TempDir() + "/enrollment.json"
	want := EnrollmentRecord{
		Version: 1,
		ClaimID: "claim-1", OrganizationID: "org-1", DeviceID: "device-1",
		Fingerprint: strings.Repeat("a", 64), TrustSetDigest: "trust-digest",
		HighestControllerEpoch: 7, Mode: "managed",
	}
	if err := SaveRecord(path, want); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("record mode = %o, want 600", info.Mode().Perm())
	}
	got, err := LoadRecord(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("record = %#v, want %#v", got, want)
	}
	if err := got.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestEnrollmentRecordPersistsCommandTrustKeys(t *testing.T) {
	path := t.TempDir() + "/enrollment.json"
	key := make([]byte, ed25519.PublicKeySize)
	want := EnrollmentRecord{
		Version: 1, ClaimID: "claim-1", OrganizationID: "org-1", DeviceID: "device-1",
		Fingerprint: strings.Repeat("a", 64), TrustSetDigest: "trust-digest", HighestControllerEpoch: 7, Mode: "managed",
		CommandSigningKeys: map[string][]byte{"platform-1": key},
	}
	if err := SaveRecord(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadRecord(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.CommandSigningKeys["platform-1"]) != ed25519.PublicKeySize {
		t.Fatalf("command trust keys = %#v", got.CommandSigningKeys)
	}
}
