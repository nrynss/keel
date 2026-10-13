-- The token below stands for the namespace prefix. Open rewrites it to the
-- namespace-qualified name, so each namespace owns its own tables and its
-- own indexes. Index names are schema-global in SQLite, so the indexes
-- carry the token too.
CREATE TABLE {{table}}_charge (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    kind       TEXT    NOT NULL,
    units      INTEGER NOT NULL,
    unit_price INTEGER NOT NULL,
    ref        TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

CREATE INDEX {{table}}_charge_kind ON {{table}}_charge (kind);
CREATE INDEX {{table}}_charge_ref ON {{table}}_charge (ref);

CREATE TABLE {{table}}_budget (
    id       INTEGER PRIMARY KEY CHECK (id = 1),
    limit_nd INTEGER NOT NULL,
    spent_nd INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE {{table}}_reservation (
    id         TEXT    PRIMARY KEY,
    amount_nd  INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE INDEX {{table}}_reservation_expires ON {{table}}_reservation (expires_at);
