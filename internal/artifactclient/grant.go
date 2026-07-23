package artifactclient

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

var ErrGrantSignature = errors.New("artifact: grant signature invalid")

type GrantVerifier interface {
	Verify(keyID string, message, signature []byte) error
}

func (g Grant) CanonicalBytes() ([]byte, error) {
	value := struct {
		OrganizationID, DeviceID, ExecutionID, ArtifactID, ObjectKey, Algorithm, KeyID, PolicyDigest, MediaType string
		SizeBytes                                                                                               int64
		PlaintextDigest, Nonce                                                                                  string
		ExpiresAt                                                                                               int64
	}{
		OrganizationID: g.OrganizationID, DeviceID: g.DeviceID, ExecutionID: g.ExecutionID,
		ArtifactID: g.ArtifactID, ObjectKey: g.ObjectKey, Algorithm: g.Algorithm, KeyID: g.KeyID,
		PolicyDigest: g.PolicyDigest, MediaType: g.MediaType, SizeBytes: g.SizeBytes,
		PlaintextDigest: g.PlaintextDigest, Nonce: g.Nonce, ExpiresAt: g.ExpiresAt.UnixNano(),
	}
	return json.Marshal(value)
}

func (g Grant) Verify(now time.Time, verifier GrantVerifier) error {
	if verifier == nil || len(g.Signature) == 0 {
		return ErrGrantSignature
	}
	if err := g.Validate(now); err != nil {
		return err
	}
	canonical, err := g.CanonicalBytes()
	if err != nil {
		return err
	}
	if err := verifier.Verify(g.KeyID, canonical, g.Signature); err != nil {
		return ErrGrantSignature
	}
	return nil
}

func (g Grant) Digest() (string, error) {
	canonical, err := g.CanonicalBytes()
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

func (g Grant) UsedNonceKey() string {
	return strings.Join([]string{g.OrganizationID, g.DeviceID, g.ExecutionID, g.ArtifactID, g.Nonce}, "\x00")
}
