package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/berkayahi/agentbridge/internal/security"
	"github.com/berkayahi/agentbridge/internal/store"
	"github.com/berkayahi/agentbridge/internal/task"
)

var failureRedactor = security.NewRedactor(security.Config{})

func (s *Store) SaveWorkspace(ctx context.Context, taskID, baseSHA, path string) error {
	return s.updateProjection(ctx, "save workspace", `
		UPDATE tasks SET base_sha = ?, worktree_path = ?, updated_at = ? WHERE id = ?`,
		baseSHA, path, timestamp(time.Now()), taskID,
	)
}

func (s *Store) SaveTelegramMessage(ctx context.Context, taskID string, messageID int64) error {
	return s.updateProjection(ctx, "save Telegram message", `
		UPDATE tasks SET telegram_message_id = ?, updated_at = ? WHERE id = ?`,
		messageID, timestamp(time.Now()), taskID,
	)
}

// SaveProviderSession atomically updates both the durable session and its task projection.
func (s *Store) SaveProviderSession(ctx context.Context, taskID string, session task.Session) error {
	if session.TaskID != taskID {
		return fmt.Errorf("provider session task mismatch: %w", store.ErrConflict)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin save provider session: %w", err)
	}
	defer tx.Rollback()

	if err := updateProjection(ctx, tx, "save provider session projection", `
		UPDATE tasks SET provider_session_id = ?, provider_thread_id = ?, updated_at = ? WHERE id = ?`,
		session.ProviderSessionID, session.ProviderThreadID, timestamp(session.UpdatedAt), taskID,
	); err != nil {
		return err
	}
	if err := upsertSession(ctx, tx, session); err != nil {
		return fmt.Errorf("upsert provider session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit provider session: %w", err)
	}
	return nil
}

func (s *Store) SaveDelivery(ctx context.Context, taskID, commitSHA, pushRef, deploymentURL string) error {
	return s.updateProjection(ctx, "save delivery", `
		UPDATE tasks SET commit_sha = ?, push_ref = ?, deployment_url = ?, updated_at = ? WHERE id = ?`,
		commitSHA, pushRef, deploymentURL, timestamp(time.Now()), taskID,
	)
}

func (s *Store) SaveFailure(ctx context.Context, taskID, reason string) error {
	return s.updateProjection(ctx, "save failure", `
		UPDATE tasks SET failure_reason = ?, updated_at = ? WHERE id = ?`,
		failureRedactor.RedactString(reason), timestamp(time.Now()), taskID,
	)
}

func (s *Store) updateProjection(ctx context.Context, operation, query string, args ...any) error {
	return updateProjection(ctx, s.db, operation, query, args...)
}

func updateProjection(ctx context.Context, db execer, operation, query string, args ...any) error {
	result, err := db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s result: %w", operation, err)
	}
	if changed != 1 {
		return fmt.Errorf("%s task: %w", operation, store.ErrNotFound)
	}
	return nil
}

var _ execer = (*sql.Tx)(nil)
