package managed

import (
	"context"

	"github.com/berkayahi/agentbridge/internal/kernel"
)

type Controller struct{ kernel *kernel.Kernel }

func New(k *kernel.Kernel) *Controller { return &Controller{kernel: k} }

func (c *Controller) Start(ctx context.Context, command kernel.StartExecution) error {
	return c.kernel.Start(ctx, command)
}
func (c *Controller) Resume(ctx context.Context, command kernel.ResumeExecution) error {
	return c.kernel.Resume(ctx, command)
}
func (c *Controller) Steer(ctx context.Context, command kernel.SteerExecution) error {
	return c.kernel.Steer(ctx, command)
}
func (c *Controller) Cancel(ctx context.Context, command kernel.CancelExecution) error {
	return c.kernel.Cancel(ctx, command)
}
func (c *Controller) Close(ctx context.Context, command kernel.CloseExecution) error {
	return c.kernel.Close(ctx, command)
}
func (c *Controller) Fork(ctx context.Context, command kernel.ForkExecution) error {
	return c.kernel.Fork(ctx, command)
}
