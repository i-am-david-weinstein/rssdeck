package sync

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync" // the standard library's, shadowed by this package's own name
	"time"

	"github.com/i-am-david-weinstein/rssdeck/internal/store"
)

// Run polls feeds until ctx is canceled. It returns when the in-flight polls
// have finished or the shutdown grace period has passed, whichever comes first.
//
// Run is the only caller that schedules anything on a timer; every other poll
// comes through Poll. Both go through the same claim, so a feed is never polled
// twice at once.
func (s *Syncer) Run(ctx context.Context) error {
	if err := s.loadSchedule(ctx); err != nil {
		return err
	}

	// In-flight polls outlive ctx on purpose: a poll that has already sent some
	// of a feed's items should finish the rest and record what it did, rather
	// than being cut off mid-feed by SIGTERM. The grace period is what bounds
	// that, and cancelPolls is the hard stop at the end of it.
	pollCtx, cancelPolls := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelPolls()

	var running sync.WaitGroup
	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()

	s.log.Info("scheduler started",
		"feeds", s.sched.len(), "interval", s.interval, "concurrency", cap(s.slots))

	for {
		select {
		case <-ctx.Done():
			s.log.Info("scheduler stopping", "grace", s.grace)
			s.drain(&running, cancelPolls)
			return nil

		case <-ticker.C:
			for _, feedID := range s.sched.due(s.now()) {
				running.Add(1)
				go func() {
					defer running.Done()
					// poll logs and records every outcome, and a feed that
					// failed is retried on its next turn, so there is nothing
					// left to do with the error here.
					_, _ = s.poll(pollCtx, feedID)
				}()
			}
		}
	}
}

// drain waits for in-flight polls, then cancels them if they outlast the grace
// period. A poll cut off that way has recorded nothing about itself, so the
// feed simply looks un-polled and is picked up again on the next start.
func (s *Syncer) drain(running *sync.WaitGroup, cancelPolls context.CancelFunc) {
	done := make(chan struct{})
	go func() {
		running.Wait()
		close(done)
	}()

	select {
	case <-done:
		return
	case <-time.After(s.grace):
		s.log.Warn("in-flight polls outlasted the shutdown grace period, canceling them", "grace", s.grace)
		cancelPolls()
	}

	// Canceling aborts the HTTP requests the polls are waiting on, so this is a
	// backstop against an unresponsive one holding up the process.
	select {
	case <-done:
	case <-time.After(s.tick + time.Second):
		s.log.Warn("in-flight polls did not stop when canceled")
	}
}

// loadSchedule puts every enabled feed on the calendar at startup.
func (s *Syncer) loadSchedule(ctx context.Context) error {
	feeds, err := s.store.ListFeeds(ctx)
	if err != nil {
		return fmt.Errorf("sync: load schedule: %w", err)
	}
	now := s.now()
	for i := range feeds {
		s.scheduleFeed(&feeds[i], now)
	}
	return nil
}

// Reschedule reloads one feed and puts it on the calendar, or takes it off when
// it is disabled or already gone. The API calls it after a feed is added or
// patched, which is what lets a change take effect without a restart.
func (s *Syncer) Reschedule(ctx context.Context, feedID int64) error {
	f, err := s.store.GetFeed(ctx, feedID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.sched.drop(feedID)
			return nil
		}
		return err
	}
	s.scheduleFeed(f, s.now())
	return nil
}

// Unschedule takes a deleted feed off the calendar. A poll already running for
// it finishes, and nothing reschedules it afterwards.
func (s *Syncer) Unschedule(feedID int64) {
	s.sched.drop(feedID)
}

// scheduleFeed places one feed on the calendar, or removes it when disabled.
func (s *Syncer) scheduleFeed(f *store.Feed, now time.Time) {
	if !f.Enabled {
		s.sched.drop(f.ID)
		return
	}
	at := s.firstPoll(f, now)
	s.sched.at(f.ID, at)
	s.log.Debug("feed scheduled", "feed_id", f.ID, "url", f.URL, "next_poll", at)
}

