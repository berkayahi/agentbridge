package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/berkayahi/agentbridge/internal/deviceidentity"
)

var (
	ErrInvalidRecoveryTranscript = errors.New("auth: invalid recovery transcript")
	ErrRecoveryTranscriptExpired = errors.New("auth: recovery transcript expired")
	ErrRecoveryTranscriptReplay  = errors.New("auth: recovery transcript already claimed")
)

const (
	// RecoveryEphemeralPublicKeySize is the public-key size for the reviewed
	// X25519 HPKE profile carried by the recovery protocol.
	RecoveryEphemeralPublicKeySize = 32
	minRecoveryChallengeSize       = 16
	maxRecoveryChallengeSize       = 256
	minKeyConfirmationSize         = 16
	maxKeyConfirmationSize         = 256
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
	if !validRecoveryID(t.RequestID) || !validRecoveryID(t.OrganizationID) || !validRecoveryID(t.DeviceID) || !validRecoveryID(t.Provider) || !validRecoveryID(t.BrowserSessionID) || len(t.EphemeralPublicKey) != RecoveryEphemeralPublicKeySize || len(t.Challenge) < minRecoveryChallengeSize || len(t.Challenge) > maxRecoveryChallengeSize || len(t.KeyConfirmation) < minKeyConfirmationSize || len(t.KeyConfirmation) > maxKeyConfirmationSize || !validFingerprint(t.DeviceFingerprint) || t.ExpiresAt.IsZero() {
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
	if !key.HasPrivate() || deviceidentity.EnrollmentFingerprint(key.PublicKey()) != t.DeviceFingerprint {
		return RecoveryTranscript{}, ErrInvalidRecoveryTranscript
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
	if err != nil || len(t.Signature) != ed25519.SignatureSize {
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

// RecoveryClaimStore makes a verified transcript one-use. The claim happens
// only after signature and enrollment-fingerprint verification, and the store
// owns the atomic check-and-set so concurrent browser attempts cannot both
// consume the same recovery exchange.
type RecoveryClaimStore interface {
	Claim(context.Context, string, time.Time, time.Time) error
}

// MemoryRecoveryClaimStore is suitable for one daemon process. A durable
// deployment should provide the same atomic semantics in its v2 store.
type MemoryRecoveryClaimStore struct {
	mu      sync.Mutex
	claimed map[string]time.Time
}

func NewMemoryRecoveryClaimStore() *MemoryRecoveryClaimStore {
	return &MemoryRecoveryClaimStore{claimed: make(map[string]time.Time)}
}

func (s *MemoryRecoveryClaimStore) Claim(ctx context.Context, key string, expiresAt, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || strings.TrimSpace(key) == "" || expiresAt.IsZero() || !now.Before(expiresAt) {
		return ErrInvalidRecoveryTranscript
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for existing, expiry := range s.claimed {
		if !now.Before(expiry) {
			delete(s.claimed, existing)
		}
	}
	if expiry, ok := s.claimed[key]; ok && now.Before(expiry) {
		return ErrRecoveryTranscriptReplay
	}
	s.claimed[key] = expiresAt
	return nil
}

// ClaimKey is the digest of the signed transcript context. It intentionally
// excludes the signature itself so a re-signature cannot bypass one-use state.
func (t RecoveryTranscript) ClaimKey() (string, error) {
	canonical, err := t.canonical()
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

// VerifyRecoveryTranscriptOnce verifies the device binding and atomically
// claims the transcript before any encrypted recovery payload is accepted.
func VerifyRecoveryTranscriptOnce(ctx context.Context, store RecoveryClaimStore, t RecoveryTranscript, publicKey []byte, now time.Time) error {
	if store == nil {
		return ErrInvalidRecoveryTranscript
	}
	if err := VerifyRecoveryTranscript(t, publicKey, now); err != nil {
		return err
	}
	key, err := t.ClaimKey()
	if err != nil {
		return ErrInvalidRecoveryTranscript
	}
	return store.Claim(ctx, key, t.ExpiresAt, now)
}

// VerifyKeyConfirmation binds a browser's key-confirmation output to the
// challenge without exposing either value in logs or durable events.
func (t RecoveryTranscript) VerifyKeyConfirmation(expectedDigest string) error {
	actual := t.ConfirmationDigest()
	if subtle.ConstantTimeCompare([]byte(actual), []byte(strings.ToLower(strings.TrimSpace(expectedDigest)))) != 1 {
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

func validFingerprint(value string) bool {
	return validHex(value, 64)
}

func validHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') || (character >= 'A' && character <= 'F')) {
			return false
		}
	}
	return true
}
