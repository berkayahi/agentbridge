// Package artifactclient transfers policy-granted encrypted artifacts.
package artifactclient

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

var (
	ErrInvalidGrant = errors.New("artifact: invalid grant")
	ErrGrantExpired = errors.New("artifact: grant expired")
	ErrGrantReplay  = errors.New("artifact: grant nonce already used")
	ErrConflict     = errors.New("artifact: immutable object conflict")
	ErrChunkOrder   = errors.New("artifact: chunk order or digest mismatch")
)

type Grant struct {
	OrganizationID  string
	DeviceID        string
	ExecutionID     string
	ArtifactID      string
	ObjectKey       string
	Algorithm       string
	KeyID           string
	PolicyDigest    string
	MediaType       string
	SizeBytes       int64
	PlaintextDigest string
	ExpiresAt       time.Time
	Nonce           string
	Signature       []byte
}

func (g Grant) Validate(now time.Time) error {
	if !valid(g.OrganizationID) || !valid(g.DeviceID) || !valid(g.ExecutionID) || !valid(g.ArtifactID) || !valid(g.ObjectKey) || !valid(g.KeyID) || !valid(g.PolicyDigest) || strings.TrimSpace(g.MediaType) == "" || g.SizeBytes < 0 || !validHex(g.PlaintextDigest, 64) || g.Algorithm != "AES-256-GCM" || strings.TrimSpace(g.Nonce) == "" || g.ExpiresAt.IsZero() {
		return ErrInvalidGrant
	}
	if !now.Before(g.ExpiresAt) {
		return ErrGrantExpired
	}
	return nil
}

type EncryptedArtifact struct {
	ArtifactID      string
	ObjectKey       string
	Algorithm       string
	KeyID           string
	Nonce           []byte
	Ciphertext      []byte
	PlaintextDigest string
	EnvelopeDigest  string
	SizeBytes       int64
}

func (a EncryptedArtifact) Validate() error {
	if !valid(a.ArtifactID) || !valid(a.ObjectKey) || a.Algorithm != "AES-256-GCM" || !valid(a.KeyID) || len(a.Nonce) != 12 || len(a.Ciphertext) == 0 || !validHex(a.PlaintextDigest, 64) || !validHex(a.EnvelopeDigest, 64) || a.SizeBytes < 0 || int64(len(a.Ciphertext)) != a.SizeBytes+16 {
		return ErrInvalidGrant
	}
	if digestBytes(append(append([]byte(nil), a.Nonce...), a.Ciphertext...)) != a.EnvelopeDigest {
		return ErrInvalidGrant
	}
	return nil
}

type Receipt struct {
	ArtifactID     string
	ObjectKey      string
	EnvelopeDigest string
	StoredBytes    int64
	FinalizedAt    time.Time
	Duplicate      bool
}

type Chunk struct {
	ArtifactID string
	ObjectKey  string
	Offset     int64
	Payload    []byte
	Final      bool
}

func digestBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func valid(value string) bool {
	return strings.TrimSpace(value) != "" && len(value) <= 512 && !strings.ContainsAny(value, "\x00\r\n")
}

func validHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') && !(r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}
