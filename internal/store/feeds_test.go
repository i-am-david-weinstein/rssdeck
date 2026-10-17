package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCreateFeedStoresEverything(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	interval := int64(600)
	feed, err := s.CreateFeed(ctx, NewFeed{
		URL:             "https://example.com/feed.xml",
		Title:           "Example Blog",
		Label:           "example-blog",
		IntervalSeconds: &interval,
		CreatedAt:       base,
	})
	if err != nil {
		t.Fatalf("CreateFeed: %v", err)
	}

	if feed.ID == 0 {
		t.Error("id = 0, want an assigned row id")
	}
	if feed.Title != "Example Blog" {
		t.Errorf("title = %q, want %q", feed.Title, "Example Blog")
	}
	if feed.Label != "example-blog" {
		t.Errorf("label = %q, want %q", feed.Label, "example-blog")
	}
	if !feed.Enabled {
		t.Error("enabled = false, want a new feed to be enabled")
	}
	if feed.IntervalSeconds == nil || *feed.IntervalSeconds != 600 {
		t.Errorf("interval_seconds = %v, want 600", feed.IntervalSeconds)
	}
	if feed.ConsecutiveFailures != 0 {
		t.Errorf("consecutive_failures = %d, want 0", feed.ConsecutiveFailures)
	}
}

func TestCreateFeedRejectsDuplicateURL(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	in := NewFeed{URL: "https://example.com/feed.xml", CreatedAt: base}
	if _, err := s.CreateFeed(ctx, in); err != nil {
		t.Fatalf("first CreateFeed: %v", err)
	}
	_, err := s.CreateFeed(ctx, in)
	if !errors.Is(err, ErrDuplicateURL) {
		t.Fatalf("second CreateFeed error = %v, want ErrDuplicateURL", err)
	}
}

func TestGetFeedByURL(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	want, err := s.CreateFeed(ctx, NewFeed{URL: "https://example.com/feed.xml", CreatedAt: base})
	if err != nil {
		t.Fatalf("CreateFeed: %v", err)
	}

	got, err := s.GetFeedByURL(ctx, want.URL)
	if err != nil {
		t.Fatalf("GetFeedByURL: %v", err)
	}
	if got.ID != want.ID {
		t.Errorf("id = %d, want %d", got.ID, want.ID)
	}
	if _, err := s.GetFeedByURL(ctx, "https://example.com/other.xml"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetFeedByURL for an unknown url = %v, want ErrNotFound", err)
	}
}

func TestListFeedsIncludesDisabledAndIsEmptySlice(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	feeds, err := s.ListFeeds(ctx)
	if err != nil {
		t.Fatalf("ListFeeds: %v", err)
	}
	if feeds == nil {
		t.Error("ListFeeds returned nil, want an empty slice so JSON encodes as []")
	}
	if len(feeds) != 0 {
		t.Fatalf("len = %d, want 0", len(feeds))
	}

	first, _ := s.CreateFeed(ctx, NewFeed{URL: "https://a.example/feed.xml", CreatedAt: base})
	second, _ := s.CreateFeed(ctx, NewFeed{URL: "https://b.example/feed.xml", CreatedAt: base})

	disabled := false
	if _, err := s.UpdateFeed(ctx, second.ID, FeedUpdate{Enabled: &disabled}); err != nil {
		t.Fatalf("UpdateFeed: %v", err)
	}

	feeds, err = s.ListFeeds(ctx)
	if err != nil {
		t.Fatalf("ListFeeds: %v", err)
	}
	if len(feeds) != 2 {
		t.Fatalf("len = %d, want 2", len(feeds))
	}
	if feeds[0].ID != first.ID || feeds[1].ID != second.ID {
		t.Errorf("ids = %d,%d, want %d,%d", feeds[0].ID, feeds[1].ID, first.ID, second.ID)
	}
	if feeds[1].Enabled {
		t.Error("the disabled feed came back enabled")
	}
}

