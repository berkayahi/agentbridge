ALTER TABLE local_tasks
    ADD COLUMN controller_owner TEXT NOT NULL DEFAULT 'standalone'
    CHECK (controller_owner IN ('standalone', 'local_control'));

CREATE INDEX local_tasks_controller_owner_idx
    ON local_tasks(controller_owner, state, updated_at, id);
