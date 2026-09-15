CREATE TABLE cost_charge (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    kind       TEXT    NOT NULL,
    units      INTEGER NOT NULL,
    unit_price INTEGER NOT NULL,
    ref        TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

CREATE INDEX cost_charge_kind ON cost_charge (kind);
CREATE INDEX cost_charge_ref ON cost_charge (ref);

CREATE TABLE cost_budget (
    id       INTEGER PRIMARY KEY CHECK (id = 1),
    limit_nd INTEGER NOT NULL,
    spent_nd INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE cost_reservation (
    id         TEXT    PRIMARY KEY,
    amount_nd  INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE INDEX cost_reservation_expires ON cost_reservation (expires_at);
