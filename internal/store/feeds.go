package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Feed is one subscribed feed and its poll bookkeeping.
type Feed struct {
	ID    int64
	URL   string
	Title string
	Label string

	Enabled bool

	// IntervalSeconds is nil when the feed follows the global poll interval.
	IntervalSeconds *int64

	// ETag and LastModified are the validators from the last successful fetch,
	// replayed as If-None-Match and If-Modified-Since on the next one.
	ETag         string
	LastModified string

	LastPolledAt        *time.Time
	LastSuccessAt       *time.Time
	ConsecutiveFailures int
	LastError           string

	CreatedAt time.Time
}

// Interval returns how often this feed should be polled, applying the global
// default when the feed has no override. The "NULL means global" rule lives
// here and nowhere else.
func (f *Feed) Interval(global time.Duration) time.Duration {
	if f.IntervalSeconds == nil || *f.IntervalSeconds <= 0 {
		return global
	}
	return time.Duration(*f.IntervalSeconds) * time.Second
}

// NewFeed is the input to CreateFeed. Title and Label come from the parsed
// document, so a feed is only ever created after a successful fetch.
type NewFeed struct {
	URL             string
	Title           string
	Label           string
	IntervalSeconds *int64
	CreatedAt       time.Time
}

// FeedUpdate is a partial update from PATCH /api/feeds/{id}. A nil field is
// left alone, which is what distinguishes "not supplied" from "set to empty".
type FeedUpdate struct {
	Label           *string
	IntervalSeconds **int64
	Enabled         *bool
}

// Validators are the HTTP cache validators a feed server returned. A nil field
// leaves the stored value untouched, which is what a 304 wants, since a 304 need
// not repeat them. A non-nil empty string clears it.
type Validators struct {
	ETag         *string
	LastModified *string
}

const feedColumns = `id, url, title, label, enabled, interval_seconds, etag, last_modified,
	last_polled_at, last_success_at, consecutive_failures, last_error, created_at`

// CreateFeed inserts a feed, returning ErrDuplicateURL when the URL is already
// subscribed.
func (s *Store) CreateFeed(ctx context.Context, f NewFeed) (*Feed, error) {
	if f.URL == "" {
		return nil, errors.New("store: create feed: empty url")
	}
	const q = `INSERT INTO feeds (url, title, label, interval_seconds, created_at)
		VALUES (?, ?, ?, ?, ?)
		RETURNING ` + feedColumns

	row := s.db.QueryRowContext(ctx, q,
		f.URL,
		nullString(f.Title),
		nullString(f.Label),
		f.IntervalSeconds,
		formatTime(f.CreatedAt),
	)
	feed, err := scanFeed(row)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: %s", ErrDuplicateURL, f.URL)
		}
		return nil, fmt.Errorf("store: create feed %s: %w", f.URL, err)
	}
	return feed, nil
}

// GetFeed returns one feed by ID, or ErrNotFound.
func (s *Store) GetFeed(ctx context.Context, id int64) (*Feed, error) {
	const q = `SELECT ` + feedColumns + ` FROM feeds WHERE id = ?`
	feed, err := scanFeed(s.db.QueryRowContext(ctx, q, id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: feed %d", ErrNotFound, id)
		}
		return nil, fmt.Errorf("store: get feed %d: %w", id, err)
	}
	return feed, nil
}

// GetFeedByURL returns the feed subscribed to url, or ErrNotFound.
func (s *Store) GetFeedByURL(ctx context.Context, url string) (*Feed, error) {
	const q = `SELECT ` + feedColumns + ` FROM feeds WHERE url = ?`
	feed, err := scanFeed(s.db.QueryRowContext(ctx, q, url))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: feed %s", ErrNotFound, url)
		}
		return nil, fmt.Errorf("store: get feed %s: %w", url, err)
	}
	return feed, nil
}

// ListFeeds returns every feed, enabled or not, oldest first. Callers that only
// poll enabled feeds filter on Feed.Enabled.
func (s *Store) ListFeeds(ctx context.Context) ([]Feed, error) {
	const q = `SELECT ` + feedColumns + ` FROM feeds ORDER BY id`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("store: list feeds: %w", err)
	}
	defer rows.Close()

	feeds := []Feed{}
	for rows.Next() {
		feed, err := scanFeed(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list feeds: %w", err)
		}
		feeds = append(feeds, *feed)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list feeds: %w", err)
	}
	return feeds, nil
}

