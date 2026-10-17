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

const feedURL = "https://example.com/feed.xml"

func TestPollSendsNewItemsOldestFirst(t *testing.T) {
	h := newHarness(t)
	f := h.addFeed(feedURL, "Example Blog")
	h.fetcher.serve(feedURL, fetchStep{
		etag:         `"v1"`,
		lastModified: "Thu, 16 Oct 2026 11:00:00 GMT",
		body: rss("Example",
			testItem{guid: "c", link: "https://example.com/c", title: "Newest", published: start.Add(-time.Hour)},
			testItem{guid: "a", link: "https://example.com/a", title: "Oldest", published: start.Add(-3 * time.Hour)},
			testItem{guid: "b", link: "https://example.com/b", title: "Middle", published: start.Add(-2 * time.Hour)},
		),
	})

	res := h.poll(f.ID)

	if res.New != 3 || res.Sent != 3 || res.Existing != 0 || res.Failed != 0 {
		t.Fatalf("result = %+v, want 3 new and 3 sent", res)
	}
	want := []string{"https://example.com/a", "https://example.com/b", "https://example.com/c"}
	if got := h.readeck.urls(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("sent %v, want oldest first %v", got, want)
	}
	for _, b := range h.readeck.bookmarks() {
		if len(b.Labels) != 2 || b.Labels[0] != "Example Blog" || b.Labels[1] != "rssdeck" {
			t.Errorf("labels = %v, want the feed label then the global one", b.Labels)
		}
	}

	item := h.item(f.ID, "a")
	if item.Status != store.StatusSent {
		t.Errorf("status = %q, want sent", item.Status)
	}
	if item.ReadeckBookmarkID != "bm-1" {
		t.Errorf("bookmark id = %q, want bm-1", item.ReadeckBookmarkID)
	}
	if item.SentAt == nil || !item.SentAt.Equal(start) {
		t.Errorf("sent at = %v, want %v", item.SentAt, start)
	}

	polled := h.reload(f.ID)
	if polled.ETag != `"v1"` || polled.LastModified != "Thu, 16 Oct 2026 11:00:00 GMT" {
		t.Errorf("validators = %q / %q, want the response values", polled.ETag, polled.LastModified)
	}
	if polled.LastSuccessAt == nil || !polled.LastSuccessAt.Equal(start) {
		t.Errorf("last success = %v, want %v", polled.LastSuccessAt, start)
	}
	if polled.ConsecutiveFailures != 0 {
		t.Errorf("consecutive failures = %d, want 0", polled.ConsecutiveFailures)
	}
}

func TestPollNeverSendsAKnownItemTwice(t *testing.T) {
	h := newHarness(t)
	f := h.addFeed(feedURL, "Example Blog")
	document := rss("Example",
		testItem{guid: "a", link: "https://example.com/a", published: start.Add(-time.Hour)},
		testItem{guid: "b", link: "https://example.com/b", published: start.Add(-2 * time.Hour)},
	)
	h.fetcher.serve(feedURL, fetchStep{body: document})

	first := h.poll(f.ID)
	firstBookmarkID := h.item(f.ID, "a").ReadeckBookmarkID
	h.clock.advance(15 * time.Minute)
	second := h.poll(f.ID)

	if firstBookmarkID == "" {
		t.Fatal("the first poll recorded no bookmark id")
	}
	if first.Sent != 2 {
		t.Fatalf("first poll sent %d, want 2", first.Sent)
	}
	if second.Sent != 0 || second.New != 0 || second.Existing != 2 {
		t.Fatalf("second poll = %+v, want nothing new and nothing sent", second)
	}
	if len(h.readeck.bookmarks()) != 2 {
		t.Fatalf("created %d bookmarks, want 2", len(h.readeck.bookmarks()))
	}
	// The second poll refreshed the rows rather than touching their delivery.
	item := h.item(f.ID, "a")
	if !item.LastSeenInFeedAt.Equal(start.Add(15 * time.Minute)) {
		t.Errorf("last seen = %v, want the second poll's time", item.LastSeenInFeedAt)
	}
	if item.ReadeckBookmarkID != firstBookmarkID {
		t.Errorf("bookmark id = %q, want %q from the first poll", item.ReadeckBookmarkID, firstBookmarkID)
	}
}

