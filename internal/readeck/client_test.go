package readeck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const token = "rdk_s3cr3t_token_value"

// recorded is one request the fake Readeck received.
type recorded struct {
	method string
	path   string
	query  string
	header http.Header
	body   []byte
}

// fake is a hand-written stand-in for a Readeck instance. It records what it was
// sent so a test can assert on the request as well as the response.
type fake struct {
	server   *httptest.Server
	mu       sync.Mutex
	requests []recorded
}

func newFake(t *testing.T, handler http.HandlerFunc) *fake {
	t.Helper()
	f := &fake{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, recorded{
			method: r.Method,
			path:   r.URL.Path,
			query:  r.URL.RawQuery,
			header: r.Header.Clone(),
			body:   body,
		})
		f.mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(f.server.Close)
	return f
}

// accepts answers every request the way Readeck answers a successful create.
func accepts(bookmarkID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Write([]byte(`{"items":[]}`))
			return
		}
		w.Header().Set("Bookmark-Id", bookmarkID)
		w.Header().Set("Location", "/api/bookmarks/"+bookmarkID)
		w.WriteHeader(http.StatusAccepted)
	}
}

func (f *fake) client(t *testing.T) *Client {
	t.Helper()
	c, err := New(f.server.URL, token, f.server.Client(), "rssdeck/1.2.3")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func (f *fake) last(t *testing.T) recorded {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("the fake received no requests")
	}
	return f.requests[len(f.requests)-1]
}

func (f *fake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func TestCreateBookmarkRequestShape(t *testing.T) {
	f := newFake(t, accepts("bk-1"))

	id, err := f.client(t).CreateBookmark(context.Background(), Bookmark{
		URL:    "https://example.com/article",
		Labels: []string{"rssdeck", "example-blog"},
	})
	if err != nil {
		t.Fatalf("CreateBookmark: %v", err)
	}
	if id != "bk-1" {
		t.Errorf("id = %q, want %q", id, "bk-1")
	}

	got := f.last(t)
	if got.method != http.MethodPost {
		t.Errorf("method = %s, want POST", got.method)
	}
	if got.path != "/api/bookmarks" {
		t.Errorf("path = %q, want /api/bookmarks", got.path)
	}
	if auth := got.header.Get("Authorization"); auth != "Bearer "+token {
		t.Errorf("Authorization = %q, want a bearer token", auth)
	}
	if ct := got.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if ua := got.header.Get("User-Agent"); ua != "rssdeck/1.2.3" {
		t.Errorf("User-Agent = %q", ua)
	}

	var sent map[string]any
	if err := json.Unmarshal(got.body, &sent); err != nil {
		t.Fatalf("decode request body %q: %v", got.body, err)
	}
	if sent["url"] != "https://example.com/article" {
		t.Errorf("url = %v", sent["url"])
	}
	labels, ok := sent["labels"].([]any)
	if !ok || len(labels) != 2 || labels[0] != "rssdeck" || labels[1] != "example-blog" {
		t.Errorf("labels = %v, want [rssdeck example-blog]", sent["labels"])
	}
	// Nothing else should be sent: Readeck fetches and titles the page itself.
	if len(sent) != 2 {
		t.Errorf("body has %d fields (%v), want only url and labels", len(sent), sent)
	}
}

func TestCreateBookmarkOmitsEmptyLabels(t *testing.T) {
	f := newFake(t, accepts("bk-1"))

	if _, err := f.client(t).CreateBookmark(context.Background(), Bookmark{
		URL: "https://example.com/article",
	}); err != nil {
		t.Fatalf("CreateBookmark: %v", err)
	}

	var sent map[string]any
	if err := json.Unmarshal(f.last(t).body, &sent); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if _, present := sent["labels"]; present {
		t.Errorf("body = %v, want no labels field when there are none", sent)
	}
}

// The design reads the id from the Bookmark-Id header, with Location as the
// documented fallback. A 202 with neither is still a success: the bookmark
// exists, and failing here would send it again and leave Readeck holding two.
func TestCreateBookmarkIdentifier(t *testing.T) {
	tests := []struct {
		name       string
		bookmarkID string
		location   string
		want       string
	}{
		{name: "header", bookmarkID: "bk-42", want: "bk-42"},
		{
			name:     "location fallback",
			location: "https://readeck.example/api/bookmarks/bk-99",
			want:     "bk-99",
		},
		{name: "location with a trailing slash", location: "/api/bookmarks/bk-7/", want: "bk-7"},
		{
			name:       "header wins over location",
			bookmarkID: "bk-header",
			location:   "/api/bookmarks/bk-location",
			want:       "bk-header",
		},
		{name: "whitespace header falls through", bookmarkID: "   ", want: ""},
		{name: "neither", want: ""},
		{name: "unparseable location", location: "://nonsense", want: ""},
		{name: "location with no path", location: "https://readeck.example", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
				if tt.bookmarkID != "" {
					w.Header().Set("Bookmark-Id", tt.bookmarkID)
				}
				if tt.location != "" {
					w.Header().Set("Location", tt.location)
				}
				w.WriteHeader(http.StatusAccepted)
			})

			id, err := f.client(t).CreateBookmark(context.Background(),
				Bookmark{URL: "https://example.com/article"})
			if err != nil {
				t.Fatalf("CreateBookmark: %v", err)
			}
			if id != tt.want {
				t.Errorf("id = %q, want %q", id, tt.want)
			}
		})
	}
}