// firstPoll is when a feed polls for the first time in this process: now plus a
// jitter spread across its interval, so forty feeds do not all fire on boot.
//
// Never earlier than one interval after its last poll, though. Without that
// guard, a container restarting every minute would poll every feed every
// minute. The jitter is drawn fresh on every start, so a feed polls at a
// different offset after a restart, which is fine and deliberate.
func (s *Syncer) firstPoll(f *store.Feed, now time.Time) time.Time {
	interval := f.Interval(s.interval)
	at := now.Add(s.jitter(interval))
	if f.LastPolledAt != nil {
		if earliest := f.LastPolledAt.Add(interval); earliest.After(at) {
			return earliest
		}
	}
	return at
}

// defaultJitter spreads a feed's first poll uniformly across its interval.
func defaultJitter(interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	return rand.N(interval)
}

// schedule is the in-memory poll calendar: when each feed is next due, and
// which feeds are being polled right now. Nothing here is persisted, so adding
// or removing a feed cannot disturb the timing of the others.
type schedule struct {
	mu    sync.Mutex
	feeds map[int64]*feedSchedule
}

// feedSchedule is one feed's entry. An entry can be busy without being
// scheduled, which is a feed polled by hand, or scheduled without being busy,
// which is the normal state. An entry that is neither is deleted.
type feedSchedule struct {
	next      time.Time
	scheduled bool
	busy      bool
}

func newSchedule() *schedule {
	return &schedule{feeds: make(map[int64]*feedSchedule)}
}

// at schedules a feed's next poll.
func (s *schedule) at(feedID int64, next time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.feeds[feedID]
	if entry == nil {
		entry = &feedSchedule{}
		s.feeds[feedID] = entry
	}
	entry.scheduled = true
	entry.next = next
}

// drop takes a feed off the calendar. A feed being polled right now keeps its
// entry until the poll finishes, so the poll still releases cleanly, but it is
// not scheduled again.
func (s *schedule) drop(feedID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.feeds[feedID]
	if entry == nil {
		return
	}
	entry.scheduled = false
	if !entry.busy {
		delete(s.feeds, feedID)
	}
}

// claim marks a feed as being polled, reporting false when a poll is already
// running for it. It works for feeds that are not on the calendar at all, which
// is how a disabled feed can still be polled on request.
func (s *schedule) claim(feedID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.feeds[feedID]
	if entry == nil {
		s.feeds[feedID] = &feedSchedule{busy: true}
		return true
	}
	if entry.busy {
		return false
	}
	entry.busy = true
	return true
}

// finish releases a claim and sets the feed's next poll. A feed dropped while
// it was being polled is forgotten here instead.
func (s *schedule) finish(feedID int64, next time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.feeds[feedID]
	if entry == nil {
		return
	}
	entry.busy = false
	if !entry.scheduled {
		delete(s.feeds, feedID)
		return
	}
	entry.next = next
}

// due claims and returns the feeds that are ready to poll, which is every
// scheduled feed whose time has come and that is not already being polled.
func (s *schedule) due(now time.Time) []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ready []int64
	for feedID, entry := range s.feeds {
		if entry.scheduled && !entry.busy && !entry.next.After(now) {
			entry.busy = true
			ready = append(ready, feedID)
		}
	}
	return ready
}

// len is how many feeds are on the calendar, for the startup log line.
func (s *schedule) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, entry := range s.feeds {
		if entry.scheduled {
			n++
		}
	}
	return n
}

// nextPoll reports when a feed is due. Tests use it to check the calendar
// without reaching into the map.
func (s *schedule) nextPoll(feedID int64) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.feeds[feedID]
	if entry == nil || !entry.scheduled {
		return time.Time{}, false
	}
	return entry.next, true
}