func TestPollReplaysStoredValidators(t *testing.T) {
	h := newHarness(t)
	f := h.addFeed(feedURL, "")
	h.fetcher.serve(feedURL,
		fetchStep{body: rss("Example"), etag: `"v1"`, lastModified: "Thu, 16 Oct 2026 11:00:00 GMT"},
		fetchStep{notModified: true},
	)

	h.poll(f.ID)
	h.clock.advance(15 * time.Minute)
	h.poll(f.ID)

	conds := h.fetcher.conditionals(feedURL)
	if len(conds) != 2 {
		t.Fatalf("fetched %d times, want 2", len(conds))
	}
	if conds[0].ETag != "" || conds[0].LastModified != "" {
		t.Errorf("first fetch sent %+v, want no validators", conds[0])
	}
	if conds[1].ETag != `"v1"` || conds[1].LastModified != "Thu, 16 Oct 2026 11:00:00 GMT" {
		t.Errorf("second fetch sent %+v, want the stored validators", conds[1])
	}
}

func TestPollClearsValidatorsTheServerStoppedSending(t *testing.T) {
	h := newHarness(t)
	f := h.addFeed(feedURL, "")
	h.fetcher.serve(feedURL,
		fetchStep{body: rss("Example"), etag: `"v1"`},
		fetchStep{body: rss("Example")},
	)

	h.poll(f.ID)
	h.clock.advance(15 * time.Minute)
	h.poll(f.ID)

	if etag := h.reload(f.ID).ETag; etag != "" {
		t.Errorf("etag = %q, want it cleared", etag)
	}
}

func TestPollNotModifiedSkipsParseAndRetention(t *testing.T) {
	h := newHarness(t)
	f := h.addFeed(feedURL, "")
	h.fetcher.serve(feedURL,
		fetchStep{body: rss("Example", testItem{guid: "a", link: "https://example.com/a"}), etag: `"v1"`},
		fetchStep{notModified: true},
	)

	h.poll(f.ID)
	h.clock.advance(15 * time.Minute)
	res := h.poll(f.ID)

	if !res.NotModified {
		t.Fatal("result does not report a 304")
	}
	if res.New != 0 || res.Existing != 0 || res.Pruned != 0 {
		t.Errorf("result = %+v, want no ingest and no retention", res)
	}
	// The sent item has not been seen since the first poll, so a 304 that
	// counted as an empty document would have deleted it.
	if _, ok := h.items(f.ID)["a"]; !ok {
		t.Error("the 304 pruned an item the feed may still carry")
	}
	polled := h.reload(f.ID)
	if polled.ETag != `"v1"` {
		t.Errorf("etag = %q, want the stored one kept", polled.ETag)
	}
	if polled.LastSuccessAt == nil || !polled.LastSuccessAt.Equal(start.Add(15*time.Minute)) {
		t.Errorf("last success = %v, want the 304's time", polled.LastSuccessAt)
	}
}

func TestPollNotModifiedStillSendsPendingItems(t *testing.T) {
	h := newHarness(t)
	// Readeck is down for the first poll, so the item is left pending, and the
	// feed answers 304 from then on. The retry has to happen anyway.
	h.readeck.respond = func(n int, _ readeck.Bookmark) (string, error) {
		if n == 1 {
			return "", errors.New("502 Bad Gateway")
		}
		return "bm-ok", nil
	}
	f := h.addFeed(feedURL, "")
	h.fetcher.serve(feedURL,
		fetchStep{body: rss("Example", testItem{guid: "a", link: "https://example.com/a"})},
		fetchStep{notModified: true},
	)

	first := h.poll(f.ID)
	h.clock.advance(15 * time.Minute)
	second := h.poll(f.ID)

	if first.Sent != 0 || first.Failed != 1 {
		t.Fatalf("first poll = %+v, want one failed send", first)
	}
	if !second.NotModified || second.Sent != 1 {
		t.Fatalf("second poll = %+v, want the pending item sent on a 304", second)
	}
	if item := h.item(f.ID, "a"); item.Status != store.StatusSent {
		t.Errorf("status = %q, want sent", item.Status)
	}
}

