package managed

import (
	"context"
	"errors"
	"time"
)

var ErrTransportClosed = errors.New("managed: transport closed")

type Transport interface {
	Receive(context.Context) (Frame, error)
	Send(context.Context, Frame) error
	Close() error
}

type ConnectionReady func(context.Context, Transport, Handshake, Handshake) error

type Connection struct {
	transport Transport
	guard     *ReplayGuard
	trust     TrustSet
	dispatch  Dispatcher
	options   ConnectionOptions
	clock     func() time.Time
}

func NewConnection(transport Transport, guard *ReplayGuard, trust TrustSet, dispatch Dispatcher) (*Connection, error) {
	return NewConnectionWithOptions(transport, guard, trust, dispatch, ConnectionOptions{})
}

func NewConnectionWithOptions(transport Transport, guard *ReplayGuard, trust TrustSet, dispatch Dispatcher, options ConnectionOptions) (*Connection, error) {
	if transport == nil || guard == nil || dispatch.Handlers == nil {
		return nil, ErrTransportClosed
	}
	if err := trust.Validate(); err != nil {
		return nil, err
	}
	if options.Clock == nil {
		options.Clock = time.Now
	}
	return &Connection{transport: transport, guard: guard, trust: trust, dispatch: dispatch, options: options, clock: options.Clock}, nil
}

func (c *Connection) Run(ctx context.Context) error {
	if c.options.RequireHandshake {
		handshaker, ok := c.transport.(HandshakeTransport)
		if !ok {
			return ErrHandshakeRequired
		}
		remote, err := handshaker.PerformHandshake(ctx, c.options.LocalHandshake)
		if err != nil {
			return err
		}
		canonical, err := remote.CanonicalSigningBytes()
		if err != nil {
			return err
		}
		if err := c.trust.Verify(remote.SigningKeyID, canonical, remote.Signature, remote.ControllerEpoch); err != nil {
			return err
		}
		if _, err := Negotiate(c.options.LocalHandshake, remote); err != nil {
			return err
		}
		if c.options.OnReady != nil {
			if err := c.options.OnReady(ctx, c.transport, c.options.LocalHandshake, remote); err != nil {
				return err
			}
		}
	}
	for {
		frame, err := c.transport.Receive(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if err := frame.Validate(c.clock().UTC()); err != nil {
			return err
		}
		canonical, err := frame.CanonicalSigningBytes()
		if err != nil {
			return err
		}
		if err := c.trust.Verify(frame.SigningKeyID, canonical, frame.Signature, frame.ControllerEpoch); err != nil {
			return err
		}
		if err := c.guard.Accept(ctx, frame, c.clock().UTC()); err != nil {
			if errors.Is(err, ErrReplay) {
				continue
			}
			return err
		}
		if err := c.dispatch.Dispatch(ctx, frame); err != nil {
			return err
		}
	}
}
