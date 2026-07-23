// Package fake provides a deterministic provider contract test double.
package fake

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/berkayahi/agentbridge/internal/provider"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

const eventCapacity = 32

var ErrScriptTooLarge = errors.New("fake provider script exceeds event bound")

type Provider struct {
	name      workmodel.Provider
	sessionID provider.ID
	events    []provider.Event

	mu    sync.Mutex
	calls []string
}

func New(name workmodel.Provider, sessionID provider.ID, events []provider.Event) *Provider {
	return &Provider{name: name, sessionID: sessionID, events: append([]provider.Event(nil), events...)}
}

func (p *Provider) Name() workmodel.Provider { return p.name }

func (p *Provider) Start(ctx context.Context, req provider.StartRequest) (provider.Session, <-chan provider.Event, error) {
	if err := ctx.Err(); err != nil {
		return provider.Session{}, nil, err
	}
	if err := req.Input.Validate(); err != nil {
		return provider.Session{}, nil, err
	}
	if len(p.events) > eventCapacity {
		return provider.Session{}, nil, ErrScriptTooLarge
	}
	p.record("start")
	session := provider.Session{ID: p.sessionID, TaskID: req.TaskID, Provider: p.name}
	return session, p.eventChannel(req.TaskID), nil
}

func (p *Provider) Resume(ctx context.Context, req provider.ResumeRequest) (provider.Session, <-chan provider.Event, error) {
	if err := ctx.Err(); err != nil {
		return provider.Session{}, nil, err
	}
	if err := req.Input.Validate(); err != nil {
		return provider.Session{}, nil, err
	}
	if len(p.events) > eventCapacity {
		return provider.Session{}, nil, ErrScriptTooLarge
	}
	p.record("resume")
	return req.Session, p.eventChannel(req.TaskID), nil
}

func (p *Provider) Steer(ctx context.Context, _ provider.Session, input provider.Input) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := input.Validate(); err != nil {
		return err
	}
	p.record("steer")
	return nil
}

func (p *Provider) Interrupt(ctx context.Context, _ provider.Session) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.record("interrupt")
	return nil
}

func (p *Provider) ResolveApproval(ctx context.Context, _ provider.ApprovalDecision) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.record("resolve_approval")
	return nil
}

func (p *Provider) Usage(ctx context.Context) (provider.Usage, error) {
	if err := ctx.Err(); err != nil {
		return provider.Usage{}, err
	}
	p.record("usage")
	return provider.Usage{Provider: p.name, ObservedAt: time.Now().UTC()}, nil
}

func (p *Provider) AuthStatus(ctx context.Context) (provider.AuthStatus, error) {
	if err := ctx.Err(); err != nil {
		return provider.AuthStatus{}, err
	}
	p.record("auth_status")
	return provider.AuthStatus{Authenticated: true, CheckedAt: time.Now().UTC()}, nil
}

func (p *Provider) Calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

func (p *Provider) record(call string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, call)
}

func (p *Provider) eventChannel(taskID provider.ID) <-chan provider.Event {
	capacity := len(p.events)
	events := make(chan provider.Event, capacity)
	for _, event := range p.events {
		event.TaskID = taskID
		events <- event
	}
	close(events)
	return events
}

var _ provider.Provider = (*Provider)(nil)
