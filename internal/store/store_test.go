package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// base is a fixed clock for tests. RFC3339 keeps whole seconds, so test times
// carry no sub-second part that a round trip would drop.
var base = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

// newStore returns an empty migrated in-memory database.
func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

func TestOpenCreatesParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "rssdeck.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
}

func TestOpenRejectsEmptyPath(t *testing.T) {
	if _, err := Open(context.Background(), ""); err == nil {
		t.Fatal("Open(\"\") succeeded, want error")
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "rssdeck.db")

	first, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	feed, err := first.CreateFeed(ctx, NewFeed{URL: "https://example.com/feed.xml", CreatedAt: base})
	if err != nil {
		t.Fatalf("CreateFeed: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer second.Close()

	got, err := second.GetFeed(ctx, feed.ID)
	if err != nil {
		t.Fatalf("GetFeed after reopen: %v", err)
	}
	if got.URL != feed.URL {
		t.Errorf("url = %q, want %q", got.URL, feed.URL)
	}
}

func TestForeignKeysEnforced(t *testing.T) {
	s := newStore(t)
	_, err := s.IngestItems(context.Background(), []NewItem{{
		FeedID: 404,
		GUID:   "orphan",
		Status: StatusPending,
		SeenAt: base,
	}})
	if err == nil {
		t.Fatal("ingest into a missing feed succeeded, want a foreign key error")
	}
}

func TestNullTimeRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	feed, err := s.CreateFeed(ctx, NewFeed{URL: "https://example.com/feed.xml", CreatedAt: base})
	if err != nil {
		t.Fatalf("CreateFeed: %v", err)
	}
	if !feed.CreatedAt.Equal(base) {
		t.Errorf("created_at = %v, want %v", feed.CreatedAt, base)
	}
	if feed.LastPolledAt != nil {
		t.Errorf("last_polled_at = %v, want nil", feed.LastPolledAt)
	}

	// A non-UTC time must come back as the same instant.
	offset := time.FixedZone("UTC+5", 5*60*60)
	local := base.In(offset)
	if err := s.RecordPollSuccess(ctx, feed.ID, local, Validators{}); err != nil {
		t.Fatalf("RecordPollSuccess: %v", err)
	}
	got, err := s.GetFeed(ctx, feed.ID)
	if err != nil {
		t.Fatalf("GetFeed: %v", err)
	}
	if got.LastPolledAt == nil || !got.LastPolledAt.Equal(base) {
		t.Errorf("last_polled_at = %v, want %v", got.LastPolledAt, base)
	}
}

func TestNullTimeScanRejectsGarbage(t *testing.T) {
	var n nullTime
	if err := n.Scan("not a timestamp"); err == nil {
		t.Fatal("Scan of a malformed timestamp succeeded, want error")
	}
	if err := n.Scan(42); err == nil {
		t.Fatal("Scan of an int succeeded, want error")
	}
	if err := n.Scan(nil); err != nil {
		t.Fatalf("Scan(nil): %v", err)
	}
	if n.Valid {
		t.Error("Scan(nil) left Valid true")
	}
	if err := n.Scan([]byte(formatTime(base))); err != nil {
		t.Fatalf("Scan([]byte): %v", err)
	}
	if !n.Valid || !n.Time.Equal(base) {
		t.Errorf("Scan([]byte) = %v (valid %v), want %v", n.Time, n.Valid, base)
	}
}

func TestErrNotFoundWraps(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if _, err := s.GetFeed(ctx, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetFeed error = %v, want ErrNotFound", err)
	}
	if _, err := s.GetItem(ctx, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetItem error = %v, want ErrNotFound", err)
	}
	if err := s.DeleteFeed(ctx, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteFeed error = %v, want ErrNotFound", err)
	}
	if err := s.DeleteToken(ctx, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteToken error = %v, want ErrNotFound", err)
	}
	if err := s.MarkItemSent(ctx, 1, "b1", base); !errors.Is(err, ErrNotFound) {
		t.Errorf("MarkItemSent error = %v, want ErrNotFound", err)
	}
	if _, _, err := s.RecordSendFailure(ctx, 1, "boom", 5); !errors.Is(err, ErrNotFound) {
		t.Errorf("RecordSendFailure error = %v, want ErrNotFound", err)
	}
	if err := s.RecordPollSuccess(ctx, 1, base, Validators{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("RecordPollSuccess error = %v, want ErrNotFound", err)
	}
	if err := s.RecordPollFailure(ctx, 1, base, "boom"); !errors.Is(err, ErrNotFound) {
		t.Errorf("RecordPollFailure error = %v, want ErrNotFound", err)
	}
}
