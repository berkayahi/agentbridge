-- 021_repository_snapshots.sql
-- Generic, restart-safe read-only repository inspection operations. This
-- stores AgentBridge operation/audit state only; downstream analysis artifacts
-- remain outside this schema.
CREATE TABLE repository_snapshot_operations (
    id TEXT PRIMARY KEY,
    idempotency_key TEXT NOT NULL UNIQUE,
    repository_profile_id TEXT NOT NULL,
    requested_ref TEXT NOT NULL,
    scoped_root TEXT NOT NULL,
    analyzer_version TEXT NOT NULL,
    request_digest TEXT NOT NULL,
    exact_commit_sha TEXT NOT NULL,
    result_digest TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status = 'completed'),
    response_payload BLOB NOT NULL CHECK (json_valid(response_payload)),
    requested_at TEXT NOT NULL,
    completed_at TEXT NOT NULL
);

CREATE INDEX repository_snapshot_profile_commit_idx
    ON repository_snapshot_operations(repository_profile_id, exact_commit_sha);
