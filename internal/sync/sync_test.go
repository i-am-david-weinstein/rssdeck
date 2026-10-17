package sync

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/i-am-david-weinstein/rssdeck/internal/feed"
	"github.com/i-am-david-weinstein/rssdeck/internal/readeck"
	"github.com/i-am-david-weinstein/rssdeck/internal/store"
)

// start is the fixed time the test clock begins at.
var start = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

// testClock is the Syncer's clock, moved by hand so nothing in these tests
// depends on how long a test takes to run.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) current() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fetchStep is one scripted answer from a feed server.
type fetchStep struct {
	body         string
	etag         string
	lastModified string
	notModified  bool
	err          error
}

// stubFetcher serves scripted answers per feed URL and records the conditional
// headers it was asked to send. The last step for a URL repeats, so a test that
// polls in a loop does not have to script every poll.
type stubFetcher struct {
	mu    sync.Mutex
	steps map[string][]fetchStep
	conds map[string][]feed.Conditional
	calls int

	// hook runs before each answer, outside the lock, for tests that need a
	// fetch to block or to observe concurrency.
	hook func(ctx context.Context, url string)
}

func newStubFetcher() *stubFetcher {
	return &stubFetcher{
		steps: make(map[string][]fetchStep),
		conds: make(map[string][]feed.Conditional),
	}
}

// serve scripts the answers for one URL.
func (f *stubFetcher) serve(url string, steps ...fetchStep) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steps[url] = steps
}

func (f *stubFetcher) Fetch(ctx context.Context, url string, c feed.Conditional) (*feed.Response, error) {
	if f.hook != nil {
		f.hook(ctx, url)
	}

	f.mu.Lock()
	f.calls++
	f.conds[url] = append(f.conds[url], c)
	queue := f.steps[url]
	if len(queue) == 0 {
		f.mu.Unlock()
		return nil, fmt.Errorf("stub fetcher: nothing scripted for %s", url)
	}
	step := queue[0]
	if len(queue) > 1 {
		f.steps[url] = queue[1:]
	}
	f.mu.Unlock()

	switch {
	case step.err != nil:
		return nil, step.err
	case step.notModified:
		return &feed.Response{NotModified: true}, nil
	default:
		return &feed.Response{ETag: step.etag, LastModified: step.lastModified, Body: []byte(step.body)}, nil
	}
}

// conditionals returns the validators replayed to a URL, one entry per fetch.
func (f *stubFetcher) conditionals(url string) []feed.Conditional {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]feed.Conditional(nil), f.conds[url]...)
}

func (f *stubFetcher) fetches() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// stubReadeck records every bookmark it is asked to create. respond, when set,
// decides the outcome of the nth call.
type stubReadeck struct {
	mu      sync.Mutex
	created []readeck.Bookmark
	respond func(n int, b readeck.Bookmark) (string, error)
}

func (r *stubReadeck) CreateBookmark(ctx context.Context, b readeck.Bookmark) (string, error) {
	r.mu.Lock()
	r.created = append(r.created, b)
	n := len(r.created)
	respond := r.respond
	r.mu.Unlock()

	if respond != nil {
		return respond(n, b)
	}
	return fmt.Sprintf("bm-%d", n), nil
}

// bookmarks returns the bookmarks created so far, in order.
func (r *stubReadeck) bookmarks() []readeck.Bookmark {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]readeck.Bookmark(nil), r.created...)
}

// urls returns the bookmarked URLs in the order they were sent, which is what
// the oldest-first ordering is asserted against.
func (r *stubReadeck) urls() []string {
	out := []string{}
	for _, b := range r.bookmarks() {
		out = append(out, b.URL)
	}
	return out
}

// harness is a Syncer wired to an in-memory database and the two stubs.
type harness struct {
	t       *testing.T
	store   *store.Store
	fetcher *stubFetcher
	readeck *stubReadeck
	clock   *testClock
	syncer  *Syncer
}

// newHarness builds a Syncer whose clock, jitter and tick are deterministic:
// time only moves when a test moves it, and a scheduled feed's first poll is
// exactly when it is due. Tweaks run before New, so a test can opt back into
// the real jitter or change any option.
func newHarness(t *testing.T, tweaks ...func(*Options)) *harness {
	t.Helper()

	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})

	clock := &testClock{now: start}
	fetcher := newStubFetcher()
	bookmarker := &stubReadeck{}

	opts := Options{
		Store:       db,
		Fetcher:     fetcher,
		Readeck:     bookmarker,
		Interval:    15 * time.Minute,
		MaxFailures: DefaultMaxFailures,
		Backfill:    DefaultBackfill,
		GlobalLabel: "rssdeck",
		now:         clock.current,
		jitter:      func(time.Duration) time.Duration { return 0 },
		tick:        time.Millisecond,
	}
	for _, tweak := range tweaks {
		tweak(&opts)
	}

	syncer, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &harness{t: t, store: db, fetcher: fetcher, readeck: bookmarker, clock: clock, syncer: syncer}
}

