package gitoperation

import "context"

// Repository persists typed Git operation intent and receipts' lifecycle.
type Repository interface {
	Create(context.Context, Operation) error
	Get(context.Context, string) (Operation, error)
	Save(context.Context, Operation) error
	GetByIdempotencyKey(context.Context, string) (Operation, error)
}
