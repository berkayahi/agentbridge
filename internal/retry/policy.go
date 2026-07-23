// Package retry contains bounded, deterministic retry decisions.
package retry

import (
	"crypto/sha256"
	"encoding/binary"
	"time"

	"github.com/berkayahi/agentbridge/internal/provider"
)

type Policy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
}

func (p Policy) ShouldRetry(failure provider.Failure, attempt int) bool {
	return failure.RetrySafe && attempt >= 0 && attempt < p.MaxAttempts
}

// Delay is deterministic for an intent/attempt pair, bounded by MaxDelay, and
// therefore remains stable across worker restarts.
func (p Policy) Delay(intentID string, attempt int) time.Duration {
	if p.BaseDelay <= 0 || attempt < 0 {
		return 0
	}
	delay := p.BaseDelay
	for i := 0; i < attempt && delay < p.MaxDelay; i++ {
		delay *= 2
	}
	if p.MaxDelay > 0 && delay > p.MaxDelay {
		delay = p.MaxDelay
	}
	digest := sha256.Sum256([]byte(intentID))
	jitter := time.Duration(binary.BigEndian.Uint64(digest[:8]) % uint64(maxDuration(delay/2, time.Nanosecond)))
	return minDuration(delay+jitter, p.MaxDelay)
}

func maxDuration(left, right time.Duration) time.Duration {
	if left > right {
		return left
	}
	return right
}
func minDuration(left, right time.Duration) time.Duration {
	if right > 0 && left > right {
		return right
	}
	return left
}
