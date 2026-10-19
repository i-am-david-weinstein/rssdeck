# Changelog

Notable changes, newest first. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[semantic versioning](https://semver.org/spec/v2.0.0.html), with the caveat
that 0.x releases are allowed to break each other.

## Unreleased

Everything below is v0.1.0, which has not been tagged yet.

### Added

- Polls RSS, Atom and JSON feeds and sends each new item to a single Readeck
  instance once. Item identity is the feed's GUID, or the link, or a hash of
  the title and publication date, scoped per feed, and a row under that
  identity is what makes an item known.
- SQLite for all state, in WAL mode, with migrations by `PRAGMA user_version`
  and numbered SQL files embedded in the binary.
- HTTP API for feeds, items and tokens, authenticated with a bearer token. The
  master token comes from the configuration; further tokens are minted through
  the API, stored as hashes, and cannot manage other tokens.
- `openapi.yaml` served by the running instance at `/api/openapi.yaml`, so an
  agent can fetch the current spec rather than trusting a copy. A test asserts
  every registered route appears in it.
- Adding a feed fetches and parses it synchronously, so a URL that is not a
  feed fails the request. The newest `backfill` items are sent, default 1, and
  the rest of the document is recorded as already seen.
- Conditional requests on every poll, with `ETag` and `Last-Modified`. A `304`
  ends the poll with nothing downloaded, parsed or deleted.
- Per-feed poll intervals, jittered first polls, bounded concurrency, and a
  floor that keeps a restart loop from polling every feed every minute.
- Retention that deletes an item's row once it has left the feed document and
  has nothing left to do. Pending items that fall out are kept and still sent.
- Failed sends retry on the following poll and become `failed_permanent` after
  `RSSDECK_MAX_FAILURES` attempts. An item with no usable URL is recorded as
  failed rather than skipped quietly.
- Configuration from flags and environment variables, with `_FILE` forms for
  the two tokens. Every invalid setting is reported at once, so fixing several
  takes one restart.
- A startup probe against Readeck, so a bad token fails the boot instead of
  turning into silent failures hours later.
- Graceful shutdown on SIGTERM: stop scheduling, finish in-flight polls within
  30 seconds, drain the API, close the database.
- `rssdeck healthcheck`, which probes the local instance so the distroless
  image can check itself without a shell.
- Container image on `gcr.io/distroless/static:nonroot` for `amd64` and
  `arm64`, with `/data` owned by the runtime user so a named volume needs no
  chown, plus a `compose.yaml` and release binaries for linux and darwin.
