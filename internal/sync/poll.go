package sync

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/i-am-david-weinstein/rssdeck/internal/feed"
	"github.com/i-am-david-weinstein/rssdeck/internal/store"
)

// PollResult is what one poll did, for the API response and the log line.
type PollResult struct {
	FeedID  int64
	FeedURL string

	// NotModified means the feed server answered 304, so nothing was parsed or
	// pruned. Pending items are still sent on such a poll.
	NotModified bool

	New      int // items the feed served that rssdeck had no row for
	Existing int // items already known, so items that will not be sent again
	Sent     int
	Failed   int // sends that did not succeed, counting both retries and give-ups
	Pruned   int // rows deleted because the feed no longer carries them

	Duration time.Duration
}

// Poll polls one feed now, and is what POST /api/feeds/{id}/poll calls.
//
// A feed that is disabled is still polled here: the scheduler leaves it alone,
// but asking for a poll explicitly is an operator decision, not an accident.
//
// ErrPollInProgress means the feed is already being polled; polling it twice at
// once could send one item twice. Any other error is a poll that could not
// complete, and the result reports what it managed first. Individual sends that
// failed are counted in Failed rather than returned, since they are retried on
// the next poll; a token Readeck rejected is returned, because it stops the
// poll from sending anything else.
func (s *Syncer) Poll(ctx context.Context, feedID int64) (*PollResult, error) {
	if !s.sched.claim(feedID) {
		return nil, fmt.Errorf("%w: feed %d", ErrPollInProgress, feedID)
	}
	return s.poll(ctx, feedID)
}

// poll does the work for an already claimed feed and always releases the claim,
// rescheduling the feed one interval out.
func (s *Syncer) poll(ctx context.Context, feedID int64) (*PollResult, error) {
	// Until the feed is loaded, the global interval is the best guess for when
	// it should poll again. It only matters if loading fails.
	interval := s.interval
	defer func() {
		s.sched.finish(feedID, s.now().Add(interval))
	}()

	if err := s.acquire(ctx); err != nil {
		return nil, err
	}
	defer s.release()

	started := s.now()
	f, err := s.store.GetFeed(ctx, feedID)
	if err != nil {
		return nil, err
	}
	interval = f.Interval(s.interval)

	res := &PollResult{FeedID: f.ID, FeedURL: f.URL}
	err = s.pollFeed(ctx, f, started, res)
	res.Duration = s.now().Sub(started)

	if err != nil {
		s.log.Error("feed poll failed",
			"feed_id", f.ID, "url", f.URL, "error", err,
			"consecutive_failures", f.ConsecutiveFailures+1, "duration", res.Duration)
		return res, err
	}
	s.log.Info("feed polled",
		"feed_id", f.ID, "url", f.URL, "not_modified", res.NotModified,
		"new", res.New, "existing", res.Existing, "sent", res.Sent,
		"failed", res.Failed, "pruned", res.Pruned, "duration", res.Duration)
	return res, nil
}

// pollFeed is one poll: a conditional fetch, then ingest, send and retention.
// Each step records what it learned about the feed before returning, so a
// failure is visible in GET /api/feeds rather than only in the log.
func (s *Syncer) pollFeed(ctx context.Context, f *store.Feed, started time.Time, res *PollResult) error {
	resp, err := s.fetcher.Fetch(ctx, f.URL, feed.Conditional{ETag: f.ETag, LastModified: f.LastModified})
	if err != nil {
		return s.failed(ctx, f, started, err)
	}

	if resp.NotModified {
		res.NotModified = true
		// A 304 need not repeat the validators, so the stored ones stay.
		if err := s.store.RecordPollSuccess(ctx, f.ID, started, store.Validators{}); err != nil {
			return err
		}
		// Parsing and retention are skipped, because a 304 says nothing about
		// which items the feed still carries. Sending is not: items left
		// pending by an earlier failure are still owed to Readeck, and a feed
		// that answers 304 for days would otherwise never retry them.
		return s.send(ctx, f, res)
	}

	doc, err := feed.Parse(resp.Body, f.URL)
	if err != nil {
		return s.failed(ctx, f, started, err)
	}

	ingested, err := s.store.IngestItems(ctx, newItems(f.ID, doc.Items, started))
	if err != nil {
		return s.failed(ctx, f, started, err)
	}
	res.New, res.Existing = ingested.New, ingested.Existing

	// Both validators are written, including empty ones: a feed server that
	// stopped sending an ETag should not have the old one replayed at it.
	etag, lastModified := resp.ETag, resp.LastModified
	if err := s.store.RecordPollSuccess(ctx, f.ID, started, store.Validators{
		ETag:         &etag,
		LastModified: &lastModified,
	}); err != nil {
		return err
	}
	// A renamed feed keeps the title and label it was added with, since the
	// label is already in Readeck on every bookmark sent so far and renaming it
	// here would split one feed's bookmarks across two labels. The log line is
	// so an operator who wants the new name knows it changed.
	if doc.Title != "" && doc.Title != f.Title {
		s.log.Debug("feed title changed", "feed_id", f.ID, "from", f.Title, "to", doc.Title)
	}

	sendErr := s.send(ctx, f, res)

	// Retention runs even when a send failed, because the parse succeeded and
	// that is what makes the comparison safe. It does not run after a canceled
	// context: the document may not have been ingested in full.
	if ctx.Err() == nil {
		pruned, err := s.store.PruneTerminalItems(ctx, f.ID, started)
		if err != nil {
			return err
		}
		res.Pruned = int(pruned)
	}
	return sendErr
}

// failed counts a poll that could not produce a usable document against the
// feed and returns the cause, which the caller logs. A canceled context is
// shutdown rather than a broken feed, so it is left out of the count.
func (s *Syncer) failed(ctx context.Context, f *store.Feed, at time.Time, cause error) error {
	if ctx.Err() != nil {
		return cause
	}
	if err := s.store.RecordPollFailure(ctx, f.ID, at, cause.Error()); err != nil {
		s.log.Error("could not record a poll failure", "feed_id", f.ID, "error", err)
	}
	return cause
}

// newItems turns a parsed document into rows to ingest, in the order the
// document served them.
//
// A keyword filter would go here: dropping an item before this point leaves it
// unknown, so it would be reconsidered on every poll, which is what a filter
// that can be edited later wants.
func newItems(feedID int64, items []feed.Item, seenAt time.Time) []store.NewItem {
	out := make([]store.NewItem, 0, len(items))
	for _, it := range items {
		out = append(out, newItem(feedID, it, store.StatusPending, seenAt))
	}
	return out
}

// newItem builds one row. An item with nothing fetchable cannot be sent however
// long it is kept, so it is recorded as permanently failed rather than dropped:
// a row the operator can see beats an item that silently vanished.
func newItem(feedID int64, it feed.Item, status store.ItemStatus, seenAt time.Time) store.NewItem {
	row := store.NewItem{
		FeedID:      feedID,
		GUID:        it.GUID,
		URL:         it.URL,
		Title:       it.Title,
		PublishedAt: it.PublishedAt,
		Status:      status,
		SeenAt:      seenAt,
	}
	if row.URL == "" {
		row.Status = store.StatusFailedPermanent
		row.LastError = noUsableURL
	}
	return row
}

const noUsableURL = "no usable URL"

// errIsCanceled reports whether err is the context being canceled rather than
// anything a feed or Readeck did.
func errIsCanceled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
