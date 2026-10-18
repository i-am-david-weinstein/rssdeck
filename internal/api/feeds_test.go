package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/i-am-david-weinstein/rssdeck/internal/store"
	rssdecksync "github.com/i-am-david-weinstein/rssdeck/internal/sync"
)

func TestCreateFeedPassesTheRequestThroughAndReportsTheFeed(t *testing.T) {
	e := newEnv(t)
	label := "Example"
	interval := int64(600)
	backfill := 3

	rec := e.do(http.MethodPost, "/api/feeds", masterToken, map[string]any{
		"url":              "https://example.com/feed.xml",
		"label":            label,
		"interval_seconds": interval,
		"backfill":         backfill,
	})
	e.expectStatus(rec, http.StatusCreated)

	var feed feedJSON
	e.decode(rec, &feed)
	switch {
	case feed.URL != "https://example.com/feed.xml":
		t.Errorf("url = %q", feed.URL)
	case feed.Label != label:
		t.Errorf("label = %q, want %q", feed.Label, label)
	case feed.IntervalSeconds == nil || *feed.IntervalSeconds != interval:
		t.Errorf("interval_seconds = %v, want %d", feed.IntervalSeconds, interval)
	case !feed.Enabled:
		t.Error("a new feed is not enabled")
	}
	if want := "/api/feeds/" + itoa(feed.ID); rec.Header().Get("Location") != want {
		t.Errorf("Location = %q, want %q", rec.Header().Get("Location"), want)
	}

	if len(e.syncer.adds) != 1 {
		t.Fatalf("syncer saw %d adds, want 1", len(e.syncer.adds))
	}
	add := e.syncer.adds[0]
	switch {
	case add.URL != "https://example.com/feed.xml":
		t.Errorf("syncer got url %q", add.URL)
	case add.Label == nil || *add.Label != label:
		t.Errorf("syncer got label %v, want %q", add.Label, label)
	case add.IntervalSeconds == nil || *add.IntervalSeconds != interval:
		t.Errorf("syncer got interval %v, want %d", add.IntervalSeconds, interval)
	case add.Backfill == nil || *add.Backfill != backfill:
		t.Errorf("syncer got backfill %v, want %d", add.Backfill, backfill)
	}

	// The feed is readable afterwards, which is what the Location header
	// promises.
	e.expectStatus(e.do(http.MethodGet, "/api/feeds/"+itoa(feed.ID), masterToken, nil), http.StatusOK)
}

// A label is three states, and the middle one is easy to lose: absent means
// "default it from the title", an empty string means "no label at all".
func TestCreateFeedDistinguishesAnAbsentLabelFromAnEmptyOne(t *testing.T) {
	e := newEnv(t)

	e.do(http.MethodPost, "/api/feeds", masterToken, map[string]any{"url": "https://a.example/feed"})
	e.do(http.MethodPost, "/api/feeds", masterToken, map[string]any{"url": "https://b.example/feed", "label": ""})

	if len(e.syncer.adds) != 2 {
		t.Fatalf("syncer saw %d adds, want 2", len(e.syncer.adds))
	}
	if e.syncer.adds[0].Label != nil {
		t.Errorf("absent label reached the syncer as %q, want nil", *e.syncer.adds[0].Label)
	}
	if e.syncer.adds[1].Label == nil || *e.syncer.adds[1].Label != "" {
		t.Errorf("empty label reached the syncer as %v, want a pointer to \"\"", e.syncer.adds[1].Label)
	}
}

