// Package scheduler serializes mutating work per repository.
package scheduler

import (
	"context"
	"errors"
	"sync"
	"time"
)

var (
	ErrClosed           = errors.New("scheduler: closed")
	ErrLeaseUnavailable = errors.New("scheduler: repository lease unavailable")
)

type LeaseStore interface {
	AcquireLease(context.Context, string, string, time.Duration) (bool, error)
	HeartbeatLease(context.Context, string, string, time.Duration) error
	ReleaseLease(context.Context, string, string) error
}

type Request struct {
	TaskID     string
	Repository string
	ReadOnly   bool
}

type Permit struct {
	release  func()
	once     sync.Once
	lost     chan struct{}
	loseOnce sync.Once
}

func (p *Permit) Release() {
	if p != nil {
		p.once.Do(p.release)
	}
}

// Done closes when durable lease ownership can no longer be proven. Callers
// must use it to cancel the mutating process before releasing the permit.
func (p *Permit) Done() <-chan struct{} { return p.lost }
func (p *Permit) lose() {
	if p != nil {
		p.loseOnce.Do(func() { close(p.lost) })
	}
}

type acquireRequest struct {
	ctx   context.Context
	req   Request
	reply chan acquireReply
}
type acquireReply struct {
	permit *Permit
	err    error
}
type releaseRequest struct{ repository string }

type Scheduler struct {
	store     LeaseStore
	owner     string
	ttl       time.Duration
	heartbeat time.Duration
	acquire   chan acquireRequest
	release   chan releaseRequest
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

func New(store LeaseStore, owner string, ttl, heartbeat time.Duration) *Scheduler {
	if ttl <= 0 {
		ttl = time.Minute
	}
	if heartbeat <= 0 {
		heartbeat = ttl / 3
	}
	s := &Scheduler{store: store, owner: owner, ttl: ttl, heartbeat: heartbeat,
		acquire: make(chan acquireRequest), release: make(chan releaseRequest), stop: make(chan struct{}), done: make(chan struct{})}
	go s.run()
	return s
}

func (s *Scheduler) Acquire(ctx context.Context, req Request) (*Permit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	reply := make(chan acquireReply, 1)
	select {
	case s.acquire <- acquireRequest{ctx: ctx, req: req, reply: reply}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
		return nil, ErrClosed
	}
	select {
	case result := <-reply:
		if err := ctx.Err(); err != nil && result.permit != nil {
			result.permit.Release()
			return nil, err
		}
		return result.permit, result.err
	case <-s.done:
		return nil, ErrClosed
	}
}

func (s *Scheduler) Close(ctx context.Context) error {
	s.closeOnce.Do(func() { close(s.stop) })
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Scheduler) run() {
	defer close(s.done)
	ticker := time.NewTicker(s.heartbeat)
	defer ticker.Stop()
	type activeLease struct {
		permit    *Permit
		heartbeat bool
	}
	active := make(map[string]activeLease)
	queues := make(map[string][]acquireRequest)
	for {
		select {
		case request := <-s.acquire:
			if request.req.ReadOnly {
				request.reply <- acquireReply{permit: &Permit{release: func() {}, lost: make(chan struct{})}}
				continue
			}
			repo := request.req.Repository
			if _, exists := active[repo]; exists {
				queues[repo] = append(queues[repo], request)
				continue
			}
			s.grant(request, func(repo string, permit *Permit) { active[repo] = activeLease{permit: permit, heartbeat: true} })
		case released := <-s.release:
			repo := released.repository
			_ = s.store.ReleaseLease(context.Background(), repo, s.owner)
			delete(active, repo)
			queue := queues[repo]
			for len(queue) > 0 {
				next := queue[0]
				queue = queue[1:]
				if next.ctx.Err() != nil {
					next.reply <- acquireReply{err: next.ctx.Err()}
					continue
				}
				s.grant(next, func(repo string, permit *Permit) { active[repo] = activeLease{permit: permit, heartbeat: true} })
				break
			}
			if len(queue) == 0 {
				delete(queues, repo)
			} else {
				queues[repo] = queue
			}
		case <-ticker.C:
			for repo, lease := range active {
				if !lease.heartbeat {
					continue
				}
				heartbeatCtx, cancel := context.WithTimeout(context.Background(), s.heartbeat)
				err := s.store.HeartbeatLease(heartbeatCtx, repo, s.owner, s.ttl)
				cancel()
				if err != nil {
					lease.permit.lose()
					lease.heartbeat = false
					active[repo] = lease
				}
			}
		case <-s.stop:
			for repo, lease := range active {
				lease.permit.lose()
				_ = s.store.ReleaseLease(context.Background(), repo, s.owner)
			}
			for _, queue := range queues {
				for _, request := range queue {
					request.reply <- acquireReply{err: ErrClosed}
				}
			}
			return
		}
	}
}

func (s *Scheduler) grant(request acquireRequest, activate func(string, *Permit)) {
	repo := request.req.Repository
	ok, err := s.store.AcquireLease(request.ctx, repo, s.owner, s.ttl)
	if err != nil {
		request.reply <- acquireReply{err: err}
		return
	}
	if !ok {
		request.reply <- acquireReply{err: ErrLeaseUnavailable}
		return
	}
	if err := request.ctx.Err(); err != nil {
		_ = s.store.ReleaseLease(context.Background(), repo, s.owner)
		request.reply <- acquireReply{err: err}
		return
	}
	permit := &Permit{lost: make(chan struct{})}
	permit.release = func() {
		select {
		case s.release <- releaseRequest{repository: repo}:
		case <-s.done:
		}
	}
	activate(repo, permit)
	request.reply <- acquireReply{permit: permit}
}
