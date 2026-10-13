CREATE TABLE {{table}}_settle_once (
    kind       TEXT    NOT NULL,
    ref        TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (kind, ref)
);
