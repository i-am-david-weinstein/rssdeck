package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ItemStatus is where an item sits in its one trip to Readeck.
type ItemStatus string

const (
	// StatusPending means discovered but not yet accepted by Readeck.
	StatusPending ItemStatus = "pending"
	// StatusSent means Readeck returned 202 and ReadeckBookmarkID is set.
	StatusSent ItemStatus = "sent"
	// StatusFailedPermanent means rssdeck gave up: too many send failures, or
	// nothing usable to send.
	StatusFailedPermanent ItemStatus = "failed_permanent"
	// StatusSeeded means the item was already in the feed when it was added, so
	// it is deliberately never sent.
	StatusSeeded ItemStatus = "seeded"
)

// Pagination bounds for ListItems, shared with the HTTP layer so both use the
// same numbers.
const (
	DefaultItemLimit = 50
	MaxItemLimit     = 500
)

// Valid reports whether s is one of the four statuses the schema allows.
func (s ItemStatus) Valid() bool {
	switch s {
	case StatusPending, StatusSent, StatusFailedPermanent, StatusSeeded:
		return true
	}
	return false
}

// Terminal reports whether an item is finished. Only terminal items are ever
// deleted by retention, and only non-terminal items are ever sent.
func (s ItemStatus) Terminal() bool {
	switch s {
	case StatusSent, StatusFailedPermanent, StatusSeeded:
		return true
	}
	return false
}

// Item is one feed entry and its delivery state.
type Item struct {
	ID     int64
	FeedID int64

	// GUID is the item's identity within its feed. Identity is per feed, so the
	// same article in two feeds becomes two bookmarks.
	GUID string

	URL         string
	Title       string
	PublishedAt *time.Time

	Status    ItemStatus
	Attempts  int
	LastError string

	ReadeckBookmarkID string
	SentAt            *time.Time

	FirstSeenAt time.Time

	// LastSeenInFeedAt is when the item last appeared in a fetched document.
	// Retention uses it to tell departed items from current ones.
	LastSeenInFeedAt time.Time
}

// NewItem is one entry from a parsed feed document, ready to ingest.
type NewItem struct {
	FeedID      int64
	GUID        string
	URL         string
	Title       string
	PublishedAt *time.Time

	// Status is pending for items to send, seeded for items present when the
	// feed was added, or failed_permanent for an item with no usable URL. An
	// item already known keeps the status it has.
	Status ItemStatus

	// LastError explains a status of failed_permanent at ingest time.
	LastError string

	// SeenAt is the time of the poll that found the item.
	SeenAt time.Time
}

// IngestResult counts what one feed document changed.
type IngestResult struct {
	New      int // rows inserted, so items that will be sent
	Existing int // rows already present, only their last-seen time refreshed
}

const itemColumns = `id, feed_id, guid, url, title, published_at, status, attempts,
	last_error, readeck_bookmark_id, sent_at, first_seen_at, last_seen_in_feed_at`

