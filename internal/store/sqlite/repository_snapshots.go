package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/berkayahi/agentbridge/internal/repositorysnapshot"
	"github.com/berkayahi/agentbridge/internal/store"
)

func (s *RuntimeStore) LoadRepositorySnapshot(ctx context.Context, idempotencyKey string) (repositorysnapshot.Operation, error) {
	var operation repositorysnapshot.Operation
	var response []byte
	var requestedAt, completedAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, idempotency_key, repository_profile_id, requested_ref,
		       scoped_root, analyzer_version, request_digest, exact_commit_sha,
		       result_digest, status, response_payload, requested_at, completed_at
		FROM repository_snapshot_operations
		WHERE idempotency_key = ?`, idempotencyKey,
	).Scan(
		&operation.ID, &operation.IdempotencyKey, &operation.RepositoryProfileID,
		&operation.RequestedRef, &operation.ScopedRoot, &operation.AnalyzerVersion,
		&operation.RequestDigest, &operation.ExactCommitSHA, &operation.ResultDigest,
		&operation.Status, &response, &requestedAt, &completedAt,
	)
	if err != nil {
		return repositorysnapshot.Operation{}, runtimeNotFound("load repository snapshot", err)
	}
	if err := json.Unmarshal(response, &operation.Response); err != nil {
		return repositorysnapshot.Operation{}, fmt.Errorf("decode repository snapshot response: %w", err)
	}
	if operation.RequestedAt, err = parseTimestamp(requestedAt); err != nil {
		return repositorysnapshot.Operation{}, err
	}
	if operation.CompletedAt, err = parseTimestamp(completedAt); err != nil {
		return repositorysnapshot.Operation{}, err
	}
	return operation, nil
}

func (s *RuntimeStore) SaveRepositorySnapshot(ctx context.Context, operation repositorysnapshot.Operation) error {
	if operation.ID == "" || operation.IdempotencyKey == "" ||
		operation.RepositoryProfileID == "" || operation.RequestedRef == "" ||
		operation.ScopedRoot == "" || operation.AnalyzerVersion == "" ||
		operation.RequestDigest == "" || operation.ExactCommitSHA == "" ||
		operation.ResultDigest == "" || operation.Status != "completed" ||
		operation.RequestedAt.IsZero() || operation.CompletedAt.IsZero() {
		return fmt.Errorf("save repository snapshot: %w", repositorysnapshot.ErrInvalidRequest)
	}
	response, err := json.Marshal(operation.Response)
	if err != nil {
		return fmt.Errorf("encode repository snapshot response: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO repository_snapshot_operations (
			id, idempotency_key, repository_profile_id, requested_ref,
			scoped_root, analyzer_version, request_digest, exact_commit_sha,
			result_digest, status, response_payload, requested_at, completed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		operation.ID, operation.IdempotencyKey, operation.RepositoryProfileID,
		operation.RequestedRef, operation.ScopedRoot, operation.AnalyzerVersion,
		operation.RequestDigest, operation.ExactCommitSHA, operation.ResultDigest,
		operation.Status, response, timestamp(operation.RequestedAt), timestamp(operation.CompletedAt),
	)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique constraint") {
			return fmt.Errorf("save repository snapshot: %w", store.ErrConflict)
		}
		return runtimeConflict("save repository snapshot", err)
	}
	return nil
}
