-- 019_task_model.sql
-- The model a task flies with is the keeper's choice at dispatch, not a daemon
-- setting, so it belongs on the task: it must survive a restart and be reported
-- back to the surface. Empty means the provider's configured default, which is
-- exactly the behaviour every task had before this column existed.
ALTER TABLE local_tasks ADD COLUMN model TEXT NOT NULL DEFAULT '';
