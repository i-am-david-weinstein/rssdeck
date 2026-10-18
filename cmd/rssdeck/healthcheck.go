package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// healthcheckTimeout bounds the probe. It is not configurable: this exists for
// Docker's HEALTHCHECK, which has a timeout of its own, and a liveness probe
// that waits half a minute has already answered the question.
const healthcheckTimeout = 5 * time.Second

// healthcheck asks the local instance whether it is serving, and is what
// Docker's HEALTHCHECK runs: the distroless image has no shell and no curl, so
// the binary has to be able to check itself.
//
// It reads the same configuration serve does, so it looks at the address the
// daemon was told to listen on, but it validates nothing. A liveness probe that
// failed because the Readeck token is missing would be reporting on the wrong
// thing.
func healthcheck(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cfg, err := loadConfig(args, stderr)
	if err != nil {
		return err
	}

	target, err := healthURL(cfg.Addr)
	if err != nil {
		return err
	}

	probeCtx, cancel := context.WithTimeout(ctx, healthcheckTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("healthcheck: build request for %s: %w", target, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("healthcheck: %s: %w", target, err)
	}
	defer resp.Body.Close()
	// The body is small and ignored, but it has to be drained for the
	// connection to be reusable, and this process is about to exit anyway.
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: %s returned %d", target, resp.StatusCode)
	}
	fmt.Fprintln(stdout, "ok")
	return nil
}

// healthURL turns a listen address into something fetchable.
//
// A daemon listening on 0.0.0.0 or :: is reachable from inside its own
// container as 127.0.0.1, which is where a healthcheck runs. Dialing the
// wildcard address itself is not portable.
func healthURL(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("healthcheck: %q is not a host:port address: %w", addr, err)
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz", nil
}
