// Package scheduler serializes mutating work per repository.
package scheduler

import (
	"context"
	"errors"
	"sync"
	"time"
)

var ErrClosed = errors.New("scheduler: closed")

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
	release func()
	once    sync.Once
}

func (p *Permit) Release() {
	if p != nil {
		p.once.Do(p.release)
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
		return result.permit, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
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
	active := make(map[string]bool)
	queues := make(map[string][]acquireRequest)
	for {
		select {
		case request := <-s.acquire:
			if request.req.ReadOnly {
				request.reply <- acquireReply{permit: &Permit{release: func() {}}}
				continue
			}
			repo := request.req.Repository
			if active[repo] {
				queues[repo] = append(queues[repo], request)
				continue
			}
			s.grant(request, active)
		case released := <-s.release:
			repo := released.repository
			_ = s.store.ReleaseLease(context.Background(), repo, s.owner)
			delete(active, repo)
			queue := queues[repo]
			for len(queue) > 0 {
				next := queue[0]
				queue = queue[1:]
				if next.ctx.Err() != nil {
					continue
				}
				s.grant(next, active)
				break
			}
			if len(queue) == 0 {
				delete(queues, repo)
			} else {
				queues[repo] = queue
			}
		case <-ticker.C:
			for repo := range active {
				_ = s.store.HeartbeatLease(context.Background(), repo, s.owner, s.ttl)
			}
		case <-s.stop:
			for repo := range active {
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

func (s *Scheduler) grant(request acquireRequest, active map[string]bool) {
	repo := request.req.Repository
	ok, err := s.store.AcquireLease(request.ctx, repo, s.owner, s.ttl)
	if err != nil {
		request.reply <- acquireReply{err: err}
		return
	}
	if !ok {
		request.reply <- acquireReply{err: errors.New("scheduler: repository lease unavailable")}
		return
	}
	active[repo] = true
	request.reply <- acquireReply{permit: &Permit{release: func() {
		select {
		case s.release <- releaseRequest{repository: repo}:
		case <-s.done:
		}
	}}}
}
