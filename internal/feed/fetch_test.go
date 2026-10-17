package feed

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newFetcher(t *testing.T, server *httptest.Server, maxBodySize int64) *Fetcher {
	t.Helper()
	return NewFetcher(server.Client(), UserAgent("1.2.3"), maxBodySize)
}

func TestFetchSendsItsIdentityAndAcceptsFeeds(t *testing.T) {
	var got *http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		w.Write([]byte("<rss/>"))
	}))
	defer server.Close()

	if _, err := newFetcher(t, server, 0).Fetch(context.Background(), server.URL, Conditional{}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if ua := got.Header.Get("User-Agent"); ua != "rssdeck/1.2.3 (+https://github.com/i-am-david-weinstein/rssdeck)" {
		t.Errorf("User-Agent = %q", ua)
	}
	if accept := got.Header.Get("Accept"); !strings.Contains(accept, "application/rss+xml") {
		t.Errorf("Accept = %q, want it to mention rss", accept)
	}
	// Nothing was stored yet, so no validators go out.
	if v := got.Header.Get("If-None-Match"); v != "" {
		t.Errorf("If-None-Match = %q, want it absent", v)
	}
	if v := got.Header.Get("If-Modified-Since"); v != "" {
		t.Errorf("If-Modified-Since = %q, want it absent", v)
	}
}

func TestFetchReplaysValidators(t *testing.T) {
	const (
		etag    = `W/"abc123"`
		lastMod = "Thu, 16 Oct 2026 12:00:00 GMT"
	)
	var got *http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		w.Header().Set("ETag", etag)
		w.Header().Set("Last-Modified", lastMod)
		w.Write([]byte("<rss/>"))
	}))
	defer server.Close()

	res, err := newFetcher(t, server, 0).Fetch(context.Background(), server.URL,
		Conditional{ETag: etag, LastModified: lastMod})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if v := got.Header.Get("If-None-Match"); v != etag {
		t.Errorf("If-None-Match = %q, want %q", v, etag)
	}
	if v := got.Header.Get("If-Modified-Since"); v != lastMod {
		t.Errorf("If-Modified-Since = %q, want %q", v, lastMod)
	}
	if res.ETag != etag {
		t.Errorf("returned etag = %q, want %q", res.ETag, etag)
	}
	if res.LastModified != lastMod {
		t.Errorf("returned last-modified = %q, want %q", res.LastModified, lastMod)
	}
	if res.NotModified {
		t.Error("NotModified is set on a 200")
	}
}

