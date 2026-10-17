package sync

import (
	"context"
	"errors"

	"github.com/i-am-david-weinstein/rssdeck/internal/readeck"
	"github.com/i-am-david-weinstein/rssdeck/internal/store"
)

// send posts a feed's pending items to Readeck, oldest first so that Readeck's
// newest-first list ends up matching the feed's chronology.
//
// A send that fails leaves the item pending and counted, to be retried on the
// next poll: the daemon is already a retry loop, so there is no inner retry.
// The one failure that stops the loop is a rejected token, which will reject
// every remaining item too.
func (s *Syncer) send(ctx context.Context, f *store.Feed, res *PollResult) error {
	pending, err := s.store.PendingItems(ctx, f.ID)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		return nil
	}
	labels := s.labels(f)
	s.log.Debug("sending pending items", "feed_id", f.ID, "pending", len(pending), "labels", labels)

	for i := range pending {
		item := &pending[i]
		if err := ctx.Err(); err != nil {
			return err
		}

		// Ingest never leaves a URL-less item pending, so this is a guard
		// against a future path that does, not an expected case. Sending it is
		// hopeless, and one attempt is enough to retire it.
		if item.URL == "" {
			res.Failed++
			s.recordFailure(ctx, f, item, noUsableURL, 1)
			continue
		}

		bookmarkID, err := s.readeck.CreateBookmark(ctx, readeck.Bookmark{URL: item.URL, Labels: labels})
		if err != nil {
			res.Failed++
			if errors.Is(err, readeck.ErrUnauthorized) {
				// Nothing about this item is wrong, so it keeps its attempts:
				// counting a bad token against them would retire a queue of
				// perfectly good items over a misconfiguration. One log line
				// for the whole poll, since every remaining item would say the
				// same thing.
				if rerr := s.store.RecordSendError(ctx, item.ID, err.Error()); rerr != nil {
					s.log.Error("could not record a send error", "item_id", item.ID, "error", rerr)
				}
				s.log.Error("readeck rejected the token, abandoning the rest of this poll",
					"feed_id", f.ID, "url", f.URL, "remaining", len(pending)-i, "error", err)
				return err
			}
			if errIsCanceled(err) {
				return err
			}
			s.recordFailure(ctx, f, item, err.Error(), s.maxFailures)
			continue
		}

		if err := s.store.MarkItemSent(ctx, item.ID, bookmarkID, s.now()); err != nil {
			return err
		}
		res.Sent++
		s.log.Info("item sent",
			"feed_id", f.ID, "feed", f.Title, "item_id", item.ID,
			"title", item.Title, "url", item.URL, "bookmark_id", bookmarkID)
	}
	return nil
}

// recordFailure counts one failed send and says what it meant. maxFailures is a
// parameter so a hopeless item can be retired in one attempt.
func (s *Syncer) recordFailure(ctx context.Context, f *store.Feed, item *store.Item, cause string, maxFailures int) {
	status, attempts, err := s.store.RecordSendFailure(ctx, item.ID, cause, maxFailures)
	if err != nil {
		s.log.Error("could not record a send failure", "item_id", item.ID, "error", err)
		return
	}
	if status == store.StatusFailedPermanent {
		s.log.Error("item permanently failed",
			"feed_id", f.ID, "item_id", item.ID, "title", item.Title,
			"url", item.URL, "attempts", attempts, "error", cause)
		return
	}
	s.log.Warn("item send failed, will retry",
		"feed_id", f.ID, "item_id", item.ID, "title", item.Title,
		"url", item.URL, "attempts", attempts, "error", cause)
}
