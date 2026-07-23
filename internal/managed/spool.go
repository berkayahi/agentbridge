package managed

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/berkayahi/agentbridge/internal/deviceidentity"
	"github.com/berkayahi/agentbridge/internal/spool"
)

var ErrInvalidSpoolMessage = errors.New("managed: invalid spool message")

type EventSpool interface {
	Replay(context.Context, spool.ReplayRequest) ([]spool.Message, error)
	Acknowledge(context.Context, spool.AcknowledgeRequest) (spool.AcknowledgeResult, error)
	Usage(context.Context) (spool.Usage, error)
}

type SpoolBridge struct {
	spool          EventSpool
	identity       deviceidentity.Key
	organizationID string
	deviceID       string
	expiresAfter   time.Duration
	clock          func() time.Time
}

func NewSpoolBridge(events EventSpool, identity deviceidentity.Key, organizationID, deviceID string, expiresAfter time.Duration, clock func() time.Time) (*SpoolBridge, error) {
	if events == nil || !identity.HasPrivate() || strings.TrimSpace(organizationID) == "" || strings.TrimSpace(deviceID) == "" || expiresAfter <= 0 {
		return nil, ErrInvalidSpoolMessage
	}
	if clock == nil {
		clock = time.Now
	}
	return &SpoolBridge{spool: events, identity: identity, organizationID: organizationID, deviceID: deviceID, expiresAfter: expiresAfter, clock: clock}, nil
}

func (b *SpoolBridge) Usage(ctx context.Context) (spool.Usage, error) {
	if b == nil || b.spool == nil {
		return spool.Usage{}, ErrInvalidSpoolMessage
	}
	return b.spool.Usage(ctx)
}

func (b *SpoolBridge) Replay(ctx context.Context, transport Transport, connectionEpoch, controllerEpoch, afterMessageID uint64, limit int) (uint64, int, error) {
	if b == nil || b.spool == nil || transport == nil || connectionEpoch == 0 || controllerEpoch == 0 {
		return afterMessageID, 0, ErrInvalidSpoolMessage
	}
	messages, err := b.spool.Replay(ctx, spool.ReplayRequest{AfterMessageID: afterMessageID, Limit: limit})
	if err != nil {
		return afterMessageID, 0, err
	}
	last := afterMessageID
	for _, message := range messages {
		frame, err := b.frame(message, connectionEpoch, controllerEpoch)
		if err != nil {
			return last, 0, err
		}
		if err := transport.Send(ctx, frame); err != nil {
			return last, 0, err
		}
		last = message.MessageID
	}
	return last, len(messages), nil
}

func (b *SpoolBridge) Acknowledge(ctx context.Context, request spool.AcknowledgeRequest) (spool.AcknowledgeResult, error) {
	if b == nil || b.spool == nil {
		return spool.AcknowledgeResult{}, ErrInvalidSpoolMessage
	}
	return b.spool.Acknowledge(ctx, request)
}

func (b *SpoolBridge) frame(message spool.Message, connectionEpoch, controllerEpoch uint64) (Frame, error) {
	if message.MessageID == 0 || message.Sequence == 0 || strings.TrimSpace(message.ExecutionID) == "" || len(message.Payload) == 0 || !message.VerifyPayload() {
		return Frame{}, ErrInvalidSpoolMessage
	}
	now := b.clock().UTC()
	issuedAt := message.CreatedAt.UTC()
	if issuedAt.IsZero() || issuedAt.After(now.Add(5*time.Minute)) {
		issuedAt = now
	}
	payloadDigest := sha256.Sum256(message.Payload)
	frame := Frame{
		Major: ProtocolMajor, Minor: ProtocolMinor, OrganizationID: b.organizationID, DeviceID: b.deviceID,
		ConnectionEpoch: connectionEpoch, ControllerEpoch: controllerEpoch, MessageID: message.MessageID,
		ExecutionID: message.ExecutionID, ResourceID: message.Type, CausationID: "spool-" + strconv.FormatUint(message.MessageID, 10),
		CorrelationID: message.ExecutionID, Sequence: message.Sequence, IssuedAt: issuedAt, ExpiresAt: now.Add(b.expiresAfter),
		PayloadType: "event", PayloadDigest: payloadDigest[:], Payload: append([]byte(nil), message.Payload...), SigningKeyID: b.identity.Fingerprint(),
	}
	canonical, err := frame.CanonicalSigningBytes()
	if err != nil {
		return Frame{}, fmt.Errorf("canonicalize spool frame: %w", err)
	}
	frame.Signature, err = b.identity.Sign(canonical)
	if err != nil {
		return Frame{}, err
	}
	return frame, nil
}
