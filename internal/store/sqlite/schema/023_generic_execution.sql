CREATE TABLE IF NOT EXISTS generic_executions (
    execution_id TEXT PRIMARY KEY,
    idempotency_key TEXT NOT NULL UNIQUE,
    request_digest TEXT NOT NULL,
    request_json TEXT NOT NULL CHECK (json_valid(request_json)),
    result_json TEXT CHECK (result_json IS NULL OR json_valid(result_json)),
    state TEXT NOT NULL CHECK (state IN ('accepted', 'running', 'completed', 'failed', 'canceled', 'human_required')),
    revision INTEGER NOT NULL DEFAULT 1 CHECK (revision > 0),
    recovery_count INTEGER NOT NULL DEFAULT 0 CHECK (recovery_count >= 0),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS generic_executions_state_idx
    ON generic_executions (state, updated_at);

CREATE TABLE IF NOT EXISTS resource_leases (
    lease_id TEXT PRIMARY KEY,
    resource_key TEXT NOT NULL,
    owner_execution_id TEXT NOT NULL REFERENCES generic_executions (execution_id) ON DELETE CASCADE,
    mode TEXT NOT NULL CHECK (mode IN ('exclusive', 'shared')),
    acquired_at TEXT NOT NULL,
    heartbeat_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    revision INTEGER NOT NULL DEFAULT 1 CHECK (revision > 0)
);

CREATE INDEX IF NOT EXISTS resource_leases_resource_idx
    ON resource_leases (resource_key, expires_at);

CREATE INDEX IF NOT EXISTS resource_leases_owner_idx
    ON resource_leases (owner_execution_id, expires_at);
