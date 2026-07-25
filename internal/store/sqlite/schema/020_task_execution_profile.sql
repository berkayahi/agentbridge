-- 020_task_execution_profile.sql
-- The generic provider execution profile is task-scoped durable input. JSON
-- keeps the profile extensible while the existing model column remains the
-- compatibility projection for model-only tasks and clients.
ALTER TABLE local_tasks ADD COLUMN execution_profile TEXT NOT NULL DEFAULT '{}'
    CHECK (json_valid(execution_profile) AND json_type(execution_profile) = 'object');