func TestPollFetchErrorRecordsFailureAndKeepsItems(t *testing.T) {
	h := newHarness(t)
	f := h.addFeed(feedURL, "")
	h.fetcher.serve(feedURL,
		fetchStep{body: rss("Example", testItem{guid: "a", link: "https://example.com/a"}), etag: `"v1"`},
		fetchStep{err: errTimeout},
	)

	h.poll(f.ID)
	h.clock.advance(15 * time.Minute)
	_, err := h.syncer.Poll(context.Background(), f.ID)

	if !errors.Is(err, errTimeout) {
		t.Fatalf("Poll: %v, want the transport error", err)
	}
	polled := h.reload(f.ID)
	if polled.ConsecutiveFailures != 1 {
		t.Errorf("consecutive failures = %d, want 1", polled.ConsecutiveFailures)
	}
	if !strings.Contains(polled.LastError, "timeout") {
		t.Errorf("last error = %q, want the transport error", polled.LastError)
	}
	if polled.LastSuccessAt == nil || !polled.LastSuccessAt.Equal(start) {
		t.Errorf("last success = %v, want the first poll's time kept", polled.LastSuccessAt)
	}
	if polled.ETag != `"v1"` {
		t.Errorf("etag = %q, want it kept", polled.ETag)
	}
	if _, ok := h.items(f.ID)["a"]; !ok {
		t.Error("a failed fetch deleted an item")
	}
}

func TestPollParseErrorRecordsFailure(t *testing.T) {
	h := newHarness(t)
	f := h.addFeed(feedURL, "")
	h.fetcher.serve(feedURL, fetchStep{body: "<html><body>not a feed</body></html>"})

	if _, err := h.syncer.Poll(context.Background(), f.ID); err == nil {
		t.Fatal("Poll: want a parse error")
	}
	polled := h.reload(f.ID)
	if polled.ConsecutiveFailures != 1 {
		t.Errorf("consecutive failures = %d, want 1", polled.ConsecutiveFailures)
	}
	if polled.LastError == "" {
		t.Error("last error is empty, want the parse error")
	}
	if polled.LastSuccessAt != nil {
		t.Errorf("last success = %v, want nil", polled.LastSuccessAt)
	}
}

func TestPollSuccessClearsAnEarlierFailure(t *testing.T) {
	h := newHarness(t)
	f := h.addFeed(feedURL, "")
	h.fetcher.serve(feedURL,
		fetchStep{err: errTimeout},
		fetchStep{body: rss("Example")},
	)

	if _, err := h.syncer.Poll(context.Background(), f.ID); err == nil {
		t.Fatal("Poll: want the transport error")
	}
	h.clock.advance(15 * time.Minute)
	h.poll(f.ID)

	polled := h.reload(f.ID)
	if polled.ConsecutiveFailures != 0 {
		t.Errorf("consecutive failures = %d, want 0", polled.ConsecutiveFailures)
	}
	if polled.LastError != "" {
		t.Errorf("last error = %q, want it cleared", polled.LastError)
	}
}

