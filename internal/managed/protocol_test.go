package managed

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"testing"
	"time"
)

func TestConnectionUsesSignedHandshakeAndCanonicalFrameBytes(t *testing.T) {
	platformPublic, platformPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2_000, 0).UTC()
	remote := Handshake{
		Major: 1, OrganizationID: "org-1", DeviceID: "device-1", ConnectionEpoch: 3,
		ControllerEpoch: 4, Capabilities: []string{"commands"}, SigningKeyID: "platform-1",
		Signature: nil,
	}
	remoteBytes, err := remote.CanonicalSigningBytes()
	if err != nil {
		t.Fatal(err)
	}
	remote.Signature = ed25519.Sign(platformPrivate, remoteBytes)
	payload := []byte(`{"command":"cancel"}`)
	digest := sha256.Sum256(payload)
	frame := Frame{
		Major: 1, OrganizationID: "org-1", DeviceID: "device-1", ConnectionEpoch: 3,
		ControllerEpoch: 4, MessageID: 1, Sequence: 1, CommandID: "command-1",
		CausationID: "cause-1", CorrelationID: "trace-1", PayloadType: "command",
		Payload: payload, PayloadDigest: digest[:], SigningKeyID: "platform-1",
		IssuedAt: now.Add(-time.Second), ExpiresAt: now.Add(time.Minute),
	}
	canonical, err := frame.CanonicalSigningBytes()
	if err != nil {
		t.Fatal(err)
	}
	frame.Signature = ed25519.Sign(platformPrivate, canonical)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	transport := &protocolTransport{remote: remote, frame: frame}
	guard, err := NewReplayGuard(&MemoryCursorStore{}, "org-1", "device-1")
	if err != nil {
		t.Fatal(err)
	}
	local := Handshake{Major: 1, OrganizationID: "org-1", DeviceID: "device-1", ConnectionEpoch: 3, ControllerEpoch: 4, SigningKeyID: "device-1", Signature: []byte("device-signature")}
	dispatched := false
	connection, err := NewConnectionWithOptions(transport, guard, TrustSet{Active: map[string]ed25519.PublicKey{"platform-1": platformPublic}, HighestEpoch: 4}, Dispatcher{Handlers: map[string]CommandHandler{
		"command": func(context.Context, Frame) error {
			dispatched = true
			cancel()
			return nil
		},
	}}, ConnectionOptions{LocalHandshake: local, RequireHandshake: true, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context cancellation", err)
	}
	if !transport.handshaken || !dispatched {
		t.Fatalf("handshaken=%v dispatched=%v", transport.handshaken, dispatched)
	}
}

type protocolTransport struct {
	remote     Handshake
	frame      Frame
	handshaken bool
	received   bool
}

func (t *protocolTransport) PerformHandshake(_ context.Context, _ Handshake) (Handshake, error) {
	t.handshaken = true
	return t.remote, nil
}

func (t *protocolTransport) Receive(ctx context.Context) (Frame, error) {
	if !t.received {
		t.received = true
		return t.frame, nil
	}
	<-ctx.Done()
	return Frame{}, ctx.Err()
}

func (*protocolTransport) Send(context.Context, Frame) error { return nil }
func (*protocolTransport) Close() error                      { return nil }
