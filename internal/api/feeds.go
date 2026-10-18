package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/i-am-david-weinstein/rssdeck/internal/store"
	"github.com/i-am-david-weinstein/rssdeck/internal/sync"
)

// feedJSON is a feed as the API reports it: what it is subscribed to, and where
// its polling stands.
//
// Optional fields are rendered as null rather than omitted, so a client always
// sees the same shape and can tell "never polled" from "field I forgot about".
// The validators (etag, last_modified) are left out: they are the daemon's
// bookkeeping and say nothing an operator can act on.
type feedJSON struct {
	ID    int64  `json:"id"`
	URL   string `json:"url"`
	Title string `json:"title"`
	Label string `json:"label"`

	Enabled bool `json:"enabled"`

	// IntervalSeconds is null when the feed follows the global interval.
	IntervalSeconds *int64 `json:"interval_seconds"`

	LastPolledAt  *time.Time `json:"last_polled_at"`
	LastSuccessAt *time.Time `json:"last_success_at"`

	// NextPollAt comes from the scheduler rather than the database, and is null
	// for a feed that is not on the calendar, which is how a disabled feed
	// reads.
	NextPollAt *time.Time `json:"next_poll_at"`

	ConsecutiveFailures int    `json:"consecutive_failures"`
	LastError           string `json:"last_error,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

func (s *Server) feedJSON(f *store.Feed) feedJSON {
	out := feedJSON{
		ID:                  f.ID,
		URL:                 f.URL,
		Title:               f.Title,
		Label:               f.Label,
		Enabled:             f.Enabled,
		IntervalSeconds:     f.IntervalSeconds,
		LastPolledAt:        f.LastPolledAt,
		LastSuccessAt:       f.LastSuccessAt,
		ConsecutiveFailures: f.ConsecutiveFailures,
		LastError:           f.LastError,
		CreatedAt:           f.CreatedAt,
	}
	if next, ok := s.syncer.NextPoll(f.ID); ok {
		out.NextPollAt = &next
	}
	return out
}

// listFeeds answers GET /api/feeds with every feed, enabled or not.
//
// There is no pagination here. Feeds are added by hand, a busy instance has
// tens of them, and a client that has to page through its own subscriptions for
// no reason is worse off than one that gets the list.
func (s *Server) listFeeds(w http.ResponseWriter, r *http.Request) error {
	feeds, err := s.store.ListFeeds(r.Context())
	if err != nil {
		return internalError(err)
	}
	out := make([]feedJSON, 0, len(feeds))
	for i := range feeds {
		out = append(out, s.feedJSON(&feeds[i]))
	}
	return writeJSON(w, http.StatusOK, map[string]any{"feeds": out})
}

// createFeedRequest is the body of POST /api/feeds.
type createFeedRequest struct {
	URL string `json:"url"`

	// Label overrides the default, which is the feed's own title cut to 64
	// characters. An empty string means no feed label, leaving only the global
	// one.
	Label *string `json:"label"`

	// IntervalSeconds overrides the global poll interval for this feed.
	IntervalSeconds *int64 `json:"interval_seconds"`

	// Backfill is how many of the feed's newest items to send now. Null uses
	// the configured default, and 0 sends none.
	Backfill *int `json:"backfill"`
}

// createFeed answers POST /api/feeds. The fetch and parse happen inside the
// request, so a URL that is not a feed comes back as a 400 an agent can read
// immediately, rather than as a poll failure it would have to go looking for.
func (s *Server) createFeed(w http.ResponseWriter, r *http.Request) error {
	var req createFeedRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}
	if req.IntervalSeconds != nil && *req.IntervalSeconds < 1 {
		return errorf(http.StatusBadRequest, codeInvalidRequest,
			"interval_seconds must be at least 1, or null to follow the global interval")
	}
	if req.Backfill != nil && *req.Backfill < 0 {
		return errorf(http.StatusBadRequest, codeInvalidRequest, "backfill cannot be negative")
	}

	f, err := s.syncer.AddFeed(r.Context(), sync.AddRequest{
		URL:             req.URL,
		Label:           req.Label,
		IntervalSeconds: req.IntervalSeconds,
		Backfill:        req.Backfill,
	})
	if err != nil {
		return addFeedError(err)
	}

	s.log.Info("feed added through the api",
		"feed_id", f.ID, "url", f.URL, "principal", principalFor(r.Context()).String())
	w.Header().Set("Location", "/api/feeds/"+strconv.FormatInt(f.ID, 10))
	return writeJSON(w, http.StatusCreated, s.feedJSON(f))
}

// addFeedError maps the ways an add can fail. All but the duplicate are 400s:
// an unreachable URL, a URL that is not a feed and a malformed URL are the
// caller's to fix, and none of them wrote anything.
func addFeedError(err error) error {
	switch {
	case errors.Is(err, sync.ErrInvalidURL):
		return wrapError(http.StatusBadRequest, codeInvalidURL, err)
	case errors.Is(err, sync.ErrNotAFeed):
		return wrapError(http.StatusBadRequest, codeNotAFeed, err)
	case errors.Is(err, sync.ErrUnreachable):
		return wrapError(http.StatusBadRequest, codeUnreachable, err)
	case errors.Is(err, store.ErrDuplicateURL):
		return wrapError(http.StatusConflict, codeFeedExists, err)
	default:
		return internalError(err)
	}
}

// getFeed answers GET /api/feeds/{id}.
func (s *Server) getFeed(w http.ResponseWriter, r *http.Request) error {
	f, err := s.feed(r)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, s.feedJSON(f))
}

// patchFeedRequest is the body of PATCH /api/feeds/{id}.
//
// interval_seconds is a json.RawMessage because this endpoint has to tell three
// cases apart: absent (leave the override alone), null (clear it, so the feed
// follows the global interval) and a number (set it). A *int64 collapses the
// first two.
type patchFeedRequest struct {
	Label           *string         `json:"label"`
	IntervalSeconds json.RawMessage `json:"interval_seconds"`
	Enabled         *bool           `json:"enabled"`
}

// patchFeed answers PATCH /api/feeds/{id}, changing only the fields the body
// mentions.
func (s *Server) patchFeed(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "feed")
	if err != nil {
		return err
	}
	var req patchFeedRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}

	update := store.FeedUpdate{Label: req.Label, Enabled: req.Enabled}
	interval, err := parseIntervalPatch(req.IntervalSeconds)
	if err != nil {
		return err
	}
	update.IntervalSeconds = interval

	f, err := s.store.UpdateFeed(r.Context(), id, update)
	if err != nil {
		return feedError(id, err)
	}

	// Only a change to the interval or to enabled affects when this feed polls
	// next, and rescheduling re-draws its jitter. A label change leaves the
	// calendar alone so that renaming a feed does not move its next poll.
	if req.Enabled != nil || interval != nil {
		if err := s.syncer.Reschedule(r.Context(), f.ID); err != nil {
			return internalError(err)
		}
	}
	s.log.Info("feed updated through the api",
		"feed_id", f.ID, "url", f.URL, "enabled", f.Enabled,
		"principal", principalFor(r.Context()).String())
	return writeJSON(w, http.StatusOK, s.feedJSON(f))
}

// parseIntervalPatch reads the three states of interval_seconds in a PATCH
// body. The outer pointer is "the body mentioned it"; the inner one is the
// value, nil meaning follow the global interval.
func parseIntervalPatch(raw json.RawMessage) (**int64, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var value *int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, errorf(http.StatusBadRequest, codeInvalidRequest,
			"interval_seconds must be a number or null")
	}
	if value != nil && *value < 1 {
		return nil, errorf(http.StatusBadRequest, codeInvalidRequest,
			"interval_seconds must be at least 1, or null to follow the global interval")
	}
	return &value, nil
}

// deleteFeed answers DELETE /api/feeds/{id}. The feed's items go with it
// through ON DELETE CASCADE, so re-adding the same URL starts clean and will
// resend.
func (s *Server) deleteFeed(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "feed")
	if err != nil {
		return err
	}
	if err := s.store.DeleteFeed(r.Context(), id); err != nil {
		return feedError(id, err)
	}
	// Taking the feed off the calendar after the row is gone is the safe
	// order: a poll in flight for it finishes, and nothing reschedules it.
	s.syncer.Unschedule(id)
	s.log.Info("feed deleted through the api",
		"feed_id", id, "principal", principalFor(r.Context()).String())
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// pollJSON is what one poll did.
type pollJSON struct {
	FeedID  int64  `json:"feed_id"`
	FeedURL string `json:"feed_url"`

	// NotModified means the feed server answered 304, so nothing was parsed or
	// pruned. Pending items are still sent on such a poll.
	NotModified bool `json:"not_modified"`

	New      int `json:"new"`
	Existing int `json:"existing"`
	Sent     int `json:"sent"`
	Failed   int `json:"failed"`
	Pruned   int `json:"pruned"`

	DurationMS int64 `json:"duration_ms"`
}

// pollFeed answers POST /api/feeds/{id}/poll, polling the feed before it
// replies so the response reports what actually happened.
//
// A disabled feed is polled too: the scheduler leaves it alone, but asking for
// a poll by hand is an operator's decision.
func (s *Server) pollFeed(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "feed")
	if err != nil {
		return err
	}

	res, err := s.syncer.Poll(r.Context(), id)
	switch {
	case errors.Is(err, sync.ErrPollInProgress):
		// Two polls of one feed at once could send the same item twice, so the
		// second one is refused rather than queued.
		return wrapError(http.StatusConflict, codePollInProgress, err)
	case errors.Is(err, store.ErrNotFound):
		return feedError(id, err)
	case err != nil:
		// The feed was unreachable, served something unparseable, or Readeck
		// refused the token. Whatever it was has been recorded against the
		// feed, so GET /api/feeds/{id} has the detail and the next scheduled
		// poll will try again.
		return wrapError(http.StatusBadGateway, codePollFailed, err)
	}

	return writeJSON(w, http.StatusOK, pollJSON{
		FeedID:      res.FeedID,
		FeedURL:     res.FeedURL,
		NotModified: res.NotModified,
		New:         res.New,
		Existing:    res.Existing,
		Sent:        res.Sent,
		Failed:      res.Failed,
		Pruned:      res.Pruned,
		DurationMS:  res.Duration.Milliseconds(),
	})
}

// feed loads the feed named by the request path.
func (s *Server) feed(r *http.Request) (*store.Feed, error) {
	id, err := pathID(r, "feed")
	if err != nil {
		return nil, err
	}
	f, err := s.store.GetFeed(r.Context(), id)
	if err != nil {
		return nil, feedError(id, err)
	}
	return f, nil
}

// feedError turns a store error about one feed into a response.
func feedError(id int64, err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return errorf(http.StatusNotFound, codeFeedNotFound, "no feed with id %d", id)
	}
	return internalError(err)
}