func TestPollPrunesTerminalItemsThatLeftTheFeed(t *testing.T) {
	h := newHarness(t)
	// Readeck refuses the second item, so it stays pending while the first is
	// sent. Both then fall out of the feed.
	h.readeck.respond = func(n int, _ readeck.Bookmark) (string, error) {
		if n == 2 {
			return "", errors.New("502 Bad Gateway")
		}
		return "bm", nil
	}
	f := h.addFeed(feedURL, "")
	h.fetcher.serve(feedURL,
		fetchStep{body: rss("Example",
			testItem{guid: "gone-sent", link: "https://example.com/1", published: start.Add(-time.Hour)},
			testItem{guid: "gone-pending", link: "https://example.com/2", published: start.Add(-30 * time.Minute)},
		)},
		fetchStep{body: rss("Example",
			testItem{guid: "still-here", link: "https://example.com/3", published: start},
		)},
	)

	h.poll(f.ID)
	h.clock.advance(15 * time.Minute)
	res := h.poll(f.ID)

	// The item that was still pending is owed to Readeck even though it has
	// left the feed, so this poll sends it, and only then is its row finished
	// with and deleted along with the item sent on the first poll.
	// /2 appears twice: the attempt Readeck refused on the first poll, then the
	// send on the second, after the feed had stopped carrying it.
	want := []string{
		"https://example.com/1",
		"https://example.com/2",
		"https://example.com/2",
		"https://example.com/3",
	}
	if got := h.readeck.urls(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("sent %v, want %v", got, want)
	}
	if res.Pruned != 2 {
		t.Errorf("pruned %d, want 2", res.Pruned)
	}
	items := h.items(f.ID)
	for _, guid := range []string{"gone-sent", "gone-pending"} {
		if _, ok := items[guid]; ok {
			t.Errorf("item %s left the feed and has nothing left to do, so its row should be gone", guid)
		}
	}
	if _, ok := items["still-here"]; !ok {
		t.Error("the item the feed still carries is missing")
	}
}

func TestPollKeepsPendingItemsThatLeftTheFeed(t *testing.T) {
	h := newHarness(t)
	// Readeck is unavailable throughout, so the item can never finish.
	h.readeck.respond = func(int, readeck.Bookmark) (string, error) {
		return "", errors.New("502 Bad Gateway")
	}
	f := h.addFeed(feedURL, "")
	h.fetcher.serve(feedURL,
		fetchStep{body: rss("Example", testItem{guid: "a", link: "https://example.com/a"})},
		fetchStep{body: rss("Example")},
	)

	h.poll(f.ID)
	h.clock.advance(15 * time.Minute)
	res := h.poll(f.ID)

	if res.Pruned != 0 {
		t.Errorf("pruned %d, want 0: queued work that has not failed is kept", res.Pruned)
	}
	item := h.item(f.ID, "a")
	if item.Status != store.StatusPending {
		t.Errorf("status = %q, want pending", item.Status)
	}
	if item.Attempts != 2 {
		t.Errorf("attempts = %d, want 2: an item out of the feed is still retried", item.Attempts)
	}
}

func TestPollDoesNotPruneItemsFromThisPoll(t *testing.T) {
	h := newHarness(t)
	f := h.addFeed(feedURL, "")
	h.fetcher.serve(feedURL, fetchStep{body: rss("Example",
		testItem{guid: "a", link: "https://example.com/a"},
	)})

	res := h.poll(f.ID)

	if res.Pruned != 0 {
		t.Errorf("pruned %d, want 0: the item was just sent and is still in the feed", res.Pruned)
	}
	if _, ok := h.items(f.ID)["a"]; !ok {
		t.Fatal("the item was deleted in the poll that created it")
	}
}

func TestPollItemWithoutUsableURLFailsPermanently(t *testing.T) {
	h := newHarness(t)
	f := h.addFeed(feedURL, "")
	h.fetcher.serve(feedURL, fetchStep{body: rss("Example",
		testItem{guid: "urn:uuid:4f2a", title: "No link anywhere"},
	)})

	res := h.poll(f.ID)

	if res.Sent != 0 || res.Failed != 0 {
		t.Errorf("result = %+v, want nothing sent and no send attempted", res)
	}
	if len(h.readeck.bookmarks()) != 0 {
		t.Errorf("sent %v, want nothing: there is no URL to bookmark", h.readeck.urls())
	}
	item := h.item(f.ID, "urn:uuid:4f2a")
	if item.Status != store.StatusFailedPermanent {
		t.Errorf("status = %q, want failed_permanent", item.Status)
	}
	if item.LastError != noUsableURL {
		t.Errorf("last error = %q, want %q", item.LastError, noUsableURL)
	}
}

