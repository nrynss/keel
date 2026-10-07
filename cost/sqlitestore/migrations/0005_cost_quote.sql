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
-- The claim index is partial because only claimed rows carry a claim
-- deadline. Open and done rows keep claim_expires_at at 0, which sits
-- inside the sweep's range, so a full index would make every quote write
-- fetch and reject every live row. The partial form bounds the index to
-- the handful of rows a claim holds right now.
CREATE INDEX cost_quote_claim ON cost_quote (claim_expires_at) WHERE state = 'claimed';