// UpdateFeed applies a partial update and returns the stored feed. With no
// fields set it is a read.
func (s *Store) UpdateFeed(ctx context.Context, id int64, u FeedUpdate) (*Feed, error) {
	var (
		sets []string
		args []any
	)
	if u.Label != nil {
		sets = append(sets, "label = ?")
		args = append(args, nullString(*u.Label))
	}
	if u.IntervalSeconds != nil {
		sets = append(sets, "interval_seconds = ?")
		args = append(args, *u.IntervalSeconds)
	}
	if u.Enabled != nil {
		sets = append(sets, "enabled = ?")
		args = append(args, *u.Enabled)
	}
	if len(sets) == 0 {
		return s.GetFeed(ctx, id)
	}

	args = append(args, id)
	q := `UPDATE feeds SET ` + strings.Join(sets, ", ") + ` WHERE id = ? RETURNING ` + feedColumns
	feed, err := scanFeed(s.db.QueryRowContext(ctx, q, args...))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: feed %d", ErrNotFound, id)
		}
		return nil, fmt.Errorf("store: update feed %d: %w", id, err)
	}
	return feed, nil
}

// DeleteFeed removes a feed and, through ON DELETE CASCADE, every item it owns.
// Re-adding the same URL therefore starts from nothing and will resend.
func (s *Store) DeleteFeed(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM feeds WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete feed %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: delete feed %d: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: feed %d", ErrNotFound, id)
	}
	return nil
}

// RecordPollSuccess marks a poll that reached the server and produced a usable
// answer, which includes a 304. It clears the failure counter and stores the
// validators to replay next time.
func (s *Store) RecordPollSuccess(ctx context.Context, id int64, at time.Time, v Validators) error {
	const q = `UPDATE feeds SET
		last_polled_at = ?,
		last_success_at = ?,
		consecutive_failures = 0,
		last_error = NULL,
		etag = CASE WHEN ? THEN ? ELSE etag END,
		last_modified = CASE WHEN ? THEN ? ELSE last_modified END
	WHERE id = ?`

	ts := formatTime(at)
	return s.execFeedUpdate(ctx, id, q,
		ts, ts,
		v.ETag != nil, nullStringPtr(v.ETag),
		v.LastModified != nil, nullStringPtr(v.LastModified),
		id,
	)
}

// RecordPollFailure records a poll that failed to fetch or parse. last_success_at
// is left alone so it keeps reading as the last time the feed actually worked.
func (s *Store) RecordPollFailure(ctx context.Context, id int64, at time.Time, cause string) error {
	const q = `UPDATE feeds SET
		last_polled_at = ?,
		consecutive_failures = consecutive_failures + 1,
		last_error = ?
	WHERE id = ?`
	return s.execFeedUpdate(ctx, id, q, formatTime(at), nullString(cause), id)
}

func (s *Store) execFeedUpdate(ctx context.Context, id int64, q string, args ...any) error {
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("store: update feed %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: update feed %d: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: feed %d", ErrNotFound, id)
	}
	return nil
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows, so one scan function
// serves single-row and list queries.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanFeed(row rowScanner) (*Feed, error) {
	var (
		f            Feed
		title        sql.NullString
		label        sql.NullString
		etag         sql.NullString
		lastModified sql.NullString
		lastError    sql.NullString
		interval     sql.NullInt64
		lastPolled   nullTime
		lastSuccess  nullTime
		createdAt    nullTime
	)
	err := row.Scan(
		&f.ID,
		&f.URL,
		&title,
		&label,
		&f.Enabled,
		&interval,
		&etag,
		&lastModified,
		&lastPolled,
		&lastSuccess,
		&f.ConsecutiveFailures,
		&lastError,
		&createdAt,
	)
	if err != nil {
		return nil, err
	}

	f.Title = text(title)
	f.Label = text(label)
	f.ETag = text(etag)
	f.LastModified = text(lastModified)
	f.LastError = text(lastError)
	if interval.Valid {
		v := interval.Int64
		f.IntervalSeconds = &v
	}
	f.LastPolledAt = lastPolled.ptr()
	f.LastSuccessAt = lastSuccess.ptr()
	f.CreatedAt = createdAt.Time
	return &f, nil
}

// nullStringPtr writes an empty string as NULL, and nil as NULL too. The CASE
// guard in the caller decides whether the value is used at all.
func nullStringPtr(s *string) any {
	if s == nil {
		return nil
	}
	return nullString(*s)
}

// isUniqueViolation reports whether err is a UNIQUE constraint failure. The
// pure-Go driver surfaces these as text, and matching on it beats importing the
// driver's error type into every caller.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(strings.ToUpper(err.Error()), "UNIQUE CONSTRAINT FAILED")
}
