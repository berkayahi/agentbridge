package managed

import (
	"context"
	"errors"
	"time"
)

type TransportFactory func(context.Context) (Transport, error)

type ClientConfig struct {
	TransportFactory TransportFactory
	Guard            *ReplayGuard
	Trust            TrustSet
	Dispatch         Dispatcher
	Backoff          Backoff
	Clock            func() time.Time
	LocalHandshake   Handshake
}

type Client struct {
	config ClientConfig
}

func NewClient(config ClientConfig) (*Client, error) {
	if config.TransportFactory == nil || config.Guard == nil {
		return nil, ErrTransportClosed
	}
	if err := config.Trust.Validate(); err != nil {
		return nil, err
	}
	if err := validateHandshake(config.LocalHandshake); err != nil {
		return nil, err
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	return &Client{config: config}, nil
}

func (c *Client) Run(ctx context.Context) error {
	attempt := 0
	for {
		transport, err := c.config.TransportFactory(ctx)
		if err == nil {
			connection, connectionErr := NewConnectionWithOptions(transport, c.config.Guard, c.config.Trust, c.config.Dispatch, ConnectionOptions{
				LocalHandshake: c.config.LocalHandshake, RequireHandshake: true, Clock: c.config.Clock,
			})
			if connectionErr == nil {
				err = connection.Run(ctx)
			} else {
				err = connectionErr
			}
			_ = transport.Close()
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, ErrRevoked) || errors.Is(err, ErrTrustRollback) || errors.Is(err, ErrUntrustedCommand) || errors.Is(err, ErrInvalidFrame) || errors.Is(err, ErrHandshakeRequired) || errors.Is(err, ErrExpiredFrame) || errors.Is(err, ErrUnknownPayloadType) || errors.Is(err, ErrInvalidFramePayload) {
			return err
		}
		attempt++
		timer := time.NewTimer(c.config.Backoff.Duration(attempt))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
