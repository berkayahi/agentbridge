package deviceidentity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

var (
	ErrInvalidClaim = errors.New("device identity: invalid enrollment claim")
	ErrInvalidProof = errors.New("device identity: invalid enrollment proof")
)

type Claim struct {
	ID                 string
	OrganizationID     string
	DeviceID           string
	BrowserFingerprint string
	ExpiresAt          time.Time
}

type Challenge struct {
	ClaimID        string
	OrganizationID string
	DeviceID       string
	Nonce          string
	TrustSetDigest string
	ExpiresAt      time.Time
}

type Proof struct {
	ClaimID        string
	OrganizationID string
	DeviceID       string
	Nonce          string
	PublicKey      []byte
	Signature      []byte
	TrustSetDigest string
	ExpiresAt      time.Time
}

func (c Claim) Validate(now time.Time) error {
	if !valid(c.ID) || !valid(c.OrganizationID) || !valid(c.DeviceID) || strings.TrimSpace(c.BrowserFingerprint) == "" || c.ExpiresAt.IsZero() || !now.Before(c.ExpiresAt) {
		return ErrInvalidClaim
	}
	return nil
}

func (c Challenge) Validate(now time.Time) error {
	if !valid(c.ClaimID) || !valid(c.OrganizationID) || !valid(c.DeviceID) || strings.TrimSpace(c.Nonce) == "" || strings.TrimSpace(c.TrustSetDigest) == "" || c.ExpiresAt.IsZero() || !now.Before(c.ExpiresAt) {
		return ErrInvalidClaim
	}
	return nil
}

func (k Key) Prove(claim Claim, challenge Challenge, now time.Time) (Proof, error) {
	if err := claim.Validate(now); err != nil {
		return Proof{}, ErrInvalidProof
	}
	if err := challenge.Validate(now); err != nil || claim.ID != challenge.ClaimID || claim.OrganizationID != challenge.OrganizationID || claim.DeviceID != challenge.DeviceID {
		return Proof{}, ErrInvalidProof
	}
	message, err := canonicalEnrollmentMessage(claim, challenge)
	if err != nil {
		return Proof{}, err
	}
	signature, err := k.Sign(message)
	if err != nil {
		return Proof{}, err
	}
	return Proof{ClaimID: claim.ID, OrganizationID: claim.OrganizationID, DeviceID: claim.DeviceID, Nonce: challenge.Nonce, PublicKey: k.PublicKey(), Signature: signature, TrustSetDigest: challenge.TrustSetDigest, ExpiresAt: challenge.ExpiresAt}, nil
}

func VerifyProof(claim Claim, challenge Challenge, proof Proof, now time.Time) error {
	if err := claim.Validate(now); err != nil {
		return ErrInvalidProof
	}
	if err := challenge.Validate(now); err != nil || proof.ClaimID != claim.ID || proof.OrganizationID != claim.OrganizationID || proof.DeviceID != claim.DeviceID || proof.Nonce != challenge.Nonce || proof.TrustSetDigest != challenge.TrustSetDigest || !now.Before(proof.ExpiresAt) {
		return ErrInvalidProof
	}
	key, err := FromPublic(proof.PublicKey)
	if err != nil {
		return ErrInvalidProof
	}
	message, err := canonicalEnrollmentMessage(claim, challenge)
	if err != nil || !key.Verify(message, proof.Signature) {
		return ErrInvalidProof
	}
	return nil
}

func canonicalEnrollmentMessage(claim Claim, challenge Challenge) ([]byte, error) {
	value := struct {
		ClaimID, OrganizationID, DeviceID, BrowserFingerprint, Nonce, TrustSetDigest string
		ExpiresAt                                                                    int64
	}{
		ClaimID: claim.ID, OrganizationID: claim.OrganizationID, DeviceID: claim.DeviceID,
		BrowserFingerprint: claim.BrowserFingerprint, Nonce: challenge.Nonce,
		TrustSetDigest: challenge.TrustSetDigest, ExpiresAt: challenge.ExpiresAt.UnixNano(),
	}
	return json.Marshal(value)
}

func EnrollmentFingerprint(publicKey []byte) string {
	digest := sha256.Sum256(publicKey)
	return hex.EncodeToString(digest[:])
}

func valid(value string) bool {
	return strings.TrimSpace(value) != "" && len(value) <= 128 && !strings.ContainsAny(value, "\x00\r\n")
}
