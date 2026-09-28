CREATE TABLE cost_settle (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    owner      TEXT    NOT NULL DEFAULT '',
    amount_nd  INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE INDEX cost_settle_created ON cost_settle (created_at);
CREATE INDEX cost_settle_owner_created ON cost_settle (owner, created_at);
