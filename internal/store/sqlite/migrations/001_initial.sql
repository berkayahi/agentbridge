CREATE TABLE IF NOT EXISTS tasks (
    id TEXT PRIMARY KEY,
    repo_profile_id TEXT NOT NULL,
    title TEXT NOT NULL,
    prompt TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('queued','preparing','running','awaiting_approval','awaiting_auth','verifying','committing','pushing','completed','failed','canceled','paused')),
    provider TEXT NOT NULL CHECK (provider IN ('codex','claude')),
    telegram_chat_id INTEGER NOT NULL,
    telegram_message_id INTEGER NOT NULL,
    base_sha TEXT NOT NULL,
    worktree_path TEXT NOT NULL,
    provider_session_id TEXT NOT NULL DEFAULT '',
    provider_thread_id TEXT NOT NULL DEFAULT '',
    commit_sha TEXT NOT NULL DEFAULT '',
    push_ref TEXT NOT NULL DEFAULT '',
    deployment_url TEXT NOT NULL DEFAULT '',
    failure_reason TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    started_at TEXT,
    finished_at TEXT
);

CREATE INDEX IF NOT EXISTS tasks_state_created_idx ON tasks(state, created_at);
CREATE INDEX IF NOT EXISTS tasks_repo_created_idx ON tasks(repo_profile_id, created_at);

CREATE TABLE IF NOT EXISTS task_events (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    event_type TEXT NOT NULL,
    visibility TEXT NOT NULL CHECK (visibility IN ('internal','user')),
    provider_event_id TEXT,
    redacted_payload BLOB NOT NULL,
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS task_events_task_order_idx ON task_events(task_id, created_at, id);
CREATE UNIQUE INDEX IF NOT EXISTS task_events_provider_dedupe_idx
    ON task_events(task_id, provider_event_id) WHERE provider_event_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS attachments (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    name TEXT NOT NULL,
    media_type TEXT NOT NULL,
    storage_path TEXT NOT NULL,
    size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS attachments_task_idx ON attachments(task_id, created_at);

CREATE TABLE IF NOT EXISTS approvals (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending','approved','rejected','expired')),
    request_payload BLOB NOT NULL,
    decision_payload BLOB,
    requested_at TEXT NOT NULL,
    expires_at TEXT,
    resolved_at TEXT
);
CREATE INDEX IF NOT EXISTS approvals_pending_idx ON approvals(status, requested_at);

CREATE TABLE IF NOT EXISTS sessions (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    provider TEXT NOT NULL CHECK (provider IN ('codex','claude')),
    provider_session_id TEXT NOT NULL,
    provider_thread_id TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    resumable INTEGER NOT NULL CHECK (resumable IN (0,1)),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_resumable_idx ON sessions(resumable, updated_at);

CREATE TABLE IF NOT EXISTS repository_leases (
    repo_profile_id TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL,
    acquired_at TEXT NOT NULL,
    heartbeat_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS repository_leases_expiry_idx ON repository_leases(expires_at);

CREATE TABLE IF NOT EXISTS auth_incidents (
    id TEXT PRIMARY KEY,
    task_id TEXT REFERENCES tasks(id) ON DELETE SET NULL,
    provider TEXT NOT NULL CHECK (provider IN ('codex','claude')),
    status TEXT NOT NULL,
    redacted_detail BLOB NOT NULL,
    detected_at TEXT NOT NULL,
    resolved_at TEXT
);
CREATE INDEX IF NOT EXISTS auth_incidents_status_idx ON auth_incidents(status, detected_at);
