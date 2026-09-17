CREATE TABLE flag_value (
    name       TEXT PRIMARY KEY,
    kind       TEXT    NOT NULL,
    value      TEXT    NOT NULL,
    changed_at INTEGER NOT NULL
);