func TestUpdateFeedLeavesOmittedFieldsAlone(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	interval := int64(600)
	feed, err := s.CreateFeed(ctx, NewFeed{
		URL:             "https://example.com/feed.xml",
		Title:           "Example Blog",
		Label:           "example-blog",
		IntervalSeconds: &interval,
		CreatedAt:       base,
	})
	if err != nil {
		t.Fatalf("CreateFeed: %v", err)
	}

	label := "renamed"
	got, err := s.UpdateFeed(ctx, feed.ID, FeedUpdate{Label: &label})
	if err != nil {
		t.Fatalf("UpdateFeed: %v", err)
	}
	if got.Label != "renamed" {
		t.Errorf("label = %q, want %q", got.Label, "renamed")
	}
	if got.IntervalSeconds == nil || *got.IntervalSeconds != 600 {
		t.Errorf("interval_seconds = %v, want it untouched at 600", got.IntervalSeconds)
	}
	if !got.Enabled {
		t.Error("enabled = false, want it untouched")
	}
	if got.Title != "Example Blog" {
		t.Errorf("title = %q, want it untouched", got.Title)
	}
}

// Clearing an interval is a different operation from omitting one, which is why
// FeedUpdate.IntervalSeconds is a pointer to a pointer.
func TestUpdateFeedClearsInterval(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	interval := int64(600)
	feed, err := s.CreateFeed(ctx, NewFeed{
		URL:             "https://example.com/feed.xml",
		IntervalSeconds: &interval,
		CreatedAt:       base,
	})
	if err != nil {
		t.Fatalf("CreateFeed: %v", err)
	}

	var clear *int64
	got, err := s.UpdateFeed(ctx, feed.ID, FeedUpdate{IntervalSeconds: &clear})
	if err != nil {
		t.Fatalf("UpdateFeed: %v", err)
	}
	if got.IntervalSeconds != nil {
		t.Errorf("interval_seconds = %v, want nil", *got.IntervalSeconds)
	}
	if got.Interval(15*time.Minute) != 15*time.Minute {
		t.Errorf("Interval = %v, want the global default", got.Interval(15*time.Minute))
	}
}

func TestUpdateFeedWithNothingSetIsARead(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	feed, err := s.CreateFeed(ctx, NewFeed{URL: "https://example.com/feed.xml", CreatedAt: base})
	if err != nil {
		t.Fatalf("CreateFeed: %v", err)
	}
	got, err := s.UpdateFeed(ctx, feed.ID, FeedUpdate{})
	if err != nil {
		t.Fatalf("UpdateFeed: %v", err)
	}
	if got.ID != feed.ID {
		t.Errorf("id = %d, want %d", got.ID, feed.ID)
	}
	if _, err := s.UpdateFeed(ctx, 404, FeedUpdate{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateFeed on a missing feed = %v, want ErrNotFound", err)
	}
	label := "x"
	if _, err := s.UpdateFeed(ctx, 404, FeedUpdate{Label: &label}); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateFeed on a missing feed = %v, want ErrNotFound", err)
	}
}

