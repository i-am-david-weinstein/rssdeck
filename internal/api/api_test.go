package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/i-am-david-weinstein/rssdeck/internal/store"
	rssdecksync "github.com/i-am-david-weinstein/rssdeck/internal/sync"
)

// masterToken is the configured token in every test. Minted tokens are created
// through the API, so they are never hard-coded here.
const masterToken = "master-token-for-tests"

// start is the fixed time the test clock begins at, so nothing in these tests
// depends on how long one takes to run.
var start = time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)

// env is one server under test with the store and syncer behind it.
type env struct {
	t      *testing.T
	server *Server
	store  *store.Store
	syncer *fakeSyncer
	now    time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	e := &env{t: t, store: st, now: start}
	e.syncer = &fakeSyncer{store: st, now: func() time.Time { return e.now }}

	server, err := New(Options{
		Store:       st,
		Syncer:      e.syncer,
		MasterToken: masterToken,
		now:         func() time.Time { return e.now },
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	e.server = server
	return e
}

// do sends a request as the given token. An empty token sends no Authorization
// header. body is sent as JSON unless it is a string, which is sent as is, so a
// test can send something that is not valid JSON.
func (e *env) do(method, path, token string, body any) *httptest.ResponseRecorder {
	e.t.Helper()

	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(b)
	default:
		encoded, err := json.Marshal(b)
		if err != nil {
			e.t.Fatalf("encode request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	e.server.ServeHTTP(rec, req)
	return rec
}

// requestFor builds a bodyless request, for the tests that set the
// Authorization header themselves.
func requestFor(method, path string) *http.Request {
	return httptest.NewRequest(method, path, nil)
}

// serve runs one prepared request through the server.
func (e *env) serve(req *http.Request) *httptest.ResponseRecorder {
	e.t.Helper()
	rec := httptest.NewRecorder()
	e.server.ServeHTTP(rec, req)
	return rec
}

// itoa renders an id for a request path.
func itoa(id int64) string {
	return strconv.FormatInt(id, 10)
}

// expectStatus fails unless the response has the wanted status, reporting the
// body when it does not: an unexpected error envelope is what explains why.
func (e *env) expectStatus(rec *httptest.ResponseRecorder, want int) {
	e.t.Helper()
	if rec.Code != want {
		e.t.Fatalf("status = %d, want %d; body: %s", rec.Code, want, rec.Body.String())
	}
}

// expectError fails unless the response is the error envelope with this code.
func (e *env) expectError(rec *httptest.ResponseRecorder, status int, code string) {
	e.t.Helper()
	e.expectStatus(rec, status)
	var body errorBody
	e.decode(rec, &body)
	if body.Code != code {
		e.t.Fatalf("error code = %q, want %q; body: %s", body.Code, code, rec.Body.String())
	}
	if body.Error == "" {
		e.t.Error("error message is empty; the envelope always carries one for humans")
	}
}

func (e *env) decode(rec *httptest.ResponseRecorder, v any) {
	e.t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		e.t.Fatalf("decode response: %v; body: %s", err, rec.Body.String())
	}
}

// addFeed subscribes a feed through the API and returns it, for the tests whose
// subject is something else.
func (e *env) addFeed(url string) feedJSON {
	e.t.Helper()
	rec := e.do(http.MethodPost, "/api/feeds", masterToken, map[string]any{"url": url})
	e.expectStatus(rec, http.StatusCreated)
	var feed feedJSON
	e.decode(rec, &feed)
	return feed
}

// ingest records items for a feed, for the tests whose subject is reading them
// back rather than discovering them.
func (e *env) ingest(feedID int64, items ...store.NewItem) {
	e.t.Helper()
	for i := range items {
		items[i].FeedID = feedID
	}
	if _, err := e.store.IngestItems(context.Background(), items); err != nil {
		e.t.Fatalf("ingest items: %v", err)
	}
}

// mintToken creates a token through the API and returns its plaintext.
func (e *env) mintToken(name string, expires *time.Time) (string, tokenJSON) {
	e.t.Helper()
	body := map[string]any{"name": name}
	if expires != nil {
		body["expires_at"] = expires.Format(time.RFC3339)
	}
	rec := e.do(http.MethodPost, "/api/tokens", masterToken, body)
	e.expectStatus(rec, http.StatusCreated)

	var created createdTokenJSON
	e.decode(rec, &created)
	if created.Token == "" {
		e.t.Fatal("minted token response carried no plaintext")
	}
	return created.Token, created.tokenJSON
}

// fakeSyncer stands in for *sync.Syncer. It writes to the same store the server
// reads, so a feed added through the API is really there, and it records what
// the handlers asked it to do.
type fakeSyncer struct {
	mu    sync.Mutex
	store *store.Store
	now   func() time.Time

	// Scripted answers. A nil error means the call succeeds.
	addErr     error
	pollErr    error
	pollResult *rssdecksync.PollResult

	adds        []rssdecksync.AddRequest
	polls       []int64
	rescheduled []int64
	unscheduled []int64

	next map[int64]time.Time
}

func (f *fakeSyncer) AddFeed(ctx context.Context, req rssdecksync.AddRequest) (*store.Feed, error) {
	f.mu.Lock()
	f.adds = append(f.adds, req)
	scripted := f.addErr
	f.mu.Unlock()

	if scripted != nil {
		return nil, scripted
	}
	title := "Feed at " + req.URL
	label := title
	if req.Label != nil {
		label = *req.Label
	}
	feed, err := f.store.CreateFeed(ctx, store.NewFeed{
		URL:             req.URL,
		Title:           title,
		Label:           label,
		IntervalSeconds: req.IntervalSeconds,
		CreatedAt:       f.now(),
	})
	if err != nil {
		return nil, err
	}
	f.schedule(feed.ID, f.now().Add(15*time.Minute))
	return feed, nil
}

func (f *fakeSyncer) Poll(ctx context.Context, feedID int64) (*rssdecksync.PollResult, error) {
	f.mu.Lock()
	f.polls = append(f.polls, feedID)
	scripted, result := f.pollErr, f.pollResult
	f.mu.Unlock()

	if scripted != nil {
		return result, scripted
	}
	// The real Syncer loads the feed first, so polling one that is gone is a
	// not-found rather than a successful poll of nothing.
	feed, err := f.store.GetFeed(ctx, feedID)
	if err != nil {
		return nil, err
	}
	if result != nil {
		return result, nil
	}
	return &rssdecksync.PollResult{FeedID: feed.ID, FeedURL: feed.URL, Duration: 1500 * time.Millisecond}, nil
}

func (f *fakeSyncer) Reschedule(_ context.Context, feedID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rescheduled = append(f.rescheduled, feedID)
	return nil
}

func (f *fakeSyncer) Unschedule(feedID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unscheduled = append(f.unscheduled, feedID)
	delete(f.next, feedID)
}

func (f *fakeSyncer) NextPoll(feedID int64) (time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	at, ok := f.next[feedID]
	return at, ok
}

func (f *fakeSyncer) schedule(feedID int64, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.next == nil {
		f.next = make(map[int64]time.Time)
	}
	f.next[feedID] = at
}

func (f *fakeSyncer) calls() (polls, rescheduled, unscheduled []int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.polls...),
		append([]int64(nil), f.rescheduled...),
		append([]int64(nil), f.unscheduled...)
}

func TestNewRejectsMissingDependencies(t *testing.T) {
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	tests := map[string]Options{
		"no store":        {Syncer: &fakeSyncer{}, MasterToken: "t"},
		"no syncer":       {Store: st, MasterToken: "t"},
		"no master token": {Store: st, Syncer: &fakeSyncer{}},
	}
	for name, options := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := New(options); err == nil {
				t.Fatal("New succeeded, want an error")
			}
		})
	}
}

