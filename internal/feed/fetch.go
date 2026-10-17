// Package feed fetches and parses RSS, Atom and JSON feeds, and decides what
// identifies an item inside its feed.
//
// Fetching and parsing are separate calls because a conditional request that
// comes back 304 must end the poll before any parsing, and because parsing is
// then testable against a corpus of files with no HTTP involved.
package feed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// DefaultMaxBodySize bounds a feed response. Feeds are tens of kilobytes;
// anything near this is a misconfigured endpoint, and reading it unbounded
// would trade a failed poll for a dead container.
const DefaultMaxBodySize = 10 << 20

// accept nudges servers that content-negotiate towards a feed, while still
// taking whatever they send.
const accept = "application/atom+xml, application/rss+xml, application/feed+json;q=0.9, " +
	"application/xml;q=0.8, text/xml;q=0.8, */*;q=0.5"

// ErrTooLarge means the response exceeded the size limit and was not read to the
// end. It is reported rather than truncated: a truncated document parses as a
// feed that has lost its trailing items, which retention would then treat as
// items the feed no longer carries.
var ErrTooLarge = errors.New("feed response exceeds the size limit")

// StatusError is a feed server answering with anything other than success or
// 304. It carries the code so a caller can tell a 404 from a 503 in a log line.
type StatusError struct {
	URL    string
	Status int
}

func (e *StatusError) Error() string {
	if text := http.StatusText(e.Status); text != "" {
		return fmt.Sprintf("feed: %s returned %d %s", e.URL, e.Status, text)
	}
	return fmt.Sprintf("feed: %s returned %d", e.URL, e.Status)
}

// UserAgent builds the User-Agent sent to feed servers. An operator reading
// their access log can see who is polling them and where to complain.
func UserAgent(version string) string {
	if version == "" {
		version = "dev"
	}
	return "rssdeck/" + version + " (+https://github.com/i-am-david-weinstein/rssdeck)"
}

// Fetcher performs conditional GETs against feed URLs. It holds no per-feed
// state, so one instance serves every concurrent poll.
type Fetcher struct {
	client      *http.Client
	userAgent   string
	maxBodySize int64
}

// NewFetcher returns a Fetcher using client, whose timeout bounds each fetch. A
// nil client, an empty user agent or a non-positive size limit fall back to
// defaults.
func NewFetcher(client *http.Client, userAgent string, maxBodySize int64) *Fetcher {
	if client == nil {
		client = http.DefaultClient
	}
	if userAgent == "" {
		userAgent = UserAgent("")
	}
	if maxBodySize <= 0 {
		maxBodySize = DefaultMaxBodySize
	}
	return &Fetcher{client: client, userAgent: userAgent, maxBodySize: maxBodySize}
}

// Conditional carries the cache validators stored from the last successful
// fetch of a feed. Empty fields are simply not sent.
type Conditional struct {
	ETag         string
	LastModified string
}

// Response is one fetch. On NotModified the feed served nothing and Body is
// empty, which ends the poll: no parse, and no retention pass, since a 304 says
// nothing about which items the feed still carries.
type Response struct {
	NotModified  bool
	ETag         string
	LastModified string
	Body         []byte
}

// Fetch requests url, replaying c as If-None-Match and If-Modified-Since.
//
// A 304 returns a Response with NotModified set. Any other non-success status is
// a *StatusError, and a response over the size limit is ErrTooLarge. Both are
// errors the caller records against the feed rather than retrying in place.
func (f *Fetcher) Fetch(ctx context.Context, url string, c Conditional) (*Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("feed: build request for %s: %w", url, err)
	}
	req.Header.Set("User-Agent", f.userAgent)
	req.Header.Set("Accept", accept)
	if c.ETag != "" {
		req.Header.Set("If-None-Match", c.ETag)
	}
	if c.LastModified != "" {
		req.Header.Set("If-Modified-Since", c.LastModified)
	}

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("feed: fetch %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return &Response{NotModified: true}, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &StatusError{URL: url, Status: resp.StatusCode}
	}

	// Read one byte past the limit so an oversized body is detected instead of
	// silently truncated.
	body, err := io.ReadAll(io.LimitReader(resp.Body, f.maxBodySize+1))
	if err != nil {
		return nil, fmt.Errorf("feed: read %s: %w", url, err)
	}
	if int64(len(body)) > f.maxBodySize {
		return nil, fmt.Errorf("%w: %s is over %d bytes", ErrTooLarge, url, f.maxBodySize)
	}

	return &Response{
		ETag:         strings.TrimSpace(resp.Header.Get("ETag")),
		LastModified: strings.TrimSpace(resp.Header.Get("Last-Modified")),
		Body:         body,
	}, nil
}