// A 304 is the common case for a feed that has not changed, and it must cost
// nothing: no body, and a signal the caller can use to end the poll.
func TestFetchNotModified(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `W/"abc123"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Write([]byte("<rss/>"))
	}))
	defer server.Close()

	res, err := newFetcher(t, server, 0).Fetch(context.Background(), server.URL,
		Conditional{ETag: `W/"abc123"`})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !res.NotModified {
		t.Error("NotModified is not set on a 304")
	}
	if len(res.Body) != 0 {
		t.Errorf("body is %d bytes, want none on a 304", len(res.Body))
	}
}

func TestFetchStatusErrors(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 410, 429, 500, 503, 599} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
			}))
			defer server.Close()

			_, err := newFetcher(t, server, 0).Fetch(context.Background(), server.URL, Conditional{})
			if err == nil {
				t.Fatalf("Fetch returned no error for %d", status)
			}
			var statusErr *StatusError
			if !errors.As(err, &statusErr) {
				t.Fatalf("error = %v (%T), want a *StatusError", err, err)
			}
			if statusErr.Status != status {
				t.Errorf("Status = %d, want %d", statusErr.Status, status)
			}
			if !strings.Contains(err.Error(), fmt.Sprint(status)) {
				t.Errorf("error = %v, want it to name the status", err)
			}
		})
	}
}

// A 2xx that is not 200 is read and left to the parser to reject, rather than
// being special-cased here.
func TestFetchAcceptsOtherSuccessCodes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte("<rss/>"))
	}))
	defer server.Close()

	res, err := newFetcher(t, server, 0).Fetch(context.Background(), server.URL, Conditional{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(res.Body) != "<rss/>" {
		t.Errorf("body = %q", res.Body)
	}
}

// An oversized body is reported, never truncated. A truncated document parses as
// a feed missing its trailing items, and retention would then delete the rows
// for those items as though the feed had dropped them.
func TestFetchRejectsAnOversizedBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("x", 2048)))
	}))
	defer server.Close()

	_, err := newFetcher(t, server, 1024).Fetch(context.Background(), server.URL, Conditional{})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("error = %v, want ErrTooLarge", err)
	}
	if !strings.Contains(err.Error(), "1024") {
		t.Errorf("error = %v, want it to name the limit", err)
	}
}

func TestFetchAcceptsABodyExactlyAtTheLimit(t *testing.T) {
	const size = 1024
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("x", size)))
	}))
	defer server.Close()

	res, err := newFetcher(t, server, size).Fetch(context.Background(), server.URL, Conditional{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(res.Body) != size {
		t.Errorf("body is %d bytes, want %d", len(res.Body), size)
	}
}

func TestFetchTransportFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := server.URL
	server.Close() // nothing is listening now

	_, err := NewFetcher(nil, UserAgent("1.2.3"), 0).Fetch(context.Background(), url, Conditional{})
	if err == nil {
		t.Fatal("Fetch succeeded against a closed server")
	}
	var statusErr *StatusError
	if errors.As(err, &statusErr) {
		t.Errorf("error = %v, want a transport error rather than a *StatusError", err)
	}
	if !strings.Contains(err.Error(), url) {
		t.Errorf("error = %v, want it to name the url", err)
	}
}

func TestFetchHonorsContextCancellation(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer func() {
		close(release)
		server.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := newFetcher(t, server, 0).Fetch(ctx, server.URL, Conditional{})
	if err == nil {
		t.Fatal("Fetch succeeded against a server that never responded")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want it to wrap context.DeadlineExceeded", err)
	}
}

func TestFetchFollowsRedirects(t *testing.T) {
	var feed *httptest.Server
	feed = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/feed.xml" {
			w.Write([]byte("<rss/>"))
			return
		}
		http.Redirect(w, r, feed.URL+"/feed.xml", http.StatusMovedPermanently)
	}))
	defer feed.Close()

	res, err := newFetcher(t, feed, 0).Fetch(context.Background(), feed.URL+"/old", Conditional{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(res.Body) != "<rss/>" {
		t.Errorf("body = %q, want the redirect to have been followed", res.Body)
	}
}

func TestFetchRejectsABadURL(t *testing.T) {
	_, err := NewFetcher(nil, "", 0).Fetch(context.Background(), "://nonsense", Conditional{})
	if err == nil {
		t.Fatal("Fetch succeeded on an unparseable url")
	}
}

func TestNewFetcherDefaults(t *testing.T) {
	f := NewFetcher(nil, "", 0)
	if f.client == nil {
		t.Error("client is nil")
	}
	if f.userAgent != UserAgent("") {
		t.Errorf("userAgent = %q, want the default", f.userAgent)
	}
	if f.maxBodySize != DefaultMaxBodySize {
		t.Errorf("maxBodySize = %d, want %d", f.maxBodySize, DefaultMaxBodySize)
	}
	if got := NewFetcher(nil, "", -1).maxBodySize; got != DefaultMaxBodySize {
		t.Errorf("maxBodySize = %d for a negative limit, want %d", got, DefaultMaxBodySize)
	}
}

func TestUserAgent(t *testing.T) {
	const url = "+https://github.com/i-am-david-weinstein/rssdeck"
	if got := UserAgent("0.1.0"); got != "rssdeck/0.1.0 ("+url+")" {
		t.Errorf("UserAgent = %q", got)
	}
	if got := UserAgent(""); got != "rssdeck/dev ("+url+")" {
		t.Errorf("UserAgent(\"\") = %q, want a dev version", got)
	}
}

// Fetch and Parse are separate calls, but the pair is what a poll actually does.
func TestFetchThenParse(t *testing.T) {
	body := readFixture(t, "wordpress_rss2.xml")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Header().Set("ETag", `"v1"`)
		w.Write(body)
	}))
	defer server.Close()

	res, err := newFetcher(t, server, 0).Fetch(context.Background(), server.URL+"/feed/", Conditional{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	doc, err := Parse(res.Body, server.URL+"/feed/")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if doc.Title != "Örjan’s Notebook" {
		t.Errorf("title = %q", doc.Title)
	}
	if len(doc.Items) != 2 {
		t.Fatalf("got %d items, want 2", len(doc.Items))
	}
	if res.ETag != `"v1"` {
		t.Errorf("etag = %q", res.ETag)
	}
}

// The fetcher holds no per-feed state, so the scheduler can share one across
// concurrent polls.
func TestFetcherIsSafeForConcurrentUse(t *testing.T) {
	body := readFixture(t, "wordpress_rss2.xml")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer server.Close()

	fetcher := newFetcher(t, server, 0)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			res, err := fetcher.Fetch(context.Background(), server.URL, Conditional{})
			if err != nil {
				errs <- err
				return
			}
			_, err = Parse(res.Body, server.URL)
			errs <- err
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent fetch and parse: %v", err)
		}
	}
}

// A connection that drops mid-body must fail the poll. The bytes that did
// arrive often parse as a feed that has lost its trailing items, and treating
// that as the feed's current contents would let retention delete the rows for
// items the feed still carries.
func TestFetchRejectsATruncatedBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		// Promise far more than is sent, then hang up.
		buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 4096\r\n\r\n")
		buf.WriteString(`<?xml version="1.0"?><rss version="2.0"><channel><title>Cut off`)
		buf.Flush()
	}))
	defer server.Close()

	_, err := newFetcher(t, server, 0).Fetch(context.Background(), server.URL, Conditional{})
	if err == nil {
		t.Fatal("Fetch succeeded on a truncated response")
	}
	if !strings.Contains(err.Error(), server.URL) {
		t.Errorf("error = %v, want it to name the url", err)
	}
}
