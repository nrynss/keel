CREATE TABLE providertask_task (
    key        TEXT PRIMARY KEY,
    task_id    TEXT NOT NULL DEFAULT '',
    state      TEXT NOT NULL DEFAULT 'running',
    error_code TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);
