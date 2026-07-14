package telegram

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrPairExpired = errors.New("telegram: pairing nonce expired")

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
	if s.source == nil {
		return Pairing{}, "", errors.New("telegram: pairing update source unavailable")
	}
	nonce, err := s.nonce(18)
	if err != nil {
		return Pairing{}, "", err
	}
	expires := s.now().Add(s.ttl)
	for {
		update, err := s.source.Next(ctx)
		if err != nil {
			return Pairing{}, nonce, err
		}
		if s.now().After(expires) {
			return Pairing{}, nonce, ErrPairExpired
		}
		message := update.Message
		if message == nil || message.Chat.Type != ChatPrivate {
			continue
		}
		fields := strings.Fields(message.Text)
		if len(fields) != 2 || fields[0] != "/pair" || fields[1] != nonce {
			continue
		}
		return Pairing{UserID: message.From.ID, ChatID: message.Chat.ID}, nonce, nil
	}
}
