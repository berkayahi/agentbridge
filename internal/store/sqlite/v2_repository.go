package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/berkayahi/agentbridge/internal/store"
)

type v2Querier interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func v2NotFound(action string, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s: %w", action, store.ErrNotFound)
	}
	return fmt.Errorf("%s: %w", action, err)
}

func v2Conflict(action string, err error) error {
	if strings.Contains(strings.ToLower(err.Error()), "constraint failed") {
		return fmt.Errorf("%s: %w", action, store.ErrConflict)
	}
	return fmt.Errorf("%s: %w", action, err)
}

func v2Changed(action string, result sql.Result) error {
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s rows affected: %w", action, err)
	}
	if count != 1 {
		return fmt.Errorf("%s: %w", action, store.ErrConflict)
	}
	return nil
}
