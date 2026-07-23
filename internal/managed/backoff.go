package managed

import (
	"math/rand"
	"time"
)

type Backoff struct {
	Base time.Duration
	Max  time.Duration
	Rand *rand.Rand
}

func (b Backoff) Duration(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	base := b.Base
	if base <= 0 {
		base = time.Second
	}
	max := b.Max
	if max <= 0 {
		max = time.Minute
	}
	if attempt > 20 {
		attempt = 20
	}
	delay := base << attempt
	if delay > max || delay < base {
		delay = max
	}
	jitter := delay / 4
	if jitter == 0 {
		return delay
	}
	if b.Rand == nil {
		return delay
	}
	return delay - jitter/2 + time.Duration(b.Rand.Int63n(int64(jitter)))
}
