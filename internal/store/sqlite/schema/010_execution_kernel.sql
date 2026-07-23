CREATE TABLE migration_ledger (
    version INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    checksum TEXT NOT NULL,
    structural_fingerprint TEXT NOT NULL,
    applied_at TEXT NOT NULL
);

CREATE TABLE repository_bindings (
    id TEXT PRIMARY KEY,
    remote_url TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE repository_leases (
    repo_profile_id TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL,
    acquired_at TEXT NOT NULL,
    heartbeat_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
);

CREATE TABLE local_tasks (
    id TEXT PRIMARY KEY,
    repo_profile_id TEXT NOT NULL DEFAULT '',
    title TEXT NOT NULL,
    prompt TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'queued',
    provider TEXT NOT NULL DEFAULT '',
    active_execution_id TEXT,
    base_sha TEXT NOT NULL DEFAULT '',
    worktree_path TEXT NOT NULL DEFAULT '',
    provider_session_id TEXT NOT NULL DEFAULT '',
    provider_thread_id TEXT NOT NULL DEFAULT '',
    commit_sha TEXT NOT NULL DEFAULT '',
    push_ref TEXT NOT NULL DEFAULT '',
    deployment_url TEXT NOT NULL DEFAULT '',
    failure_reason TEXT NOT NULL DEFAULT '',
    started_at TEXT,
    finished_at TEXT,
    revision INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE INDEX local_tasks_created_idx ON local_tasks(created_at, id);
CREATE INDEX local_tasks_state_idx ON local_tasks(state, updated_at, id);

-- Presentation identifiers belong to an adapter boundary, never to the
-- canonical local task record.
CREATE TABLE task_presentations (
    local_task_id TEXT PRIMARY KEY REFERENCES local_tasks(id) ON DELETE CASCADE,
    telegram_chat_id INTEGER NOT NULL DEFAULT 0,
    telegram_message_id INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE sessions (
    id TEXT PRIMARY KEY,
    runtime_id TEXT NOT NULL,
    repository_id TEXT NOT NULL REFERENCES repository_bindings(id),
    local_task_id TEXT NOT NULL REFERENCES local_tasks(id),
    active_local_task_id TEXT REFERENCES local_tasks(id),
    provider_session_id TEXT NOT NULL DEFAULT '',
    provider_thread_id TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT '',
    resumable INTEGER NOT NULL DEFAULT 0 CHECK (resumable IN (0,1)),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX sessions_one_active_task_idx ON sessions(active_local_task_id) WHERE active_local_task_id IS NOT NULL;

CREATE TABLE executions (
    id TEXT PRIMARY KEY,
    local_task_id TEXT NOT NULL REFERENCES local_tasks(id),
    session_id TEXT NOT NULL REFERENCES sessions(id),
    runtime_id TEXT NOT NULL,
    repository_id TEXT NOT NULL REFERENCES repository_bindings(id),
    retry_of_execution_id TEXT REFERENCES executions(id),
    state TEXT NOT NULL,
    attempt INTEGER NOT NULL CHECK (attempt >= 0),
    fencing_epoch INTEGER NOT NULL CHECK (fencing_epoch > 0),
    command_id TEXT NOT NULL,
    max_transient_attempts INTEGER NOT NULL CHECK (max_transient_attempts >= 0),
    policy_snapshot BLOB NOT NULL,
    source_state TEXT NOT NULL DEFAULT '',
    started_at TEXT,
    finished_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX executions_one_active_task_idx
    ON executions(local_task_id)
    WHERE state NOT IN ('completed', 'failed', 'canceled');

CREATE TABLE git_checkpoints (
    id TEXT PRIMARY KEY,
    execution_id TEXT NOT NULL REFERENCES executions(id),
    repository_id TEXT NOT NULL REFERENCES repository_bindings(id),
    commit_sha TEXT NOT NULL,
    remote_ref TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE git_operations (
    id TEXT PRIMARY KEY,
    execution_id TEXT NOT NULL REFERENCES executions(id),
    kind TEXT NOT NULL,
    target_ref TEXT NOT NULL,
    expected_old_sha TEXT NOT NULL,
    idempotency_key TEXT NOT NULL UNIQUE,
    state TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE execution_events (
    id TEXT PRIMARY KEY,
    local_task_id TEXT REFERENCES local_tasks(id) ON DELETE CASCADE,
    execution_id TEXT NOT NULL REFERENCES executions(id) ON DELETE CASCADE,
    event_type TEXT NOT NULL,
    visibility TEXT NOT NULL,
    provider_event_id TEXT,
    redacted_payload BLOB NOT NULL,
    created_at TEXT NOT NULL
);
CREATE UNIQUE INDEX execution_events_provider_dedupe_idx
    ON execution_events(execution_id, provider_event_id) WHERE provider_event_id IS NOT NULL;

CREATE TABLE approvals (
    id TEXT PRIMARY KEY,
    local_task_id TEXT REFERENCES local_tasks(id) ON DELETE CASCADE,
    execution_id TEXT NOT NULL REFERENCES executions(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    status TEXT NOT NULL,
    request_payload BLOB NOT NULL,
    decision_payload BLOB,
    requested_at TEXT NOT NULL,
    expires_at TEXT,
    resolved_at TEXT
);

CREATE TABLE attachments (
    id TEXT PRIMARY KEY,
    local_task_id TEXT REFERENCES local_tasks(id) ON DELETE CASCADE,
    execution_id TEXT NOT NULL REFERENCES executions(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    name TEXT NOT NULL,
    media_type TEXT NOT NULL,
    storage_path TEXT NOT NULL,
    size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
    sha256 TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);

CREATE TABLE command_inbox (
    id TEXT PRIMARY KEY,
    execution_id TEXT NOT NULL REFERENCES executions(id),
    fencing_epoch INTEGER NOT NULL,
    idempotency_key TEXT NOT NULL UNIQUE,
    policy_snapshot BLOB NOT NULL,
    expires_at TEXT NOT NULL,
    accepted_at TEXT
);

CREATE TABLE command_results (
    command_id TEXT PRIMARY KEY REFERENCES command_inbox(id) ON DELETE CASCADE,
    result_payload BLOB NOT NULL,
    recorded_at TEXT NOT NULL
);

CREATE TABLE spool_metadata (
    key TEXT PRIMARY KEY,
    value BLOB NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE auth_incidents (
    id TEXT PRIMARY KEY,
    local_task_id TEXT REFERENCES local_tasks(id) ON DELETE SET NULL,
    execution_id TEXT REFERENCES executions(id) ON DELETE SET NULL,
    provider TEXT NOT NULL,
    status TEXT NOT NULL,
    redacted_detail BLOB NOT NULL,
    detected_at TEXT NOT NULL,
    resolved_at TEXT
);

CREATE TABLE intent_evidence (
    id TEXT PRIMARY KEY,
    execution_id TEXT REFERENCES executions(id) ON DELETE SET NULL,
    kind TEXT NOT NULL,
    runtime_id TEXT NOT NULL,
    target_task_id TEXT NOT NULL DEFAULT '',
    result_task_id TEXT NOT NULL DEFAULT '',
    payload_ref TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL,
    claim_owner TEXT NOT NULL DEFAULT '',
    safe_progress TEXT NOT NULL DEFAULT '',
    safe_result TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    claimed_at TEXT,
    lease_expires_at TEXT,
    completed_at TEXT
);

CREATE TABLE retry_evidence (
    id TEXT PRIMARY KEY,
    execution_id TEXT REFERENCES executions(id) ON DELETE SET NULL,
    task_id TEXT NOT NULL,
    failure_class TEXT NOT NULL,
    attempt INTEGER NOT NULL CHECK (attempt >= 0),
    next_attempt_at TEXT NOT NULL,
    last_checkpoint_at TEXT NOT NULL,
    safe_summary TEXT NOT NULL,
    status TEXT NOT NULL,
    claim_owner TEXT NOT NULL DEFAULT '',
    claimed_at TEXT,
    lease_expires_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
