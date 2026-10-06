CREATE TABLE cost_quote (
    id               TEXT    PRIMARY KEY,
    owner            TEXT    NOT NULL,
    price_nd         INTEGER NOT NULL,
    denomination     TEXT    NOT NULL DEFAULT '',
    state            TEXT    NOT NULL DEFAULT 'open',
    outcome_nd       INTEGER NOT NULL DEFAULT 0,
    outcome_measured INTEGER NOT NULL DEFAULT 0,
    reservation_id   TEXT    NOT NULL DEFAULT '',
    claim_expires_at INTEGER NOT NULL DEFAULT 0,
    expires_at       INTEGER NOT NULL,
    created_at       INTEGER NOT NULL
);

CREATE INDEX cost_quote_expires ON cost_quote (expires_at);
CREATE INDEX cost_quote_owner ON cost_quote (owner);
CREATE INDEX cost_quote_claim ON cost_quote (claim_expires_at);
