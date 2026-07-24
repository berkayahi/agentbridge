ALTER TABLE local_task_devices ADD COLUMN last_observed_cursor INTEGER NOT NULL DEFAULT 0 CHECK (last_observed_cursor >= 0);
