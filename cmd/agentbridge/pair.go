package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/berkayahi/agentbridge/internal/config"
	"github.com/berkayahi/agentbridge/internal/telegram"
)

const telegramPairTTL = 5 * time.Minute

type liveTelegramPairer struct {
	service *telegram.PairService
	cancel  context.CancelFunc
	done    <-chan struct{}
}

type liveTelegramPairAttempt struct {
	attempt *telegram.PairAttempt
	owner   *liveTelegramPairer
}

func newTelegramPairer(ctx context.Context, configPath string) (pairer, error) {
	if strings.TrimSpace(configPath) == "" {
		return nil, errors.New("pairing config path is required")
	}
	info, err := os.Lstat(configPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("pairing config is unavailable")
	}
	credential, err := (config.CredentialReader{}).Read("telegram_bot_token")
	if err != nil {
		return nil, err
	}
	client, err := telegram.NewClient(credential.Value(), telegram.ClientOptions{})
	if err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		client.Run(runCtx)
	}()
	return &liveTelegramPairer{
		service: telegram.NewPairService(client, nil, nil, telegramPairTTL),
		cancel:  cancel,
		done:    done,
	}, nil
}

func (p *liveTelegramPairer) Begin() (pairAttempt, error) {
	attempt, err := p.service.Begin()
	if err != nil {
		p.cancel()
		return nil, err
	}
	return &liveTelegramPairAttempt{attempt: attempt, owner: p}, nil
}

func (a *liveTelegramPairAttempt) Nonce() string { return a.attempt.Nonce() }

func (a *liveTelegramPairAttempt) Wait(ctx context.Context) (telegram.Pairing, error) {
	type result struct {
		pairing telegram.Pairing
		err     error
	}
	waitCtx, stop := context.WithCancel(ctx)
	resultCh := make(chan result, 1)
	go func() {
		pairing, err := a.attempt.Wait(waitCtx)
		resultCh <- result{pairing: pairing, err: err}
	}()
	var observed result
	select {
	case observed = <-resultCh:
	case <-a.owner.done:
		stop()
		observed.err = errors.New("Telegram pairing transport stopped")
	}
	stop()
	a.owner.cancel()
	select {
	case <-a.owner.done:
	case <-time.After(5 * time.Second):
		if observed.err == nil {
			observed.err = errors.New("Telegram pairing transport did not stop")
		}
	}
	return observed.pairing, observed.err
}
