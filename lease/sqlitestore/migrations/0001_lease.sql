CREATE TABLE lease_entry (
    id          TEXT    PRIMARY KEY,
    state       TEXT    NOT NULL,
    opened_at   INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,
    closed_at   INTEGER NOT NULL DEFAULT 0,
    estimate_nd INTEGER NOT NULL,
    settled_nd  INTEGER NOT NULL DEFAULT 0,
    reported_nd INTEGER NOT NULL DEFAULT 0,
    reconciled  INTEGER NOT NULL DEFAULT 0,
    kind        TEXT    NOT NULL DEFAULT '',
    owner       TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX lease_entry_expires ON lease_entry (expires_at);
CREATE INDEX lease_entry_state ON lease_entry (state);
