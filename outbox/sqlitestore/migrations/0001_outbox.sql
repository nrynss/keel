CREATE TABLE outbox_entry (
    seq      INTEGER PRIMARY KEY,
    id       TEXT    NOT NULL UNIQUE,
    payload  BLOB    NOT NULL,
    failures INTEGER NOT NULL DEFAULT 0,
    added_at INTEGER NOT NULL
);