func TestCreateFeedErrors(t *testing.T) {
	tests := map[string]struct {
		err    error
		status int
		code   string
	}{
		"malformed url": {fmt.Errorf("%w: no host", rssdecksync.ErrInvalidURL), http.StatusBadRequest, codeInvalidURL},
		"not a feed":    {fmt.Errorf("%w: html", rssdecksync.ErrNotAFeed), http.StatusBadRequest, codeNotAFeed},
		"unreachable":   {fmt.Errorf("%w: dial tcp", rssdecksync.ErrUnreachable), http.StatusBadRequest, codeUnreachable},
		"duplicate":     {fmt.Errorf("%w: x", store.ErrDuplicateURL), http.StatusConflict, codeFeedExists},
		"database down": {errors.New("disk I/O error"), http.StatusInternalServerError, codeInternalError},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.syncer.addErr = tc.err

			rec := e.do(http.MethodPost, "/api/feeds", masterToken,
				map[string]any{"url": "https://example.com/feed.xml"})
			e.expectError(rec, tc.status, tc.code)

			if tc.status == http.StatusInternalServerError {
				// The cause is for the log, not for the client.
				var body errorBody
				e.decode(rec, &body)
				if body.Error != "internal error" {
					t.Errorf("internal failure reported as %q, want it not to leak the cause", body.Error)
				}
			}
		})
	}
}

func TestCreateFeedValidatesOptionsBeforeFetching(t *testing.T) {
	tests := map[string]map[string]any{
		"zero interval":     {"url": "https://example.com/feed.xml", "interval_seconds": 0},
		"negative interval": {"url": "https://example.com/feed.xml", "interval_seconds": -60},
		"negative backfill": {"url": "https://example.com/feed.xml", "backfill": -1},
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.expectError(e.do(http.MethodPost, "/api/feeds", masterToken, body),
				http.StatusBadRequest, codeInvalidRequest)
			if len(e.syncer.adds) != 0 {
				t.Error("an invalid request reached the syncer, so a feed server was poked for nothing")
			}
		})
	}
}

func TestListFeedsReportsPollStatus(t *testing.T) {
	e := newEnv(t)
	scheduled := e.addFeed("https://scheduled.example/feed")
	unscheduled := e.addFeed("https://unscheduled.example/feed")

	next := start.Add(9 * time.Minute)
	e.syncer.schedule(scheduled.ID, next)
	e.syncer.Unschedule(unscheduled.ID)

	rec := e.do(http.MethodGet, "/api/feeds", masterToken, nil)
	e.expectStatus(rec, http.StatusOK)

	var listed struct {
		Feeds []feedJSON `json:"feeds"`
	}
	e.decode(rec, &listed)
	if len(listed.Feeds) != 2 {
		t.Fatalf("listed %d feeds, want 2", len(listed.Feeds))
	}

	byID := map[int64]feedJSON{}
	for _, f := range listed.Feeds {
		byID[f.ID] = f
	}
	if got := byID[scheduled.ID].NextPollAt; got == nil || !got.Equal(next) {
		t.Errorf("next_poll_at = %v, want %v", got, next)
	}
	if got := byID[unscheduled.ID].NextPollAt; got != nil {
		t.Errorf("next_poll_at = %v for a feed that is not on the calendar, want null", got)
	}
}

func TestListFeedsIsEmptyRatherThanNull(t *testing.T) {
	e := newEnv(t)
	rec := e.do(http.MethodGet, "/api/feeds", masterToken, nil)
	e.expectStatus(rec, http.StatusOK)

	// A client iterating the array should not have to handle null as well as
	// an empty list.
	if body := rec.Body.String(); !strings.Contains(body, `"feeds": []`) {
		t.Errorf("empty listing rendered as %s", body)
	}
}

func TestGetFeedNotFound(t *testing.T) {
	e := newEnv(t)
	e.expectError(e.do(http.MethodGet, "/api/feeds/99", masterToken, nil),
		http.StatusNotFound, codeFeedNotFound)
}

