package policy

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"
)

var (
	ErrConfirmationInvalid = errors.New("policy: invalid local confirmation")
	ErrConfirmationReplay  = errors.New("policy: local confirmation already used")
)

type Confirmation struct {
	ExecutionID  string
	Effect       Effect
	PolicyDigest string
	Nonce        string
	Origin       string
	ExpiresAt    time.Time
	Used         bool
}

type LocalConfirmation struct{ used map[string]struct{} }

func NewLocalConfirmation() *LocalConfirmation {
	return &LocalConfirmation{used: make(map[string]struct{})}
}

func (c *LocalConfirmation) Issue(executionID string, effect Effect, digest string, origin string, expiresAt time.Time) (Confirmation, error) {
	if executionID == "" || effect == "" || digest == "" || origin == "" || expiresAt.IsZero() {
		return Confirmation{}, ErrConfirmationInvalid
	}
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return Confirmation{}, err
	}
	return Confirmation{ExecutionID: executionID, Effect: effect, PolicyDigest: digest, Nonce: hex.EncodeToString(buffer), Origin: origin, ExpiresAt: expiresAt.UTC()}, nil
}

func (c *LocalConfirmation) Consume(value Confirmation, executionID string, effect Effect, digest string, now time.Time) error {
	if value.Used || value.ExecutionID != executionID || value.Effect != effect || value.PolicyDigest != digest || value.ExpiresAt.IsZero() || !now.Before(value.ExpiresAt) || !localOrigin(value.Origin) {
		return ErrConfirmationInvalid
	}
	if _, exists := c.used[value.Nonce]; exists {
		return ErrConfirmationReplay
	}
	c.used[value.Nonce] = struct{}{}
	return nil
}

func localOrigin(origin string) bool {
	return origin == "local_control_socket" || origin == "local_diagnostic_ui" || origin == "physical_operator"
}
