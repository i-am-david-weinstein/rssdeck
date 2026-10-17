// Package sync owns rssdeck's poll loop: it decides when a feed is due, turns a
// fetched document into item rows, sends the new ones to Readeck, and deletes
// the rows that have nothing left to do.
//
// The once-only guarantee lives here and in internal/store. An item's row is
// what makes it known, so a poll only sends items that have no row yet, and
// retention only deletes rows for items the feed has stopped serving.
//
// Scheduling is in-memory and owned entirely by this package. Nothing about
// when a feed polls next is persisted, so adding or removing a feed never
// disturbs the timing of the others.
package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/i-am-david-weinstein/rssdeck/internal/feed"
	"github.com/i-am-david-weinstein/rssdeck/internal/readeck"
	"github.com/i-am-david-weinstein/rssdeck/internal/store"
)

// Defaults applied by New to the options left at zero, except DefaultBackfill:
// a backfill of zero is a real choice, so New uses whatever it is given and the
// configuration layer supplies this number.
const (
	DefaultInterval      = 15 * time.Minute
	DefaultConcurrency   = 4
	DefaultMaxFailures   = 5
	DefaultBackfill      = 1
	DefaultShutdownGrace = 30 * time.Second

	// defaultTick is how often the scheduler looks for due feeds. Poll
	// intervals are minutes, so a second of imprecision is free and a timer per
	// feed is not worth the bookkeeping.
	defaultTick = time.Second
)

// Errors callers are expected to branch on. The three AddFeed errors are all the
// caller's fault rather than the daemon's, which is what makes them a 400.
var (
	// ErrPollInProgress means the feed is already being polled, by the
	// scheduler or by another request. Polling it twice at once could send the
	// same item twice.
	ErrPollInProgress = errors.New("sync: feed is already being polled")

	// ErrInvalidURL means the URL is not an absolute http or https URL.
	ErrInvalidURL = errors.New("sync: invalid feed url")

	// ErrUnreachable means the feed URL could not be fetched.
	ErrUnreachable = errors.New("sync: feed could not be fetched")

	// ErrNotAFeed means the URL answered with something that is not a feed.
	ErrNotAFeed = errors.New("sync: url is not a feed")
)

// FeedFetcher is the part of *feed.Fetcher the poll loop needs. It is an
// interface so tests can serve documents, 304s and transport errors without an
// HTTP server.
type FeedFetcher interface {
	Fetch(ctx context.Context, url string, c feed.Conditional) (*feed.Response, error)
}

// Bookmarker is the part of *readeck.Client the poll loop needs.
type Bookmarker interface {
	CreateBookmark(ctx context.Context, b readeck.Bookmark) (string, error)
}

// Options configures a Syncer. Store, Fetcher and Readeck are required;
// everything else has a default.
type Options struct {
	Store   *store.Store
	Fetcher FeedFetcher
	Readeck Bookmarker
	Logger  *slog.Logger

	// Interval is the global poll interval, used by every feed that has no
	// interval of its own.
	Interval time.Duration

	// Concurrency bounds how many feeds are polled at once, including polls
	// requested through the API.
	Concurrency int

	// MaxFailures is how many failed sends an item takes before it is given up
	// on as failed_permanent.
	MaxFailures int

	// Backfill is how many of a feed's newest items AddFeed sends when the
	// request does not say. Zero seeds the whole document and sends nothing.
	Backfill int

	// GlobalLabel is added to every bookmark. Empty disables it, so it has no
	// default here.
	GlobalLabel string

	// ShutdownGrace is how long Run waits for in-flight polls after its context
	// is canceled.
	ShutdownGrace time.Duration

	// now, jitter and tick are the scheduler's clock, replaced by tests to make
	// timing deterministic. Production always uses the defaults.
	now    func() time.Time
	jitter func(time.Duration) time.Duration
	tick   time.Duration
}

// Syncer polls feeds and sends their items to Readeck. One instance serves the
// scheduler and the HTTP API, which is what keeps a feed from being polled
// twice at once. It is safe for concurrent use.
type Syncer struct {
	store   *store.Store
	fetcher FeedFetcher
	readeck Bookmarker
	log     *slog.Logger

	interval    time.Duration
	maxFailures int
	backfill    int
	globalLabel string
	grace       time.Duration
	tick        time.Duration

	now    func() time.Time
	jitter func(time.Duration) time.Duration

	// slots bounds concurrent polls. A poll holds one slot for its whole run,
	// fetch through send, because the point is to be polite to feed servers and
	// to Readeck at the same time.
	slots chan struct{}

	sched *schedule
}

// New validates options and returns a Syncer. It does not touch the database or
// the network; Run and AddFeed do that.
func New(o Options) (*Syncer, error) {
	switch {
	case o.Store == nil:
		return nil, errors.New("sync: nil store")
	case o.Fetcher == nil:
		return nil, errors.New("sync: nil fetcher")
	case o.Readeck == nil:
		return nil, errors.New("sync: nil readeck client")
	}

	s := &Syncer{
		store:       o.Store,
		fetcher:     o.Fetcher,
		readeck:     o.Readeck,
		log:         o.Logger,
		interval:    o.Interval,
		maxFailures: o.MaxFailures,
		backfill:    max(o.Backfill, 0),
		globalLabel: o.GlobalLabel,
		grace:       o.ShutdownGrace,
		tick:        o.tick,
		now:         o.now,
		jitter:      o.jitter,
		sched:       newSchedule(),
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.interval <= 0 {
		s.interval = DefaultInterval
	}
	if s.maxFailures < 1 {
		s.maxFailures = DefaultMaxFailures
	}
	if s.grace <= 0 {
		s.grace = DefaultShutdownGrace
	}
	if s.tick <= 0 {
		s.tick = defaultTick
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.jitter == nil {
		s.jitter = defaultJitter
	}

	concurrency := o.Concurrency
	if concurrency < 1 {
		concurrency = DefaultConcurrency
	}
	s.slots = make(chan struct{}, concurrency)
	return s, nil
}

// labels are the labels one feed's bookmarks carry: the feed's own label, then
// the global one. A feed labelled the same as the global label does not get it
// twice.
func (s *Syncer) labels(f *store.Feed) []string {
	var labels []string
	if f.Label != "" {
		labels = append(labels, f.Label)
	}
	if s.globalLabel != "" && !slices.Contains(labels, s.globalLabel) {
		labels = append(labels, s.globalLabel)
	}
	return labels
}

// acquire takes a concurrency slot, giving up if ctx is canceled first.
func (s *Syncer) acquire(ctx context.Context) error {
	select {
	case s.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("sync: waiting for a poll slot: %w", ctx.Err())
	}
}

func (s *Syncer) release() {
	<-s.slots
}
