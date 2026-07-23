package sqlite

import (
	"context"
	"fmt"

	"github.com/berkayahi/agentbridge/internal/repository"
)

type repositoryRepository struct{ db v2Querier }

func (r repositoryRepository) CreateBinding(ctx context.Context, value repository.Binding) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO repository_bindings (id, remote_url, created_at) VALUES (?, ?, ?)`, value.ID(), value.RemoteURL(), timestamp(value.CreatedAt()))
	if err != nil {
		return v2Conflict("create repository binding", err)
	}
	return nil
}

func (r repositoryRepository) GetBinding(ctx context.Context, id string) (repository.Binding, error) {
	var input repository.BindingInput
	var created string
	if err := r.db.QueryRowContext(ctx, `SELECT id, remote_url, created_at FROM repository_bindings WHERE id = ?`, id).Scan(&input.ID, &input.RemoteURL, &created); err != nil {
		return repository.Binding{}, v2NotFound("get repository binding", err)
	}
	var err error
	input.CreatedAt, err = parseTimestamp(created)
	if err != nil {
		return repository.Binding{}, err
	}
	return repository.NewBinding(input)
}

func (r repositoryRepository) CreateCheckpoint(ctx context.Context, value repository.Checkpoint) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO git_checkpoints (id, execution_id, repository_id, commit_sha, remote_ref, created_at) VALUES (?, '', ?, ?, ?, ?)`, value.ID(), value.RepositoryID(), value.CommitSHA(), value.RemoteRef(), timestamp(value.CreatedAt()))
	if err != nil {
		return v2Conflict("create repository checkpoint", err)
	}
	return nil
}

func (r repositoryRepository) ListCheckpoints(ctx context.Context, repositoryID string) ([]repository.Checkpoint, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, repository_id, commit_sha, remote_ref, created_at FROM git_checkpoints WHERE repository_id = ? ORDER BY created_at, id`, repositoryID)
	if err != nil {
		return nil, fmt.Errorf("list repository checkpoints: %w", err)
	}
	defer rows.Close()
	var values []repository.Checkpoint
	for rows.Next() {
		var input repository.CheckpointInput
		var created string
		if err := rows.Scan(&input.ID, &input.RepositoryID, &input.CommitSHA, &input.RemoteRef, &created); err != nil {
			return nil, fmt.Errorf("scan repository checkpoint: %w", err)
		}
		var err error
		input.CreatedAt, err = parseTimestamp(created)
		if err != nil {
			return nil, err
		}
		value, err := repository.NewCheckpoint(input)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

var _ repository.Repository = repositoryRepository{}
