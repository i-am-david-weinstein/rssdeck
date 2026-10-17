# rssdeck — design

A small daemon that polls RSS/Atom feeds and sends new items to a [Readeck](https://readeck.org) instance exactly once.

Target deployment is a home server or a personal VPS, as a single container with a single volume. Management happens over an HTTP API so that scripts and agents can drive it.

## Goals

- One feed item reaches Readeck once, and only once.
- Few dependencies, small image, low memory.
- SQLite for all state.
- Configurable poll cadence, globally and per feed.
- Feeds added and removed at runtime without a restart.
- Polite to feed servers: conditional requests, jittered scheduling, bounded concurrency.

## Out of scope for v0.1

Listed so expectations are set in the README: web UI, MCP server, multiple Readeck instances, token scopes, Prometheus metrics, feed autodiscovery from an HTML page, pushing full article content to Readeck, keyword filtering rules, authenticated/private feeds, and CORS support.

Keyword filtering is the most likely first follow-up, so the sync loop gets an obvious insertion point for a filter step without building one now.

## Architecture

One process, one SQLite file, two internal subsystems:

- **Scheduler** — a ticker that selects feeds that are due, polls up to 4 concurrently, and sends newly discovered items to Readeck.
- **HTTP API** — the only management surface. Always listening, always authenticated.

Both read and write the same database through `internal/store`. The daemon uses a single SQLite connection (`SetMaxOpenConns(1)`), which eliminates `SQLITE_BUSY` entirely for this workload.

### Dependencies

Direct:

- `github.com/mmcdole/gofeed` — RSS, Atom and JSON Feed parsing. Real feeds have broken namespaces, mixed date formats and CDATA abuse; hand-rolling this over `encoding/xml` is a trap.
- `modernc.org/sqlite` — pure-Go SQLite, so `CGO_ENABLED=0` and a static binary.

gofeed brings a small transitive set (goxpp, goquery, `x/net`, `x/text`). Everything else is stdlib: `net/http`, `log/slog`, `flag`, `encoding/json`, `embed`.

Dev-only: `golangci-lint` and GoReleaser, both CI-side.

## Data model

SQLite at `/data/rssdeck.db`. Connection pragmas: `journal_mode=WAL`, `busy_timeout=5000`, `synchronous=NORMAL`, `foreign_keys=on`.

WAL creates `rssdeck.db-wal` and `rssdeck.db-shm` beside the main file, so operators must mount the **directory**, never bind-mount the single `.db` file. Bind-mounting the file breaks WAL and atomic rename. This goes in the README.

All timestamps are RFC3339 UTC text. SQLite has no date type, and text stays readable from the `sqlite3` CLI.

```sql
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
```

### Item status

| status             | meaning                                                   |
|--------------------|-----------------------------------------------------------|
| `pending`          | discovered, not yet accepted by Readeck                    |
| `sent`             | Readeck returned 202; `readeck_bookmark_id` is populated   |
| `failed_permanent` | gave up after `MAX_FAILURES`, or unrecoverable (no URL)    |
| `seeded`           | present when the feed was added, deliberately never sent   |

`sent`, `failed_permanent` and `seeded` are terminal.

### Migrations

`PRAGMA user_version` plus numbered SQL files in an `embed.FS` (`001_init.sql`, `002_….sql`). At startup, read `user_version`, apply every file above it inside one transaction, set the new value. No dependency, no extra table, about 40 lines.

SQLite's `ALTER TABLE` is limited, so some future changes will need the create-copy-drop-rename rebuild. Modern SQLite supports `DROP COLUMN` and `RENAME COLUMN`, which covers most cases.

## Sync loop

### Scheduling

Scheduling is in-memory and owned entirely by the scheduler. No schedule state is persisted, so adding and removing feeds never disturbs the timing of other feeds.

The scheduler keeps a map of feed ID to next poll time. A feed enters the map when the process starts or when it's added at runtime, with its first poll jittered uniformly across its interval so forty feeds don't fire simultaneously on boot. After each poll, the next time is `now + (interval_seconds ?? global interval)`.

Jitter is recomputed on every process start, so the same feed polls at a different offset after a restart. That's fine and intentional.

One guard against restart loops: a feed's first poll is `max(now + jitter, last_polled_at + interval)`, using the persisted `last_polled_at`. Without it, a container restarting every minute would poll every feed every minute.

### Fetching

- `User-Agent: rssdeck/<version> (+https://github.com/i-am-david-weinstein/rssdeck)`
- Send `If-None-Match` from the stored `etag` and `If-Modified-Since` from `last_modified`.
- `304 Not Modified` ends the poll immediately: no download, no parse, no retention pass.
- Response body read through `io.LimitReader` at `MAX_BODY_SIZE` (10MB). Exceeding it fails the poll with a recorded error rather than exhausting memory.
- 30s timeout.

### Identity

Per feed, an item's identity is the first of:

1. `Item.GUID`
2. `Item.Link`
3. `sha256(title + published_at)`

Identity is scoped to the feed. The same article in two feeds produces two bookmarks, which is the intended behavior.

### Item URL

`Item.Link`, resolved against the feed's base URL when relative. Falls back to `Item.GUID` when that parses as an absolute http(s) URL. An item with no usable URL is recorded as `failed_permanent` with reason `no usable URL` — never skipped silently.

### Ingest

For each item in the parsed document, in feed order:

1. Row exists for `(feed_id, guid)` → update `last_seen_in_feed_at`, nothing else.
2. Otherwise insert as `pending`.

An item's row is what makes it known. While an item is present in the feed document, its row is present, so it is never resent.

No cap on items ingested or sent per poll.

### Sending

Pending items are posted oldest-first by `published_at`, so Readeck's newest-first list matches the feed's chronology.

Each send is `POST /api/bookmarks` with `{"url": …, "labels": [...]}`. Labels are the feed's `label` (when set) plus the global label (when set). On `202`, store the `Bookmark-Id` response header into `readeck_bookmark_id`, set `sent_at`, mark `sent`.

Failure handling:

- Any non-2xx or transport error increments `attempts` and records `last_error`. The item stays `pending` and is retried on the next poll; the daemon is already a retry loop, so there's no inner retry.
- At `attempts >= MAX_FAILURES` (default 5) the item becomes `failed_permanent`.
- A `401` from Readeck is logged once per run rather than once per item.

### Retention

The point of retention is to keep the database from growing without bound. Once an item has left the feed and has nothing left to do, its row is deleted.

After a **successful parse only**:

```
delete items of this feed where status is terminal
    and last_seen_in_feed_at < this_poll_started_at
```

A 304 or a fetch error skips this entirely. Neither tells us anything about which items the feed still carries, and treating them as "the feed is empty" would delete the lot.

Pending items that fall out of the feed are kept and still sent. They're queued work that hasn't failed.

Known consequence: if a feed drops an item and later serves it again — republished on edit, pagination quirks, a server restored from an old backup — rssdeck has no memory of it and sends it a second time. The window is small, since deletion only happens once the item is already gone from the feed, and Readeck appears to deduplicate on URL. Accepted deliberately in exchange for not carrying a permanent record of every item ever seen.

### Adding a feed

`POST /api/feeds` fetches and parses synchronously, returning `400` with the parse error when the URL isn't a feed. Agents need that signal immediately, and a broken feed at add time is nearly always a typo.

On success: store the parsed title, default `label` to that title truncated at 64 characters, then seed. The newest `backfill` items (default 1) are inserted as `pending`; every other item in the document is inserted as `seeded`. A default of 1 makes a successful add visible in Readeck without dumping an archive into it.

Deleting a feed purges its items via `ON DELETE CASCADE`. Re-adding the same URL starts clean and will resend.

## Readeck integration

Single instance, configured by `READECK_URL` and a token.

- `POST {READECK_URL}/api/bookmarks`, `Authorization: Bearer <token>`, JSON body.
- Success is `202 Accepted`; the new bookmark is identified by the `Bookmark-Id` header (also derivable from `Location`).
- At startup, one authenticated probe against Readeck. A bad token fails the boot instead of surfacing as silent failures hours later.

## HTTP API

Always listening on `RSSDECK_ADDR` (default `0.0.0.0:8080`). Exposure is the operator's job via port mapping or a reverse proxy; TLS is not terminated in-process.

### Authentication

`Authorization: Bearer <token>`.

- The **master token** comes from `RSSDECK_API_TOKEN` or `RSSDECK_API_TOKEN_FILE`, compared in constant time. The daemon refuses to start without it — no loopback exception, because that assumption breaks the moment someone uses host networking.
- **Minted tokens** are created through the API, stored as SHA-256 hashes with an 8-character visible prefix, and returned in plaintext exactly once. Optional `expires_at`; `last_used_at` updated on use.
- Token management is **master-token only**. A compromised agent token cannot mint itself persistence or revoke yours.

`GET /healthz` and `GET /api/openapi.yaml` are unauthenticated.

### Endpoints

```
GET    /healthz                   liveness, unauthenticated
GET    /api/openapi.yaml          spec, unauthenticated

GET    /api/feeds                 list feeds with poll status
POST   /api/feeds                 {url, label?, interval_seconds?, backfill?}
GET    /api/feeds/{id}
PATCH  /api/feeds/{id}            {label?, interval_seconds?, enabled?}
DELETE /api/feeds/{id}            purges its items
POST   /api/feeds/{id}/poll       poll now

GET    /api/items                 ?feed_id=&status=&limit=&offset=

GET    /api/tokens                master only
POST   /api/tokens                master only, {name, expires_at?}
DELETE /api/tokens/{id}           master only
```

### Conventions

- Errors: `{"error": "human readable message", "code": "feed_not_found"}`. The code is stable and branchable; the message is for humans.
- Pagination on `/api/items`: `limit` default 50, max 500, plus `offset`, with `total` in the response body.
- Request bodies capped at 64KB via `http.MaxBytesReader`, returning `413`. Our bodies are a few hundred bytes; without the cap, one oversized POST can OOM the container.
- No CORS headers. Browser clients are out of scope; the header set can be added in ten lines when one exists.

### OpenAPI

`openapi.yaml` is hand-written (~200 lines) and served by the running instance so an agent can always fetch a current spec. Codegen in either direction costs a toolchain and dependencies that ten endpoints don't justify. A CI test walks the registered route table and asserts every route and method appears in the spec, which catches the common drift. If the API passes ~25 endpoints, revisit spec-first codegen.

## Configuration

Flags and environment variables, precedence **flag > env > default**. No config file: there are a dozen settings and feeds live in the database, so a config format would be pure bikeshedding surface.

| Variable | Default | Notes |
|---|---|---|
| `RSSDECK_API_TOKEN` / `_FILE` | — | required; `_FILE` wins if both set |
| `RSSDECK_ADDR` | `0.0.0.0:8080` | |
| `RSSDECK_DB_PATH` | `/data/rssdeck.db` | parent dir created if missing |
| `RSSDECK_READECK_URL` | — | required |
| `RSSDECK_READECK_TOKEN` / `_FILE` | — | required; `_FILE` wins |
| `RSSDECK_POLL_INTERVAL` | `15m` | global default, overridable per feed |
| `RSSDECK_DEFAULT_BACKFILL` | `1` | overridable per add |
| `RSSDECK_GLOBAL_LABEL` | `rssdeck` | empty string disables |
| `RSSDECK_MAX_FAILURES` | `5` | per item, before `failed_permanent` |
| `RSSDECK_HTTP_TIMEOUT` | `30s` | feed fetches and Readeck posts |
| `RSSDECK_MAX_BODY_SIZE` | `10MB` | feed response cap |
| `RSSDECK_POLL_CONCURRENCY` | `4` | |
| `RSSDECK_LOG_LEVEL` | `info` | |
| `RSSDECK_LOG_FORMAT` | `text` | or `json` |

Tokens are never written to the database in plaintext, never logged, and redacted from the startup config summary.

Startup validates all four required values and probes Readeck before serving.

## Operations

### Binary entry points

`rssdeck` (serve, the default), `rssdeck healthcheck` (hits `/healthz`, for Docker `HEALTHCHECK`, since distroless has no shell), `rssdeck version`.

### Container

`gcr.io/distroless/static:nonroot` — about 2MB, includes CA certificates (required for HTTPS feeds) and runs as UID 65532.

`VOLUME /data`. The published `compose.yaml` uses a named volume so the nonroot permissions edge never hits the happy path; the README covers `chown 65532:65532` for bind-mount users.

### Logging

`log/slog`, text by default because a human is usually reading `docker logs`.

INFO covers every poll (including 304s and no-change results), each item sent with feed, title, URL and bookmark ID, each feed error, each permanent failure, and startup/shutdown. DEBUG adds timings, item counts and per-request detail. Twenty feeds at 15 minutes is roughly 2,000 lines a day.

### Shutdown

On SIGTERM: stop scheduling, let in-flight polls finish with a 30s deadline, drain the HTTP server, close the database.

## Repo layout

Standard Go layout: `cmd/rssdeck` for the entry point, `internal/` for everything else, since nothing here is meant to be imported by other projects.

```
cmd/rssdeck/          main, flag parsing, subcommand dispatch
internal/config/      env + flag resolution, validation
internal/store/       SQLite, migrations (embedded SQL), queries
internal/feed/        fetching, conditional GET, parsing, identity
internal/readeck/     API client
internal/sync/        scheduler and the send loop
internal/api/         HTTP handlers, auth middleware
testdata/             real-world feed fixtures
```

Flag parsing is stdlib `flag` with manual subcommand dispatch. Cobra would be the largest dependency in a project whose pitch is "few dependencies," for three subcommands.

## Testing

Unit tests only for v0.1.

- Table-driven parser tests over a `testdata/` corpus of genuinely ugly real-world feeds.
- Dedup and retention logic against an in-memory SQLite database. This is where the once-only guarantee lives and gets the heaviest coverage.
- The Readeck client against an `httptest` server covering 202 with `Bookmark-Id`, 401, 5xx and timeouts.
- API handlers against `httptest`, including auth rejection and master-only enforcement.
- The route/spec coverage check.

Hand-written fakes, no mocking framework.

## Release

MIT. Module `github.com/i-am-david-weinstein/rssdeck`.

GitHub Actions on push and PR: `go build`, `go vet`, `go test ./...`, `golangci-lint`.

On a `vX.Y.Z` tag: multi-arch (`amd64`, `arm64`) image to `ghcr.io/i-am-david-weinstein/rssdeck` tagged `vX.Y.Z`, `X.Y` and `latest`, plus GoReleaser binaries attached to the GitHub release for people not using Docker.

Docs: README with a copy-pasteable `docker run`, a `compose.yaml`, and curl examples; LICENSE; CONTRIBUTING.md; CHANGELOG.md.

## Build order

Each step leaves something runnable, and the correctness core lands before the surface.

1. `internal/store` — schema, migrations, queries.
2. `internal/feed` — fetch with conditional GET, parse, identity; fixture tests.
3. `internal/readeck` — client against a fake.
4. `internal/sync` — scheduler, ingest, dedup, retention, send. Most heavily tested.
5. `internal/api` — handlers, auth, tokens, OpenAPI spec.
6. Dockerfile, compose, CI, GoReleaser.
7. README and the rest of the docs.
