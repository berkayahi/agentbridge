package managed

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"strings"

	"github.com/berkayahi/agentbridge/internal/deviceidentity"
)

var (
	ErrUntrustedCommand = errors.New("managed: command signer is not trusted")
	ErrTrustRollback    = errors.New("managed: command trust epoch rollback")
	ErrRevoked          = errors.New("managed: device trust is revoked")
)

type TrustSet struct {
	Active       map[string]ed25519.PublicKey
	Next         map[string]ed25519.PublicKey
	HighestEpoch uint64
	Revoked      bool
}

type TrustUpdate struct {
	Active         map[string]ed25519.PublicKey
	Next           map[string]ed25519.PublicKey
	Epoch          uint64
	RequireOverlap bool
}

func (t TrustSet) Verify(keyID string, message, signature []byte, epoch uint64) error {
	if t.Revoked {
		return ErrRevoked
	}
	if epoch < t.HighestEpoch {
		return ErrTrustRollback
	}
	key, ok := t.Active[keyID]
	if !ok || len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, message, signature) {
		return fmt.Errorf("%w: %s", ErrUntrustedCommand, keyID)
	}
	return nil
}

func (t TrustSet) Apply(update TrustUpdate) (TrustSet, error) {
	if update.Epoch <= t.HighestEpoch || len(update.Active) == 0 {
		return TrustSet{}, ErrTrustRollback
	}
	if update.RequireOverlap && !overlap(t.Active, update.Active) {
		return TrustSet{}, errors.New("managed: trust rotation has no active overlap")
	}
	return TrustSet{Active: cloneKeys(update.Active), Next: cloneKeys(update.Next), HighestEpoch: update.Epoch}, nil
}

func (t TrustSet) Validate() error {
	if t.HighestEpoch == 0 || len(t.Active) == 0 {
		return ErrUntrustedCommand
	}
	for id, key := range t.Active {
		if strings.TrimSpace(id) == "" || len(key) != ed25519.PublicKeySize {
			return ErrUntrustedCommand
		}
	}
	return nil
}

// TrustSetFromEnrollment converts only the public command-signing roots from
// an enrollment record. A revoked or quarantined identity can never bootstrap
// a managed connection, even when its old public keys are still present.
func TrustSetFromEnrollment(record deviceidentity.EnrollmentRecord) (TrustSet, error) {
	if record.Revoked || record.Quarantined || record.Mode != "managed" || record.HighestControllerEpoch == 0 {
		return TrustSet{}, ErrRevoked
	}
	active := make(map[string]ed25519.PublicKey, len(record.CommandSigningKeys))
	for id, key := range record.CommandSigningKeys {
		active[id] = append(ed25519.PublicKey(nil), key...)
	}
	trust := TrustSet{Active: active, HighestEpoch: record.HighestControllerEpoch}
	if err := trust.Validate(); err != nil {
		return TrustSet{}, err
	}
	return trust, nil
}

func cloneKeys(input map[string]ed25519.PublicKey) map[string]ed25519.PublicKey {
	result := make(map[string]ed25519.PublicKey, len(input))
	for id, key := range input {
		result[id] = append(ed25519.PublicKey(nil), key...)
	}
	return result
}

func overlap(left, right map[string]ed25519.PublicKey) bool {
	for id := range left {
		if _, ok := right[id]; ok {
			return true
		}
	}
	return false
}
