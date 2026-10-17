package sync

import (
	"context"
	"testing"
	"time"

	"github.com/i-am-david-weinstein/rssdeck/internal/store"
)

func TestFirstPollIsJitteredAcrossTheInterval(t *testing.T) {
	// The real jitter, since that is what is under test here.
	h := newHarness(t, func(o *Options) { o.jitter = nil })
	f := &store.Feed{ID: 1, Enabled: true}

	spread := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		at := h.syncer.firstPoll(f, start)
		offset := at.Sub(start)
		if offset < 0 || offset >= 15*time.Minute {
			t.Fatalf("first poll offset = %v, want it inside the interval", offset)
		}
		spread[offset.Truncate(time.Minute)] = true
	}
	if len(spread) < 5 {
		t.Errorf("offsets landed in %d minutes of the interval, want them spread out", len(spread))
	}
}

func TestFirstPollUsesThePerFeedInterval(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.jitter = nil })
	minute := int64(60)
	f := &store.Feed{ID: 1, Enabled: true, IntervalSeconds: &minute}

	for i := 0; i < 50; i++ {
		if offset := h.syncer.firstPoll(f, start).Sub(start); offset < 0 || offset >= time.Minute {
			t.Fatalf("first poll offset = %v, want it inside the feed's own minute", offset)
		}
	}
}

func TestFirstPollWaitsAnIntervalAfterTheLastPoll(t *testing.T) {
	h := newHarness(t)
	lastPolled := start.Add(-time.Minute)
	f := &store.Feed{ID: 1, Enabled: true, LastPolledAt: &lastPolled}

	// Without this guard a container restarting every minute would poll every
	// feed on every start.
	at := h.syncer.firstPoll(f, start)
	if want := lastPolled.Add(15 * time.Minute); !at.Equal(want) {
		t.Errorf("first poll = %v, want %v", at, want)
	}
}

func TestFirstPollIgnoresAnOldLastPoll(t *testing.T) {
	h := newHarness(t)
	lastPolled := start.Add(-24 * time.Hour)
	f := &store.Feed{ID: 1, Enabled: true, LastPolledAt: &lastPolled}

	if at := h.syncer.firstPoll(f, start); !at.Equal(start) {
		t.Errorf("first poll = %v, want %v: a feed overdue since yesterday polls now", at, start)
	}
}

func TestRunSchedulesEnabledFeedsOnly(t *testing.T) {
	h := newHarness(t)
	enabled := h.addFeed("https://a.example/feed", "")
	disabled := h.addFeed("https://b.example/feed", "")
	off := false
	if _, err := h.store.UpdateFeed(context.Background(), disabled.ID, store.FeedUpdate{Enabled: &off}); err != nil {
		t.Fatalf("UpdateFeed: %v", err)
	}

	if err := h.syncer.loadSchedule(context.Background()); err != nil {
		t.Fatalf("loadSchedule: %v", err)
	}

	if _, ok := h.syncer.sched.nextPoll(enabled.ID); !ok {
		t.Error("the enabled feed is not on the calendar")
	}
	if _, ok := h.syncer.sched.nextPoll(disabled.ID); ok {
		t.Error("a disabled feed is on the calendar")
	}
}

func TestRunPollsDueFeedsAndReschedulesThem(t *testing.T) {
	h := newHarness(t)
	f := h.addFeed(feedURL, "")
	h.fetcher.serve(feedURL, fetchStep{body: rss("Example",
		testItem{guid: "a", link: "https://example.com/a"},
	)})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.syncer.Run(ctx) }()

	waitFor(t, "the feed to be polled", func() bool { return h.fetcher.fetches() > 0 })
	// One interval out, so the next tick does not poll it again.
	waitFor(t, "the feed to be rescheduled", func() bool {
		at, ok := h.syncer.sched.nextPoll(f.ID)
		return ok && at.Equal(start.Add(15*time.Minute))
	})

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := h.fetcher.fetches(); n != 1 {
		t.Errorf("%d fetches, want 1: the feed is not due again for an interval", n)
	}
	if item := h.item(f.ID, "a"); item.Status != store.StatusSent {
		t.Errorf("status = %q, want sent", item.Status)
	}
}

