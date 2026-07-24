ALTER TABLE local_control_events ADD COLUMN task_cursor INTEGER NOT NULL DEFAULT 0;

UPDATE local_control_events
SET task_cursor = (
    SELECT COUNT(*)
    FROM local_control_events AS prior
    WHERE prior.local_task_id = local_control_events.local_task_id
      AND prior.cursor <= local_control_events.cursor
)
WHERE local_task_id IS NOT NULL;

CREATE INDEX local_control_events_task_sequence_idx
    ON local_control_events(local_task_id, task_cursor);
CREATE UNIQUE INDEX local_control_events_task_sequence_unique_idx
    ON local_control_events(local_task_id, task_cursor)
    WHERE local_task_id IS NOT NULL;
