CREATE TABLE revision_document (
    id         TEXT    PRIMARY KEY,
    owner      TEXT    NOT NULL,
    head       INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE INDEX revision_document_owner ON revision_document (owner);

CREATE TABLE revision_entry (
    document     TEXT    NOT NULL REFERENCES revision_document (id) ON DELETE CASCADE,
    number       INTEGER NOT NULL,
    parent       INTEGER NOT NULL,
    author       TEXT    NOT NULL,
    proposal     TEXT    NOT NULL DEFAULT '',
    body         BLOB    NOT NULL,
    committed_at INTEGER NOT NULL,
    PRIMARY KEY (document, number)
);

CREATE TABLE revision_pin (
    document  TEXT    NOT NULL,
    name      TEXT    NOT NULL,
    number    INTEGER NOT NULL,
    pinned_at INTEGER NOT NULL,
    PRIMARY KEY (document, name),
    FOREIGN KEY (document, number) REFERENCES revision_entry (document, number) ON DELETE CASCADE
);

CREATE INDEX revision_pin_number ON revision_pin (document, number);

CREATE TABLE revision_proposal (
    id         TEXT    PRIMARY KEY,
    document   TEXT    NOT NULL REFERENCES revision_document (id) ON DELETE CASCADE,
    parent     INTEGER NOT NULL,
    author     TEXT    NOT NULL,
    body       BLOB    NOT NULL,
    state      TEXT    NOT NULL,
    number     INTEGER NOT NULL DEFAULT 0,
    decided_by TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    decided_at INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX revision_proposal_document ON revision_proposal (document, created_at);
