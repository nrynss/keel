CREATE TABLE media (
    id           TEXT PRIMARY KEY,
    owner        TEXT NOT NULL DEFAULT '',
    media_group  TEXT NOT NULL DEFAULT '',
    content_type TEXT NOT NULL,
    size_bytes   INTEGER NOT NULL,
    visibility   TEXT NOT NULL DEFAULT 'private',
    created_at   INTEGER NOT NULL
);
