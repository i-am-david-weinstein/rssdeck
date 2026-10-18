package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeReadeck is a Readeck instance: it answers the startup probe and records
// the bookmarks it is asked to create.
type fakeReadeck struct {
	*httptest.Server

	mu sync.Mutex

	// probeStatus is what the probe's GET answers, so a test can make the
	// token look wrong.
	probeStatus int

	created []string
	labels  [][]string
	tokens  []string
}

func newFakeReadeck(t *testing.T) *fakeReadeck {
	t.Helper()
	r := &fakeReadeck{probeStatus: http.StatusOK}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.tokens = append(r.tokens, req.Header.Get("Authorization"))

		switch req.Method {
		case http.MethodGet:
			// The startup probe.
			if r.probeStatus != http.StatusOK {
				w.WriteHeader(r.probeStatus)
				return
			}
			_, _ = w.Write([]byte(`[]`))

		case http.MethodPost:
			var body struct {
				URL    string   `json:"url"`
				Labels []string `json:"labels"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			r.created = append(r.created, body.URL)
			r.labels = append(r.labels, body.Labels)
			// Readeck accepts a bookmark with 202 and names it in a header.
			w.Header().Set("Bookmark-Id", fmt.Sprintf("bookmark-%d", len(r.created)))
			w.WriteHeader(http.StatusAccepted)

		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *fakeReadeck) bookmarks() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.created...)
}

// labelsFor is the labels sent with the nth bookmark.
func (r *fakeReadeck) labelsFor(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n >= len(r.labels) {
		return nil
	}
	return append([]string(nil), r.labels[n]...)
}

// newFeedServer serves one RSS document with two items, newest last, the way a
// real feed often does.
func newFeedServer(t *testing.T) *httptest.Server {
	t.Helper()
	const document = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Integration feed</title>
    <link>https://feed.example</link>
    <item>
      <title>Older post</title>
      <link>https://feed.example/older</link>
      <guid>older</guid>
      <pubDate>Mon, 12 Oct 2026 10:00:00 GMT</pubDate>
    </item>
    <item>
      <title>Newest post</title>
      <link>https://feed.example/newest</link>
      <guid>newest</guid>
      <pubDate>Fri, 16 Oct 2026 10:00:00 GMT</pubDate>
    </item>
  </channel>
</rss>`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write([]byte(document))
	}))
	t.Cleanup(server.Close)
	return server
}

// serveArgs is a complete configuration for a test daemon, as flags.
func serveArgs(addr, dbPath, readeckURL string, extra ...string) []string {
	args := []string{
		"-addr", addr,
		"-db-path", dbPath,
		"-api-token", "test-master-token",
		"-readeck-url", readeckURL,
		"-readeck-token", "readeck-test-token",
		// Long enough that the scheduler never polls on its own during a test:
		// what the test drives is the add and the explicit poll.
		"-poll-interval", "1h",
		"-log-level", "error",
	}
	return append(args, extra...)
}

// start runs serve in the background and waits until it answers /healthz,
// returning a function that shuts it down and reports what serve returned.
func start(t *testing.T, args []string) (addr string, stop func() error) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, args, io.Discard, io.Discard) }()

	// The address is in the arguments, two positions after -addr.
	for i, arg := range args {
		if arg == "-addr" && i+1 < len(args) {
			addr = args[i+1]
		}
	}

	waitForHealth(t, addr, done)
	return addr, func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(30 * time.Second):
			return fmt.Errorf("serve did not return after its context was canceled")
		}
	}
}

// waitForHealth blocks until the daemon is serving, or fails the test. It
// watches serve's own result too, so a boot failure is reported as itself
// rather than as a timeout.
func waitForHealth(t *testing.T, addr string, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case err := <-done:
			t.Fatalf("serve returned before it was listening: %v", err)
		default:
		}

		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon at %s never became healthy: %v", addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// request sends an authenticated API request to the running daemon.
