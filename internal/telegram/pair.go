package telegram

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

var ErrPairExpired = errors.New("telegram: pairing nonce expired")
var ErrPairUsed = errors.New("telegram: pairing nonce already used")

type UpdateSource interface {
	Next(context.Context) (Update, error)
}

type Pairing struct{ UserID, ChatID int64 }

type PairService struct {
	source UpdateSource
	nonce  func(int) (string, error)
	now    func() time.Time
	ttl    time.Duration
}

type PairAttempt struct {
	source  UpdateSource
	nonce   string
	now     func() time.Time
	expires time.Time
	ttl     time.Duration
	mu      sync.Mutex
	used    bool
}

func NewPairService(source UpdateSource, nonce func(int) (string, error), now func() time.Time, ttl time.Duration) *PairService {
	if nonce == nil {
		nonce = GeneratePairNonce
	}
	if now == nil {
		now = time.Now
	}
	return &PairService{source: source, nonce: nonce, now: now, ttl: ttl}
}

func GeneratePairNonce(bytes int) (string, error) {
	if bytes < 16 {
		bytes = 16
	}
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate pairing nonce: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func (s *PairService) Pair(ctx context.Context) (Pairing, string, error) {
	attempt, err := s.Begin()
	if err != nil {
		return Pairing{}, "", err
	}
	pairing, err := attempt.Wait(ctx)
	return pairing, attempt.Nonce(), err
}

func (s *PairService) Begin() (*PairAttempt, error) {
	if s.source == nil {
		return nil, errors.New("telegram: pairing update source unavailable")
	}
	nonce, err := s.nonce(18)
	if err != nil {
		return nil, err
	}
	return &PairAttempt{source: s.source, nonce: nonce, now: s.now, expires: s.now().Add(s.ttl), ttl: s.ttl}, nil
}

func (a *PairAttempt) Nonce() string { return a.nonce }

func (a *PairAttempt) Wait(ctx context.Context) (Pairing, error) {
	a.mu.Lock()
	if a.used {
		a.mu.Unlock()
		return Pairing{}, ErrPairUsed
	}
	a.used = true
	a.mu.Unlock()
	pairCtx, cancel := context.WithTimeout(ctx, a.ttl)
	defer cancel()
	for {
		update, err := a.source.Next(pairCtx)
		if err != nil {
			if errors.Is(pairCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				return Pairing{}, ErrPairExpired
			}
			return Pairing{}, err
		}
		if a.now().After(a.expires) {
			return Pairing{}, ErrPairExpired
		}
		message := update.Message
		if message == nil || message.Chat.Type != ChatPrivate {
			continue
		}
		fields := strings.Fields(message.Text)
		if len(fields) != 2 || fields[0] != "/pair" || fields[1] != a.nonce {
			continue
		}
		return Pairing{UserID: message.From.ID, ChatID: message.Chat.ID}, nil
	}
}
