-- Durable, idempotent read-only repository understanding operations. Provider
-- output is retained only after strict schema validation and never as a raw
-- transcript.
CREATE TABLE repository_understanding_operations (
    id TEXT PRIMARY KEY,
    idempotency_key TEXT NOT NULL UNIQUE,
    repository_profile_id TEXT NOT NULL,
    expected_commit_sha TEXT NOT NULL,
    role TEXT NOT NULL,
    request_digest TEXT NOT NULL,
    result_digest TEXT NOT NULL,
    status TEXT NOT NULL,
    response_payload BLOB NOT NULL CHECK (json_valid(response_payload)),
    requested_at TEXT NOT NULL,
    completed_at TEXT NOT NULL
);

CREATE INDEX repository_understanding_profile_commit_idx
    ON repository_understanding_operations(repository_profile_id, expected_commit_sha);
