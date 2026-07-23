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

type Connection struct {
	transport Transport
	guard     *ReplayGuard
	trust     TrustSet
	dispatch  Dispatcher
	clock     func() time.Time
}

func NewConnection(transport Transport, guard *ReplayGuard, trust TrustSet, dispatch Dispatcher) (*Connection, error) {
	if transport == nil || guard == nil || dispatch.Handlers == nil {
		return nil, ErrTransportClosed
	}
	if err := trust.Validate(); err != nil {
		return nil, err
	}
	return &Connection{transport: transport, guard: guard, trust: trust, dispatch: dispatch, clock: time.Now}, nil
}

func (c *Connection) Run(ctx context.Context) error {
	for {
		frame, err := c.transport.Receive(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		canonical := append([]byte(frame.PayloadType+"\x00"), frame.Payload...)
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
