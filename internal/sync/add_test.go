package sync

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/i-am-david-weinstein/rssdeck/internal/readeck"
	"github.com/i-am-david-weinstein/rssdeck/internal/store"
)

// threeItems is a document whose newest entry is not the first one, so a
// backfill that took document order would pick the wrong item.
func threeItems() string {
	return rss("Example Blog",
		testItem{guid: "middle", link: "https://example.com/middle", title: "Middle", published: start.Add(-2 * time.Hour)},
		testItem{guid: "newest", link: "https://example.com/newest", title: "Newest", published: start.Add(-time.Hour)},
		testItem{guid: "oldest", link: "https://example.com/oldest", title: "Oldest", published: start.Add(-3 * time.Hour)},
	)
}

func TestAddFeedSeedsEverythingAndSendsTheNewest(t *testing.T) {
	h := newHarness(t)
	h.fetcher.serve(feedURL, fetchStep{body: threeItems(), etag: `"v1"`})

	f, err := h.syncer.AddFeed(context.Background(), AddRequest{URL: feedURL})
	if err != nil {
		t.Fatalf("AddFeed: %v", err)
	}

	if f.Title != "Example Blog" {
		t.Errorf("title = %q, want the feed's own title", f.Title)
	}
	if f.Label != "Example Blog" {
		t.Errorf("label = %q, want it defaulted from the title", f.Label)
	}
	if f.ETag != `"v1"` {
		t.Errorf("etag = %q, want the add's fetch to count as the first poll", f.ETag)
	}
	if f.LastPolledAt == nil || !f.LastPolledAt.Equal(start) {
		t.Errorf("last polled = %v, want %v", f.LastPolledAt, start)
	}

	items := h.items(f.ID)
	if len(items) != 3 {
		t.Fatalf("%d rows, want 3: every item in the document is recorded", len(items))
	}
	if status := items["newest"].Status; status != store.StatusSent {
		t.Errorf("newest item status = %q, want sent", status)
	}
	for _, guid := range []string{"middle", "oldest"} {
		if status := items[guid].Status; status != store.StatusSeeded {
			t.Errorf("%s status = %q, want seeded", guid, status)
		}
	}
	if sent := h.readeck.urls(); len(sent) != 1 || sent[0] != "https://example.com/newest" {
		t.Errorf("sent %v, want only the newest item", sent)
	}
	if labels := h.readeck.bookmarks()[0].Labels; strings.Join(labels, "|") != "Example Blog|rssdeck" {
		t.Errorf("labels = %v, want the feed label then the global one", labels)
	}

	// Added feeds are polled on the schedule, an interval out, since the
	// document has just been read.
	at, ok := h.syncer.sched.nextPoll(f.ID)
	if !ok {
		t.Fatal("the new feed is not on the calendar")
	}
	if !at.Equal(start.Add(15 * time.Minute)) {
		t.Errorf("next poll = %v, want an interval after the add", at)
	}
}

func TestAddFeedBackfillCounts(t *testing.T) {
	cases := map[string]struct {
		backfill *int
		wantSent []string
	}{
		"default of one": {backfill: nil, wantSent: []string{"https://example.com/newest"}},
		"none":           {backfill: ptr(0), wantSent: []string{}},
		"two":            {backfill: ptr(2), wantSent: []string{"https://example.com/middle", "https://example.com/newest"}},
		"more than the document holds": {backfill: ptr(10), wantSent: []string{
			"https://example.com/oldest", "https://example.com/middle", "https://example.com/newest",
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.fetcher.serve(feedURL, fetchStep{body: threeItems()})

			f, err := h.syncer.AddFeed(context.Background(), AddRequest{URL: feedURL, Backfill: tc.backfill})
			if err != nil {
				t.Fatalf("AddFeed: %v", err)
			}

			// Sends are oldest first, whichever items the backfill picked.
			if got := h.readeck.urls(); strings.Join(got, ",") != strings.Join(tc.wantSent, ",") {
				t.Errorf("sent %v, want %v", got, tc.wantSent)
			}
			if len(h.items(f.ID)) != 3 {
				t.Errorf("%d rows, want every item recorded whatever the backfill", len(h.items(f.ID)))
			}
		})
	}
}

func TestAddFeedUndatedItemsAreSeededBeforeDatedOnes(t *testing.T) {
	h := newHarness(t)
	h.fetcher.serve(feedURL, fetchStep{body: rss("Example",
		testItem{guid: "undated", link: "https://example.com/undated"},
		testItem{guid: "dated", link: "https://example.com/dated", published: start.Add(-time.Hour)},
	)})

	f, err := h.syncer.AddFeed(context.Background(), AddRequest{URL: feedURL})
	if err != nil {
		t.Fatalf("AddFeed: %v", err)
	}

	// An undated entry could be anything, so the dated one is the better guess
	// at what a reader would call recent.
	if status := h.item(f.ID, "dated").Status; status != store.StatusSent {
		t.Errorf("dated item status = %q, want sent", status)
	}
	if status := h.item(f.ID, "undated").Status; status != store.StatusSeeded {
		t.Errorf("undated item status = %q, want seeded", status)
	}
}

