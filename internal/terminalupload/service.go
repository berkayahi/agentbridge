// Package terminalupload gates explicit terminal-output artifact transfer.
package terminalupload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/berkayahi/agentbridge/internal/artifactclient"
	"github.com/berkayahi/agentbridge/internal/egressguard"
)

var (
	ErrDisabled      = errors.New("terminal upload: disabled by policy")
	ErrInvalidPolicy = errors.New("terminal upload: invalid policy")
	ErrQuota         = errors.New("terminal upload: byte quota exceeded")
	ErrGrantBinding  = errors.New("terminal upload: grant does not bind sanitized payload")
)

type Policy struct {
	Enabled   bool
	MaxBytes  int64
	ExpiresAt time.Time
}

type Request struct {
	Policy  Policy
	Grant   artifactclient.Grant
	Key     []byte
	Payload []byte
}

type Uploader interface {
	Upload(context.Context, artifactclient.Grant, []byte, []byte) (artifactclient.Receipt, error)
}

type Service struct {
	uploader Uploader
	guard    *egressguard.Guard
	now      func() time.Time
}

func NewService(uploader Uploader, guard *egressguard.Guard, now func() time.Time) (*Service, error) {
	if uploader == nil || guard == nil {
		return nil, ErrInvalidPolicy
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{uploader: uploader, guard: guard, now: now}, nil
}

func (s *Service) Upload(ctx context.Context, request Request) (artifactclient.Receipt, error) {
	if s == nil || s.uploader == nil || s.guard == nil {
		return artifactclient.Receipt{}, ErrInvalidPolicy
	}
	now := s.now().UTC()
	if !request.Policy.Enabled {
		return artifactclient.Receipt{}, ErrDisabled
	}
	if request.Policy.MaxBytes <= 0 || request.Policy.ExpiresAt.IsZero() {
		return artifactclient.Receipt{}, ErrInvalidPolicy
	}
	if !now.Before(request.Policy.ExpiresAt) {
		return artifactclient.Receipt{}, ErrInvalidPolicy
	}
	if int64(len(request.Payload)) > request.Policy.MaxBytes {
		return artifactclient.Receipt{}, ErrQuota
	}
	if err := request.Grant.Validate(now); err != nil {
		return artifactclient.Receipt{}, err
	}
	sanitized, err := s.guard.Check(egressguard.ClassTerminalOutput, request.Payload)
	if err != nil {
		return artifactclient.Receipt{}, err
	}
	if int64(len(sanitized)) != request.Grant.SizeBytes || payloadDigest(sanitized) != strings.ToLower(request.Grant.PlaintextDigest) {
		return artifactclient.Receipt{}, ErrGrantBinding
	}
	return s.uploader.Upload(ctx, request.Grant, request.Key, sanitized)
}

func payloadDigest(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
