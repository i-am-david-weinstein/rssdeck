package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/i-am-david-weinstein/rssdeck/internal/api"
	"github.com/i-am-david-weinstein/rssdeck/internal/config"
	"github.com/i-am-david-weinstein/rssdeck/internal/feed"
	"github.com/i-am-david-weinstein/rssdeck/internal/readeck"
	"github.com/i-am-david-weinstein/rssdeck/internal/store"
	rssdecksync "github.com/i-am-david-weinstein/rssdeck/internal/sync"
)

// Timeouts the daemon sets for itself rather than from configuration.
const (
	// readHeaderTimeout bounds how long a client may take to send its request
	// headers. Without it, an idle connection holds a goroutine indefinitely.
	readHeaderTimeout = 10 * time.Second

	// drainTimeout bounds the HTTP shutdown, which happens after the polls
	// have been given their own grace period.
	drainTimeout = 30 * time.Second
)

// loadConfig resolves settings for a subcommand.
func loadConfig(args []string, output io.Writer) (*config.Config, error) {
	return config.Load(args, nil, output)
}

// serve runs the daemon: the poll scheduler and the HTTP API, over one database.
//
// Startup is deliberately strict. Settings are validated, the database is
// opened and migrated, and Readeck is probed once with the real token before
// anything is served. A wrong token that surfaced hours later as items which
// will not send is the failure this ordering exists to prevent.
func serve(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cfg, err := loadConfig(args, stderr)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	log := cfg.Logger(stdout)
	log.Info("rssdeck starting", append([]any{"version", version}, cfg.LogAttrs()...)...)
	if cfg.Impolite() {
		log.Warn("the global poll interval is faster than once a minute, which feed servers may not welcome",
			"poll_interval", cfg.PollInterval)
	}

	st, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	// The database is closed last, after the scheduler and the API have both
	// stopped using it.
	defer func() {
		if err := st.Close(); err != nil {
			log.Error("could not close the database", "error", err)
		}
	}()
	log.Info("database ready", "path", cfg.DBPath)

	client := &http.Client{Timeout: cfg.HTTPTimeout}
	userAgent := feed.UserAgent(version)

	bookmarks, err := readeck.New(cfg.ReadeckURL, cfg.ReadeckToken, client, userAgent)
	if err != nil {
		return err
	}
	if err := probeReadeck(ctx, bookmarks, cfg.HTTPTimeout); err != nil {
		return err
	}
	log.Info("readeck reachable", "url", bookmarks.BaseURL())

	syncer, err := rssdecksync.New(rssdecksync.Options{
		Store:       st,
		Fetcher:     feed.NewFetcher(client, userAgent, cfg.MaxBodySize),
		Readeck:     bookmarks,
		Logger:      log,
		Interval:    cfg.PollInterval,
		Concurrency: cfg.PollConcurrency,
		MaxFailures: cfg.MaxFailures,
		Backfill:    cfg.Backfill,
		GlobalLabel: cfg.GlobalLabel,
	})
	if err != nil {
		return err
	}

	handler, err := api.New(api.Options{
		Store:       st,
		Syncer:      syncer,
		Logger:      log,
		MasterToken: cfg.APIToken,
	})
	if err != nil {
		return err
	}

	// Listening before the scheduler starts means a port already in use fails
	// the boot, rather than leaving a daemon that polls feeds and answers
	// nobody.
	var listenCfg net.ListenConfig
	listener, err := listenCfg.Listen(ctx, "tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Addr, err)
	}

	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
	}
	return runUntilSignal(ctx, log, syncer, server, listener)
}

// runUntilSignal runs the scheduler and the API until the context is canceled or
// one of them fails, then shuts both down in order.
//
// The order on the way down is the design's: stop scheduling, let in-flight
// polls finish, drain the HTTP server, and only then close the database, which
// the caller's deferred Close does. A poll that has already sent some of a
// feed's items should finish the rest and record what it did rather than be cut
// off mid-feed.
func runUntilSignal(
	ctx context.Context,
	log *slog.Logger,
	syncer *rssdecksync.Syncer,
	server *http.Server,
	listener net.Listener,
) error {
	// A child context so a failure on either side stops the other, without
	// reaching back into the signal context the caller owns.
	runCtx, stop := context.WithCancel(ctx)
	defer stop()

	scheduler := make(chan error, 1)
	go func() { scheduler <- syncer.Run(runCtx) }()

	// Named for what it reports rather than for the package, which it would
	// otherwise shadow.
	serving := make(chan error, 1)
	go func() { serving <- server.Serve(listener) }()
	log.Info("api listening", "addr", listener.Addr().String())

	// Which side has already reported, so the waits below never block on a
	// channel that has nothing left to deliver.
	var (
		runErr           error
		schedulerStopped bool
		servingStopped   bool
	)
	select {
	case <-ctx.Done():
		// A signal is what cancels this context in production, and
		// signal.NotifyContext does not record which one, so there is nothing
		// more to say here than that it arrived.
		log.Info("shutting down")
	case err := <-scheduler:
		// Run only returns early when it could not load the schedule, which
		// means the database is unusable.
		schedulerStopped = true
		runErr = err
		log.Error("scheduler stopped", "error", err)
	case err := <-serving:
		servingStopped = true
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = fmt.Errorf("serve http: %w", err)
			log.Error("api stopped", "error", err)
		}
	}

	stop()
	// Run drains in-flight polls under its own grace period before returning,
	// so waiting here is waiting for the polls.
	if !schedulerStopped {
		if err := <-scheduler; err != nil && runErr == nil {
			runErr = err
		}
	}

	if !servingStopped {
		drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainTimeout)
		defer cancel()
		if err := server.Shutdown(drainCtx); err != nil {
			log.Warn("the api did not drain cleanly", "error", err)
		}
		// Shutdown makes Serve return, which is what this waits for. A clean
		// drain reports ErrServerClosed and means nothing; anything else is
		// worth a line, but not worth failing a shutdown that is already
		// underway.
		if err := <-serving; err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Warn("the api did not stop cleanly", "error", err)
		}
	}

	log.Info("stopped")
	return runErr
}

// probeReadeck makes the one authenticated request that decides whether the
// daemon is configured correctly, and explains what a failure means.
func probeReadeck(ctx context.Context, bookmarks *readeck.Client, timeout time.Duration) error {
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	err := bookmarks.Probe(probeCtx)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, readeck.ErrUnauthorized):
		return fmt.Errorf("readeck rejected the token at %s; check RSSDECK_READECK_TOKEN: %w",
			bookmarks.BaseURL(), err)
	default:
		return fmt.Errorf("readeck at %s could not be reached; check RSSDECK_READECK_URL: %w",
			bookmarks.BaseURL(), err)
	}
}