func request(t *testing.T, method, url, body string) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, url, err)
	}
	req.Header.Set("Authorization", "Bearer test-master-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

// The wiring is the subject here: config, store, fetcher, Readeck client,
// scheduler and API are separate packages with their own tests, and this is the
// only place that proves they are plugged into each other.
func TestServeAddsAFeedAndSendsItsBackfill(t *testing.T) {
	isolateEnv(t)
	readeck := newFakeReadeck(t)
	feedServer := newFeedServer(t)
	dbPath := filepath.Join(t.TempDir(), "state", "rssdeck.db")

	addr, stop := start(t, serveArgs(freeAddr(t), dbPath, readeck.URL))
	base := "http://" + addr

	resp := request(t, http.MethodPost, base+"/api/feeds", `{"url":"`+feedServer.URL+`"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /api/feeds = %d: %s", resp.StatusCode, body)
	}
	var feed struct {
		ID    int64  `json:"id"`
		Title string `json:"title"`
		Label string `json:"label"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&feed); err != nil {
		t.Fatalf("decode feed: %v", err)
	}
	if feed.Title != "Integration feed" {
		t.Errorf("title = %q, want the parsed feed title", feed.Title)
	}
	if feed.Label != "Integration feed" {
		t.Errorf("label = %q, want it defaulted from the title", feed.Label)
	}

	// The default backfill is one item, so only the newest reached Readeck and
	// the rest of the document was seeded.
	bookmarks := readeck.bookmarks()
	if len(bookmarks) != 1 || bookmarks[0] != "https://feed.example/newest" {
		t.Fatalf("readeck received %v, want just the newest item", bookmarks)
	}

	// The feed's own label and the global one both reach Readeck, which is the
	// one path that proves RSSDECK_GLOBAL_LABEL is wired through.
	if labels := readeck.labelsFor(0); !slices.Equal(labels, []string{"Integration feed", "rssdeck"}) {
		t.Errorf("bookmark labels = %v, want the feed label and the global one", labels)
	}

	items := request(t, http.MethodGet, base+"/api/items", "")
	defer items.Body.Close()
	var page struct {
		Items []struct {
			GUID              string `json:"guid"`
			Status            string `json:"status"`
			ReadeckBookmarkID string `json:"readeck_bookmark_id"`
		} `json:"items"`
		Total int `json:"total"`
	}
	if err := json.NewDecoder(items.Body).Decode(&page); err != nil {
		t.Fatalf("decode items: %v", err)
	}
	if page.Total != 2 {
		t.Fatalf("total = %d, want both items recorded", page.Total)
	}
	status := map[string]string{}
	bookmarkIDs := map[string]string{}
	for _, item := range page.Items {
		status[item.GUID] = item.Status
		bookmarkIDs[item.GUID] = item.ReadeckBookmarkID
	}
	if status["newest"] != "sent" {
		t.Errorf("newest item is %q, want sent", status["newest"])
	}
	if bookmarkIDs["newest"] == "" {
		t.Error("the sent item has no Readeck bookmark id, so the response header was not stored")
	}
	if status["older"] != "seeded" {
		t.Errorf("older item is %q, want seeded", status["older"])
	}

	// A second poll finds nothing new, which is the once-only guarantee holding
	// across the whole daemon rather than inside one package's tests.
	poll := request(t, http.MethodPost, fmt.Sprintf("%s/api/feeds/%d/poll", base, feed.ID), "")
	defer poll.Body.Close()
	var result struct {
		New      int `json:"new"`
		Existing int `json:"existing"`
		Sent     int `json:"sent"`
	}
	if err := json.NewDecoder(poll.Body).Decode(&result); err != nil {
		t.Fatalf("decode poll result: %v", err)
	}
	if result.New != 0 || result.Existing != 2 || result.Sent != 0 {
		t.Errorf("second poll = %+v, want nothing new and nothing sent", result)
	}
	if got := readeck.bookmarks(); len(got) != 1 {
		t.Errorf("readeck received %d bookmarks after a second poll, want 1", len(got))
	}

	if err := stop(); err != nil {
		t.Fatalf("serve returned %v, want a clean shutdown", err)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("the database is not at %s: %v", dbPath, err)
	}
}

// A cancelled context is SIGTERM, and the daemon is expected to stop cleanly
// without the operator seeing an error.
func TestServeShutsDownCleanlyWithNoWork(t *testing.T) {
	isolateEnv(t)
	readeck := newFakeReadeck(t)
	dbPath := filepath.Join(t.TempDir(), "rssdeck.db")

	_, stop := start(t, serveArgs(freeAddr(t), dbPath, readeck.URL))
	if err := stop(); err != nil {
		t.Fatalf("serve returned %v, want nil", err)
	}
}

// The probe exists so a bad token fails the boot instead of surfacing hours
// later as items that will not send.
func TestServeFailsWhenReadeckRejectsTheToken(t *testing.T) {
	isolateEnv(t)
	readeck := newFakeReadeck(t)
	readeck.probeStatus = http.StatusUnauthorized
	dbPath := filepath.Join(t.TempDir(), "rssdeck.db")

	err := serve(context.Background(), serveArgs(freeAddr(t), dbPath, readeck.URL), io.Discard, io.Discard)
	if err == nil {
		t.Fatal("serve started with a token Readeck rejects")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("error does not point at the token: %v", err)
	}
}

func TestServeFailsWhenReadeckIsUnreachable(t *testing.T) {
	isolateEnv(t)
	dbPath := filepath.Join(t.TempDir(), "rssdeck.db")

	args := serveArgs(freeAddr(t), dbPath, "http://"+freeAddr(t), "-http-timeout", "2s")
	err := serve(context.Background(), args, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("serve started with no Readeck to talk to")
	}
	if !strings.Contains(err.Error(), "READECK_URL") {
		t.Errorf("error does not point at the URL: %v", err)
	}
}

// Listening happens before the scheduler starts, so a port already in use is a
// boot failure rather than a daemon that polls feeds and answers nobody.
func TestServeFailsWhenTheAddressIsTaken(t *testing.T) {
	isolateEnv(t)
	readeck := newFakeReadeck(t)
	occupied := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer occupied.Close()
	dbPath := filepath.Join(t.TempDir(), "rssdeck.db")

	addr := strings.TrimPrefix(occupied.URL, "http://")
	err := serve(context.Background(), serveArgs(addr, dbPath, readeck.URL), io.Discard, io.Discard)
	if err == nil {
		t.Fatal("serve started on an address already in use")
	}
	if !strings.Contains(err.Error(), "listen") {
		t.Errorf("error does not say what failed: %v", err)
	}
}

func TestServeRefusesToStartWithoutItsRequiredSettings(t *testing.T) {
	isolateEnv(t)
	err := serve(context.Background(), []string{"-db-path", filepath.Join(t.TempDir(), "rssdeck.db")},
		io.Discard, io.Discard)
	if err == nil {
		t.Fatal("serve started with no API token and no Readeck instance")
	}
	for _, want := range []string{"API_TOKEN", "READECK_URL", "READECK_TOKEN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}