func TestAddFeedLabels(t *testing.T) {
	longTitle := strings.Repeat("Ü", 100)
	cases := map[string]struct {
		title     string
		requested *string
		want      string
	}{
		"defaults to the title":      {title: "Example Blog", want: "Example Blog"},
		"truncates a long title":     {title: longTitle, want: strings.Repeat("Ü", maxLabelLength)},
		"an explicit label wins":     {title: "Example Blog", requested: ptr("tech"), want: "tech"},
		"an explicit empty label":    {title: "Example Blog", requested: ptr(""), want: ""},
		"a title that is only space": {title: "   ", want: ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.fetcher.serve(feedURL, fetchStep{body: rss(tc.title)})

			f, err := h.syncer.AddFeed(context.Background(), AddRequest{URL: feedURL, Label: tc.requested})
			if err != nil {
				t.Fatalf("AddFeed: %v", err)
			}
			if f.Label != tc.want {
				t.Errorf("label = %q, want %q", f.Label, tc.want)
			}
		})
	}
}

func TestAddFeedStoresAPerFeedInterval(t *testing.T) {
	h := newHarness(t)
	h.fetcher.serve(feedURL, fetchStep{body: rss("Example")})
	minute := int64(60)

	f, err := h.syncer.AddFeed(context.Background(), AddRequest{URL: feedURL, IntervalSeconds: &minute})
	if err != nil {
		t.Fatalf("AddFeed: %v", err)
	}

	if f.IntervalSeconds == nil || *f.IntervalSeconds != 60 {
		t.Fatalf("interval = %v, want 60 seconds", f.IntervalSeconds)
	}
	at, _ := h.syncer.sched.nextPoll(f.ID)
	if !at.Equal(start.Add(time.Minute)) {
		t.Errorf("next poll = %v, want a minute after the add", at)
	}
}

func TestAddFeedRejectsWhatIsNotAFeed(t *testing.T) {
	cases := map[string]struct {
		url  string
		step fetchStep
		want error
	}{
		"not a url":            {url: "example.com/feed.xml", want: ErrInvalidURL},
		"wrong scheme":         {url: "ftp://example.com/feed.xml", want: ErrInvalidURL},
		"no host":              {url: "https:///feed.xml", want: ErrInvalidURL},
		"empty":                {url: "   ", want: ErrInvalidURL},
		"unreachable":          {url: feedURL, step: fetchStep{err: errTimeout}, want: ErrUnreachable},
		"an html page":         {url: feedURL, step: fetchStep{body: "<html><body>hello</body></html>"}, want: ErrNotAFeed},
		"json that is no feed": {url: feedURL, step: fetchStep{body: `{"items": []}`}, want: ErrNotAFeed},
		"a bare 304":           {url: feedURL, step: fetchStep{notModified: true}, want: ErrNotAFeed},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.fetcher.serve(tc.url, tc.step)

			_, err := h.syncer.AddFeed(context.Background(), AddRequest{URL: tc.url})

			if !errors.Is(err, tc.want) {
				t.Fatalf("AddFeed: %v, want %v", err, tc.want)
			}
			// Nothing is written for a feed that could not be read.
			feeds, err := h.store.ListFeeds(context.Background())
			if err != nil {
				t.Fatalf("ListFeeds: %v", err)
			}
			if len(feeds) != 0 {
				t.Errorf("%d feeds stored, want none", len(feeds))
			}
		})
	}
}

func TestAddFeedRejectsADuplicateBeforeFetching(t *testing.T) {
	h := newHarness(t)
	h.fetcher.serve(feedURL, fetchStep{body: rss("Example")})
	if _, err := h.syncer.AddFeed(context.Background(), AddRequest{URL: feedURL}); err != nil {
		t.Fatalf("AddFeed: %v", err)
	}
	before := h.fetcher.fetches()

	_, err := h.syncer.AddFeed(context.Background(), AddRequest{URL: feedURL})

	if !errors.Is(err, store.ErrDuplicateURL) {
		t.Fatalf("AddFeed: %v, want ErrDuplicateURL", err)
	}
	if after := h.fetcher.fetches(); after != before {
		t.Errorf("%d fetches, want %d: a known feed is not fetched again", after, before)
	}
}

func TestAddFeedSucceedsWhenTheBackfillCannotBeSent(t *testing.T) {
	h := newHarness(t)
	h.readeck.respond = func(int, readeck.Bookmark) (string, error) {
		return "", readeck.ErrUnauthorized
	}
	h.fetcher.serve(feedURL, fetchStep{body: threeItems()})

	f, err := h.syncer.AddFeed(context.Background(), AddRequest{URL: feedURL})
	if err != nil {
		t.Fatalf("AddFeed: %v, want the add to succeed: the item is queued either way", err)
	}

	item := h.item(f.ID, "newest")
	if item.Status != store.StatusPending {
		t.Errorf("status = %q, want pending so the next poll retries it", item.Status)
	}
}

func ptr[T any](v T) *T { return &v }
