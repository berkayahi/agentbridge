package managed

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"testing"
	"time"
)

func TestFileStateStorePersistsInboxBeforeDispatch(t *testing.T) {
	now := time.Unix(1_000, 0).UTC()
	path := t.TempDir() + "/managed-state.json"
	store, err := NewFileStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := NewReplayGuardWithInbox(store, "org-1", "device-1")
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"command":"cancel"}`)
	digest := sha256.Sum256(payload)
	frame := Frame{
		Major: 1, OrganizationID: "org-1", DeviceID: "device-1", ConnectionEpoch: 1,
		ControllerEpoch: 2, MessageID: 4, Sequence: 1, CommandID: "command-1",
		CausationID: "cause-1", CorrelationID: "trace-1", PayloadType: "command",
		Payload: payload, PayloadDigest: digest[:], SigningKeyID: "platform-1",
		Signature: []byte("signature"), IssuedAt: now.Add(-time.Second), ExpiresAt: now.Add(time.Minute),
	}
	if err := guard.Accept(context.Background(), frame, now); err != nil {
		t.Fatal(err)
	}
	wantTrust := TrustSet{Active: map[string]ed25519.PublicKey{"platform-1": make(ed25519.PublicKey, ed25519.PublicKeySize)}, HighestEpoch: 4}
	if err := store.SaveTrust(context.Background(), wantTrust); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewFileStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := NewReplayGuardWithInbox(reloaded, "org-1", "device-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Accept(context.Background(), frame, now); !errors.Is(err, ErrReplay) {
		t.Fatalf("replayed frame error = %v, want ErrReplay", err)
	}
	cursor, err := reloaded.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cursor.MessageID != frame.MessageID || cursor.ControllerEpoch != frame.ControllerEpoch {
		t.Fatalf("cursor = %#v, want persisted frame cursor", cursor)
	}
	gotTrust, err := reloaded.LoadTrust(context.Background())
	if err != nil || gotTrust.HighestEpoch != wantTrust.HighestEpoch || len(gotTrust.Active["platform-1"]) != ed25519.PublicKeySize {
		t.Fatalf("trust = %#v err=%v, want persisted trust set", gotTrust, err)
	}
}
