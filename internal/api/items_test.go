package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/i-am-david-weinstein/rssdeck/internal/store"
)

// itemsPage is the shape of GET /api/items.
type itemsPage struct {
	Items  []itemJSON `json:"items"`
	Total  int        `json:"total"`
	Limit  int        `json:"limit"`
	Offset int        `json:"offset"`
}

func (e *env) listItems(query string) itemsPage {
	e.t.Helper()
	rec := e.do(http.MethodGet, "/api/items"+query, masterToken, nil)
	e.expectStatus(rec, http.StatusOK)
	var page itemsPage
	e.decode(rec, &page)
	return page
}

// seedItems records three items on one feed, published an hour apart, and
// returns the feed's id.
func (e *env) seedItems() int64 {
	e.t.Helper()
	feed := e.addFeed("https://example.com/feed.xml")
	at := func(h int) *time.Time {
		t := start.Add(time.Duration(h) * time.Hour)
		return &t
	}
	e.ingest(feed.ID,
		store.NewItem{GUID: "oldest", URL: "https://example.com/1", Title: "Oldest",
			PublishedAt: at(1), Status: store.StatusSent, SeenAt: start},
		store.NewItem{GUID: "middle", URL: "https://example.com/2", Title: "Middle",
			PublishedAt: at(2), Status: store.StatusPending, SeenAt: start},
		store.NewItem{GUID: "newest", URL: "https://example.com/3", Title: "Newest",
			PublishedAt: at(3), Status: store.StatusPending, SeenAt: start},
	)
	return feed.ID
}

func TestListItemsIsNewestFirst(t *testing.T) {
	e := newEnv(t)
	e.seedItems()

	page := e.listItems("")
	if page.Total != 3 {
		t.Fatalf("total = %d, want 3", page.Total)
	}
	want := []string{"Newest", "Middle", "Oldest"}
	for i, title := range want {
		if page.Items[i].Title != title {
			t.Fatalf("item %d is %q, want %q", i, page.Items[i].Title, title)
		}
	}
	if page.Limit != store.DefaultItemLimit || page.Offset != 0 {
		t.Errorf("limit/offset = %d/%d, want %d/0", page.Limit, page.Offset, store.DefaultItemLimit)
	}
}

func TestListItemsRendersDeliveryState(t *testing.T) {
	e := newEnv(t)
	feed := e.addFeed("https://example.com/feed.xml")
	e.ingest(feed.ID, store.NewItem{
		GUID:      "no-url",
		Title:     "Nothing to fetch",
		Status:    store.StatusFailedPermanent,
		LastError: "no usable URL",
		SeenAt:    start,
	})

	page := e.listItems("")
	if len(page.Items) != 1 {
		t.Fatalf("listed %d items, want 1", len(page.Items))
	}
	item := page.Items[0]
	switch {
	case item.FeedID != feed.ID:
		t.Errorf("feed_id = %d, want %d", item.FeedID, feed.ID)
	case item.Status != string(store.StatusFailedPermanent):
		t.Errorf("status = %q", item.Status)
	case item.LastError != "no usable URL":
		t.Errorf("last_error = %q", item.LastError)
	case item.SentAt != nil:
		t.Errorf("sent_at = %v for an item that was never sent, want null", item.SentAt)
	case item.ReadeckBookmarkID != "":
		t.Errorf("readeck_bookmark_id = %q for an item that was never sent", item.ReadeckBookmarkID)
	case item.FirstSeenAt.IsZero() || item.LastSeenInFeedAt.IsZero():
		t.Errorf("seen timestamps = %v / %v", item.FirstSeenAt, item.LastSeenInFeedAt)
	}
}

func TestListItemsFilters(t *testing.T) {
	e := newEnv(t)
	feedID := e.seedItems()
	other := e.addFeed("https://other.example/feed")
	e.ingest(other.ID, store.NewItem{GUID: "elsewhere", URL: "https://other.example/1",
		Status: store.StatusPending, SeenAt: start})

	if page := e.listItems("?feed_id=" + itoa(feedID)); page.Total != 3 {
		t.Errorf("total for one feed = %d, want 3", page.Total)
	}
	if page := e.listItems("?status=pending"); page.Total != 3 {
		t.Errorf("total pending = %d, want 3 (two on one feed, one on the other)", page.Total)
	}
	page := e.listItems("?feed_id=" + itoa(feedID) + "&status=sent")
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].GUID != "oldest" {
		t.Errorf("filtering by feed and status gave %+v", page)
	}
}

func TestListItemsPages(t *testing.T) {
	e := newEnv(t)
	e.seedItems()

	page := e.listItems("?limit=2")
	if page.Total != 3 || len(page.Items) != 2 || page.Items[0].GUID != "newest" {
		t.Fatalf("first page = %+v", page)
	}

	page = e.listItems("?limit=2&offset=2")
	if page.Total != 3 || len(page.Items) != 1 || page.Items[0].GUID != "oldest" {
		t.Fatalf("second page = %+v", page)
	}
	if page.Offset != 2 {
		t.Errorf("offset = %d, want 2", page.Offset)
	}

	// Past the end is an empty page rather than an error.
	page = e.listItems("?offset=99")
	if page.Total != 3 || len(page.Items) != 0 {
		t.Fatalf("page past the end = %+v", page)
	}
}

// A client asking for more than the maximum wants as much as it can have, so
// the limit is clamped rather than rejected. The response reports the page size
// that was actually used.
func TestListItemsClampsTheLimit(t *testing.T) {
	e := newEnv(t)
	e.seedItems()

	if page := e.listItems("?limit=100000"); page.Limit != store.MaxItemLimit {
		t.Errorf("limit = %d, want it clamped to %d", page.Limit, store.MaxItemLimit)
	}
	if page := e.listItems("?limit=0"); page.Limit != store.DefaultItemLimit {
		t.Errorf("limit = %d for limit=0, want the default %d", page.Limit, store.DefaultItemLimit)
	}
}

func TestListItemsIsEmptyRatherThanNull(t *testing.T) {
	e := newEnv(t)
	rec := e.do(http.MethodGet, "/api/items", masterToken, nil)
	e.expectStatus(rec, http.StatusOK)
	if body := rec.Body.String(); !strings.Contains(body, `"items": []`) {
		t.Errorf("empty listing rendered as %s", body)
	}
}

func TestListItemsRejectsBadParameters(t *testing.T) {
	tests := map[string]struct {
		query string
		code  string
	}{
		"unknown status":  {"?status=delivered", codeInvalidStatus},
		"feed id zero":    {"?feed_id=0", codeInvalidRequest},
		"feed id text":    {"?feed_id=mine", codeInvalidRequest},
		"negative limit":  {"?limit=-1", codeInvalidRequest},
		"limit text":      {"?limit=all", codeInvalidRequest},
		"negative offset": {"?offset=-5", codeInvalidRequest},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.expectError(e.do(http.MethodGet, "/api/items"+tc.query, masterToken, nil),
				http.StatusBadRequest, tc.code)
		})
	}
}
