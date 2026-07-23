package localtask

import "context"

// Repository persists local task identity and its active-execution pointer.
type Repository interface {
	Create(context.Context, LocalTask) error
	Get(context.Context, string) (LocalTask, error)
	Save(context.Context, LocalTask) error
}
