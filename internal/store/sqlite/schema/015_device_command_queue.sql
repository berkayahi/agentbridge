CREATE TABLE local_device_commands (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES local_tasks(id) ON DELETE CASCADE,
    device_id TEXT NOT NULL REFERENCES local_devices(id) ON DELETE RESTRICT,
    assignment_epoch INTEGER NOT NULL CHECK (assignment_epoch > 0),
    operation TEXT NOT NULL CHECK (operation IN ('start', 'resume', 'approve', 'cancel', 'verify', 'commit')),
    request_hash TEXT NOT NULL,
    request_payload BLOB NOT NULL,
    revision INTEGER NOT NULL CHECK (revision > 0),
    state TEXT NOT NULL CHECK (state IN ('pending', 'in_flight', 'completed', 'failed')),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX local_device_commands_replay_idx ON local_device_commands(device_id, state, created_at, id);
CREATE INDEX local_device_commands_task_idx ON local_device_commands(task_id, state, created_at, id);
