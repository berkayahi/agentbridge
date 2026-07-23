package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/berkayahi/agentbridge/internal/deviceidentity"
)

var (
	ErrInvalidRecoveryTranscript = errors.New("auth: invalid recovery transcript")
	ErrRecoveryTranscriptExpired = errors.New("auth: recovery transcript expired")
)

// RecoveryTranscript is the signed binding around an ephemeral recovery
// exchange. Encryption is deliberately delegated to the reviewed HPKE
// implementation used by the managed protocol; this type never stores
// plaintext provider credentials.
type RecoveryTranscript struct {
	RequestID          string
	OrganizationID     string
	DeviceID           string
	Provider           string
	BrowserSessionID   string
	EphemeralPublicKey []byte
	Challenge          []byte
	KeyConfirmation    []byte
	ExpiresAt          time.Time
	DeviceFingerprint  string
	Signature          []byte
}

func (t RecoveryTranscript) Validate(now time.Time) error {
	if !validRecoveryID(t.RequestID) || !validRecoveryID(t.OrganizationID) || !validRecoveryID(t.DeviceID) || !validRecoveryID(t.Provider) || !validRecoveryID(t.BrowserSessionID) || len(t.EphemeralPublicKey) == 0 || len(t.Challenge) == 0 || len(t.KeyConfirmation) == 0 || strings.TrimSpace(t.DeviceFingerprint) == "" || t.ExpiresAt.IsZero() {
		return ErrInvalidRecoveryTranscript
	}
	if !now.Before(t.ExpiresAt) {
		return ErrRecoveryTranscriptExpired
	}
	return nil
}

func (t RecoveryTranscript) Sign(key deviceidentity.Key, now time.Time) (RecoveryTranscript, error) {
	if err := t.Validate(now); err != nil {
		return RecoveryTranscript{}, err
	}
	message, err := t.canonical()
	if err != nil {
		return RecoveryTranscript{}, err
	}
	signature, err := key.Sign(message)
	if err != nil {
		return RecoveryTranscript{}, err
	}
	t.Signature = signature
	return t, nil
}

func VerifyRecoveryTranscript(t RecoveryTranscript, publicKey []byte, now time.Time) error {
	if err := t.Validate(now); err != nil {
		return err
	}
	key, err := deviceidentity.FromPublic(publicKey)
	if err != nil {
		return ErrInvalidRecoveryTranscript
	}
	message, err := t.canonical()
	if err != nil || !key.Verify(message, t.Signature) {
		return ErrInvalidRecoveryTranscript
	}
	if deviceidentity.EnrollmentFingerprint(publicKey) != t.DeviceFingerprint {
		return ErrInvalidRecoveryTranscript
	}
	return nil
}

func (t RecoveryTranscript) ConfirmationDigest() string {
	digest := sha256.Sum256(append(append([]byte(nil), t.Challenge...), t.KeyConfirmation...))
	return hex.EncodeToString(digest[:])
}

func (t RecoveryTranscript) canonical() ([]byte, error) {
	value := struct {
		RequestID, OrganizationID, DeviceID, Provider, BrowserSessionID, DeviceFingerprint string
		EphemeralPublicKey, Challenge, KeyConfirmation                                     []byte
		ExpiresAt                                                                          int64
	}{
		RequestID: t.RequestID, OrganizationID: t.OrganizationID, DeviceID: t.DeviceID,
		Provider: t.Provider, BrowserSessionID: t.BrowserSessionID, DeviceFingerprint: t.DeviceFingerprint,
		EphemeralPublicKey: t.EphemeralPublicKey, Challenge: t.Challenge, KeyConfirmation: t.KeyConfirmation,
		ExpiresAt: t.ExpiresAt.UnixNano(),
	}
	return json.Marshal(value)
}

func validRecoveryID(value string) bool {
	return strings.TrimSpace(value) != "" && len(value) <= 256 && !strings.ContainsAny(value, "\x00\r\n")
}
