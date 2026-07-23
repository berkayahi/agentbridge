package managed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/deviceidentity"
	"github.com/berkayahi/agentbridge/internal/spool"
)

func TestSpoolBridgeSendsOrderedDeviceSignedEvents(t *testing.T) {
	key, err := deviceidentity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"event":"done"}`)
	digest := sha256.Sum256(payload)
	store := &bridgeSpool{messages: []spool.Message{{MessageID: 2, ExecutionID: "execution-2", Sequence: 2, Type: "completed", Payload: payload, PayloadHash: hex.EncodeToString(digest[:]), CreatedAt: time.Unix(2_000, 0).UTC()}}}
	bridge, err := NewSpoolBridge(store, key, "org-1", "device-1", time.Minute, func() time.Time { return time.Unix(2_000, 0).UTC() })
	if err != nil {
		t.Fatal(err)
	}
	transport := &bridgeTransport{}
	last, count, err := bridge.Replay(context.Background(), transport, 3, 4, 0, 128)
	if err != nil || last != 2 || count != 1 || len(transport.frames) != 1 {
		t.Fatalf("replay last=%d count=%d frames=%d err=%v", last, count, len(transport.frames), err)
	}
	frame := transport.frames[0]
	if frame.PayloadType != "event" || frame.MessageID != 2 || frame.SigningKeyID != key.Fingerprint() {
		t.Fatalf("spool frame = %#v", frame)
	}
	canonical, err := frame.CanonicalSigningBytes()
	if err != nil || !key.Verify(canonical, frame.Signature) {
		t.Fatalf("spool signature invalid: %v", err)
	}
}

type bridgeSpool struct{ messages []spool.Message }

func (s *bridgeSpool) Replay(context.Context, spool.ReplayRequest) ([]spool.Message, error) {
	return append([]spool.Message(nil), s.messages...), nil
}
func (*bridgeSpool) Acknowledge(context.Context, spool.AcknowledgeRequest) (spool.AcknowledgeResult, error) {
	return spool.AcknowledgeResult{}, nil
}
func (*bridgeSpool) Usage(context.Context) (spool.Usage, error) { return spool.Usage{}, nil }

type bridgeTransport struct{ frames []Frame }

func (*bridgeTransport) Receive(ctx context.Context) (Frame, error) { return Frame{}, ctx.Err() }
func (t *bridgeTransport) Send(_ context.Context, frame Frame) error {
	t.frames = append(t.frames, frame)
	return nil
}
func (*bridgeTransport) Close() error { return nil }