// IngestItems records every item in one fetched document, in feed order, inside
// a single transaction.
//
// This is where the once-only guarantee lives. An item's row is what makes it
// known, so a row that already exists has only its last-seen time refreshed:
// never its status, attempts or URL. An item stays known for as long as the feed
// keeps serving it, so it is never sent twice.
func (s *Store) IngestItems(ctx context.Context, items []NewItem) (IngestResult, error) {
	var result IngestResult
	if len(items) == 0 {
		return result, nil
	}
	for i, it := range items {
		if !it.Status.Valid() {
			return result, fmt.Errorf("store: ingest item %d (%s): invalid status %q", i, it.GUID, it.Status)
		}
		if it.GUID == "" {
			return result, fmt.Errorf("store: ingest item %d: empty guid", i)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, fmt.Errorf("store: begin ingest: %w", err)
	}
	defer tx.Rollback()

	// Refreshing an existing row and inserting a new one are two statements
	// rather than one upsert, because the result of the UPDATE is an unambiguous
	// answer to "was this item already known?". An upsert's RETURNING cannot
	// distinguish the two when a document repeats a GUID, which real feeds do.
	touch, err := tx.PrepareContext(ctx,
		`UPDATE items SET last_seen_in_feed_at = ? WHERE feed_id = ? AND guid = ?`)
	if err != nil {
		return result, fmt.Errorf("store: prepare ingest update: %w", err)
	}
	defer touch.Close()

	insert, err := tx.PrepareContext(ctx,
		`INSERT INTO items (feed_id, guid, url, title, published_at, status, last_error,
			first_seen_at, last_seen_in_feed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return result, fmt.Errorf("store: prepare ingest insert: %w", err)
	}
	defer insert.Close()

	for _, it := range items {
		seen := formatTime(it.SeenAt)

		res, err := touch.ExecContext(ctx, seen, it.FeedID, it.GUID)
		if err != nil {
			return result, fmt.Errorf("store: refresh item %s: %w", it.GUID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return result, fmt.Errorf("store: refresh item %s: %w", it.GUID, err)
		}
		if n > 0 {
			result.Existing++
			continue
		}

		_, err = insert.ExecContext(ctx,
			it.FeedID,
			it.GUID,
			nullString(it.URL),
			nullString(it.Title),
			timePtr(it.PublishedAt),
			string(it.Status),
			nullString(it.LastError),
			seen,
			seen,
		)
		if err != nil {
			return result, fmt.Errorf("store: insert item %s: %w", it.GUID, err)
		}
		result.New++
	}

	if err := tx.Commit(); err != nil {
		return result, fmt.Errorf("store: commit ingest: %w", err)
	}
	return result, nil
}

// PendingItems returns a feed's unsent items, oldest first, so that Readeck's
// newest-first list ends up matching the feed's chronology. Items with no
// publication date fall back to when rssdeck first saw them.
func (s *Store) PendingItems(ctx context.Context, feedID int64) ([]Item, error) {
	const q = `SELECT ` + itemColumns + ` FROM items
		WHERE feed_id = ? AND status = 'pending'
		ORDER BY COALESCE(published_at, first_seen_at) ASC, id ASC`

	rows, err := s.db.QueryContext(ctx, q, feedID)
	if err != nil {
		return nil, fmt.Errorf("store: pending items for feed %d: %w", feedID, err)
	}
	defer rows.Close()

	items, err := scanItems(rows)
	if err != nil {
		return nil, fmt.Errorf("store: pending items for feed %d: %w", feedID, err)
	}
	return items, nil
}

// GetItem returns one item by ID, or ErrNotFound.
func (s *Store) GetItem(ctx context.Context, id int64) (*Item, error) {
	const q = `SELECT ` + itemColumns + ` FROM items WHERE id = ?`
	item, err := scanItem(s.db.QueryRowContext(ctx, q, id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: item %d", ErrNotFound, id)
		}
		return nil, fmt.Errorf("store: get item %d: %w", id, err)
	}
	return item, nil
}

// MarkItemSent records a 202 from Readeck. bookmarkID comes from the Bookmark-Id
// response header.
func (s *Store) MarkItemSent(ctx context.Context, id int64, bookmarkID string, at time.Time) error {
	const q = `UPDATE items SET
		status = 'sent',
		readeck_bookmark_id = ?,
		sent_at = ?,
		last_error = NULL
	WHERE id = ?`

	res, err := s.db.ExecContext(ctx, q, nullString(bookmarkID), formatTime(at), id)
	if err != nil {
		return fmt.Errorf("store: mark item %d sent: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: mark item %d sent: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: item %d", ErrNotFound, id)
	}
	return nil
}

// RecordSendFailure counts one failed send and returns the item's resulting
// status and attempt count. The item stays pending, and so is retried on the
// next poll, until it reaches maxFailures and becomes failed_permanent. The
// daemon is already a retry loop, so there is no inner retry.
func (s *Store) RecordSendFailure(ctx context.Context, id int64, cause string, maxFailures int) (ItemStatus, int, error) {
	if maxFailures < 1 {
		maxFailures = 1
	}
	// On the right-hand side of a SET, a column reads as its pre-update value,
	// so attempts + 1 is the count this failure produces. RETURNING reports the
	// values after the update.
	const q = `UPDATE items SET
		attempts = attempts + 1,
		last_error = ?,
		status = CASE WHEN attempts + 1 >= ? THEN 'failed_permanent' ELSE status END
	WHERE id = ?
	RETURNING status, attempts`

	var (
		status   string
		attempts int
	)
	err := s.db.QueryRowContext(ctx, q, nullString(cause), maxFailures, id).Scan(&status, &attempts)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", 0, fmt.Errorf("%w: item %d", ErrNotFound, id)
		}
		return "", 0, fmt.Errorf("store: record failure for item %d: %w", id, err)
	}
	return ItemStatus(status), attempts, nil
}

// PruneTerminalItems deletes the feed's finished items that were absent from the
// document fetched at pollStartedAt, and returns how many it removed.
//
// Callers must only run this after a successful parse. A 304 or a fetch error
// says nothing about which items the feed still carries, and treating either as
// an empty document would delete every row.
//
// Pending items are never pruned, even once they leave the feed. They are queued
// work that has not failed.
func (s *Store) PruneTerminalItems(ctx context.Context, feedID int64, pollStartedAt time.Time) (int64, error) {
	const q = `DELETE FROM items
		WHERE feed_id = ?
		  AND status IN ('sent', 'failed_permanent', 'seeded')
		  AND last_seen_in_feed_at < ?`

	res, err := s.db.ExecContext(ctx, q, feedID, formatTime(pollStartedAt))
	if err != nil {
		return 0, fmt.Errorf("store: prune items for feed %d: %w", feedID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: prune items for feed %d: %w", feedID, err)
	}
	return n, nil
}

// ItemFilter narrows and pages GET /api/items. A nil FeedID or Status means no
// filter on that column.
type ItemFilter struct {
	FeedID *int64
	Status *ItemStatus
	Limit  int
	Offset int
}

// ListItems returns a page of items newest first, plus the total matching the
// filter before paging. Limit is clamped to MaxItemLimit, and 0 means
// DefaultItemLimit.
func (s *Store) ListItems(ctx context.Context, f ItemFilter) ([]Item, int, error) {
	if f.Status != nil && !f.Status.Valid() {
		return nil, 0, fmt.Errorf("store: list items: invalid status %q", *f.Status)
	}

	var (
		where []string
		args  []any
	)
	if f.FeedID != nil {
		where = append(where, "feed_id = ?")
		args = append(args, *f.FeedID)
	}
	if f.Status != nil {
		where = append(where, "status = ?")
		args = append(args, string(*f.Status))
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}

	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM items`+clause, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: count items: %w", err)
	}

	limit := f.Limit
	switch {
	case limit <= 0:
		limit = DefaultItemLimit
	case limit > MaxItemLimit:
		limit = MaxItemLimit
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}

	q := `SELECT ` + itemColumns + ` FROM items` + clause +
		` ORDER BY COALESCE(published_at, first_seen_at) DESC, id DESC LIMIT ? OFFSET ?`
	rows, err := s.db.QueryContext(ctx, q, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("store: list items: %w", err)
	}
	defer rows.Close()

	items, err := scanItems(rows)
	if err != nil {
		return nil, 0, fmt.Errorf("store: list items: %w", err)
	}
	return items, total, nil
}

func scanItems(rows *sql.Rows) ([]Item, error) {
	items := []Item{}
	for rows.Next() {
		item, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, *item)
	}
	return items, rows.Err()
}

func scanItem(row rowScanner) (*Item, error) {
	var (
		it         Item
		url        sql.NullString
		title      sql.NullString
		lastError  sql.NullString
		bookmarkID sql.NullString
		status     string
		published  nullTime
		sentAt     nullTime
		firstSeen  nullTime
		lastSeen   nullTime
	)
	err := row.Scan(
		&it.ID,
		&it.FeedID,
		&it.GUID,
		&url,
		&title,
		&published,
		&status,
		&it.Attempts,
		&lastError,
		&bookmarkID,
		&sentAt,
		&firstSeen,
		&lastSeen,
	)
	if err != nil {
		return nil, err
	}

	it.URL = text(url)
	it.Title = text(title)
	it.LastError = text(lastError)
	it.ReadeckBookmarkID = text(bookmarkID)
	it.Status = ItemStatus(status)
	it.PublishedAt = published.ptr()
	it.SentAt = sentAt.ptr()
	it.FirstSeenAt = firstSeen.Time
	it.LastSeenInFeedAt = lastSeen.Time
	return &it, nil
}
