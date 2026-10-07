CREATE TABLE s3_multipart (
    blob_id      TEXT PRIMARY KEY,
    upload_id    TEXT NOT NULL,
    owner        TEXT NOT NULL DEFAULT '',
    media_group  TEXT NOT NULL DEFAULT '',
    content_type TEXT NOT NULL,
    visibility   TEXT NOT NULL DEFAULT 'private',
    size_bytes   INTEGER NOT NULL,
    sha256       TEXT NOT NULL,
    part_size    INTEGER NOT NULL,
    part_count   INTEGER NOT NULL,
    created_at   INTEGER NOT NULL
);
