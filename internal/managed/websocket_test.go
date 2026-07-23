package managed

import (
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/deviceidentity"
)

func TestSignHandshakeUsesDeviceIdentity(t *testing.T) {
	key, err := deviceidentity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	handshake := Handshake{
		Major: 1, Minor: 0, OrganizationID: "org-1", DeviceID: "device-1",
		ConnectionEpoch: 3, ControllerEpoch: 4, Capabilities: []string{"commands"},
	}
	signed, err := SignHandshake(handshake, key)
	if err != nil {
		t.Fatal(err)
	}
	if signed.SigningKeyID != key.Fingerprint() || len(signed.Signature) != ed25519.SignatureSize {
		t.Fatalf("signed handshake = %#v, want device key binding", signed)
	}
	if err := VerifyHandshakeSignature(signed, key.PublicKey()); err != nil {
		t.Fatal(err)
	}
	signed.DeviceID = "other-device"
	if err := VerifyHandshakeSignature(signed, key.PublicKey()); err == nil {
		t.Fatal("mutated handshake verified")
	}
}

func TestWebSocketURLRequiresSecureOutboundGateway(t *testing.T) {
	for _, url := range []string{"", "ws://gateway.example/connect", "https://gateway.example/connect", "wss://user:secret@gateway.example/connect", "wss://"} {
		if err := ValidateWebSocketURL(url); err == nil {
			t.Fatalf("ValidateWebSocketURL(%q) succeeded", url)
		}
	}
	if err := ValidateWebSocketURL("wss://gateway.example/connect"); err != nil {
		t.Fatal(err)
	}
}

func TestWebSocketEnvelopeRoundTripRejectsOversizedPayload(t *testing.T) {
	now := time.Unix(2_000, 0).UTC()
	frame := Frame{
		Major: 1, OrganizationID: "org-1", DeviceID: "device-1", ConnectionEpoch: 1,
		ControllerEpoch: 1, MessageID: 1, Sequence: 1, CommandID: "command-1",
		CausationID: "cause-1", CorrelationID: "trace-1", PayloadType: "command",
		Payload: []byte(`{"kind":"cancel"}`), PayloadDigest: make([]byte, 32), Signature: []byte("sig"), SigningKeyID: "platform-1",
		IssuedAt: now, ExpiresAt: now.Add(time.Minute),
	}
	encoded, err := marshalWebSocketEnvelope(webSocketEnvelope{Type: "frame", Frame: &frame})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := unmarshalWebSocketEnvelope(encoded)
	if err != nil || decoded.Type != "frame" || decoded.Frame == nil || decoded.Frame.MessageID != frame.MessageID {
		t.Fatalf("decoded envelope = %#v err=%v", decoded, err)
	}
	if _, err := marshalWebSocketEnvelope(webSocketEnvelope{Type: "frame", Frame: &Frame{Payload: make([]byte, MaxPayloadBytes+1)}}); err == nil {
		t.Fatal("oversized frame was encoded")
	}
}

func TestPersistentClientAdvancesLocalConnectionEpoch(t *testing.T) {
	key, err := deviceidentity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	state, err := NewFileStateStore(t.TempDir() + "/managed-state.json")
	if err != nil {
		t.Fatal(err)
	}
	platformKey := make(ed25519.PublicKey, ed25519.PublicKeySize)
	if err := state.SaveTrust(context.Background(), TrustSet{Active: map[string]ed25519.PublicKey{"platform-1": platformKey}, HighestEpoch: 4}); err != nil {
		t.Fatal(err)
	}
	client, err := NewPersistentClient(PersistentClientConfig{
		State: state, Identity: key, OrganizationID: "org-1", DeviceID: "device-1",
		WebSocket: WebSocketConfig{URL: "wss://gateway.example/connect"},
		Dispatch:  Dispatcher{Handlers: map[string]CommandHandler{"command": func(context.Context, Frame) error { return nil }}},
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := client.config.LocalHandshakeFactory()
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.config.LocalHandshakeFactory()
	if err != nil {
		t.Fatal(err)
	}
	if first.ConnectionEpoch != 1 || second.ConnectionEpoch != 2 {
		t.Fatalf("connection epochs = %d, %d; want 1, 2", first.ConnectionEpoch, second.ConnectionEpoch)
	}
	if err := VerifyHandshakeSignature(first, key.PublicKey()); err != nil {
		t.Fatal(err)
	}
}

func TestPersistentClientBootstrapsTrustFromEnrollmentRecord(t *testing.T) {
	key, err := deviceidentity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	platformPublic := make(ed25519.PublicKey, ed25519.PublicKeySize)
	state, err := NewFileStateStore(t.TempDir() + "/managed-state.json")
	if err != nil {
		t.Fatal(err)
	}
	record := deviceidentity.EnrollmentRecord{
		Version: 1, ClaimID: "claim-1", OrganizationID: "org-1", DeviceID: "device-1",
		Fingerprint: key.Fingerprint(), TrustSetDigest: "trust-digest", HighestControllerEpoch: 4,
		Mode: "managed", CommandSigningKeys: map[string][]byte{"platform-1": platformPublic},
	}
	if _, err := NewPersistentClient(PersistentClientConfig{
		State: state, WebSocket: WebSocketConfig{URL: "wss://gateway.example/connect"}, Identity: key,
		Enrollment: &record, OrganizationID: "org-1", DeviceID: "device-1",
		Dispatch: Dispatcher{Handlers: map[string]CommandHandler{"command": func(context.Context, Frame) error { return nil }}},
	}); err != nil {
		t.Fatal(err)
	}
	trust, err := state.LoadTrust(context.Background())
	if err != nil || trust.HighestEpoch != 4 || len(trust.Active["platform-1"]) != ed25519.PublicKeySize {
		t.Fatalf("bootstrapped trust = %#v err=%v", trust, err)
	}
}
