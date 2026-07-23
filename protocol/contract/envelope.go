// Package contract contains the small, language-neutral validation rules that
// sit beside the generated protobuf package. The protobuf schema remains the
// wire format; this package makes the signed-envelope invariants reusable by
// devices before generated adapters are compiled.
package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	MajorVersion    uint32 = 1
	MinorVersion    uint32 = 0
	MaxFrameBytes          = 4 << 20
	MaxPayloadBytes        = 3 << 20
)

var (
	ErrInvalid        = errors.New("protocol: invalid envelope")
	ErrExpired        = errors.New("protocol: expired envelope")
	ErrUnknownPayload = errors.New("protocol: unknown payload")
)

type Header struct {
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
}

type Envelope struct {
	Header
	PayloadType   string
	Payload       []byte
	PayloadDigest []byte
	SigningKeyID  string
	Signature     []byte
}

type Verifier interface {
	Verify(keyID string, message, signature []byte) error
}

func (e Envelope) Validate(now time.Time) error {
	if e.Major != MajorVersion || e.Minor > MinorVersion {
		return fmt.Errorf("%w: unsupported protocol version %d.%d", ErrInvalid, e.Major, e.Minor)
	}
	if !validID(e.OrganizationID) || !validID(e.DeviceID) || e.ConnectionEpoch == 0 || e.ControllerEpoch == 0 || e.MessageID == 0 || e.Sequence == 0 {
		return ErrInvalid
	}
	if !validID(e.CausationID) || !validID(e.CorrelationID) || e.IssuedAt.IsZero() || !e.ExpiresAt.After(e.IssuedAt) {
		return ErrInvalid
	}
	if e.ExpiresAt.IsZero() || !now.Before(e.ExpiresAt) {
		return ErrExpired
	}
	if len(e.Payload) == 0 || len(e.Payload) > MaxPayloadBytes || len(e.Signature) == 0 || strings.TrimSpace(e.SigningKeyID) == "" {
		return ErrInvalid
	}
	if !knownPayload(e.PayloadType) {
		return ErrUnknownPayload
	}
	digest := sha256.Sum256(e.Payload)
	if !equalBytes(e.PayloadDigest, digest[:]) {
		return ErrInvalid
	}
	canonical, err := e.CanonicalSigningBytes()
	if err != nil || len(canonical)+len(e.Signature) > MaxFrameBytes {
		return ErrInvalid
	}
	return nil
}

func (e Envelope) Verify(now time.Time, verifier Verifier) error {
	if verifier == nil {
		return ErrInvalid
	}
	if err := e.Validate(now); err != nil {
		return err
	}
	canonical, err := e.CanonicalSigningBytes()
	if err != nil {
		return err
	}
	return verifier.Verify(e.SigningKeyID, canonical, e.Signature)
}

// CanonicalSigningBytes deliberately excludes Signature. The field order is
// fixed by this struct and encoding/json sorts no maps in the signed object.
// Generated protobuf adapters must feed the same semantic fields here.
func (e Envelope) CanonicalSigningBytes() ([]byte, error) {
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
		Major: e.Major, Minor: e.Minor, OrganizationID: e.OrganizationID, DeviceID: e.DeviceID,
		ConnectionEpoch: e.ConnectionEpoch, ControllerEpoch: e.ControllerEpoch, MessageID: e.MessageID,
		CommandID: e.CommandID, ExecutionID: e.ExecutionID, SessionID: e.SessionID, ResourceID: e.ResourceID,
		CausationID: e.CausationID, CorrelationID: e.CorrelationID, Sequence: e.Sequence,
		IssuedAt: e.IssuedAt.UnixNano(), ExpiresAt: e.ExpiresAt.UnixNano(), PayloadType: e.PayloadType,
		PayloadDigest: hex.EncodeToString(e.PayloadDigest), SigningKeyID: e.SigningKeyID, Payload: e.Payload,
	}
	return json.Marshal(value)
}

func validID(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= 128 && !strings.ContainsAny(value, "\x00\r\n")
}

func knownPayload(value string) bool {
	switch value {
	case "enrollment", "command", "event", "capability", "recovery", "artifact":
		return true
	default:
		return false
	}
}

func equalBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
