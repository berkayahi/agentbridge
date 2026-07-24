CREATE TABLE local_projects (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    revision INTEGER NOT NULL DEFAULT 1 CHECK (revision > 0),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE local_boards (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES local_projects(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    revision INTEGER NOT NULL DEFAULT 1 CHECK (revision > 0),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX local_boards_project_idx ON local_boards(project_id, created_at, id);

CREATE TABLE local_task_contexts (
    local_task_id TEXT PRIMARY KEY REFERENCES local_tasks(id) ON DELETE CASCADE,
    project_id TEXT NOT NULL REFERENCES local_projects(id) ON DELETE RESTRICT,
    board_id TEXT NOT NULL REFERENCES local_boards(id) ON DELETE RESTRICT,
    created_at TEXT NOT NULL
);
CREATE INDEX local_task_contexts_board_idx ON local_task_contexts(board_id, local_task_id);

CREATE TABLE local_control_events (
    cursor INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    resource_type TEXT NOT NULL,
    resource_id TEXT NOT NULL,
    local_task_id TEXT REFERENCES local_tasks(id) ON DELETE CASCADE,
    revision INTEGER NOT NULL CHECK (revision > 0),
    event_type TEXT NOT NULL,
    payload BLOB NOT NULL,
    created_at TEXT NOT NULL
);
CREATE INDEX local_control_events_task_cursor_idx ON local_control_events(local_task_id, cursor);

CREATE TABLE local_command_idempotency (
    idempotency_key TEXT PRIMARY KEY,
    operation TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    response_payload BLOB NOT NULL,
    created_at TEXT NOT NULL
);
