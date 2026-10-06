-- Users, sessions, sign-in identities and one time sign-in codes.
--
-- A user row is a guest until a sign-in attaches an identity and flips
-- its kind, so the user id never changes and the guest's rows become the
-- account's rows. The identity key is the provider name and the provider
-- subject, never an address. Codes keep hashes only, so a stolen
-- database copy verifies nothing and reveals neither the code nor the
-- address.
CREATE TABLE users (
    id           TEXT    NOT NULL PRIMARY KEY,
    kind         TEXT    NOT NULL,
    created_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL
);

CREATE TABLE sessions (
    id         TEXT    NOT NULL PRIMARY KEY,
    user_id    TEXT    NOT NULL REFERENCES users (id),
    created_at INTEGER NOT NULL,
    revoked    INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX sessions_user_idx ON sessions (user_id);

CREATE TABLE identities (
    id         TEXT    NOT NULL PRIMARY KEY,
    user_id    TEXT    NOT NULL REFERENCES users (id),
    provider   TEXT    NOT NULL,
    subject    TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    UNIQUE (provider, subject)
);

CREATE INDEX identities_user_idx ON identities (user_id);

CREATE TABLE sign_in_codes (
    id                 TEXT    NOT NULL PRIMARY KEY,
    address_hash       TEXT    NOT NULL,
    code_hash          TEXT    NOT NULL,
    requesting_session TEXT    NOT NULL REFERENCES sessions (id),
    expires_at         INTEGER NOT NULL,
    attempts           INTEGER NOT NULL DEFAULT 0,
    used_at            INTEGER NOT NULL DEFAULT 0,
    created_at         INTEGER NOT NULL
);

CREATE INDEX sign_in_codes_address_created_idx
    ON sign_in_codes (address_hash, created_at);
