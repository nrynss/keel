ALTER TABLE {{table}}_charge ADD COLUMN denomination TEXT NOT NULL DEFAULT '';

ALTER TABLE {{table}}_budget ADD COLUMN denomination TEXT NOT NULL DEFAULT '';
ALTER TABLE {{table}}_budget ADD COLUMN minor_units INTEGER NOT NULL DEFAULT 0;

CREATE TABLE {{table}}_grant (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    amount_nd  INTEGER NOT NULL,
    drawn_nd   INTEGER NOT NULL DEFAULT 0,
    grant_key  TEXT    NOT NULL DEFAULT '',
    expires_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE UNIQUE INDEX {{table}}_grant_key ON {{table}}_grant (grant_key) WHERE grant_key <> '';
CREATE INDEX {{table}}_grant_expires ON {{table}}_grant (expires_at);