// Readeck documents 202 for a create, but any 2xx means it took the bookmark.
// Rejecting a 200 would resend an item Readeck already has.
func TestCreateBookmarkAcceptsAnySuccess(t *testing.T) {
	for _, status := range []int{200, 201, 202, 204} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Bookmark-Id", "bk-1")
				w.WriteHeader(status)
			})
			id, err := f.client(t).CreateBookmark(context.Background(),
				Bookmark{URL: "https://example.com/article"})
			if err != nil {
				t.Fatalf("CreateBookmark: %v", err)
			}
			if id != "bk-1" {
				t.Errorf("id = %q, want bk-1", id)
			}
		})
	}
}

// A rejected token is a configuration problem, not a per-item one. It has to be
// distinguishable so the daemon can log it once a run and fail its own startup.
func TestCreateBookmarkUnauthorized(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				w.Write([]byte(`{"message":"invalid token"}`))
			})

			_, err := f.client(t).CreateBookmark(context.Background(),
				Bookmark{URL: "https://example.com/article"})
			if !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("error = %v, want ErrUnauthorized", err)
			}
			var statusErr *StatusError
			if errors.As(err, &statusErr) {
				t.Errorf("error = %v, want ErrUnauthorized rather than a *StatusError", err)
			}
			if !strings.Contains(err.Error(), fmt.Sprint(status)) {
				t.Errorf("error = %v, want it to name the status", err)
			}
		})
	}
}

func TestCreateBookmarkStatusErrors(t *testing.T) {
	for _, status := range []int{400, 404, 409, 422, 429, 500, 502, 503, 599} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				w.Write([]byte(`{"message":"nope"}`))
			})

			_, err := f.client(t).CreateBookmark(context.Background(),
				Bookmark{URL: "https://example.com/article"})
			if err == nil {
				t.Fatalf("CreateBookmark succeeded on %d", status)
			}
			var statusErr *StatusError
			if !errors.As(err, &statusErr) {
				t.Fatalf("error = %v (%T), want a *StatusError", err, err)
			}
			if statusErr.Status != status {
				t.Errorf("Status = %d, want %d", statusErr.Status, status)
			}
			if errors.Is(err, ErrUnauthorized) {
				t.Errorf("error = %v, want it kept apart from ErrUnauthorized", err)
			}
			if !strings.Contains(err.Error(), "nope") {
				t.Errorf("error = %v, want it to quote the response body", err)
			}
		})
	}
}

// Readeck explains a rejection in the body, so the message carries it. A
// multi-line or oversized body must not take the log with it.
func TestStatusErrorBodyIsBoundedAndFlat(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("line one\n   line two\n\n" + strings.Repeat("x", 4096)))
	})

	_, err := f.client(t).CreateBookmark(context.Background(),
		Bookmark{URL: "https://example.com/article"})
	if err == nil {
		t.Fatal("CreateBookmark succeeded, want an error")
	}
	var statusErr *StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("error = %v, want a *StatusError", err)
	}
	if strings.ContainsAny(statusErr.Body, "\n\r") {
		t.Errorf("body excerpt %q contains a newline", statusErr.Body)
	}
	if len(statusErr.Body) > errorBodyLimit {
		t.Errorf("body excerpt is %d bytes, want at most %d", len(statusErr.Body), errorBodyLimit)
	}
	if !strings.HasPrefix(statusErr.Body, "line one line two") {
		t.Errorf("body excerpt = %q, want whitespace collapsed", statusErr.Body)
	}
}

