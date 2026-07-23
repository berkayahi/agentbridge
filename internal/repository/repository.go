package repository

import "context"

// Repository persists locally approved repository bindings and Git evidence.
type Repository interface {
	CreateBinding(context.Context, Binding) error
	GetBinding(context.Context, string) (Binding, error)
	CreateCheckpoint(context.Context, Checkpoint) error
	ListCheckpoints(context.Context, string) ([]Checkpoint, error)
}
