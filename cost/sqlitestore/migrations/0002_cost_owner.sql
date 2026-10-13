CREATE TABLE {{table}}_owner_budget (
    owner    TEXT    PRIMARY KEY,
    limit_nd INTEGER NOT NULL,
    spent_nd INTEGER NOT NULL DEFAULT 0
);

ALTER TABLE {{table}}_reservation ADD COLUMN owner TEXT NOT NULL DEFAULT '';

CREATE INDEX {{table}}_reservation_owner ON {{table}}_reservation (owner);
