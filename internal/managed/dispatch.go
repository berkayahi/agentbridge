package managed

import (
	"context"
	"errors"
)

var ErrUnknownCommand = errors.New("managed: unknown command")

type CommandHandler func(context.Context, Frame) error

type Dispatcher struct {
	Handlers map[string]CommandHandler
}

func (d Dispatcher) Dispatch(ctx context.Context, frame Frame) error {
	handler, ok := d.Handlers[frame.PayloadType]
	if !ok || handler == nil {
		return ErrUnknownCommand
	}
	return handler(ctx, frame)
}
