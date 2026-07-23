package intent

import (
	"context"
	"time"
)

// Repository is the durable intent store. Implementations must claim and
// complete with compare-and-set semantics, never with an in-memory lease.
type Repository interface {
	Create(context.Context, Intent) error
	Get(context.Context, string) (Intent, error)
	Claim(context.Context, string, string, time.Time, time.Duration) (Intent, error)
	Complete(context.Context, string, string, string, time.Time) (Intent, error)
	Reconcile(context.Context, string, string, string, time.Time) (Intent, error)
}

// Service is the application-facing durable intent boundary.
type Service struct{ repository Repository }

func NewService(repository Repository) *Service { return &Service{repository: repository} }
func (s *Service) Create(ctx context.Context, value Intent) error {
	return s.repository.Create(ctx, value)
}
func (s *Service) Get(ctx context.Context, id string) (Intent, error) {
	return s.repository.Get(ctx, id)
}
func (s *Service) Claim(ctx context.Context, id, owner string, now time.Time, lease time.Duration) (Intent, error) {
	return s.repository.Claim(ctx, id, owner, now, lease)
}
func (s *Service) Complete(ctx context.Context, id, owner, result string, now time.Time) (Intent, error) {
	return s.repository.Complete(ctx, id, owner, result, now)
}
func (s *Service) Reconcile(ctx context.Context, id, owner, progress string, now time.Time) (Intent, error) {
	return s.repository.Reconcile(ctx, id, owner, progress, now)
}
