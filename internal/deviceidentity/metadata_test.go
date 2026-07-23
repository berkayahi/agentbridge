package deviceidentity

import (
	"context"
	"os"
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
	if got != want {
		t.Fatalf("record = %#v, want %#v", got, want)
	}
	if err := got.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
}