func TestStatusErrorMessage(t *testing.T) {
	tests := []struct {
		name string
		err  *StatusError
		want string
	}{
		{name: "known code", err: &StatusError{Status: 500}, want: "readeck returned 500 Internal Server Error"},
		{
			name: "with a body",
			err:  &StatusError{Status: 422, Body: `{"message":"bad url"}`},
			want: `readeck returned 422 Unprocessable Entity: {"message":"bad url"}`,
		},
		{name: "unknown code", err: &StatusError{Status: 599}, want: "readeck returned 599"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Error(); got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCreateBookmarkTransportFailure(t *testing.T) {
	f := newFake(t, accepts("bk-1"))
	c := f.client(t)
	f.server.Close() // nothing is listening now

	_, err := c.CreateBookmark(context.Background(), Bookmark{URL: "https://example.com/article"})
	if err == nil {
		t.Fatal("CreateBookmark succeeded against a closed server")
	}
	var statusErr *StatusError
	if errors.As(err, &statusErr) {
		t.Errorf("error = %v, want a transport error rather than a *StatusError", err)
	}
}

func TestCreateBookmarkTimeout(t *testing.T) {
	release := make(chan struct{})
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		<-release
	})
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := f.client(t).CreateBookmark(ctx, Bookmark{URL: "https://example.com/article"})
	if err == nil {
		t.Fatal("CreateBookmark succeeded against a server that never answered")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want it to wrap context.DeadlineExceeded", err)
	}
}

// An item with no URL is recorded as permanently failed before it ever gets
// here, so reaching this is a bug worth an error rather than a request.
func TestCreateBookmarkRejectsAnEmptyURL(t *testing.T) {
	f := newFake(t, accepts("bk-1"))
	for _, raw := range []string{"", "   "} {
		if _, err := f.client(t).CreateBookmark(context.Background(), Bookmark{URL: raw}); err == nil {
			t.Errorf("CreateBookmark(%q) succeeded, want an error", raw)
		}
	}
	if f.count() != 0 {
		t.Errorf("the fake received %d requests, want none", f.count())
	}
}

// Nothing may put the token in an error, since those reach the log and the API.
func TestErrorsNeverLeakTheToken(t *testing.T) {
	statuses := []int{401, 403, 500}
	for _, status := range statuses {
		f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			w.Write([]byte("denied"))
		})
		_, err := f.client(t).CreateBookmark(context.Background(),
			Bookmark{URL: "https://example.com/article"})
		if err == nil {
			t.Fatalf("status %d: CreateBookmark succeeded", status)
		}
		if strings.Contains(err.Error(), token) {
			t.Errorf("status %d: error %q contains the token", status, err)
		}
	}

	// The transport error path names the URL, which must not carry the token
	// either.
	f := newFake(t, accepts("bk-1"))
	c := f.client(t)
	f.server.Close()
	_, err := c.CreateBookmark(context.Background(), Bookmark{URL: "https://example.com/article"})
	if err == nil {
		t.Fatal("CreateBookmark succeeded against a closed server")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("transport error %q contains the token", err)
	}
}

func TestProbe(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		f := newFake(t, accepts("bk-1"))
		if err := f.client(t).Probe(context.Background()); err != nil {
			t.Fatalf("Probe: %v", err)
		}
		got := f.last(t)
		if got.method != http.MethodGet {
			t.Errorf("method = %s, want GET", got.method)
		}
		// The probe reads the path the daemon posts to, so it cannot pass while
		// the one path that matters is unreachable.
		if got.path != "/api/bookmarks" {
			t.Errorf("path = %q, want /api/bookmarks", got.path)
		}
		if got.query != "limit=1" {
			t.Errorf("query = %q, want limit=1", got.query)
		}
		if auth := got.header.Get("Authorization"); auth != "Bearer "+token {
			t.Errorf("Authorization = %q, want a bearer token", auth)
		}
	})

	t.Run("bad token fails the boot", func(t *testing.T) {
		f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		})
		if err := f.client(t).Probe(context.Background()); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("error = %v, want ErrUnauthorized", err)
		}
	})

	t.Run("unreachable instance fails the boot", func(t *testing.T) {
		f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		})
		err := f.client(t).Probe(context.Background())
		var statusErr *StatusError
		if !errors.As(err, &statusErr) || statusErr.Status != http.StatusBadGateway {
			t.Fatalf("error = %v, want a 502 *StatusError", err)
		}
	})

	t.Run("no server", func(t *testing.T) {
		f := newFake(t, accepts("bk-1"))
		c := f.client(t)
		f.server.Close()
		if err := c.Probe(context.Background()); err == nil {
			t.Fatal("Probe succeeded against a closed server")
		}
	})
}

