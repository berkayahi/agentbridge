package approval

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/berkayahi/agentbridge/internal/policy"
)

var ErrBindingMismatch = errors.New("approval: binding mismatch")

// Binding is the immutable authorization context for an approval. It is
// carried in the durable request payload and must be presented unchanged when
// a caller uses the bound decision path.
type Binding struct {
	ExecutionID           string        `json:"execution_id"`
	OperationDigest       string        `json:"operation_digest"`
	PolicyDigest          string        `json:"policy_digest"`
	Effect                policy.Effect `json:"effect"`
	SteppedUpAuthEvidence string        `json:"stepped_up_auth_evidence,omitempty"`
	Nonce                 string        `json:"nonce"`
}

func (b Binding) Valid() bool {
	return strings.TrimSpace(b.ExecutionID) != "" &&
		strings.TrimSpace(b.OperationDigest) != "" &&
		strings.TrimSpace(b.PolicyDigest) != "" &&
		b.Effect.Valid() && strings.TrimSpace(b.Nonce) != ""
}

func (b Binding) Digest() string {
	if !b.Valid() {
		return ""
	}
	value := strings.Join([]string{b.ExecutionID, b.OperationDigest, b.PolicyDigest, string(b.Effect), b.SteppedUpAuthEvidence, b.Nonce}, "\x00")
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func (b Binding) Matches(expected Binding) bool {
	return b == expected
}

func (b Binding) NotExpired(now time.Time, expiresAt time.Time) bool {
	return !expiresAt.IsZero() && now.Before(expiresAt)
}

func ValidateBinding(expected, supplied Binding) error {
	if !expected.Valid() || !supplied.Valid() || !expected.Matches(supplied) {
		return ErrBindingMismatch
	}
	return nil
}
