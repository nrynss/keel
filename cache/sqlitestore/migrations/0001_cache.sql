-- The table token below is rewritten to the namespace-qualified table name
-- at open time, so each namespace owns its own table.
CREATE TABLE {{table}} (
    key          TEXT    PRIMARY KEY,
    payload      BLOB    NOT NULL,
    content_type TEXT    NOT NULL,
    charge_ref   TEXT    NOT NULL,
    blob_id      TEXT    NOT NULL,
    refusal      INTEGER NOT NULL,
    created_at   INTEGER NOT NULL
);
