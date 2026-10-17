CREATE TABLE feeds (
    id                   INTEGER PRIMARY KEY,
    url                  TEXT NOT NULL UNIQUE,
    title                TEXT,
    label                TEXT,
    enabled              INTEGER NOT NULL DEFAULT 1,
    interval_seconds     INTEGER,           -- NULL = use global default
    etag                 TEXT,
    last_modified        TEXT,
    last_polled_at       TEXT,
    last_success_at      TEXT,
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    last_error           TEXT,
    created_at           TEXT NOT NULL
);

CREATE TABLE items (
    id                   INTEGER PRIMARY KEY,
    feed_id              INTEGER NOT NULL REFERENCES feeds(id) ON DELETE CASCADE,
    guid                 TEXT NOT NULL,
    url                  TEXT,
    title                TEXT,
    published_at         TEXT,
    status               TEXT NOT NULL
                         CHECK (status IN ('pending','sent','failed_permanent','seeded')),
    attempts             INTEGER NOT NULL DEFAULT 0,
    last_error           TEXT,
    readeck_bookmark_id  TEXT,
    sent_at              TEXT,
    first_seen_at        TEXT NOT NULL,
    last_seen_in_feed_at TEXT NOT NULL,
    UNIQUE (feed_id, guid)
);

CREATE INDEX items_feed_status ON items (feed_id, status);
CREATE INDEX items_status      ON items (status);

CREATE TABLE tokens (
    id           INTEGER PRIMARY KEY,
    name         TEXT NOT NULL,
    prefix       TEXT NOT NULL,     -- first 8 chars, shown in listings
    hash         BLOB NOT NULL UNIQUE,
    expires_at   TEXT,              -- NULL = never
    last_used_at TEXT,
    created_at   TEXT NOT NULL
);