func TestPatchFeedAppliesOnlyWhatTheBodyMentions(t *testing.T) {
	e := newEnv(t)
	feed := e.addFeed("https://example.com/feed.xml")

	rec := e.do(http.MethodPatch, "/api/feeds/"+itoa(feed.ID), masterToken,
		map[string]any{"label": "Renamed"})
	e.expectStatus(rec, http.StatusOK)

	var patched feedJSON
	e.decode(rec, &patched)
	switch {
	case patched.Label != "Renamed":
		t.Errorf("label = %q, want Renamed", patched.Label)
	case patched.URL != feed.URL:
		t.Errorf("url = %q, want it untouched", patched.URL)
	case !patched.Enabled:
		t.Error("enabled changed on a label-only patch")
	}

	// Renaming a feed must not move its next poll.
	if _, rescheduled, _ := e.syncer.calls(); len(rescheduled) != 0 {
		t.Errorf("a label-only patch rescheduled the feed: %v", rescheduled)
	}
}

func TestPatchFeedIntervalHasThreeStates(t *testing.T) {
	e := newEnv(t)
	rec := e.do(http.MethodPost, "/api/feeds", masterToken, map[string]any{
		"url":              "https://example.com/feed.xml",
		"interval_seconds": 600,
	})
	e.expectStatus(rec, http.StatusCreated)
	var feed feedJSON
	e.decode(rec, &feed)
	path := "/api/feeds/" + itoa(feed.ID)

	// Absent: the override stays.
	rec = e.do(http.MethodPatch, path, masterToken, map[string]any{"label": "Example"})
	e.expectStatus(rec, http.StatusOK)
	var patched feedJSON
	e.decode(rec, &patched)
	if patched.IntervalSeconds == nil || *patched.IntervalSeconds != 600 {
		t.Fatalf("interval_seconds = %v after a patch that did not mention it, want 600", patched.IntervalSeconds)
	}

	// A number: the override changes, and the feed is rescheduled so it takes
	// effect without a restart.
	rec = e.do(http.MethodPatch, path, masterToken, map[string]any{"interval_seconds": 120})
	e.expectStatus(rec, http.StatusOK)
	e.decode(rec, &patched)
	if patched.IntervalSeconds == nil || *patched.IntervalSeconds != 120 {
		t.Fatalf("interval_seconds = %v, want 120", patched.IntervalSeconds)
	}

	// Null: the override is cleared, so the feed follows the global interval.
	rec = e.do(http.MethodPatch, path, masterToken, `{"interval_seconds":null}`)
	e.expectStatus(rec, http.StatusOK)
	e.decode(rec, &patched)
	if patched.IntervalSeconds != nil {
		t.Fatalf("interval_seconds = %v after a null patch, want null", patched.IntervalSeconds)
	}

	if _, rescheduled, _ := e.syncer.calls(); len(rescheduled) != 2 {
		t.Errorf("feed was rescheduled %d times, want 2 (once per interval change)", len(rescheduled))
	}
}

func TestPatchFeedDisablingReschedules(t *testing.T) {
	e := newEnv(t)
	feed := e.addFeed("https://example.com/feed.xml")

	rec := e.do(http.MethodPatch, "/api/feeds/"+itoa(feed.ID), masterToken,
		map[string]any{"enabled": false})
	e.expectStatus(rec, http.StatusOK)

	var patched feedJSON
	e.decode(rec, &patched)
	if patched.Enabled {
		t.Error("enabled = true after disabling the feed")
	}
	_, rescheduled, _ := e.syncer.calls()
	if len(rescheduled) != 1 || rescheduled[0] != feed.ID {
		t.Errorf("rescheduled = %v, want [%d]", rescheduled, feed.ID)
	}
}

func TestPatchFeedRejectsBadIntervals(t *testing.T) {
	e := newEnv(t)
	feed := e.addFeed("https://example.com/feed.xml")
	path := "/api/feeds/" + itoa(feed.ID)

	for _, body := range []string{`{"interval_seconds":0}`, `{"interval_seconds":"600"}`} {
		e.expectError(e.do(http.MethodPatch, path, masterToken, body),
			http.StatusBadRequest, codeInvalidRequest)
	}
}

