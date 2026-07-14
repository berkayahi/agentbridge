ALTER TABLE attachments ADD COLUMN sha256 TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS attachments_task_sha256_idx
    ON attachments(task_id, sha256) WHERE sha256 <> '';