func TestPollRetriesAFailedSendUntilMaxFailures(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.MaxFailures = 3 })
	h.readeck.respond = func(int, readeck.Bookmark) (string, error) {
		return "", errors.New("500 Internal Server Error")
	}
	f := h.addFeed(feedURL, "")
	h.fetcher.serve(feedURL, fetchStep{body: rss("Example",
		testItem{guid: "a", link: "https://example.com/a"},
	)})

	for poll := 1; poll <= 3; poll++ {
		res := h.poll(f.ID)
		if res.Failed != 1 {
			t.Fatalf("poll %d: failed = %d, want 1", poll, res.Failed)
		}
		item := h.item(f.ID, "a")
		if item.Attempts != poll {
			t.Fatalf("poll %d: attempts = %d, want %d", poll, item.Attempts, poll)
		}
		wantStatus := store.StatusPending
		if poll == 3 {
			wantStatus = store.StatusFailedPermanent
		}
		if item.Status != wantStatus {
			t.Fatalf("poll %d: status = %q, want %q", poll, item.Status, wantStatus)
		}
		h.clock.advance(15 * time.Minute)
	}

	// Retired items are not retried, so the fourth poll sends nothing.
	before := len(h.readeck.bookmarks())
	h.poll(f.ID)
	if after := len(h.readeck.bookmarks()); after != before {
		t.Errorf("a retired item was sent again: %d sends, want %d", after, before)
	}
}

func TestPollUnauthorizedStopsSendingAndSpendsNoAttempts(t *testing.T) {
	h := newHarness(t)
	h.readeck.respond = func(int, readeck.Bookmark) (string, error) {
		return "", readeck.ErrUnauthorized
	}
	f := h.addFeed(feedURL, "")
	h.fetcher.serve(feedURL, fetchStep{body: rss("Example",
		testItem{guid: "a", link: "https://example.com/a", published: start.Add(-3 * time.Hour)},
		testItem{guid: "b", link: "https://example.com/b", published: start.Add(-2 * time.Hour)},
		testItem{guid: "c", link: "https://example.com/c", published: start.Add(-time.Hour)},
	)})

	res, err := h.syncer.Poll(context.Background(), f.ID)

	if !errors.Is(err, readeck.ErrUnauthorized) {
		t.Fatalf("Poll: %v, want ErrUnauthorized", err)
	}
	if n := len(h.readeck.bookmarks()); n != 1 {
		t.Errorf("tried %d sends, want 1: the rest would be rejected too", n)
	}
	if res.Sent != 0 || res.Failed != 1 {
		t.Errorf("result = %+v, want one failed send", res)
	}
	for guid, item := range h.items(f.ID) {
		if item.Status != store.StatusPending {
			t.Errorf("item %s: status = %q, want pending", guid, item.Status)
		}
		if item.Attempts != 0 {
			t.Errorf("item %s: attempts = %d, want 0: a bad token is not the item's fault", guid, item.Attempts)
		}
	}
	if item := h.item(f.ID, "a"); !strings.Contains(item.LastError, "token") {
		t.Errorf("last error = %q, want the rejected token recorded", item.LastError)
	}
	// The poll itself reached the feed and parsed it, so the feed is fine.
	if polled := h.reload(f.ID); polled.ConsecutiveFailures != 0 {
		t.Errorf("consecutive failures = %d, want 0: the feed is not what failed", polled.ConsecutiveFailures)
	}
}

func TestPollDuplicateGUIDsInOneDocumentSendOnce(t *testing.T) {
	h := newHarness(t)
	f := h.addFeed(feedURL, "")
	h.fetcher.serve(feedURL, fetchStep{body: rss("Example",
		testItem{guid: "a", link: "https://example.com/a"},
		testItem{guid: "a", link: "https://example.com/a-again"},
	)})

	res := h.poll(f.ID)

	if res.New != 1 || res.Existing != 1 || res.Sent != 1 {
		t.Errorf("result = %+v, want one row and one send", res)
	}
}

