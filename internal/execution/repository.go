package execution

import "context"

// Repository persists immutable execution assignments and lifecycle state.
type Repository interface {
	Create(context.Context, Execution) error
	Get(context.Context, string) (Execution, error)
	Save(context.Context, Execution) error
	ListByTask(context.Context, string) ([]Execution, error)
}
