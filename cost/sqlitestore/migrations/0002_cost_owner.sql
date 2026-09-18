CREATE TABLE cost_owner_budget (
    owner    TEXT    PRIMARY KEY,
    limit_nd INTEGER NOT NULL,
    spent_nd INTEGER NOT NULL DEFAULT 0
);

ALTER TABLE cost_reservation ADD COLUMN owner TEXT NOT NULL DEFAULT '';

CREATE INDEX cost_reservation_owner ON cost_reservation (owner);
