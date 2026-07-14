-- AgentBridge versions before schema migration 2 encoded timestamps with
-- time.RFC3339Nano. That encoder always emitted a UTC `Z` suffix but omitted
-- trailing fractional zeroes. SQLite compares TEXT lexicographically, so a
-- later `.1Z` value sorted before an exact-second `Z` value. Validate that the
-- legacy encoder's UTC contract holds, then preserve all available precision
-- while padding the fraction to exactly nine digits.

CREATE TEMP TABLE timestamp_migration_validation (
    value TEXT NOT NULL CHECK (
        substr(value, 5, 1) = '-'
        AND substr(value, 8, 1) = '-'
        AND substr(value, 11, 1) = 'T'
        AND substr(value, 14, 1) = ':'
        AND substr(value, 17, 1) = ':'
        AND substr(value, -1, 1) = 'Z'
        AND (
            length(value) = 20
            OR (
                length(value) BETWEEN 22 AND 30
                AND substr(value, 20, 1) = '.'
                AND substr(value, 21, length(value) - 21) NOT GLOB '*[^0-9]*'
            )
        )
    )
);

INSERT INTO timestamp_migration_validation(value)
SELECT created_at FROM tasks
UNION ALL SELECT updated_at FROM tasks
UNION ALL SELECT started_at FROM tasks WHERE started_at IS NOT NULL
UNION ALL SELECT finished_at FROM tasks WHERE finished_at IS NOT NULL
UNION ALL SELECT created_at FROM task_events
UNION ALL SELECT created_at FROM attachments
UNION ALL SELECT requested_at FROM approvals
UNION ALL SELECT expires_at FROM approvals WHERE expires_at IS NOT NULL
UNION ALL SELECT resolved_at FROM approvals WHERE resolved_at IS NOT NULL
UNION ALL SELECT created_at FROM sessions
UNION ALL SELECT updated_at FROM sessions
UNION ALL SELECT acquired_at FROM repository_leases
UNION ALL SELECT heartbeat_at FROM repository_leases
UNION ALL SELECT expires_at FROM repository_leases
UNION ALL SELECT detected_at FROM auth_incidents
UNION ALL SELECT resolved_at FROM auth_incidents WHERE resolved_at IS NOT NULL;

UPDATE tasks SET
    created_at = substr(created_at, 1, 19) || '.' || substr(CASE WHEN substr(created_at, 20, 1) = '.' THEN substr(created_at, 21, length(created_at) - 21) ELSE '' END || '000000000', 1, 9) || 'Z',
    updated_at = substr(updated_at, 1, 19) || '.' || substr(CASE WHEN substr(updated_at, 20, 1) = '.' THEN substr(updated_at, 21, length(updated_at) - 21) ELSE '' END || '000000000', 1, 9) || 'Z',
    started_at = CASE WHEN started_at IS NULL THEN NULL ELSE substr(started_at, 1, 19) || '.' || substr(CASE WHEN substr(started_at, 20, 1) = '.' THEN substr(started_at, 21, length(started_at) - 21) ELSE '' END || '000000000', 1, 9) || 'Z' END,
    finished_at = CASE WHEN finished_at IS NULL THEN NULL ELSE substr(finished_at, 1, 19) || '.' || substr(CASE WHEN substr(finished_at, 20, 1) = '.' THEN substr(finished_at, 21, length(finished_at) - 21) ELSE '' END || '000000000', 1, 9) || 'Z' END;

UPDATE task_events SET created_at = substr(created_at, 1, 19) || '.' || substr(CASE WHEN substr(created_at, 20, 1) = '.' THEN substr(created_at, 21, length(created_at) - 21) ELSE '' END || '000000000', 1, 9) || 'Z';
UPDATE attachments SET created_at = substr(created_at, 1, 19) || '.' || substr(CASE WHEN substr(created_at, 20, 1) = '.' THEN substr(created_at, 21, length(created_at) - 21) ELSE '' END || '000000000', 1, 9) || 'Z';

UPDATE approvals SET
    requested_at = substr(requested_at, 1, 19) || '.' || substr(CASE WHEN substr(requested_at, 20, 1) = '.' THEN substr(requested_at, 21, length(requested_at) - 21) ELSE '' END || '000000000', 1, 9) || 'Z',
    expires_at = CASE WHEN expires_at IS NULL THEN NULL ELSE substr(expires_at, 1, 19) || '.' || substr(CASE WHEN substr(expires_at, 20, 1) = '.' THEN substr(expires_at, 21, length(expires_at) - 21) ELSE '' END || '000000000', 1, 9) || 'Z' END,
    resolved_at = CASE WHEN resolved_at IS NULL THEN NULL ELSE substr(resolved_at, 1, 19) || '.' || substr(CASE WHEN substr(resolved_at, 20, 1) = '.' THEN substr(resolved_at, 21, length(resolved_at) - 21) ELSE '' END || '000000000', 1, 9) || 'Z' END;

UPDATE sessions SET
    created_at = substr(created_at, 1, 19) || '.' || substr(CASE WHEN substr(created_at, 20, 1) = '.' THEN substr(created_at, 21, length(created_at) - 21) ELSE '' END || '000000000', 1, 9) || 'Z',
    updated_at = substr(updated_at, 1, 19) || '.' || substr(CASE WHEN substr(updated_at, 20, 1) = '.' THEN substr(updated_at, 21, length(updated_at) - 21) ELSE '' END || '000000000', 1, 9) || 'Z';

UPDATE repository_leases SET
    acquired_at = substr(acquired_at, 1, 19) || '.' || substr(CASE WHEN substr(acquired_at, 20, 1) = '.' THEN substr(acquired_at, 21, length(acquired_at) - 21) ELSE '' END || '000000000', 1, 9) || 'Z',
    heartbeat_at = substr(heartbeat_at, 1, 19) || '.' || substr(CASE WHEN substr(heartbeat_at, 20, 1) = '.' THEN substr(heartbeat_at, 21, length(heartbeat_at) - 21) ELSE '' END || '000000000', 1, 9) || 'Z',
    expires_at = substr(expires_at, 1, 19) || '.' || substr(CASE WHEN substr(expires_at, 20, 1) = '.' THEN substr(expires_at, 21, length(expires_at) - 21) ELSE '' END || '000000000', 1, 9) || 'Z';

UPDATE auth_incidents SET
    detected_at = substr(detected_at, 1, 19) || '.' || substr(CASE WHEN substr(detected_at, 20, 1) = '.' THEN substr(detected_at, 21, length(detected_at) - 21) ELSE '' END || '000000000', 1, 9) || 'Z',
    resolved_at = CASE WHEN resolved_at IS NULL THEN NULL ELSE substr(resolved_at, 1, 19) || '.' || substr(CASE WHEN substr(resolved_at, 20, 1) = '.' THEN substr(resolved_at, 21, length(resolved_at) - 21) ELSE '' END || '000000000', 1, 9) || 'Z' END;

DROP TABLE timestamp_migration_validation;
