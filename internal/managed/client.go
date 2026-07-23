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
			connection, connectionErr := NewConnection(transport, c.config.Guard, c.config.Trust, c.config.Dispatch)
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
		if errors.Is(err, ErrRevoked) || errors.Is(err, ErrTrustRollback) || errors.Is(err, ErrUntrustedCommand) || errors.Is(err, ErrInvalidFrame) {
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
