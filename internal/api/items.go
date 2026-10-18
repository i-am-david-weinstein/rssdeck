package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/i-am-david-weinstein/rssdeck/internal/store"
)

// itemJSON is one feed entry and its delivery state. Everything optional is
// rendered as null rather than omitted, so the shape does not change with the
// item's status.
type itemJSON struct {
	ID     int64 `json:"id"`
	FeedID int64 `json:"feed_id"`

	// GUID is the item's identity within its feed, which is why the same
	// article in two feeds becomes two bookmarks.
	GUID string `json:"guid"`

	URL         string     `json:"url"`
	Title       string     `json:"title"`
	PublishedAt *time.Time `json:"published_at"`

	Status    string `json:"status"`
	Attempts  int    `json:"attempts"`
	LastError string `json:"last_error,omitempty"`

	ReadeckBookmarkID string     `json:"readeck_bookmark_id,omitempty"`
	SentAt            *time.Time `json:"sent_at"`

	FirstSeenAt      time.Time `json:"first_seen_at"`
	LastSeenInFeedAt time.Time `json:"last_seen_in_feed_at"`
}

func newItemJSON(it *store.Item) itemJSON {
	return itemJSON{
		ID:                it.ID,
		FeedID:            it.FeedID,
		GUID:              it.GUID,
		URL:               it.URL,
		Title:             it.Title,
		PublishedAt:       it.PublishedAt,
		Status:            string(it.Status),
		Attempts:          it.Attempts,
		LastError:         it.LastError,
		ReadeckBookmarkID: it.ReadeckBookmarkID,
		SentAt:            it.SentAt,
		FirstSeenAt:       it.FirstSeenAt,
		LastSeenInFeedAt:  it.LastSeenInFeedAt,
	}
}

// listItems answers GET /api/items, newest first, with total reporting how many
// match the filter before paging.
//
// Items are the one collection that can grow large, so this is the only paged
// endpoint. The limit is clamped rather than rejected: a client asking for more
// than the maximum wants as much as it can have.
func (s *Server) listItems(w http.ResponseWriter, r *http.Request) error {
	query := r.URL.Query()

	filter := store.ItemFilter{}
	if raw := query.Get("feed_id"); raw != "" {
		feedID, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || feedID < 1 {
			return errorf(http.StatusBadRequest, codeInvalidRequest,
				"feed_id must be a positive integer, got %q", raw)
		}
		filter.FeedID = &feedID
	}
	if raw := query.Get("status"); raw != "" {
		status := store.ItemStatus(raw)
		if !status.Valid() {
			return errorf(http.StatusBadRequest, codeInvalidStatus,
				"status must be one of pending, sent, failed_permanent, seeded; got %q", raw)
		}
		filter.Status = &status
	}

	limit, err := intParam(query.Get("limit"), "limit", store.DefaultItemLimit)
	if err != nil {
		return err
	}
	offset, err := intParam(query.Get("offset"), "offset", 0)
	if err != nil {
		return err
	}
	// The limit is resolved here rather than left to the store, so the response
	// reports the page size it actually used.
	switch {
	case limit <= 0:
		limit = store.DefaultItemLimit
	case limit > store.MaxItemLimit:
		limit = store.MaxItemLimit
	}
	filter.Limit, filter.Offset = limit, offset

	items, total, err := s.store.ListItems(r.Context(), filter)
	if err != nil {
		return internalError(err)
	}

	out := make([]itemJSON, 0, len(items))
	for i := range items {
		out = append(out, newItemJSON(&items[i]))
	}
	return writeJSON(w, http.StatusOK, map[string]any{
		"items":  out,
		"total":  total,
		"limit":  filter.Limit,
		"offset": filter.Offset,
	})
}

// intParam reads a non-negative integer query parameter, falling back to def
// when it is absent or empty.
func intParam(raw, name string, def int) (int, error) {
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		return 0, errorf(http.StatusBadRequest, codeInvalidRequest,
			"%s must be a non-negative integer, got %q", name, raw)
	}
	return v, nil
}
