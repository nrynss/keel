CREATE TABLE cache_entry (
    key          TEXT    PRIMARY KEY,
    payload      BLOB    NOT NULL,
    content_type TEXT    NOT NULL,
    charge_ref   TEXT    NOT NULL,
    blob_id      TEXT    NOT NULL,
    refusal      INTEGER NOT NULL,
    created_at   INTEGER NOT NULL
);