func TestHealthIsUnauthenticated(t *testing.T) {
	e := newEnv(t)

	rec := e.do(http.MethodGet, "/healthz", "", nil)
	e.expectStatus(rec, http.StatusOK)

	var body map[string]string
	e.decode(rec, &body)
	if body["status"] != "ok" {
		t.Fatalf("status = %q, want ok", body["status"])
	}
}

func TestUnknownPathIsJSON(t *testing.T) {
	e := newEnv(t)
	rec := e.do(http.MethodGet, "/api/nope", masterToken, nil)
	e.expectError(rec, http.StatusNotFound, codeNotFound)
}

func TestUnsupportedMethodNamesTheAllowedOnes(t *testing.T) {
	e := newEnv(t)

	rec := e.do(http.MethodPut, "/api/feeds", masterToken, map[string]any{})
	e.expectError(rec, http.StatusMethodNotAllowed, codeMethodNotAllowed)

	allow := rec.Header().Get("Allow")
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
		if !strings.Contains(allow, method) {
			t.Errorf("Allow = %q, want it to contain %s", allow, method)
		}
	}
}

func TestOversizedBodyIsRejected(t *testing.T) {
	e := newEnv(t)

	// Valid JSON, just far larger than any body this API documents.
	body := `{"url":"https://example.com/` + strings.Repeat("a", maxRequestBody) + `"}`
	rec := e.do(http.MethodPost, "/api/feeds", masterToken, body)
	e.expectError(rec, http.StatusRequestEntityTooLarge, codeRequestTooLarge)

	if adds := len(e.syncer.adds); adds != 0 {
		t.Fatalf("syncer saw %d adds, want 0: an oversized body must not reach it", adds)
	}
}

func TestMalformedAndUnknownFieldsAreRejected(t *testing.T) {
	tests := map[string]string{
		"not json":        `{"url":`,
		"not an object":   `"https://example.com/feed.xml"`,
		"unknown field":   `{"url":"https://example.com/feed.xml","labels":["typo"]}`,
		"trailing object": `{"url":"https://example.com/feed.xml"}{"url":"https://example.com/other.xml"}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			rec := e.do(http.MethodPost, "/api/feeds", masterToken, body)
			e.expectError(rec, http.StatusBadRequest, codeInvalidRequest)
		})
	}
}

func TestPathIDMustBeAPositiveInteger(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{"/api/feeds/abc", "/api/feeds/0", "/api/feeds/-1"} {
		rec := e.do(http.MethodGet, path, masterToken, nil)
		e.expectError(rec, http.StatusBadRequest, codeInvalidRequest)
	}
}

// A trailing slash is a different path, and net/http's mux does not redirect
// for patterns like these. Answering it as an unknown endpoint beats a redirect
// a scripted client would have to follow.
func TestTrailingSlashIsNotFound(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{"/healthz/", "/api/feeds/", "/api/feeds/1/", "/api/items/"} {
		e.expectError(e.do(http.MethodGet, path, masterToken, nil), http.StatusNotFound, codeNotFound)
	}
}
