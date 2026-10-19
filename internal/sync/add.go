package sync

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/i-am-david-weinstein/rssdeck/internal/feed"
	"github.com/i-am-david-weinstein/rssdeck/internal/store"
)

// maxLabelLength bounds a label defaulted from a feed's title. Readeck shows
// labels in lists, and some feeds title themselves in a full sentence.
const maxLabelLength = 64

// AddRequest is the input to AddFeed, from POST /api/feeds.
type AddRequest struct {
	URL string

	// Label overrides the default, which is the feed's own title truncated to
	// 64 characters. An empty string means no feed label at all, leaving only
	// the global one.
	Label *string

	// IntervalSeconds overrides the global poll interval for this feed.
	IntervalSeconds *int64

	// Backfill is how many of the feed's newest items to send. Nil uses the
	// configured default, and zero sends none of them.
	Backfill *int
}

// AddFeed subscribes to a feed and returns it.
//
// The fetch and parse happen here rather than on the next tick, so a URL that
// is not a feed is an immediate ErrNotAFeed instead of an error an agent would
// have to poll for. At that point nothing has been written.
//
// Everything in the document is recorded as already seen, except the newest
// Backfill items, which are sent now. That is what makes a successful add show
// up in Readeck without dumping the feed's whole archive into it. A send that
// fails leaves its item pending for the next poll and does not fail the add.
func (s *Syncer) AddFeed(ctx context.Context, req AddRequest) (*store.Feed, error) {
	feedURL, err := normalizeURL(req.URL)
	if err != nil {
		return nil, err
	}

	// A duplicate is worth catching before the fetch, so re-adding a feed by
	// mistake does not poke the feed server. CreateFeed is still the authority
	// on it, for two adds racing.
	if _, err := s.store.GetFeedByURL(ctx, feedURL); err == nil {
		return nil, fmt.Errorf("%w: %s", store.ErrDuplicateURL, feedURL)
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	started := s.now()
	resp, err := s.fetcher.Fetch(ctx, feedURL, feed.Conditional{})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	if resp.NotModified {
		// Nothing was sent to make a 304 possible, so a server answering with
		// one is broken rather than a feed rssdeck can poll.
		return nil, fmt.Errorf("%w: %s answered 304 to an unconditional request", ErrNotAFeed, feedURL)
	}
	doc, err := feed.Parse(resp.Body, feedURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotAFeed, err)
	}

	f, err := s.store.CreateFeed(ctx, store.NewFeed{
		URL:             feedURL,
		Title:           doc.Title,
		Label:           label(req.Label, doc.Title),
		IntervalSeconds: req.IntervalSeconds,
		CreatedAt:       started,
	})
	if err != nil {
		return nil, err
	}

	// The fetch above counts as this feed's first poll: it stores the
	// validators for the next one, and its timestamp is what keeps the feed
	// from being polled again immediately.
	if err := s.store.RecordPollSuccess(ctx, f.ID, started, store.Validators{
		ETag:         &resp.ETag,
		LastModified: &resp.LastModified,
	}); err != nil {
		return nil, err
	}

	backfill := s.backfill
	if req.Backfill != nil {
		backfill = max(*req.Backfill, 0)
	}
	if _, err := s.store.IngestItems(ctx, seedItems(f.ID, doc.Items, backfill, started)); err != nil {
		// A feed left behind with none of its items recorded would send the
		// whole document on its first poll, which is the one thing seeding
		// exists to prevent. Undoing the add is the safe way to fail.
		if derr := s.store.DeleteFeed(ctx, f.ID); derr != nil {
			s.log.Error("could not remove a feed whose items failed to seed", "feed_id", f.ID, "error", derr)
		}
		return nil, err
	}

	// Re-read before scheduling, because the next poll is an interval after
	// the fetch above and that is what last_polled_at now says.
	stored, err := s.store.GetFeed(ctx, f.ID)
	if err != nil {
		return nil, err
	}
	s.scheduleFeed(stored, started)
	s.log.Info("feed added",
		"feed_id", stored.ID, "url", stored.URL, "title", stored.Title, "label", stored.Label,
		"items", len(doc.Items), "backfill", backfill)

	// Sending is best effort: the items are queued either way, and failing the
	// add would leave the operator with a feed they would have to add again.
	var res PollResult
	if err := s.send(ctx, stored, &res); err != nil {
		s.log.Error("could not send the backfill for a new feed", "feed_id", stored.ID, "error", err)
	}
	return stored, nil
}

// normalizeURL checks that a subscription URL is something that can be fetched.
// Only the scheme and host are checked: a feed URL is otherwise whatever the
// publisher says it is, so nothing here is rewritten.
func normalizeURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("%w: empty url", ErrInvalidURL)
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidURL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("%w: %s must be http or https", ErrInvalidURL, trimmed)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("%w: %s has no host", ErrInvalidURL, trimmed)
	}
	return trimmed, nil
}

// label is the feed label to store: what the request asked for, or the feed's
// own title cut to a length that reads well in Readeck. Truncation is by rune,
// so a multi-byte title does not end in half a character.
func label(requested *string, title string) string {
	if requested != nil {
		return strings.TrimSpace(*requested)
	}
	runes := []rune(strings.TrimSpace(title))
	if len(runes) > maxLabelLength {
		return strings.TrimSpace(string(runes[:maxLabelLength]))
	}
	return string(runes)
}

// seedItems turns the document a feed was added from into rows: the newest
// backfill items as pending, so they are sent, and everything else as seeded,
// which is a row that exists only to say the item was already there.
//
// Rows stay in document order, since that is the order ingest expects.
func seedItems(feedID int64, items []feed.Item, backfill int, seenAt time.Time) []store.NewItem {
	send := make(map[int]bool, backfill)
	for _, i := range newestFirst(items) {
		if len(send) >= backfill {
			break
		}
		send[i] = true
	}

	rows := make([]store.NewItem, 0, len(items))
	for i, it := range items {
		status := store.StatusSeeded
		if send[i] {
			status = store.StatusPending
		}
		rows = append(rows, newItem(feedID, it, status, seenAt))
	}
	return rows
}

// newestFirst orders item positions by publication date, newest first, with
// document order breaking ties.
//
// Items with no date sort last. A backfill is meant to be the recent items, and
// an undated entry could be anything, so one is only sent when the document has
// nothing better to offer.
func newestFirst(items []feed.Item) []int {
	order := make([]int, len(items))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		x, y := items[order[a]].PublishedAt, items[order[b]].PublishedAt
		switch {
		case x == nil && y == nil:
			return false
		case x == nil:
			return false
		case y == nil:
			return true
		default:
			return x.After(*y)
		}
	})
	return order
}
