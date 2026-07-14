package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/berkayahi/agentbridge/internal/security"
	"github.com/berkayahi/agentbridge/internal/telegram"
)

type liveApplication interface {
	Start(context.Context) error
	HandleUpdate(context.Context, telegram.Update) (string, error)
	Shutdown(context.Context) error
}

type liveTelegram interface {
	Run(context.Context)
	Next(context.Context) (telegram.Update, error)
}

type liveHTTP interface {
	Listen(string) error
	ShutdownWithContext(context.Context) error
}

type liveDaemon struct {
	application liveApplication
	telegram    liveTelegram
	http        liveHTTP
	listen      string
	logger      *slog.Logger

	mu        sync.Mutex
	cancel    context.CancelFunc
	workers   sync.WaitGroup
	workerEnd chan struct{}
}

func newLiveDaemon(application liveApplication, telegramClient liveTelegram, httpServer liveHTTP, listen string) *liveDaemon {
	logger := slog.New(security.NewLogHandler(slog.Default().Handler(), nil))
	return &liveDaemon{application: application, telegram: telegramClient, http: httpServer, listen: listen, logger: logger}
}

func (d *liveDaemon) Start(ctx context.Context) error {
	if d.application == nil || d.telegram == nil || d.http == nil || d.listen == "" {
		return errors.New("daemon has incomplete live dependencies")
	}
	return d.application.Start(ctx)
}

func (d *liveDaemon) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	workerEnd := make(chan struct{})
	d.mu.Lock()
	d.cancel = cancel
	d.workerEnd = workerEnd
	d.mu.Unlock()

	httpResult := make(chan error, 1)
	d.workers.Add(3)
	go func() {
		defer d.workers.Done()
		d.telegram.Run(runCtx)
	}()
	go func() {
		defer d.workers.Done()
		d.routeUpdates(runCtx)
	}()
	go func() {
		defer d.workers.Done()
		httpResult <- d.http.Listen(d.listen)
	}()
	go func() {
		d.workers.Wait()
		close(workerEnd)
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-httpResult:
		if err == nil {
			return errors.New("dashboard listener stopped unexpectedly")
		}
		return err
	}
}

func (d *liveDaemon) routeUpdates(ctx context.Context) {
	for {
		update, err := d.telegram.Next(ctx)
		if err != nil {
			if ctx.Err() == nil {
				d.logger.Error("Telegram update stream stopped", "error_type", fmt.Sprintf("%T", err))
			}
			return
		}
		if _, err := d.application.HandleUpdate(ctx, update); err != nil && ctx.Err() == nil {
			d.logger.Warn("Telegram update was rejected", "error_type", fmt.Sprintf("%T", err))
		}
	}
}

func (d *liveDaemon) Shutdown(ctx context.Context) error {
	d.mu.Lock()
	cancel, workerEnd := d.cancel, d.workerEnd
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	httpErr := d.http.ShutdownWithContext(ctx)
	if workerEnd != nil {
		select {
		case <-workerEnd:
		case <-ctx.Done():
			return errors.Join(httpErr, ctx.Err())
		}
	}
	return errors.Join(httpErr, d.application.Shutdown(ctx))
}

var _ daemonRuntime = (*liveDaemon)(nil)