// addFeed inserts a feed without going through AddFeed, for tests about
// polling rather than about adding.
func (h *harness) addFeed(url, label string) *store.Feed {
	h.t.Helper()
	f, err := h.store.CreateFeed(context.Background(), store.NewFeed{
		URL:       url,
		Title:     "Example",
		Label:     label,
		CreatedAt: h.clock.current(),
	})
	if err != nil {
		h.t.Fatalf("CreateFeed(%s): %v", url, err)
	}
	return f
}

// poll polls a feed and fails the test if it could not complete.
func (h *harness) poll(feedID int64) *PollResult {
	h.t.Helper()
	res, err := h.syncer.Poll(context.Background(), feedID)
	if err != nil {
		h.t.Fatalf("Poll(%d): %v", feedID, err)
	}
	return res
}

// items returns every row a feed has, keyed by GUID.
func (h *harness) items(feedID int64) map[string]store.Item {
	h.t.Helper()
	rows, _, err := h.store.ListItems(context.Background(), store.ItemFilter{FeedID: &feedID, Limit: store.MaxItemLimit})
	if err != nil {
		h.t.Fatalf("ListItems: %v", err)
	}
	byGUID := make(map[string]store.Item, len(rows))
	for _, row := range rows {
		byGUID[row.GUID] = row
	}
	return byGUID
}

// item returns one row by GUID.
func (h *harness) item(feedID int64, guid string) store.Item {
	h.t.Helper()
	row, ok := h.items(feedID)[guid]
	if !ok {
		h.t.Fatalf("no item %q for feed %d", guid, feedID)
	}
	return row
}

// reload re-reads a feed's bookkeeping.
func (h *harness) reload(feedID int64) *store.Feed {
	h.t.Helper()
	f, err := h.store.GetFeed(context.Background(), feedID)
	if err != nil {
		h.t.Fatalf("GetFeed(%d): %v", feedID, err)
	}
	return f
}

// testItem is one entry to render into a feed document.
type testItem struct {
	guid      string
	link      string
	title     string
	published time.Time
}

// rss renders an RSS 2.0 document. The parser is tested against real feeds in
// internal/feed, so these only have to be well formed.
func rss(title string, items ...testItem) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<rss version="2.0"><channel>` + "\n")
	fmt.Fprintf(&b, "<title>%s</title>\n", title)
	for _, it := range items {
		b.WriteString("<item>\n")
		if it.title != "" {
			fmt.Fprintf(&b, "<title>%s</title>\n", it.title)
		}
		if it.link != "" {
			fmt.Fprintf(&b, "<link>%s</link>\n", it.link)
		}
		if it.guid != "" {
			fmt.Fprintf(&b, `<guid isPermaLink="false">%s</guid>`+"\n", it.guid)
		}
		if !it.published.IsZero() {
			fmt.Fprintf(&b, "<pubDate>%s</pubDate>\n", it.published.Format(time.RFC1123Z))
		}
		b.WriteString("</item>\n")
	}
	b.WriteString("</channel></rss>\n")
	return b.String()
}

// errTimeout stands in for a transport failure.
var errTimeout = errors.New("dial tcp: i/o timeout")

func TestNewRequiresDependencies(t *testing.T) {
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	cases := map[string]Options{
		"no store":   {Fetcher: newStubFetcher(), Readeck: &stubReadeck{}},
		"no fetcher": {Store: db, Readeck: &stubReadeck{}},
		"no readeck": {Store: db, Fetcher: newStubFetcher()},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := New(opts); err == nil {
				t.Fatal("New: want an error")
			}
		})
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	s, err := New(Options{Store: db, Fetcher: newStubFetcher(), Readeck: &stubReadeck{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.interval != DefaultInterval {
		t.Errorf("interval = %v, want %v", s.interval, DefaultInterval)
	}
	if s.maxFailures != DefaultMaxFailures {
		t.Errorf("maxFailures = %d, want %d", s.maxFailures, DefaultMaxFailures)
	}
	if cap(s.slots) != DefaultConcurrency {
		t.Errorf("concurrency = %d, want %d", cap(s.slots), DefaultConcurrency)
	}
	if s.grace != DefaultShutdownGrace {
		t.Errorf("grace = %v, want %v", s.grace, DefaultShutdownGrace)
	}
	// A backfill of zero is a real choice, so New must not invent one.
	if s.backfill != 0 {
		t.Errorf("backfill = %d, want 0", s.backfill)
	}
}

func TestLabelsCombineFeedAndGlobal(t *testing.T) {
	h := newHarness(t)
	cases := map[string]struct {
		feedLabel string
		global    string
		want      []string
	}{
		"both":            {feedLabel: "Example Blog", global: "rssdeck", want: []string{"Example Blog", "rssdeck"}},
		"no feed label":   {feedLabel: "", global: "rssdeck", want: []string{"rssdeck"}},
		"no global label": {feedLabel: "Example Blog", global: "", want: []string{"Example Blog"}},
		"same label":      {feedLabel: "rssdeck", global: "rssdeck", want: []string{"rssdeck"}},
		"neither":         {feedLabel: "", global: "", want: nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h.syncer.globalLabel = tc.global
			got := h.syncer.labels(&store.Feed{Label: tc.feedLabel})
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("labels = %v, want %v", got, tc.want)
			}
		})
	}
}
