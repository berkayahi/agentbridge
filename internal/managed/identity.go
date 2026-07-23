package managed

import (
	"crypto/ed25519"
	"errors"
	"fmt"

	"github.com/berkayahi/agentbridge/internal/deviceidentity"
)

var ErrInvalidHandshakeSignature = errors.New("managed: invalid handshake signature")

// SignHandshake binds the complete negotiated identity and capability set to
// the enrolled device key. The signature is intentionally excluded from the
// canonical input so a peer can verify the same transcript.
func SignHandshake(handshake Handshake, key deviceidentity.Key) (Handshake, error) {
	if !key.HasPrivate() {
		return Handshake{}, ErrInvalidHandshakeSignature
	}
	handshake.SigningKeyID = key.Fingerprint()
	handshake.Signature = nil
	if err := validateHandshakeFields(handshake); err != nil {
		return Handshake{}, err
	}
	canonical, err := handshake.CanonicalSigningBytes()
	if err != nil {
		return Handshake{}, fmt.Errorf("canonicalize handshake: %w", err)
	}
	signature, err := key.Sign(canonical)
	if err != nil {
		return Handshake{}, err
	}
	handshake.Signature = signature
	return handshake, nil
}

func VerifyHandshakeSignature(handshake Handshake, publicKey []byte) error {
	if err := validateHandshake(handshake); err != nil || len(publicKey) != ed25519.PublicKeySize {
		return ErrInvalidHandshakeSignature
	}
	key, err := deviceidentity.FromPublic(publicKey)
	if err != nil || handshake.SigningKeyID != key.Fingerprint() {
		return ErrInvalidHandshakeSignature
	}
	signature := append([]byte(nil), handshake.Signature...)
	handshake.Signature = nil
	canonical, err := handshake.CanonicalSigningBytes()
	if err != nil || !key.Verify(canonical, signature) {
		return ErrInvalidHandshakeSignature
	}
	return nil
}

func validateHandshakeFields(handshake Handshake) error {
	if handshake.Major == 0 || handshake.OrganizationID == "" || handshake.DeviceID == "" || handshake.ConnectionEpoch == 0 || handshake.ControllerEpoch == 0 || handshake.SigningKeyID == "" {
		return ErrHandshakeRequired
	}
	return nil
}
