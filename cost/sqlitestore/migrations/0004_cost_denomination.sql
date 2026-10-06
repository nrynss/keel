ALTER TABLE cost_charge ADD COLUMN denomination TEXT NOT NULL DEFAULT '';

ALTER TABLE cost_budget ADD COLUMN denomination TEXT NOT NULL DEFAULT '';
ALTER TABLE cost_budget ADD COLUMN minor_units INTEGER NOT NULL DEFAULT 0;

CREATE TABLE cost_grant (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    amount_nd  INTEGER NOT NULL,
    drawn_nd   INTEGER NOT NULL DEFAULT 0,
    grant_key  TEXT    NOT NULL DEFAULT '',
    expires_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE UNIQUE INDEX cost_grant_key ON cost_grant (grant_key) WHERE grant_key <> '';
CREATE INDEX cost_grant_expires ON cost_grant (expires_at);
