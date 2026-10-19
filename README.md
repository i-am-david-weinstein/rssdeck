# rssdeck

A small daemon that polls RSS, Atom and JSON feeds and sends new items to a
[Readeck](https://readeck.org) instance once each.

It runs as one container with one volume, keeps all its state in SQLite, and is
managed over an HTTP API, so a script or an agent can add and remove feeds
without a restart. There is no web UI.

## Quick start

Two tokens and a Readeck URL are all it needs. Generate the API token first,
since nothing starts without it:

```sh
openssl rand -hex 32
```

Then:

```sh
docker run -d --name rssdeck \
  -p 127.0.0.1:8080:8080 \
  -v rssdeck-data:/data \
  -e RSSDECK_API_TOKEN=the-token-you-just-generated \
  -e RSSDECK_READECK_URL=https://readeck.example \
  -e RSSDECK_READECK_TOKEN=your-readeck-api-token \
  ghcr.io/i-am-david-weinstein/rssdeck:latest
```

The port is published on `127.0.0.1` because the API speaks plain HTTP. Put a
reverse proxy with TLS in front before exposing it anywhere else.

Check that it came up:

```sh
curl -s localhost:8080/healthz
```

```json
{
  "status": "ok"
}
```

Add a feed:

```sh
curl -s -X POST localhost:8080/api/feeds \
  -H "Authorization: Bearer $RSSDECK_API_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com/feed.xml"}'
```

```json
{
  "id": 1,
  "url": "https://example.com/feed.xml",
  "title": "Example Blog",
  "label": "Example Blog",
  "enabled": true,
  "interval_seconds": null,
  "last_polled_at": "2026-10-19T01:25:00Z",
  "last_success_at": "2026-10-19T01:25:00Z",
  "next_poll_at": "2026-10-19T01:40:00Z",
  "consecutive_failures": 0,
  "created_at": "2026-10-19T01:25:00Z"
}
```

The feed is fetched and parsed during that request, so a URL that is not a feed
fails right there rather than on some later poll. The newest item goes to
Readeck immediately, which is how you know the whole path works. Everything
older in the document is recorded as already seen and never sent. Pass
`"backfill": 10` to send the ten newest instead, or `0` to send nothing.

### Docker Compose

[`compose.yaml`](compose.yaml) in this repo is ready to copy. It expects a
`.env` beside it:

```sh
RSSDECK_API_TOKEN=...
RSSDECK_READECK_URL=https://readeck.example
RSSDECK_READECK_TOKEN=...
```

```sh
docker compose up -d
```

If Readeck also runs in Docker on the same host, put both services on one
network and point `RSSDECK_READECK_URL` at `http://readeck:8000`. The compose
file has the two lines commented out.

## Configuration

Flags and environment variables, with flags winning over the environment and
the environment winning over the default. There is no config file. Feeds live
in the database, so there would be a dozen settings in it and nothing else.

| Variable | Flag | Default | Notes |
|---|---|---|---|
| `RSSDECK_API_TOKEN` | `-api-token` | required | the master token for the API |
| `RSSDECK_API_TOKEN_FILE` | `-api-token-file` | | reads the token from a file; wins over the above |
| `RSSDECK_READECK_URL` | `-readeck-url` | required | instance root, for example `https://readeck.example` |
| `RSSDECK_READECK_TOKEN` | `-readeck-token` | required | Readeck API token with bookmark write access |
| `RSSDECK_READECK_TOKEN_FILE` | `-readeck-token-file` | | reads it from a file; wins over the above |
| `RSSDECK_ADDR` | `-addr` | `0.0.0.0:8080` | |
| `RSSDECK_DB_PATH` | `-db-path` | `/data/rssdeck.db` | the parent directory is created if missing |
| `RSSDECK_POLL_INTERVAL` | `-poll-interval` | `15m` | global, overridable per feed |
| `RSSDECK_DEFAULT_BACKFILL` | `-default-backfill` | `1` | overridable per add |
| `RSSDECK_GLOBAL_LABEL` | `-global-label` | `rssdeck` | on every bookmark; empty string disables it |
| `RSSDECK_MAX_FAILURES` | `-max-failures` | `5` | failed sends before an item is given up on |
| `RSSDECK_HTTP_TIMEOUT` | `-http-timeout` | `30s` | feed fetches and Readeck posts |
| `RSSDECK_MAX_BODY_SIZE` | `-max-body-size` | `10MB` | cap on a feed response |
| `RSSDECK_POLL_CONCURRENCY` | `-poll-concurrency` | `4` | feeds polled at once |
| `RSSDECK_LOG_LEVEL` | `-log-level` | `info` | `debug`, `info`, `warn` or `error` |
| `RSSDECK_LOG_FORMAT` | `-log-format` | `text` | or `json` |

The `_FILE` forms exist for Docker and Kubernetes secrets. Tokens are never
logged, and the startup summary prints `api_token=set` rather than the value.

A missing or unparseable setting fails the boot, and all such problems are
reported at once, so fixing three of them takes one restart.

## HTTP API

Every route under `/api/` needs `Authorization: Bearer <token>`. `/healthz` and
`/api/openapi.yaml` do not.

```
GET    /healthz                   liveness
GET    /api/openapi.yaml          this API's spec, served by the running instance

GET    /api/feeds                 list feeds with poll status
POST   /api/feeds                 {url, label?, interval_seconds?, backfill?}
GET    /api/feeds/{id}
PATCH  /api/feeds/{id}            {label?, interval_seconds?, enabled?}
DELETE /api/feeds/{id}            also deletes its items
POST   /api/feeds/{id}/poll       poll now, synchronously

GET    /api/items                 ?feed_id=&status=&limit=&offset=

GET    /api/tokens                master token only
POST   /api/tokens                master token only
DELETE /api/tokens/{id}           master token only
```

An agent that needs the current shape of all of this can fetch
`/api/openapi.yaml` from the running instance instead of trusting this README.

### Examples

List feeds, with when each was last polled and when it is next due:

```sh
curl -s localhost:8080/api/feeds -H "Authorization: Bearer $TOKEN"
```

Slow one feed down to twice a day and relabel it:

```sh
curl -s -X PATCH localhost:8080/api/feeds/1 \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"label":"Example","interval_seconds":43200}'
```

Poll a feed now rather than waiting for its turn. The response reports what the
poll did, so this is also the way to debug one:

```sh
curl -s -X POST localhost:8080/api/feeds/1/poll -H "Authorization: Bearer $TOKEN"
```

See what happened to recent items:

```sh
curl -s "localhost:8080/api/items?limit=2" -H "Authorization: Bearer $TOKEN"
```

```json
{
  "items": [
    {
      "id": 1,
      "feed_id": 1,
      "guid": "https://example.com/1",
      "url": "https://example.com/1",
      "title": "First post",
      "published_at": "2026-10-18T10:00:00Z",
      "status": "sent",
      "attempts": 0,
      "readeck_bookmark_id": "WxYz1234",
      "sent_at": "2026-10-19T01:25:00Z",
      "first_seen_at": "2026-10-19T01:25:00Z",
      "last_seen_in_feed_at": "2026-10-19T01:25:00Z"
    },
    {
      "id": 2,
      "feed_id": 1,
      "guid": "https://example.com/2",
      "url": "https://example.com/2",
      "title": "Second post",
      "published_at": "2026-10-18T09:00:00Z",
      "status": "seeded",
      "attempts": 0,
      "sent_at": null,
      "first_seen_at": "2026-10-19T01:25:00Z",
      "last_seen_in_feed_at": "2026-10-19T01:25:00Z"
    }
  ],
  "limit": 2,
  "offset": 0,
  "total": 2
}
```

Filter by what went wrong: `?status=failed_permanent` for items that were given
up on, `?status=pending` for ones still queued.

### Tokens for agents

Rather than handing out the master token, mint one per consumer. The plaintext
appears in this response and nowhere else, because the database holds only its
hash:

```sh
curl -s -X POST localhost:8080/api/tokens \
  -H "Authorization: Bearer $MASTER_TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"feed-curator agent","expires_at":"2027-01-01T00:00:00Z"}'
```

```json
{
  "id": 1,
  "name": "feed-curator agent",
  "prefix": "3hTsM5IM",
  "expires_at": "2027-01-01T00:00:00Z",
  "last_used_at": null,
  "created_at": "2026-10-19T01:25:07Z",
  "token": "3hTsM5IM...the-rest-of-it"
}
```

A minted token can drive feeds and items. It cannot touch `/api/tokens`, which
answers `403 master_token_required`, so a leaked agent token can neither mint
itself a replacement nor revoke yours. Revoke one with
`DELETE /api/tokens/{id}`, effective on its next request.

### Errors

```json
{
  "error": "feed could not be fetched: feed: https://example.com/nope returned 404 Not Found",
  "code": "feed_unreachable"
}
```

Branch on `code`, which is stable. The `error` text is for humans and may be
reworded. The codes in use are `invalid_request`, `invalid_url`,
`invalid_status`, `not_a_feed`, `feed_unreachable`, `feed_exists`,
`feed_not_found`, `poll_failed`, `poll_in_progress`, `token_not_found`,
`unauthorized`, `master_token_required`, `method_not_allowed`, `not_found`,
`request_too_large` and `internal_error`.

## How the once-only guarantee works

Each item gets an identity, scoped to its feed: the feed's GUID for it, or its
link, or a hash of its title and publication date, whichever comes first. A row
in SQLite under that identity is what makes the item known. So an item is sent
when its row is created, and never again, because the row is still there on the
next poll.

The same article in two feeds is two rows and two bookmarks. That is intended.

Items are sent oldest first, so Readeck's newest-first list ends up in the same
order as the feed.

### Failure handling

A send that fails leaves the item `pending`, bumps its `attempts` and records
the error. The next poll retries it. The daemon is already a retry loop, so
there is no inner one. After `RSSDECK_MAX_FAILURES` tries the item becomes
`failed_permanent` and is left alone.

An item with no usable URL is recorded as `failed_permanent` immediately rather
than skipped quietly, so `?status=failed_permanent` is the full account of what
did not make it.

### Item states

| status | meaning |
|---|---|
| `pending` | discovered, not yet accepted by Readeck |
| `sent` | Readeck took it; `readeck_bookmark_id` is filled in |
| `failed_permanent` | given up on, or unusable from the start |
| `seeded` | already in the feed when it was added, deliberately never sent |

`sent`, `failed_permanent` and `seeded` are final.

### Retention

Rows are deleted once an item has dropped out of the feed document and has
nothing left to do, which keeps the database from growing forever. A poll that
returned `304 Not Modified` or failed deletes nothing, since neither says
anything about what the feed still carries. Pending items that fall out of the
feed are kept and still sent, because they are work that has not failed yet.

One consequence worth knowing: if a feed drops an item and later serves it
again, after an edit or a restore from an old backup, rssdeck has forgotten it
and sends it a second time. The window is small, since the row only goes away
once the item is already gone from the feed. Carrying a permanent record of
every item ever seen was the alternative, and it is not worth it.

### Being polite to feed servers

Conditional requests go out on every poll, using the stored `ETag` and
`Last-Modified`, and a `304` ends the poll with nothing downloaded or parsed.
Each feed's first poll after a restart is jittered across its interval so forty
feeds do not all fire at boot, and no more than
`RSSDECK_POLL_CONCURRENCY` feeds are polled at once. A feed is never polled
sooner than its interval allows, even if the container restarts every minute.

The User-Agent says who is calling:

```
rssdeck/v0.1.0 (+https://github.com/i-am-david-weinstein/rssdeck)
```

## Operations

### The volume holds a directory, not a file

SQLite runs in WAL mode, which puts `rssdeck.db-wal` and `rssdeck.db-shm`
beside the database. Mount the directory. Bind-mounting the single `.db` file
breaks WAL and the atomic rename it depends on.

The named volume in the examples above needs no setup. If you would rather see
the database on the host, give the directory to the image's user first. The
process runs as UID 65532, and on a Linux host it will not be able to open its
database in a directory it does not own:

```sh
mkdir -p /srv/rssdeck
chown 65532:65532 /srv/rssdeck
docker run -d --name rssdeck -v /srv/rssdeck:/data ...
```

Docker Desktop on macOS and Windows maps ownership across its file sharing
layer, so the chown is unnecessary there and the database comes out owned by
you. Doing it anyway costs nothing and is what the Linux host needs.

### Backups

Stop the container and copy the directory, or use the `sqlite3` CLI against a
running instance:

```sh
sqlite3 /srv/rssdeck/rssdeck.db ".backup '/backups/rssdeck.db'"
```

Losing the database means every item currently in every feed gets sent again,
so it is worth backing up.

### Logs and health

Logs go to stdout as `log/slog` text, because a human is usually reading
`docker logs`. Set `RSSDECK_LOG_FORMAT=json` for a log shipper. INFO covers
every poll, including the ones that changed nothing, each item sent with its
feed and bookmark ID, every feed error, and startup and shutdown. Twenty feeds
at fifteen minutes is roughly 2,000 lines a day. DEBUG adds timings and
per-request detail.

The image carries its own `HEALTHCHECK`, so `docker ps` reports health with no
configuration. It works by running the binary against itself, since the
distroless image has no shell and no curl:

```sh
docker exec rssdeck /rssdeck healthcheck
```

On SIGTERM the daemon stops scheduling, lets in-flight polls finish within 30
seconds, drains the HTTP server and closes the database.

### Startup

Two things have to be true before it will serve. All four required settings
must be present and parseable, and one authenticated request to Readeck must
succeed. A bad Readeck token fails the boot rather than turning into silent
failures hours later.

## Subcommands

```
rssdeck              serve, the default
rssdeck healthcheck  probe the local instance, for Docker's HEALTHCHECK
rssdeck version      print the version
rssdeck help         flags and their defaults
```

## Not in v0.1

Listed so you know what you are choosing: a web UI, an MCP server, more than
one Readeck instance, token scopes, Prometheus metrics, finding a feed from a
page's HTML, pushing full article text to Readeck, keyword filtering rules,
authenticated or private feeds, and CORS headers.

Keyword filtering is the likeliest first addition, and the sync loop already
has the place to put it.

## Building from source

Go 1.26 or newer. Nothing else, and no C toolchain: the SQLite driver is pure
Go, so the binary is static.

```sh
go build -o rssdeck ./cmd/rssdeck
go test ./...
```

Release binaries and the container image both stamp the version at link time:

```sh
go build -ldflags "-X main.version=v0.1.0" -o rssdeck ./cmd/rssdeck
```

For people who would rather not run Docker, each release has
`linux` and `darwin` binaries for `amd64` and `arm64` attached to it.

## Design

[DESIGN.md](DESIGN.md) is the full design, written before the code, including
the decisions this README only states the outcome of. Read it before opening a
pull request.

## License

MIT. See [LICENSE](LICENSE), and [CONTRIBUTING.md](CONTRIBUTING.md) if you want
to send a change.
