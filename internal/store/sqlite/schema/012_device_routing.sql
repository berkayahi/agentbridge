CREATE TABLE local_devices (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('local_mac', 'raspberry_pi')),
    fingerprint TEXT NOT NULL UNIQUE,
    public_key BLOB NOT NULL,
    endpoint TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL CHECK (state IN ('paired', 'unreachable', 'revoked')),
    connection_epoch INTEGER NOT NULL CHECK (connection_epoch > 0),
    revision INTEGER NOT NULL CHECK (revision > 0),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE local_pairing_challenges (
    id TEXT PRIMARY KEY,
    device_id TEXT NOT NULL,
    browser_fingerprint TEXT NOT NULL,
    nonce TEXT NOT NULL UNIQUE,
    trust_set_digest TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    used_at TEXT,
    created_at TEXT NOT NULL
);
CREATE INDEX local_pairing_challenges_expiry_idx ON local_pairing_challenges(expires_at, id);

CREATE TABLE local_task_devices (
    local_task_id TEXT PRIMARY KEY REFERENCES local_tasks(id) ON DELETE CASCADE,
    device_id TEXT NOT NULL REFERENCES local_devices(id) ON DELETE RESTRICT,
    assignment_epoch INTEGER NOT NULL CHECK (assignment_epoch > 0),
    last_ack_cursor INTEGER NOT NULL DEFAULT 0 CHECK (last_ack_cursor >= 0),
    state TEXT NOT NULL CHECK (state IN ('assigned', 'unreachable', 'fenced')),
    updated_at TEXT NOT NULL
);
CREATE INDEX local_task_devices_device_idx ON local_task_devices(device_id, state, updated_at);

INSERT INTO local_devices (
    id, name, kind, fingerprint, public_key, endpoint, state,
    connection_epoch, revision, created_at, updated_at
) VALUES (
    'local-mac', 'This Mac', 'local_mac', 'local-runtime', X'', '', 'paired',
    1, 1, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
);

INSERT INTO local_task_devices (
    local_task_id, device_id, assignment_epoch, last_ack_cursor, state, updated_at
)
SELECT id, 'local-mac', 1, 0, 'assigned', updated_at
FROM local_tasks;
