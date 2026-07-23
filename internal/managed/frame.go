package managed

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	ProtocolMajor   uint32 = 1
	ProtocolMinor   uint32 = 0
	MaxFrameBytes          = 4 << 20
	MaxPayloadBytes        = 3 << 20
)

var (
	ErrExpiredFrame        = errors.New("managed: expired frame")
	ErrUnknownPayloadType  = errors.New("managed: unknown payload type")
	ErrInvalidFramePayload = errors.New("managed: invalid frame payload")
)

// Frame is the transport-neutral projection of protocol/device/v1.Envelope.
// Its canonical bytes intentionally match protocol/contract so command
// signatures are independent of the selected transport encoding.
type Frame struct {
	Major           uint32
	Minor           uint32
	OrganizationID  string
	DeviceID        string
	ConnectionEpoch uint64
	ControllerEpoch uint64
	MessageID       uint64
	CommandID       string
	ExecutionID     string
	SessionID       string
	ResourceID      string
	CausationID     string
	CorrelationID   string
	Sequence        uint64
	IssuedAt        time.Time
	ExpiresAt       time.Time
	PayloadType     string
	PayloadDigest   []byte
	Payload         []byte
	Signature       []byte
	SigningKeyID    string
}

func (f Frame) Validate(now time.Time) error {
	if f.Major != ProtocolMajor || f.Minor > ProtocolMinor {
		return ErrInvalidFramePayload
	}
	if !validFrameID(f.OrganizationID) || !validFrameID(f.DeviceID) || f.ConnectionEpoch == 0 || f.ControllerEpoch == 0 || f.MessageID == 0 || f.Sequence == 0 {
		return ErrInvalidFramePayload
	}
	if !validFrameID(f.CausationID) || !validFrameID(f.CorrelationID) || f.IssuedAt.IsZero() || f.ExpiresAt.IsZero() || !f.ExpiresAt.After(f.IssuedAt) {
		return ErrInvalidFramePayload
	}
	if !now.Before(f.ExpiresAt) {
		return ErrExpiredFrame
	}
	if f.IssuedAt.After(now.Add(5 * time.Minute)) {
		return ErrInvalidFramePayload
	}
	if len(f.Payload) == 0 || len(f.Payload) > MaxPayloadBytes || len(f.Signature) == 0 || !validFrameID(f.SigningKeyID) {
		return ErrInvalidFramePayload
	}
	if !knownFramePayload(f.PayloadType) {
		return ErrUnknownPayloadType
	}
	if f.PayloadType == "command" && !validFrameID(f.CommandID) {
		return ErrInvalidFramePayload
	}
	digest := sha256.Sum256(f.Payload)
	if len(f.PayloadDigest) != len(digest) || subtle.ConstantTimeCompare(f.PayloadDigest, digest[:]) != 1 {
		return ErrInvalidFramePayload
	}
	canonical, err := f.CanonicalSigningBytes()
	if err != nil || len(canonical)+len(f.Signature) > MaxFrameBytes {
		return ErrInvalidFramePayload
	}
	return nil
}

func (f Frame) CanonicalSigningBytes() ([]byte, error) {
	value := struct {
		Major, Minor                                  uint32
		OrganizationID, DeviceID                      string
		ConnectionEpoch, ControllerEpoch, MessageID   uint64
		CommandID, ExecutionID, SessionID, ResourceID string
		CausationID, CorrelationID                    string
		Sequence                                      uint64
		IssuedAt, ExpiresAt                           int64
		PayloadType                                   string
		PayloadDigest                                 string
		SigningKeyID                                  string
		Payload                                       []byte
	}{
		Major: f.Major, Minor: f.Minor, OrganizationID: f.OrganizationID, DeviceID: f.DeviceID,
		ConnectionEpoch: f.ConnectionEpoch, ControllerEpoch: f.ControllerEpoch, MessageID: f.MessageID,
		CommandID: f.CommandID, ExecutionID: f.ExecutionID, SessionID: f.SessionID, ResourceID: f.ResourceID,
		CausationID: f.CausationID, CorrelationID: f.CorrelationID, Sequence: f.Sequence,
		IssuedAt: f.IssuedAt.UnixNano(), ExpiresAt: f.ExpiresAt.UnixNano(), PayloadType: f.PayloadType,
		PayloadDigest: hex.EncodeToString(f.PayloadDigest), SigningKeyID: f.SigningKeyID, Payload: f.Payload,
	}
	return json.Marshal(value)
}

func validFrameID(value string) bool {
	return strings.TrimSpace(value) != "" && len(value) <= 128 && !strings.ContainsAny(value, "\x00\r\n")
}

func knownFramePayload(value string) bool {
	switch value {
	case "enrollment", "command", "event", "capability", "recovery", "artifact":
		return true
	default:
		return false
	}
}

func (f Frame) String() string {
	return fmt.Sprintf("%d.%d/%s/%s/%d", f.Major, f.Minor, f.OrganizationID, f.DeviceID, f.MessageID)
}