func TestFeedInterval(t *testing.T) {
	global := 15 * time.Minute
	tests := []struct {
		name    string
		seconds *int64
		want    time.Duration
	}{
		{"nil falls back to global", nil, global},
		{"override wins", ptr(int64(60)), time.Minute},
		{"zero falls back to global", ptr(int64(0)), global},
		{"negative falls back to global", ptr(int64(-5)), global},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := Feed{IntervalSeconds: tt.seconds}
			if got := f.Interval(global); got != tt.want {
				t.Errorf("Interval = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDeleteFeedCascadesToItems(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	feed, err := s.CreateFeed(ctx, NewFeed{URL: "https://example.com/feed.xml", CreatedAt: base})
	if err != nil {
		t.Fatalf("CreateFeed: %v", err)
	}
	if _, err := s.IngestItems(ctx, []NewItem{
		{FeedID: feed.ID, GUID: "a", URL: "https://example.com/a", Status: StatusPending, SeenAt: base},
		{FeedID: feed.ID, GUID: "b", URL: "https://example.com/b", Status: StatusSeeded, SeenAt: base},
	}); err != nil {
		t.Fatalf("IngestItems: %v", err)
	}

	if err := s.DeleteFeed(ctx, feed.ID); err != nil {
		t.Fatalf("DeleteFeed: %v", err)
	}

	_, total, err := s.ListItems(ctx, ItemFilter{})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if total != 0 {
		t.Errorf("%d items survived the feed, want 0", total)
	}
}

func TestRecordPollSuccessClearsFailures(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	feed, err := s.CreateFeed(ctx, NewFeed{URL: "https://example.com/feed.xml", CreatedAt: base})
	if err != nil {
		t.Fatalf("CreateFeed: %v", err)
	}

	if err := s.RecordPollFailure(ctx, feed.ID, base, "dial tcp: connection refused"); err != nil {
		t.Fatalf("RecordPollFailure: %v", err)
	}
	if err := s.RecordPollFailure(ctx, feed.ID, base.Add(time.Minute), "500 Internal Server Error"); err != nil {
		t.Fatalf("RecordPollFailure: %v", err)
	}

	got, err := s.GetFeed(ctx, feed.ID)
	if err != nil {
		t.Fatalf("GetFeed: %v", err)
	}
	if got.ConsecutiveFailures != 2 {
		t.Errorf("consecutive_failures = %d, want 2", got.ConsecutiveFailures)
	}
	if got.LastError != "500 Internal Server Error" {
		t.Errorf("last_error = %q, want the most recent failure", got.LastError)
	}
	if got.LastSuccessAt != nil {
		t.Errorf("last_success_at = %v, want nil after only failures", got.LastSuccessAt)
	}
	if got.LastPolledAt == nil || !got.LastPolledAt.Equal(base.Add(time.Minute)) {
		t.Errorf("last_polled_at = %v, want the time of the last attempt", got.LastPolledAt)
	}

	etag := `W/"abc123"`
	lastMod := "Thu, 16 Oct 2026 12:00:00 GMT"
	at := base.Add(2 * time.Minute)
	err = s.RecordPollSuccess(ctx, feed.ID, at, Validators{ETag: &etag, LastModified: &lastMod})
	if err != nil {
		t.Fatalf("RecordPollSuccess: %v", err)
	}

	got, err = s.GetFeed(ctx, feed.ID)
	if err != nil {
		t.Fatalf("GetFeed: %v", err)
	}
	if got.ConsecutiveFailures != 0 {
		t.Errorf("consecutive_failures = %d, want 0", got.ConsecutiveFailures)
	}
	if got.LastError != "" {
		t.Errorf("last_error = %q, want it cleared", got.LastError)
	}
	if got.ETag != etag {
		t.Errorf("etag = %q, want %q", got.ETag, etag)
	}
	if got.LastModified != lastMod {
		t.Errorf("last_modified = %q, want %q", got.LastModified, lastMod)
	}
	if got.LastSuccessAt == nil || !got.LastSuccessAt.Equal(at) {
		t.Errorf("last_success_at = %v, want %v", got.LastSuccessAt, at)
	}
}

// A 304 reports success without repeating the validators, so it must not wipe
// the stored ones. Only a non-nil value replaces them.
func TestRecordPollSuccessValidatorSemantics(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	feed, err := s.CreateFeed(ctx, NewFeed{URL: "https://example.com/feed.xml", CreatedAt: base})
	if err != nil {
		t.Fatalf("CreateFeed: %v", err)
	}

	etag := `W/"abc123"`
	lastMod := "Thu, 16 Oct 2026 12:00:00 GMT"
	if err := s.RecordPollSuccess(ctx, feed.ID, base, Validators{ETag: &etag, LastModified: &lastMod}); err != nil {
		t.Fatalf("seed validators: %v", err)
	}

	// A 304 passes nothing.
	if err := s.RecordPollSuccess(ctx, feed.ID, base.Add(time.Minute), Validators{}); err != nil {
		t.Fatalf("RecordPollSuccess: %v", err)
	}
	got, err := s.GetFeed(ctx, feed.ID)
	if err != nil {
		t.Fatalf("GetFeed: %v", err)
	}
	if got.ETag != etag || got.LastModified != lastMod {
		t.Errorf("validators = %q/%q, want them kept as %q/%q",
			got.ETag, got.LastModified, etag, lastMod)
	}

	// A 200 without an ETag clears it, so a stale validator is not replayed.
	empty := ""
	if err := s.RecordPollSuccess(ctx, feed.ID, base.Add(2*time.Minute), Validators{ETag: &empty}); err != nil {
		t.Fatalf("RecordPollSuccess: %v", err)
	}
	got, err = s.GetFeed(ctx, feed.ID)
	if err != nil {
		t.Fatalf("GetFeed: %v", err)
	}
	if got.ETag != "" {
		t.Errorf("etag = %q, want it cleared", got.ETag)
	}
	if got.LastModified != lastMod {
		t.Errorf("last_modified = %q, want it kept as %q", got.LastModified, lastMod)
	}
}

func ptr[T any](v T) *T { return &v }

func TestCreateFeedRejectsEmptyURL(t *testing.T) {
	if _, err := newStore(t).CreateFeed(context.Background(), NewFeed{CreatedAt: base}); err == nil {
		t.Fatal("CreateFeed with no url succeeded, want an error")
	}
}