func TestPollDisabledFeedIsStillPolledOnRequest(t *testing.T) {
	h := newHarness(t)
	f := h.addFeed(feedURL, "")
	disabled := false
	if _, err := h.store.UpdateFeed(context.Background(), f.ID, store.FeedUpdate{Enabled: &disabled}); err != nil {
		t.Fatalf("UpdateFeed: %v", err)
	}
	h.fetcher.serve(feedURL, fetchStep{body: rss("Example",
		testItem{guid: "a", link: "https://example.com/a"},
	)})

	res := h.poll(f.ID)

	if res.Sent != 1 {
		t.Errorf("result = %+v, want the explicitly requested poll to send", res)
	}
	// Polling by hand must not put a disabled feed back on the calendar.
	if _, scheduled := h.syncer.sched.nextPoll(f.ID); scheduled {
		t.Error("a disabled feed was scheduled by polling it on request")
	}
}

func TestPollRefusesASecondConcurrentPoll(t *testing.T) {
	h := newHarness(t)
	f := h.addFeed(feedURL, "")
	h.fetcher.serve(feedURL, fetchStep{body: rss("Example")})

	started := make(chan struct{}, 2)
	release := make(chan struct{})
	h.fetcher.hook = func(context.Context, string) {
		started <- struct{}{}
		<-release
	}

	done := make(chan error, 1)
	go func() {
		_, err := h.syncer.Poll(context.Background(), f.ID)
		done <- err
	}()
	<-started

	if _, err := h.syncer.Poll(context.Background(), f.ID); !errors.Is(err, ErrPollInProgress) {
		t.Fatalf("second Poll: %v, want ErrPollInProgress", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first Poll: %v", err)
	}

	// The claim is released, so the next poll goes through.
	if _, err := h.syncer.Poll(context.Background(), f.ID); err != nil {
		t.Fatalf("Poll after the first finished: %v", err)
	}
}

func TestPollBoundsConcurrency(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Concurrency = 2 })
	urls := []string{
		"https://a.example/feed", "https://b.example/feed",
		"https://c.example/feed", "https://d.example/feed",
	}
	var ids []int64
	for _, u := range urls {
		ids = append(ids, h.addFeed(u, "").ID)
		h.fetcher.serve(u, fetchStep{body: rss("Example")})
	}

	arrived := make(chan struct{}, len(urls))
	release := make(chan struct{})
	h.fetcher.hook = func(context.Context, string) {
		arrived <- struct{}{}
		<-release
	}

	done := make(chan error, len(ids))
	for _, id := range ids {
		go func() {
			_, err := h.syncer.Poll(context.Background(), id)
			done <- err
		}()
	}

	for i := 0; i < 2; i++ {
		select {
		case <-arrived:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d polls started, want 2 running at once", i)
		}
	}
	// A third fetch here would mean the limit is not holding.
	select {
	case <-arrived:
		t.Fatal("a third poll started while two were already running")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	for range ids {
		if err := <-done; err != nil {
			t.Fatalf("Poll: %v", err)
		}
	}
	if n := h.fetcher.fetches(); n != len(urls) {
		t.Errorf("%d fetches, want %d", n, len(urls))
	}
}

func TestPollUnknownFeed(t *testing.T) {
	h := newHarness(t)
	_, err := h.syncer.Poll(context.Background(), 404)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Poll: %v, want ErrNotFound", err)
	}
	// A failed poll of a feed that does not exist leaves nothing behind.
	if _, scheduled := h.syncer.sched.nextPoll(404); scheduled {
		t.Error("an unknown feed was left on the calendar")
	}
}

func TestPollCanceledContextIsNotAFeedFailure(t *testing.T) {
	h := newHarness(t)
	f := h.addFeed(feedURL, "")
	h.fetcher.serve(feedURL, fetchStep{err: context.Canceled})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.fetcher.hook = func(context.Context, string) { cancel() }

	if _, err := h.syncer.Poll(ctx, f.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Poll: %v, want the cancellation", err)
	}

	// Shutdown is not the feed misbehaving, so it does not count against it.
	polled := h.reload(f.ID)
	if polled.ConsecutiveFailures != 0 {
		t.Errorf("consecutive failures = %d, want 0", polled.ConsecutiveFailures)
	}
	if polled.LastError != "" {
		t.Errorf("last error = %q, want nothing recorded", polled.LastError)
	}
}
