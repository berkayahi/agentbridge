package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/berkayahi/agentbridge/internal/repositorysnapshot"
	"github.com/berkayahi/agentbridge/internal/store"
)

func (s *RuntimeStore) LoadRepositoryUnderstanding(ctx context.Context, idempotencyKey string) (repositorysnapshot.UnderstandingOperation, error) {
	return s.loadRepositoryUnderstanding(ctx, "idempotency_key", idempotencyKey)
}

func (s *RuntimeStore) LoadRepositoryUnderstandingByID(ctx context.Context, operationID string) (repositorysnapshot.UnderstandingOperation, error) {
	return s.loadRepositoryUnderstanding(ctx, "id", operationID)
}

func (s *RuntimeStore) loadRepositoryUnderstanding(ctx context.Context, column, value string) (repositorysnapshot.UnderstandingOperation, error) {
	var operation repositorysnapshot.UnderstandingOperation
	var response []byte
	var requestedAt, completedAt string
	query := `
		SELECT id, idempotency_key, repository_profile_id, expected_commit_sha,
		       role, request_digest, result_digest, status, response_payload,
		       requested_at, completed_at
		FROM repository_understanding_operations
		WHERE ` + column + ` = ?`
	err := s.db.QueryRowContext(ctx, query, value).Scan(
		&operation.ID, &operation.IdempotencyKey, &operation.RepositoryProfileID,
		&operation.ExpectedCommitSHA, &operation.Role, &operation.RequestDigest,
		&operation.ResultDigest, &operation.Status, &response, &requestedAt, &completedAt,
	)
	if err != nil {
		return repositorysnapshot.UnderstandingOperation{}, runtimeNotFound("load repository understanding", err)
	}
	if err := json.Unmarshal(response, &operation.Response); err != nil {
		return repositorysnapshot.UnderstandingOperation{}, fmt.Errorf("decode repository understanding response: %w", err)
	}
	if operation.RequestedAt, err = parseTimestamp(requestedAt); err != nil {
		return repositorysnapshot.UnderstandingOperation{}, err
	}
	if operation.CompletedAt, err = parseTimestamp(completedAt); err != nil {
		return repositorysnapshot.UnderstandingOperation{}, err
	}
	return operation, nil
}

func (s *RuntimeStore) SaveRepositoryUnderstanding(ctx context.Context, operation repositorysnapshot.UnderstandingOperation) error {
	if operation.ID == "" || operation.IdempotencyKey == "" || operation.RepositoryProfileID == "" || operation.ExpectedCommitSHA == "" || !operation.Role.Valid() || operation.RequestDigest == "" || operation.ResultDigest == "" || operation.Status == "" || operation.RequestedAt.IsZero() || operation.CompletedAt.IsZero() {
		return fmt.Errorf("save repository understanding: %w", repositorysnapshot.ErrInvalidRequest)
	}
	response, err := json.Marshal(operation.Response)
	if err != nil {
		return fmt.Errorf("encode repository understanding response: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO repository_understanding_operations (
		    id, idempotency_key, repository_profile_id, expected_commit_sha,
		    role, request_digest, result_digest, status, response_payload,
		    requested_at, completed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		operation.ID, operation.IdempotencyKey, operation.RepositoryProfileID,
		operation.ExpectedCommitSHA, operation.Role, operation.RequestDigest,
		operation.ResultDigest, operation.Status, response,
		timestamp(operation.RequestedAt), timestamp(operation.CompletedAt),
	)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique constraint") {
			return fmt.Errorf("save repository understanding: %w", store.ErrConflict)
		}
		return runtimeConflict("save repository understanding", err)
	}
	return nil
}
