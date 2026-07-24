INSERT OR IGNORE INTO local_projects (
    id, name, revision, created_at, updated_at
)
SELECT
    'imported-workspace', 'Imported workspace', 1,
    strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE EXISTS (SELECT 1 FROM local_tasks);

INSERT OR IGNORE INTO local_boards (
    id, project_id, name, revision, created_at, updated_at
)
SELECT
    'imported-board', 'imported-workspace', 'Imported tasks', 1,
    strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE EXISTS (SELECT 1 FROM local_tasks);

INSERT OR IGNORE INTO local_task_contexts (
    local_task_id, project_id, board_id, created_at
)
SELECT id, 'imported-workspace', 'imported-board', created_at
FROM local_tasks
WHERE NOT EXISTS (
    SELECT 1 FROM local_task_contexts context WHERE context.local_task_id = local_tasks.id
);
