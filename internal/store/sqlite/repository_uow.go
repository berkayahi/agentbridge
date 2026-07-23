package sqlite

import (
	"context"
	"fmt"

	"github.com/berkayahi/agentbridge/internal/store"
)

func (s *Store) Repositories() store.Repositories {
	return repositoriesFor(s.db)
}

func repositoriesFor(db v2Querier) store.Repositories {
	return store.Repositories{
		LocalTasks: localTaskRepository{db: db}, Executions: executionRepository{db: db},
		Sessions: sessionRepository{db: db}, Repositories: repositoryRepository{db: db},
		GitOperations: gitOperationRepository{db: db}, Events: eventRepository{db: db},
	}
}

// Within executes a v2 unit of work and commits every repository mutation and
// durable event together.
func (s *Store) Within(ctx context.Context, fn func(store.Repositories) error) error {
	if fn == nil {
		return fmt.Errorf("sqlite unit of work: callback is nil")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin v2 unit of work: %w", err)
	}
	defer tx.Rollback()
	if err := fn(repositoriesFor(tx)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit v2 unit of work: %w", err)
	}
	return nil
}

var _ store.UnitOfWork = (*Store)(nil)
