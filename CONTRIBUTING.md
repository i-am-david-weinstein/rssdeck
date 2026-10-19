# Contributing

Thanks for looking. A few things about how this repo works, so a change you
spend time on has somewhere to land.

## Read the design first

[DESIGN.md](DESIGN.md) was written before any code and it is the spec. It
records not only what rssdeck does but why, including the alternatives that
were rejected. Its "Build order" section is the plan.

If your change contradicts the design, that is worth doing, but say so in the
pull request and give the reasoning. A change that silently departs from it is
the one thing likely to be sent back.

## Running it

Go 1.26 or newer, and nothing else. The SQLite driver is pure Go, so there is
no C toolchain to install.

```sh
go build ./...
go vet ./...
go test ./...
```

CI runs those with `-race` plus
[golangci-lint](https://golangci-lint.run) and a two-architecture image build.
To match it locally:

```sh
CGO_ENABLED=1 go test -race ./...
golangci-lint run
docker build -t rssdeck:dev .
```

`.golangci.yml` enables the standard linters plus the ones that catch what this
codebase can actually get wrong, such as an undrained HTTP body or rows left
open on its single SQLite connection.

### Running against a real Readeck

The daemon makes one authenticated request to Readeck at startup and refuses to
serve if it fails, so you need either a real instance or a stand-in that
answers `GET /api/bookmarks` and `POST /api/bookmarks`:

```sh
RSSDECK_API_TOKEN=dev \
RSSDECK_READECK_URL=http://localhost:8000 \
RSSDECK_READECK_TOKEN=your-token \
RSSDECK_DB_PATH=./dev.db \
RSSDECK_ADDR=127.0.0.1:8080 \
RSSDECK_LOG_LEVEL=debug \
go run ./cmd/rssdeck
```

## Tests

Unit tests only, with hand-written fakes and no mocking framework.

The dedup and retention logic in `internal/sync` is where the once-only
guarantee lives, and it gets the heaviest coverage. A change there needs a test
that would fail without it.

Parser changes belong in `internal/feed/testdata` as a fixture. The corpus is
deliberately full of ugly real-world feeds: broken namespaces, mixed date
formats, duplicate GUIDs, items with no link. If you hit a feed in the wild
that rssdeck mishandles, the fixture is the most useful thing you can send,
with or without a fix.

Adding a route means updating `internal/api/openapi.yaml`. A test walks the
registered routes and fails when one is missing from the spec, so this is not
optional.

## Dependencies

There are two: `gofeed` for parsing and `modernc.org/sqlite` for storage.
Everything else is the standard library, and the small image and low memory
footprint follow from that.

A pull request that adds a dependency needs to say what it does that the
standard library cannot. That is a real argument sometimes, and
hand-rolling feed parsing over `encoding/xml` is exactly the trap `gofeed` is
here to avoid. It is just an argument that has to be made.

## Pull requests

One self-contained change per pull request, with the reasoning in the
description rather than only in the diff. Anything that departs from DESIGN.md,
or that you made a judgement call on, belongs under a heading where a reviewer
will see it.

Commit messages explain why, not what. The diff already says what.

Before opening a pull request, run the four commands above. `go build`,
`go vet`, the race tests and the linter all have to pass, and they will be
checked anyway.

## Reporting a bug

The useful report has the feed URL if there is one, the log lines around the
problem at `RSSDECK_LOG_LEVEL=debug`, the output of `rssdeck version`, and the
relevant row from `GET /api/items?feed_id=N`, which says what rssdeck thinks
the item's state is.

Please do not paste tokens. Both the API token and the Readeck token appear in
full in whatever you used to set them.

## Scope

The "Not in v0.1" list in the [README](README.md#not-in-v01) is a list of
things deliberately left out, not a backlog. Before building one of them, open
an issue. Keyword filtering is the likeliest first addition and the sync loop
already has the place to put it.

## License

Contributions are under the MIT license, the same as the rest of the project.
