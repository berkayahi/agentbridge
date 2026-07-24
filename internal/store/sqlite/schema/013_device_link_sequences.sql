CREATE TABLE local_device_link_counters (
    device_id TEXT PRIMARY KEY REFERENCES local_devices(id) ON DELETE CASCADE,
    message_id INTEGER NOT NULL DEFAULT 0 CHECK (message_id >= 0),
    sequence INTEGER NOT NULL DEFAULT 0 CHECK (sequence >= 0),
    updated_at TEXT NOT NULL
);
