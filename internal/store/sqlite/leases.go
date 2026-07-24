package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/berkayahi/agentbridge/internal/store"
)

func (s *LegacyStore) AcquireLease(ctx context.Context, repoProfileID, ownerID string, ttl time.Duration) (bool, error) {
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO repository_leases (repo_profile_id, owner_id, acquired_at, heartbeat_at, expires_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(repo_profile_id) DO UPDATE SET
			owner_id = excluded.owner_id,
			acquired_at = excluded.acquired_at,
			heartbeat_at = excluded.heartbeat_at,
			expires_at = excluded.expires_at
		WHERE repository_leases.expires_at <= excluded.acquired_at
		   OR repository_leases.owner_id = excluded.owner_id`,
		repoProfileID, ownerID, timestamp(now), timestamp(now), timestamp(now.Add(ttl)),
	)
	if err != nil {
		return false, fmt.Errorf("acquire lease: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read acquire lease result: %w", err)
	}
	return changed == 1, nil
}

func (s *LegacyStore) HeartbeatLease(ctx context.Context, repoProfileID, ownerID string, ttl time.Duration) error {
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `
		UPDATE repository_leases SET heartbeat_at = ?, expires_at = ?
		WHERE repo_profile_id = ? AND owner_id = ?`,
		timestamp(now), timestamp(now.Add(ttl)), repoProfileID, ownerID,
	)
	if err != nil {
		return fmt.Errorf("heartbeat lease: %w", err)
	}
	return requireLeaseOwner(result, "heartbeat")
}

func (s *LegacyStore) ReleaseLease(ctx context.Context, repoProfileID, ownerID string) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM repository_leases WHERE repo_profile_id = ? AND owner_id = ?", repoProfileID, ownerID)
	if err != nil {
		return fmt.Errorf("release lease: %w", err)
	}
	return requireLeaseOwner(result, "release")
}

func requireLeaseOwner(result interface{ RowsAffected() (int64, error) }, action string) error {
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read %s lease result: %w", action, err)
	}
	if changed != 1 {
		return fmt.Errorf("%s lease ownership: %w", action, store.ErrConflict)
	}
	return nil
}

func (s *LegacyStore) ExpiredLeases(ctx context.Context) ([]store.Lease, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT repo_profile_id, owner_id, acquired_at, heartbeat_at, expires_at
		FROM repository_leases WHERE expires_at <= ? ORDER BY expires_at, repo_profile_id`, timestamp(time.Now()))
	if err != nil {
		return nil, fmt.Errorf("query expired leases: %w", err)
	}
	defer rows.Close()
	var values []store.Lease
	for rows.Next() {
		var value store.Lease
		var acquired, heartbeat, expires string
		if err := rows.Scan(&value.RepoProfileID, &value.OwnerID, &acquired, &heartbeat, &expires); err != nil {
			return nil, fmt.Errorf("scan lease: %w", err)
		}
		if value.AcquiredAt, err = parseTimestamp(acquired); err != nil {
			return nil, err
		}
		if value.HeartbeatAt, err = parseTimestamp(heartbeat); err != nil {
			return nil, err
		}
		if value.ExpiresAt, err = parseTimestamp(expires); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}
