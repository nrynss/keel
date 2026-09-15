CREATE TABLE jobs (
    id         TEXT    NOT NULL PRIMARY KEY,
    kind       TEXT    NOT NULL,
    status     TEXT    NOT NULL,
    attempt    INTEGER NOT NULL,
    parent_id  TEXT    NOT NULL DEFAULT '',
    root_id    TEXT    NOT NULL,
    progress   TEXT    NOT NULL DEFAULT '',
    result     BLOB,
    error      TEXT    NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL
);

CREATE INDEX jobs_status_idx ON jobs (status);

CREATE INDEX jobs_root_idx ON jobs (root_id, attempt);
