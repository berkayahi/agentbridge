package session

import "context"

// Repository persists provider-opaque session bindings.
type Repository interface {
	Create(context.Context, Session) error
	Get(context.Context, string) (Session, error)
	Save(context.Context, Session) error
	ListResumable(context.Context) ([]Session, error)
}
