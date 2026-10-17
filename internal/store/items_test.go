package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// newFeed creates a feed to hang items off.
func newFeed(t *testing.T, s *Store, url string) *Feed {
	t.Helper()
	feed, err := s.CreateFeed(context.Background(), NewFeed{URL: url, CreatedAt: base})
	if err != nil {
		t.Fatalf("CreateFeed(%s): %v", url, err)
	}
	return feed
}

// guids reads the GUIDs out of a slice of items, for order assertions.
func guids(items []Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.GUID
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestIngestItemsInsertsNewItems(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	feed := newFeed(t, s, "https://example.com/feed.xml")

	published := base.Add(-time.Hour)
	result, err := s.IngestItems(ctx, []NewItem{
		{
			FeedID:      feed.ID,
			GUID:        "guid-1",
			URL:         "https://example.com/one",
			Title:       "One",
			PublishedAt: &published,
			Status:      StatusPending,
			SeenAt:      base,
		},
		{FeedID: feed.ID, GUID: "guid-2", URL: "https://example.com/two", Status: StatusSeeded, SeenAt: base},
	})
	if err != nil {
		t.Fatalf("IngestItems: %v", err)
	}
	if result.New != 2 || result.Existing != 0 {
		t.Errorf("result = %+v, want New 2 Existing 0", result)
	}

	items, total, err := s.ListItems(ctx, ItemFilter{})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if total != 2 {
		t.Fatalf("total = %d, want 2", total)
	}

	byGUID := map[string]Item{}
	for _, it := range items {
		byGUID[it.GUID] = it
	}
	one := byGUID["guid-1"]
	if one.URL != "https://example.com/one" {
		t.Errorf("url = %q, want %q", one.URL, "https://example.com/one")
	}
	if one.Title != "One" {
		t.Errorf("title = %q, want %q", one.Title, "One")
	}
	if one.PublishedAt == nil || !one.PublishedAt.Equal(published) {
		t.Errorf("published_at = %v, want %v", one.PublishedAt, published)
	}
	if one.Status != StatusPending {
		t.Errorf("status = %q, want pending", one.Status)
	}
	if !one.FirstSeenAt.Equal(base) || !one.LastSeenInFeedAt.Equal(base) {
		t.Errorf("seen times = %v/%v, want both %v", one.FirstSeenAt, one.LastSeenInFeedAt, base)
	}
	if byGUID["guid-2"].Status != StatusSeeded {
		t.Errorf("status = %q, want seeded", byGUID["guid-2"].Status)
	}
}

// The once-only guarantee: a known item is only ever touched on its last-seen
// time. Its status, attempts and bookmark id survive every later poll.
func TestIngestItemsNeverResurrectsAKnownItem(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	feed := newFeed(t, s, "https://example.com/feed.xml")

	item := NewItem{
		FeedID: feed.ID,
		GUID:   "guid-1",
		URL:    "https://example.com/one",
		Title:  "One",
		Status: StatusPending,
		SeenAt: base,
	}
	if _, err := s.IngestItems(ctx, []NewItem{item}); err != nil {
		t.Fatalf("first IngestItems: %v", err)
	}

	pending, err := s.PendingItems(ctx, feed.ID)
	if err != nil {
		t.Fatalf("PendingItems: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("len(pending) = %d, want 1", len(pending))
	}
	if err := s.MarkItemSent(ctx, pending[0].ID, "bookmark-7", base.Add(time.Second)); err != nil {
		t.Fatalf("MarkItemSent: %v", err)
	}

	// The feed keeps serving the item for three more polls, and the add-time
	// status in the document would be pending again.
	for i := 1; i <= 3; i++ {
		item.SeenAt = base.Add(time.Duration(i) * time.Hour)
		result, err := s.IngestItems(ctx, []NewItem{item})
		if err != nil {
			t.Fatalf("re-ingest %d: %v", i, err)
		}
		if result.New != 0 || result.Existing != 1 {
			t.Errorf("re-ingest %d result = %+v, want New 0 Existing 1", i, result)
		}
	}

	got, err := s.GetItem(ctx, pending[0].ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if got.Status != StatusSent {
		t.Errorf("status = %q, want it still sent", got.Status)
	}
	if got.ReadeckBookmarkID != "bookmark-7" {
		t.Errorf("readeck_bookmark_id = %q, want it kept", got.ReadeckBookmarkID)
	}
	if !got.LastSeenInFeedAt.Equal(base.Add(3 * time.Hour)) {
		t.Errorf("last_seen_in_feed_at = %v, want %v", got.LastSeenInFeedAt, base.Add(3*time.Hour))
	}
	if !got.FirstSeenAt.Equal(base) {
		t.Errorf("first_seen_at = %v, want the original %v", got.FirstSeenAt, base)
	}

	again, err := s.PendingItems(ctx, feed.ID)
	if err != nil {
		t.Fatalf("PendingItems: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("%d items queued for a second send, want 0", len(again))
	}
}

// Changed metadata on a known item is ignored. Rewriting a URL after the
// bookmark exists would only create a second one later.
func TestIngestItemsIgnoresChangedMetadata(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	feed := newFeed(t, s, "https://example.com/feed.xml")

	if _, err := s.IngestItems(ctx, []NewItem{{
		FeedID: feed.ID, GUID: "guid-1", URL: "https://example.com/one",
		Title: "Original", Status: StatusPending, SeenAt: base,
	}}); err != nil {
		t.Fatalf("IngestItems: %v", err)
	}
	if _, err := s.IngestItems(ctx, []NewItem{{
		FeedID: feed.ID, GUID: "guid-1", URL: "https://example.com/rewritten",
		Title: "Edited", Status: StatusPending, SeenAt: base.Add(time.Hour),
	}}); err != nil {
		t.Fatalf("re-ingest: %v", err)
	}

	items, _, err := s.ListItems(ctx, ItemFilter{})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("len = %d, want 1", len(items))
	}
	if items[0].URL != "https://example.com/one" {
		t.Errorf("url = %q, want the original", items[0].URL)
	}
	if items[0].Title != "Original" {
		t.Errorf("title = %q, want the original", items[0].Title)
	}
}

// Real feeds repeat a GUID within one document. The second occurrence must count
// as already known, not as a new item.
func TestIngestItemsHandlesDuplicateGUIDInOneDocument(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	feed := newFeed(t, s, "https://example.com/feed.xml")

	item := NewItem{FeedID: feed.ID, GUID: "guid-1", URL: "https://example.com/one", Status: StatusPending, SeenAt: base}
	result, err := s.IngestItems(ctx, []NewItem{item, item, item})
	if err != nil {
		t.Fatalf("IngestItems: %v", err)
	}
	if result.New != 1 {
		t.Errorf("New = %d, want 1", result.New)
	}
	if result.Existing != 2 {
		t.Errorf("Existing = %d, want 2", result.Existing)
	}

	_, total, err := s.ListItems(ctx, ItemFilter{})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if total != 1 {
		t.Errorf("total = %d, want 1 row", total)
	}
}

// The same article in two feeds is two bookmarks, because identity is scoped to
// the feed.
func TestIngestItemsScopesIdentityToFeed(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	first := newFeed(t, s, "https://a.example/feed.xml")
	second := newFeed(t, s, "https://b.example/feed.xml")

	for _, feedID := range []int64{first.ID, second.ID} {
		result, err := s.IngestItems(ctx, []NewItem{{
			FeedID: feedID, GUID: "shared-guid", URL: "https://example.com/one",
			Status: StatusPending, SeenAt: base,
		}})
		if err != nil {
			t.Fatalf("IngestItems for feed %d: %v", feedID, err)
		}
		if result.New != 1 {
			t.Errorf("feed %d: New = %d, want 1", feedID, result.New)
		}
	}
}

func TestIngestItemsRejectsBadInput(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	feed := newFeed(t, s, "https://example.com/feed.xml")

	tests := []struct {
		name string
		item NewItem
		want string
	}{
		{
			name: "unknown status",
			item: NewItem{FeedID: feed.ID, GUID: "guid-1", Status: ItemStatus("queued"), SeenAt: base},
			want: "invalid status",
		},
		{
			name: "empty status",
			item: NewItem{FeedID: feed.ID, GUID: "guid-1", SeenAt: base},
			want: "invalid status",
		},
		{
			name: "empty guid",
			item: NewItem{FeedID: feed.ID, Status: StatusPending, SeenAt: base},
			want: "empty guid",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s.IngestItems(ctx, []NewItem{tt.item})
			if err == nil {
				t.Fatalf("IngestItems succeeded, want an error containing %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}

	if _, total, err := s.ListItems(ctx, ItemFilter{}); err != nil || total != 0 {
		t.Errorf("total = %d (err %v), want nothing written", total, err)
	}
}

func TestIngestItemsEmptySliceIsANoop(t *testing.T) {
	result, err := newStore(t).IngestItems(context.Background(), nil)
	if err != nil {
		t.Fatalf("IngestItems(nil): %v", err)
	}
	if result != (IngestResult{}) {
		t.Errorf("result = %+v, want the zero value", result)
	}
}

// One document is one transaction, so a failure part-way through leaves none of
// its items behind to be half-ingested on the next poll.
func TestIngestItemsIsAtomic(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	feed := newFeed(t, s, "https://example.com/feed.xml")

	_, err := s.IngestItems(ctx, []NewItem{
		{FeedID: feed.ID, GUID: "guid-1", URL: "https://example.com/one", Status: StatusPending, SeenAt: base},
		{FeedID: 404, GUID: "guid-2", URL: "https://example.com/two", Status: StatusPending, SeenAt: base},
	})
	if err == nil {
		t.Fatal("IngestItems succeeded with an item pointing at a missing feed, want error")
	}

	_, total, err := s.ListItems(ctx, ItemFilter{})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if total != 0 {
		t.Errorf("%d items survived a rolled-back ingest, want 0", total)
	}
}

// An item with no usable URL is recorded, not skipped, so it shows up in the API
// with a reason instead of vanishing.
func TestIngestItemsRecordsUnsendableItems(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	feed := newFeed(t, s, "https://example.com/feed.xml")

	if _, err := s.IngestItems(ctx, []NewItem{{
		FeedID:    feed.ID,
		GUID:      "guid-1",
		Title:     "No link anywhere",
		Status:    StatusFailedPermanent,
		LastError: "no usable URL",
		SeenAt:    base,
	}}); err != nil {
		t.Fatalf("IngestItems: %v", err)
	}

	items, _, err := s.ListItems(ctx, ItemFilter{})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("len = %d, want 1", len(items))
	}
	if items[0].Status != StatusFailedPermanent {
		t.Errorf("status = %q, want failed_permanent", items[0].Status)
	}
	if items[0].LastError != "no usable URL" {
		t.Errorf("last_error = %q, want %q", items[0].LastError, "no usable URL")
	}

	pending, err := s.PendingItems(ctx, feed.ID)
	if err != nil {
		t.Fatalf("PendingItems: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("%d unsendable items queued for sending, want 0", len(pending))
	}
}

// Readeck lists newest first, so sending oldest first makes its order match the
// feed's chronology.
func TestPendingItemsAreOldestFirst(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	feed := newFeed(t, s, "https://example.com/feed.xml")

	newest := base.Add(-1 * time.Hour)
	middle := base.Add(-2 * time.Hour)
	oldest := base.Add(-3 * time.Hour)

	_, err := s.IngestItems(ctx, []NewItem{
		{FeedID: feed.ID, GUID: "newest", URL: "https://example.com/3", PublishedAt: &newest, Status: StatusPending, SeenAt: base},
		{FeedID: feed.ID, GUID: "oldest", URL: "https://example.com/1", PublishedAt: &oldest, Status: StatusPending, SeenAt: base},
		{FeedID: feed.ID, GUID: "middle", URL: "https://example.com/2", PublishedAt: &middle, Status: StatusPending, SeenAt: base},
	})
	if err != nil {
		t.Fatalf("IngestItems: %v", err)
	}

	pending, err := s.PendingItems(ctx, feed.ID)
	if err != nil {
		t.Fatalf("PendingItems: %v", err)
	}
	want := []string{"oldest", "middle", "newest"}
	if got := guids(pending); !equalStrings(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// An item with no date falls back to when rssdeck first saw it, which keeps the
// order deterministic instead of leaving undated items to sort arbitrarily.
func TestPendingItemsFallBackToFirstSeen(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	feed := newFeed(t, s, "https://example.com/feed.xml")

	dated := base.Add(-30 * time.Minute)
	if _, err := s.IngestItems(ctx, []NewItem{
		{FeedID: feed.ID, GUID: "undated-early", URL: "https://example.com/1", Status: StatusPending, SeenAt: base.Add(-time.Hour)},
	}); err != nil {
		t.Fatalf("IngestItems: %v", err)
	}
	if _, err := s.IngestItems(ctx, []NewItem{
		{FeedID: feed.ID, GUID: "dated", URL: "https://example.com/2", PublishedAt: &dated, Status: StatusPending, SeenAt: base},
		{FeedID: feed.ID, GUID: "undated-late", URL: "https://example.com/3", Status: StatusPending, SeenAt: base},
	}); err != nil {
		t.Fatalf("IngestItems: %v", err)
	}

	pending, err := s.PendingItems(ctx, feed.ID)
	if err != nil {
		t.Fatalf("PendingItems: %v", err)
	}
	want := []string{"undated-early", "dated", "undated-late"}
	if got := guids(pending); !equalStrings(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestPendingItemsIsScopedToOneFeed(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	mine := newFeed(t, s, "https://a.example/feed.xml")
	other := newFeed(t, s, "https://b.example/feed.xml")

	if _, err := s.IngestItems(ctx, []NewItem{
		{FeedID: mine.ID, GUID: "mine", URL: "https://a.example/1", Status: StatusPending, SeenAt: base},
		{FeedID: other.ID, GUID: "theirs", URL: "https://b.example/1", Status: StatusPending, SeenAt: base},
	}); err != nil {
		t.Fatalf("IngestItems: %v", err)
	}

	pending, err := s.PendingItems(ctx, mine.ID)
	if err != nil {
		t.Fatalf("PendingItems: %v", err)
	}
	if got := guids(pending); !equalStrings(got, []string{"mine"}) {
		t.Errorf("pending = %v, want just the one feed's item", got)
	}
}

func TestMarkItemSent(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	feed := newFeed(t, s, "https://example.com/feed.xml")

	if _, err := s.IngestItems(ctx, []NewItem{
		{FeedID: feed.ID, GUID: "guid-1", URL: "https://example.com/one", Status: StatusPending, SeenAt: base},
	}); err != nil {
		t.Fatalf("IngestItems: %v", err)
	}
	pending, err := s.PendingItems(ctx, feed.ID)
	if err != nil {
		t.Fatalf("PendingItems: %v", err)
	}

	// A failure, then a retry that succeeds, must leave no stale error behind.
	if _, _, err := s.RecordSendFailure(ctx, pending[0].ID, "502 Bad Gateway", 5); err != nil {
		t.Fatalf("RecordSendFailure: %v", err)
	}
	sentAt := base.Add(time.Minute)
	if err := s.MarkItemSent(ctx, pending[0].ID, "bookmark-7", sentAt); err != nil {
		t.Fatalf("MarkItemSent: %v", err)
	}

	got, err := s.GetItem(ctx, pending[0].ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if got.Status != StatusSent {
		t.Errorf("status = %q, want sent", got.Status)
	}
	if got.ReadeckBookmarkID != "bookmark-7" {
		t.Errorf("readeck_bookmark_id = %q, want %q", got.ReadeckBookmarkID, "bookmark-7")
	}
	if got.SentAt == nil || !got.SentAt.Equal(sentAt) {
		t.Errorf("sent_at = %v, want %v", got.SentAt, sentAt)
	}
	if got.LastError != "" {
		t.Errorf("last_error = %q, want it cleared by the successful send", got.LastError)
	}
	if got.Attempts != 1 {
		t.Errorf("attempts = %d, want the 1 failed attempt kept as history", got.Attempts)
	}
}

func TestRecordSendFailureGivesUpAtMaxFailures(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	feed := newFeed(t, s, "https://example.com/feed.xml")

	if _, err := s.IngestItems(ctx, []NewItem{
		{FeedID: feed.ID, GUID: "guid-1", URL: "https://example.com/one", Status: StatusPending, SeenAt: base},
	}); err != nil {
		t.Fatalf("IngestItems: %v", err)
	}
	pending, err := s.PendingItems(ctx, feed.ID)
	if err != nil {
		t.Fatalf("PendingItems: %v", err)
	}
	id := pending[0].ID

	const maxFailures = 3
	for attempt := 1; attempt <= maxFailures; attempt++ {
		status, attempts, err := s.RecordSendFailure(ctx, id, "502 Bad Gateway", maxFailures)
		if err != nil {
			t.Fatalf("RecordSendFailure %d: %v", attempt, err)
		}
		if attempts != attempt {
			t.Errorf("attempt %d: attempts = %d, want %d", attempt, attempts, attempt)
		}

		wantStatus := StatusPending
		if attempt == maxFailures {
			wantStatus = StatusFailedPermanent
		}
		if status != wantStatus {
			t.Errorf("attempt %d: status = %q, want %q", attempt, status, wantStatus)
		}

		// A pending item is retried on the next poll; a failed one is not.
		queued, err := s.PendingItems(ctx, feed.ID)
		if err != nil {
			t.Fatalf("PendingItems: %v", err)
		}
		wantQueued := 1
		if attempt == maxFailures {
			wantQueued = 0
		}
		if len(queued) != wantQueued {
			t.Errorf("attempt %d: %d items queued, want %d", attempt, len(queued), wantQueued)
		}
	}

	got, err := s.GetItem(ctx, id)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if got.LastError != "502 Bad Gateway" {
		t.Errorf("last_error = %q, want the last cause recorded", got.LastError)
	}
}

func TestRecordSendFailureTreatsMaxBelowOneAsOne(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	feed := newFeed(t, s, "https://example.com/feed.xml")

	if _, err := s.IngestItems(ctx, []NewItem{
		{FeedID: feed.ID, GUID: "guid-1", URL: "https://example.com/one", Status: StatusPending, SeenAt: base},
	}); err != nil {
		t.Fatalf("IngestItems: %v", err)
	}
	pending, _ := s.PendingItems(ctx, feed.ID)

	status, attempts, err := s.RecordSendFailure(ctx, pending[0].ID, "boom", 0)
	if err != nil {
		t.Fatalf("RecordSendFailure: %v", err)
	}
	if status != StatusFailedPermanent {
		t.Errorf("status = %q, want failed_permanent", status)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
}

// Retention is the only thing keeping the database bounded, and the only thing
// that can lose an item. Both halves get checked here.
func TestPruneTerminalItems(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	feed := newFeed(t, s, "https://example.com/feed.xml")
	other := newFeed(t, s, "https://b.example/feed.xml")

	departed := base
	pollStart := base.Add(time.Hour)

	// Four items the feed has dropped, one per status, plus a current one.
	if _, err := s.IngestItems(ctx, []NewItem{
		{FeedID: feed.ID, GUID: "gone-sent", URL: "https://example.com/1", Status: StatusPending, SeenAt: departed},
		{FeedID: feed.ID, GUID: "gone-failed", URL: "https://example.com/2", Status: StatusPending, SeenAt: departed},
		{FeedID: feed.ID, GUID: "gone-seeded", URL: "https://example.com/3", Status: StatusSeeded, SeenAt: departed},
		{FeedID: feed.ID, GUID: "gone-pending", URL: "https://example.com/4", Status: StatusPending, SeenAt: departed},
		{FeedID: other.ID, GUID: "other-sent", URL: "https://b.example/1", Status: StatusPending, SeenAt: departed},
	}); err != nil {
		t.Fatalf("IngestItems: %v", err)
	}

	byGUID := map[string]int64{}
	items, _, err := s.ListItems(ctx, ItemFilter{})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	for _, it := range items {
		byGUID[it.GUID] = it.ID
	}

	if err := s.MarkItemSent(ctx, byGUID["gone-sent"], "bookmark-1", departed); err != nil {
		t.Fatalf("MarkItemSent: %v", err)
	}
	if _, _, err := s.RecordSendFailure(ctx, byGUID["gone-failed"], "410 Gone", 1); err != nil {
		t.Fatalf("RecordSendFailure: %v", err)
	}
	if err := s.MarkItemSent(ctx, byGUID["other-sent"], "bookmark-2", departed); err != nil {
		t.Fatalf("MarkItemSent: %v", err)
	}

	// A sent item the feed still carries, seen by the poll being pruned for.
	if _, err := s.IngestItems(ctx, []NewItem{
		{FeedID: feed.ID, GUID: "current-sent", URL: "https://example.com/5", Status: StatusPending, SeenAt: pollStart},
	}); err != nil {
		t.Fatalf("IngestItems: %v", err)
	}
	current, err := s.PendingItems(ctx, feed.ID)
	if err != nil {
		t.Fatalf("PendingItems: %v", err)
	}
	for _, it := range current {
		if it.GUID == "current-sent" {
			if err := s.MarkItemSent(ctx, it.ID, "bookmark-3", pollStart); err != nil {
				t.Fatalf("MarkItemSent: %v", err)
			}
		}
	}

	deleted, err := s.PruneTerminalItems(ctx, feed.ID, pollStart)
	if err != nil {
		t.Fatalf("PruneTerminalItems: %v", err)
	}
	if deleted != 3 {
		t.Errorf("deleted = %d, want the 3 terminal departed items", deleted)
	}

	remaining, _, err := s.ListItems(ctx, ItemFilter{FeedID: &feed.ID})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	left := map[string]bool{}
	for _, it := range remaining {
		left[it.GUID] = true
	}
	// Queued work that has not failed is kept even once the feed drops it.
	if !left["gone-pending"] {
		t.Error("a departed pending item was pruned, want it kept and still sent")
	}
	if !left["current-sent"] {
		t.Error("a sent item still in the feed was pruned, want it kept as the dedup record")
	}
	for _, guid := range []string{"gone-sent", "gone-failed", "gone-seeded"} {
		if left[guid] {
			t.Errorf("%s survived the prune, want it deleted", guid)
		}
	}

	// Pruning one feed must not touch another's rows.
	otherItems, _, err := s.ListItems(ctx, ItemFilter{FeedID: &other.ID})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(otherItems) != 1 {
		t.Errorf("the other feed has %d items, want 1 untouched", len(otherItems))
	}
}

// Pruning an item means forgetting it, and a feed that serves it again will get
// a second bookmark. The window is deliberate, so pin the behavior down.
func TestPrunedItemIsSentAgainIfTheFeedBringsItBack(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	feed := newFeed(t, s, "https://example.com/feed.xml")

	item := NewItem{FeedID: feed.ID, GUID: "guid-1", URL: "https://example.com/one", Status: StatusPending, SeenAt: base}
	if _, err := s.IngestItems(ctx, []NewItem{item}); err != nil {
		t.Fatalf("IngestItems: %v", err)
	}
	pending, _ := s.PendingItems(ctx, feed.ID)
	if err := s.MarkItemSent(ctx, pending[0].ID, "bookmark-1", base); err != nil {
		t.Fatalf("MarkItemSent: %v", err)
	}

	if _, err := s.PruneTerminalItems(ctx, feed.ID, base.Add(time.Hour)); err != nil {
		t.Fatalf("PruneTerminalItems: %v", err)
	}

	item.SeenAt = base.Add(2 * time.Hour)
	result, err := s.IngestItems(ctx, []NewItem{item})
	if err != nil {
		t.Fatalf("re-ingest: %v", err)
	}
	if result.New != 1 {
		t.Errorf("New = %d, want 1: a pruned item is no longer known", result.New)
	}
}

func TestListItemsFiltersAndPages(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	feed := newFeed(t, s, "https://a.example/feed.xml")
	other := newFeed(t, s, "https://b.example/feed.xml")

	var batch []NewItem
	for i := 0; i < 5; i++ {
		published := base.Add(time.Duration(i) * time.Minute)
		batch = append(batch, NewItem{
			FeedID:      feed.ID,
			GUID:        string(rune('a' + i)),
			URL:         "https://a.example/" + string(rune('a'+i)),
			PublishedAt: &published,
			Status:      StatusPending,
			SeenAt:      base,
		})
	}
	batch = append(batch, NewItem{
		FeedID: other.ID, GUID: "other", URL: "https://b.example/1", Status: StatusSeeded, SeenAt: base,
	})
	if _, err := s.IngestItems(ctx, batch); err != nil {
		t.Fatalf("IngestItems: %v", err)
	}

	// Newest first.
	items, total, err := s.ListItems(ctx, ItemFilter{FeedID: &feed.ID})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if total != 5 {
		t.Errorf("total = %d, want 5", total)
	}
	if got := guids(items); !equalStrings(got, []string{"e", "d", "c", "b", "a"}) {
		t.Errorf("order = %v, want newest first", got)
	}

	// Paging reports the full total, not the page size.
	page, total, err := s.ListItems(ctx, ItemFilter{FeedID: &feed.ID, Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if total != 5 {
		t.Errorf("total = %d, want 5", total)
	}
	if got := guids(page); !equalStrings(got, []string{"c", "b"}) {
		t.Errorf("page = %v, want c,b", got)
	}

	// Past the end is an empty page, not an error.
	page, _, err = s.ListItems(ctx, ItemFilter{FeedID: &feed.ID, Offset: 500})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(page) != 0 {
		t.Errorf("len = %d, want 0", len(page))
	}

	seeded := StatusSeeded
	items, total, err = s.ListItems(ctx, ItemFilter{Status: &seeded})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].GUID != "other" {
		t.Errorf("status filter gave %v (total %d), want just the seeded item", guids(items), total)
	}

	bad := ItemStatus("queued")
	if _, _, err := s.ListItems(ctx, ItemFilter{Status: &bad}); err == nil {
		t.Error("ListItems accepted an unknown status, want an error")
	}
}

func TestListItemsClampsLimit(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	feed := newFeed(t, s, "https://example.com/feed.xml")

	var batch []NewItem
	for i := 0; i < 3; i++ {
		batch = append(batch, NewItem{
			FeedID: feed.ID, GUID: string(rune('a' + i)),
			URL: "https://example.com/" + string(rune('a'+i)), Status: StatusPending, SeenAt: base,
		})
	}
	if _, err := s.IngestItems(ctx, batch); err != nil {
		t.Fatalf("IngestItems: %v", err)
	}

	for _, limit := range []int{0, -1, MaxItemLimit + 1000} {
		items, _, err := s.ListItems(ctx, ItemFilter{Limit: limit})
		if err != nil {
			t.Fatalf("ListItems(limit %d): %v", limit, err)
		}
		if len(items) != 3 {
			t.Errorf("limit %d returned %d items, want all 3", limit, len(items))
		}
	}

	items, _, err := s.ListItems(ctx, ItemFilter{Offset: -5, Limit: 1})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 1 {
		t.Errorf("len = %d, want 1 with a negative offset treated as 0", len(items))
	}
}

func TestItemStatus(t *testing.T) {
	for _, s := range []ItemStatus{StatusPending, StatusSent, StatusFailedPermanent, StatusSeeded} {
		if !s.Valid() {
			t.Errorf("%q is not Valid, want it to be", s)
		}
	}
	for _, s := range []ItemStatus{"", "queued", "PENDING"} {
		if s.Valid() {
			t.Errorf("%q is Valid, want it rejected", s)
		}
	}

	terminal := map[ItemStatus]bool{
		StatusPending:         false,
		StatusSent:            true,
		StatusFailedPermanent: true,
		StatusSeeded:          true,
	}
	for status, want := range terminal {
		if got := status.Terminal(); got != want {
			t.Errorf("%q.Terminal() = %v, want %v", status, got, want)
		}
	}
}

func TestGetItemNotFoundAfterDelete(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	feed := newFeed(t, s, "https://example.com/feed.xml")

	if _, err := s.IngestItems(ctx, []NewItem{
		{FeedID: feed.ID, GUID: "guid-1", URL: "https://example.com/one", Status: StatusSeeded, SeenAt: base},
	}); err != nil {
		t.Fatalf("IngestItems: %v", err)
	}
	items, _, err := s.ListItems(ctx, ItemFilter{})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	id := items[0].ID

	if _, err := s.PruneTerminalItems(ctx, feed.ID, base.Add(time.Hour)); err != nil {
		t.Fatalf("PruneTerminalItems: %v", err)
	}
	if _, err := s.GetItem(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetItem = %v, want ErrNotFound", err)
	}
}
