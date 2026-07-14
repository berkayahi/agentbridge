package telegram

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPairAcceptsOneTimeNonceOnlyFromPrivateChat(t *testing.T) {
	now := time.Unix(1_000, 0)
	source := &sliceUpdateSource{updates: []Update{
		{ID: 1, Message: &IncomingMessage{Chat: Chat{ID: -1, Type: ChatGroup}, From: User{ID: 8}, Text: "/pair secret"}},
		{ID: 2, Message: &IncomingMessage{Chat: Chat{ID: 22, Type: ChatPrivate}, From: User{ID: 11}, Text: "/pair wrong"}},
		{ID: 3, Message: &IncomingMessage{Chat: Chat{ID: 22, Type: ChatPrivate}, From: User{ID: 11}, Text: "/pair secret"}},
	}}
	svc := NewPairService(source, func(int) (string, error) { return "secret", nil }, func() time.Time { return now }, time.Minute)
	pairing, nonce, err := svc.Pair(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if nonce != "secret" || pairing.UserID != 11 || pairing.ChatID != 22 {
		t.Fatalf("pair result = %#v, %q", pairing, nonce)
	}
}

func TestPairExpiresAndHonorsCancellation(t *testing.T) {
	now := time.Unix(1_000, 0)
	source := &sliceUpdateSource{updates: []Update{{ID: 1, Message: &IncomingMessage{Chat: Chat{ID: 22, Type: ChatPrivate}, From: User{ID: 11}, Text: "/pair secret"}}}, onNext: func() { now = now.Add(2 * time.Minute) }}
	svc := NewPairService(source, func(int) (string, error) { return "secret", nil }, func() time.Time { return now }, time.Minute)
	if _, _, err := svc.Pair(context.Background()); !errors.Is(err, ErrPairExpired) {
		t.Fatalf("error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	svc = NewPairService(&blockingUpdateSource{}, func(int) (string, error) { return "secret", nil }, time.Now, time.Minute)
	if _, _, err := svc.Pair(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

func TestGeneratePairNonceIsOpaque(t *testing.T) {
	a, err := GeneratePairNonce(18)
	if err != nil {
		t.Fatal(err)
	}
	b, err := GeneratePairNonce(18)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) < 20 || a == b || strings.ContainsAny(a, " /+") {
		t.Fatalf("nonces = %q, %q", a, b)
	}
}

type sliceUpdateSource struct {
	updates []Update
	onNext  func()
}

func (s *sliceUpdateSource) Next(ctx context.Context) (Update, error) {
	if s.onNext != nil {
		s.onNext()
	}
	if len(s.updates) == 0 {
		return Update{}, errors.New("empty")
	}
	u := s.updates[0]
	s.updates = s.updates[1:]
	return u, nil
}

type blockingUpdateSource struct{}

func (*blockingUpdateSource) Next(ctx context.Context) (Update, error) {
	<-ctx.Done()
	return Update{}, ctx.Err()
}