func TestNewValidatesItsInput(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		token   string
		wantErr string
	}{
		{name: "ok", baseURL: "https://readeck.example", token: token},
		{name: "empty token", baseURL: "https://readeck.example", wantErr: "empty token"},
		{name: "whitespace token", baseURL: "https://readeck.example", token: "  ", wantErr: "empty token"},
		{name: "no scheme", baseURL: "readeck.example", token: token, wantErr: "must be http or https"},
		{name: "ftp scheme", baseURL: "ftp://readeck.example", token: token, wantErr: "must be http or https"},
		{name: "no host", baseURL: "https://", token: token, wantErr: "has no host"},
		{name: "unparseable", baseURL: "://nonsense", token: token, wantErr: "parse url"},
		{name: "empty", baseURL: "", token: token, wantErr: "must be http or https"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := New(tt.baseURL, tt.token, nil, "")
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				if c.userAgent == "" {
					t.Error("userAgent is empty, want a default")
				}
				if c.client == nil {
					t.Error("client is nil, want a default")
				}
				return
			}
			if err == nil {
				t.Fatalf("New succeeded, want an error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// Operators write the instance URL by hand, so a trailing slash, a query string
// or a reverse proxy subpath all have to land on the same endpoint.
func TestNewNormalizesTheBaseURL(t *testing.T) {
	tests := []struct {
		name     string
		baseURL  string
		want     string
		endpoint string
	}{
		{
			name:     "bare",
			baseURL:  "https://readeck.example",
			want:     "https://readeck.example",
			endpoint: "https://readeck.example/api/bookmarks",
		},
		{
			name:     "trailing slash",
			baseURL:  "https://readeck.example/",
			want:     "https://readeck.example",
			endpoint: "https://readeck.example/api/bookmarks",
		},
		{
			name:     "many trailing slashes",
			baseURL:  "https://readeck.example///",
			want:     "https://readeck.example",
			endpoint: "https://readeck.example/api/bookmarks",
		},
		{
			name:     "surrounding whitespace",
			baseURL:  "  https://readeck.example \n",
			want:     "https://readeck.example",
			endpoint: "https://readeck.example/api/bookmarks",
		},
		{
			name:     "reverse proxy subpath",
			baseURL:  "https://home.example/readeck/",
			want:     "https://home.example/readeck",
			endpoint: "https://home.example/readeck/api/bookmarks",
		},
		{
			name:     "stray query and fragment",
			baseURL:  "https://readeck.example/?a=b#c",
			want:     "https://readeck.example",
			endpoint: "https://readeck.example/api/bookmarks",
		},
		{
			name:     "http is allowed for a home server",
			baseURL:  "http://192.168.1.10:8000",
			want:     "http://192.168.1.10:8000",
			endpoint: "http://192.168.1.10:8000/api/bookmarks",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := New(tt.baseURL, token, nil, "")
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if c.BaseURL() != tt.want {
				t.Errorf("BaseURL() = %q, want %q", c.BaseURL(), tt.want)
			}
			if got := c.resolve("/api/bookmarks"); got != tt.endpoint {
				t.Errorf("resolved endpoint = %q, want %q", got, tt.endpoint)
			}
		})
	}
}

func TestResolveKeepsTheQuery(t *testing.T) {
	c, err := New("https://home.example/readeck", token, nil, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := c.resolve("/api/bookmarks?limit=1")
	want := "https://home.example/readeck/api/bookmarks?limit=1"
	if got != want {
		t.Errorf("resolve = %q, want %q", got, want)
	}
}

// One client serves all four concurrent polls.
func TestClientIsSafeForConcurrentUse(t *testing.T) {
	f := newFake(t, accepts("bk-1"))
	c := f.client(t)

	const senders = 8
	errs := make(chan error, senders)
	for i := 0; i < senders; i++ {
		go func(i int) {
			_, err := c.CreateBookmark(context.Background(), Bookmark{
				URL:    fmt.Sprintf("https://example.com/article/%d", i),
				Labels: []string{"rssdeck"},
			})
			errs <- err
		}(i)
	}
	for i := 0; i < senders; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent CreateBookmark: %v", err)
		}
	}
	if f.count() != senders {
		t.Errorf("the fake received %d requests, want %d", f.count(), senders)
	}
}