func TestPatchFeedNotFound(t *testing.T) {
	e := newEnv(t)
	e.expectError(e.do(http.MethodPatch, "/api/feeds/99", masterToken, map[string]any{"label": "x"}),
		http.StatusNotFound, codeFeedNotFound)
}

func TestDeleteFeedPurgesItemsAndUnschedules(t *testing.T) {
	e := newEnv(t)
	feed := e.addFeed("https://example.com/feed.xml")
	e.ingest(feed.ID, store.NewItem{
		FeedID: feed.ID,
		GUID:   "item-1",
		URL:    "https://example.com/1",
		Status: store.StatusSent,
		SeenAt: start,
	})

	e.expectStatus(e.do(http.MethodDelete, "/api/feeds/"+itoa(feed.ID), masterToken, nil),
		http.StatusNoContent)

	_, _, unscheduled := e.syncer.calls()
	if len(unscheduled) != 1 || unscheduled[0] != feed.ID {
		t.Errorf("unscheduled = %v, want [%d]", unscheduled, feed.ID)
	}

	items, total, err := e.store.ListItems(context.Background(), store.ItemFilter{})
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if total != 0 || len(items) != 0 {
		t.Errorf("deleting a feed left %d items behind", total)
	}

	e.expectError(e.do(http.MethodDelete, "/api/feeds/"+itoa(feed.ID), masterToken, nil),
		http.StatusNotFound, codeFeedNotFound)
}

func TestPollFeedReportsWhatThePollDid(t *testing.T) {
	e := newEnv(t)
	feed := e.addFeed("https://example.com/feed.xml")
	e.syncer.pollResult = &rssdecksync.PollResult{
		FeedID:   feed.ID,
		FeedURL:  feed.URL,
		New:      4,
		Existing: 11,
		Sent:     4,
		Failed:   1,
		Pruned:   2,
		Duration: 2 * time.Second,
	}

	rec := e.do(http.MethodPost, "/api/feeds/"+itoa(feed.ID)+"/poll", masterToken, nil)
	e.expectStatus(rec, http.StatusOK)

	var result pollJSON
	e.decode(rec, &result)
	switch {
	case result.FeedID != feed.ID:
		t.Errorf("feed_id = %d, want %d", result.FeedID, feed.ID)
	case result.New != 4 || result.Existing != 11 || result.Sent != 4 || result.Failed != 1 || result.Pruned != 2:
		t.Errorf("counts = %+v", result)
	case result.DurationMS != 2000:
		t.Errorf("duration_ms = %d, want 2000", result.DurationMS)
	}

	polls, _, _ := e.syncer.calls()
	if len(polls) != 1 || polls[0] != feed.ID {
		t.Errorf("polls = %v, want [%d]", polls, feed.ID)
	}
}

func TestPollFeedErrors(t *testing.T) {
	tests := map[string]struct {
		err    error
		status int
		code   string
	}{
		"already polling": {
			fmt.Errorf("%w: feed 1", rssdecksync.ErrPollInProgress), http.StatusConflict, codePollInProgress,
		},
		"gone": {
			fmt.Errorf("%w: feed 1", store.ErrNotFound), http.StatusNotFound, codeFeedNotFound,
		},
		"feed server broken": {
			errors.New("unexpected EOF"), http.StatusBadGateway, codePollFailed,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			feed := e.addFeed("https://example.com/feed.xml")
			e.syncer.pollErr = tc.err

			rec := e.do(http.MethodPost, "/api/feeds/"+itoa(feed.ID)+"/poll", masterToken, nil)
			e.expectError(rec, tc.status, tc.code)
		})
	}
}

func TestPollFeedNotFound(t *testing.T) {
	e := newEnv(t)
	e.expectError(e.do(http.MethodPost, "/api/feeds/99/poll", masterToken, nil),
		http.StatusNotFound, codeFeedNotFound)
}
