CREATE TABLE {{table}}_settle (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    owner      TEXT    NOT NULL DEFAULT '',
    amount_nd  INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE INDEX {{table}}_settle_created ON {{table}}_settle (created_at);
CREATE INDEX {{table}}_settle_owner_created ON {{table}}_settle (owner, created_at);
