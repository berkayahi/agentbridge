// Package update verifies immutable AgentBridge release identity before install.
package update

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"runtime"
	"strings"
	"time"
)

var (
	ErrInvalidMetadata = errors.New("update: invalid metadata")
	ErrExpiredMetadata = errors.New("update: metadata expired")
	ErrRollback        = errors.New("update: rollback or freeze detected")
)

type BinaryIdentity struct {
	ProductVersion string
	BuildTag       string
	SourceCommit   string
	ArtifactDigest string
	GOOS           string
	GOARCH         string
}

func (b BinaryIdentity) Validate() error {
	if !semverCore.MatchString(b.ProductVersion) || strings.TrimSpace(b.BuildTag) == "" || !hexString(b.SourceCommit, 40) || !hexString(b.ArtifactDigest, 64) || b.GOOS == "" || b.GOARCH == "" {
		return ErrInvalidMetadata
	}
	return nil
}

type Metadata struct {
	Version    uint64
	ExpiresAt  time.Time
	Identity   BinaryIdentity
	SignerIDs  []string
	Signatures map[string][]byte
}

func (m Metadata) Validate(now time.Time) error {
	if m.Version == 0 || m.ExpiresAt.IsZero() || !now.Before(m.ExpiresAt) || m.Identity.Validate() != nil || len(m.SignerIDs) == 0 {
		if !now.Before(m.ExpiresAt) && !m.ExpiresAt.IsZero() {
			return ErrExpiredMetadata
		}
		return ErrInvalidMetadata
	}
	if m.Identity.GOOS != runtime.GOOS || m.Identity.GOARCH != runtime.GOARCH {
		return ErrInvalidMetadata
	}
	for _, id := range m.SignerIDs {
		if strings.TrimSpace(id) == "" || len(m.Signatures[id]) == 0 {
			return ErrInvalidMetadata
		}
	}
	return nil
}

func (m Metadata) CanonicalBytes() ([]byte, error) {
	value := struct {
		Version   uint64
		ExpiresAt int64
		Identity  BinaryIdentity
		SignerIDs []string
	}{m.Version, m.ExpiresAt.UnixNano(), m.Identity, append([]string(nil), m.SignerIDs...)}
	return json.Marshal(value)
}

func (m Metadata) Digest() (string, error) {
	value, err := m.CanonicalBytes()
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:]), nil
}

var semverCore = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func hexString(value string, length int) bool {
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