func TestRunPollsAgainWhenTheIntervalPasses(t *testing.T) {
	h := newHarness(t)
	f := h.addFeed(feedURL, "")
	h.fetcher.serve(feedURL, fetchStep{body: rss("Example")})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.syncer.Run(ctx) }()

	// Waiting for the reschedule rather than the fetch, because the next poll
	// is an interval after the first one finishes.
	waitFor(t, "the first poll to finish", func() bool {
		at, ok := h.syncer.sched.nextPoll(f.ID)
		return ok && at.Equal(start.Add(15*time.Minute))
	})
	h.clock.advance(15 * time.Minute)
	waitFor(t, "the second poll", func() bool { return h.fetcher.fetches() == 2 })

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestRunStopsPollingAnUnscheduledFeed(t *testing.T) {
	h := newHarness(t)
	f := h.addFeed(feedURL, "")
	h.fetcher.serve(feedURL, fetchStep{body: rss("Example")})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.syncer.Run(ctx) }()

	waitFor(t, "the first poll", func() bool { return h.fetcher.fetches() == 1 })
	h.syncer.Unschedule(f.ID)
	h.clock.advance(time.Hour)

	// Nothing should poll it again, however overdue it looks.
	time.Sleep(20 * time.Millisecond)
	if n := h.fetcher.fetches(); n != 1 {
		t.Errorf("%d fetches, want 1: an unscheduled feed was polled", n)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestRescheduleFollowsTheEnabledFlag(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	f := h.addFeed(feedURL, "")
	if err := h.syncer.loadSchedule(ctx); err != nil {
		t.Fatalf("loadSchedule: %v", err)
	}

	off := false
	if _, err := h.store.UpdateFeed(ctx, f.ID, store.FeedUpdate{Enabled: &off}); err != nil {
		t.Fatalf("UpdateFeed: %v", err)
	}
	if err := h.syncer.Reschedule(ctx, f.ID); err != nil {
		t.Fatalf("Reschedule: %v", err)
	}
	if _, ok := h.syncer.sched.nextPoll(f.ID); ok {
		t.Error("a disabled feed stayed on the calendar")
	}

	on := true
	if _, err := h.store.UpdateFeed(ctx, f.ID, store.FeedUpdate{Enabled: &on}); err != nil {
		t.Fatalf("UpdateFeed: %v", err)
	}
	if err := h.syncer.Reschedule(ctx, f.ID); err != nil {
		t.Fatalf("Reschedule: %v", err)
	}
	if _, ok := h.syncer.sched.nextPoll(f.ID); !ok {
		t.Error("an enabled feed was not put back on the calendar")
	}
}

func TestRescheduleForgetsADeletedFeed(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	f := h.addFeed(feedURL, "")
	if err := h.syncer.loadSchedule(ctx); err != nil {
		t.Fatalf("loadSchedule: %v", err)
	}
	if err := h.store.DeleteFeed(ctx, f.ID); err != nil {
		t.Fatalf("DeleteFeed: %v", err)
	}

	if err := h.syncer.Reschedule(ctx, f.ID); err != nil {
		t.Fatalf("Reschedule: %v", err)
	}
	if _, ok := h.syncer.sched.nextPoll(f.ID); ok {
		t.Error("a deleted feed stayed on the calendar")
	}
}

func TestRunLetsAnInFlightPollFinishOnShutdown(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.ShutdownGrace = 5 * time.Second })
	f := h.addFeed(feedURL, "")
	h.fetcher.serve(feedURL, fetchStep{body: rss("Example",
		testItem{guid: "a", link: "https://example.com/a"},
	)})

	fetching := make(chan struct{}, 1)
	release := make(chan struct{})
	h.fetcher.hook = func(context.Context, string) {
		fetching <- struct{}{}
		<-release
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.syncer.Run(ctx) }()
	<-fetching

	cancel()
	select {
	case <-done:
		t.Fatal("Run returned while a poll was still in flight")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after the poll finished")
	}

	// The poll ran to completion on a context of its own, so its work landed.
	if item := h.item(f.ID, "a"); item.Status != store.StatusSent {
		t.Errorf("status = %q, want the in-flight poll to have finished its send", item.Status)
	}
}

func TestRunCancelsPollsThatOutlastTheGracePeriod(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.ShutdownGrace = 20 * time.Millisecond })
	h.addFeed(feedURL, "")
	h.fetcher.serve(feedURL, fetchStep{body: rss("Example")})

	fetching := make(chan struct{}, 1)
	canceled := make(chan struct{})
	h.fetcher.hook = func(ctx context.Context, _ string) {
		fetching <- struct{}{}
		<-ctx.Done()
		close(canceled)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.syncer.Run(ctx) }()
	<-fetching
	cancel()

	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("the stuck poll was never canceled")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after canceling the stuck poll")
	}
}

func TestScheduleDropWhileBusyDoesNotReschedule(t *testing.T) {
	s := newSchedule()
	s.at(1, start)
	if !s.claim(1) {
		t.Fatal("claim: want the feed claimed")
	}

	s.drop(1)
	s.finish(1, start.Add(time.Hour))

	if _, ok := s.nextPoll(1); ok {
		t.Error("a feed dropped mid-poll was put back on the calendar")
	}
	if n := s.len(); n != 0 {
		t.Errorf("calendar holds %d feeds, want 0", n)
	}
}

func TestScheduleDueClaimsWhatItReturns(t *testing.T) {
	s := newSchedule()
	s.at(1, start)
	s.at(2, start.Add(time.Hour))

	due := s.due(start)
	if len(due) != 1 || due[0] != 1 {
		t.Fatalf("due = %v, want just feed 1", due)
	}
	// Claimed, so a second pass over the same moment does not hand it out
	// again and a manual poll is refused while it runs.
	if again := s.due(start); len(again) != 0 {
		t.Errorf("due = %v, want nothing: feed 1 is already being polled", again)
	}
	if s.claim(1) {
		t.Error("claim: want false while the feed is being polled")
	}
}

// waitFor polls a condition until it holds, which is how these tests wait on
// the scheduler's goroutines without sleeping for a fixed time.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
