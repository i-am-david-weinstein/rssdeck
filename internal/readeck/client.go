// Package readeck posts bookmarks to a Readeck instance.
//
// The client never retries. The daemon polls on a timer and leaves a failed item
// pending, so it is already a retry loop; retrying in here would only turn one
// slow Readeck into a stalled poll.
package readeck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
)

// ErrUnauthorized means Readeck rejected the token. It covers 403 as well as
// 401: both say the credentials are wrong rather than the request, so neither is
// worth retrying per item, and either should stop the daemon from starting.
var ErrUnauthorized = errors.New("readeck rejected the token")

// StatusError is Readeck answering with an unexpected status. It carries the
// code and a short excerpt of the body, which is usually where Readeck explains
// what it disliked.
type StatusError struct {
	Status int
	Body   string
}

func (e *StatusError) Error() string {
	msg := fmt.Sprintf("readeck returned %d", e.Status)
	if text := http.StatusText(e.Status); text != "" {
		msg += " " + text
	}
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

// errorBodyLimit bounds how much of an error response is read for the message.
// Enough for a JSON error, not enough for an HTML error page to fill the log.
const errorBodyLimit = 512

// Client talks to one Readeck instance. It holds no per-request state, so one
// instance serves every concurrent poll.
type Client struct {
	client    *http.Client
	baseURL   *url.URL
	token     string
	userAgent string
}

// New returns a client for the Readeck instance at baseURL, which is the
// instance root rather than its API path. client's timeout bounds each request.
func New(baseURL, token string, client *http.Client, userAgent string) (*Client, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("readeck: empty token")
	}
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil, fmt.Errorf("readeck: parse url %q: %w", baseURL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("readeck: url %q must be http or https", baseURL)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("readeck: url %q has no host", baseURL)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawQuery = ""
	parsed.Fragment = ""

	if client == nil {
		client = http.DefaultClient
	}
	if userAgent == "" {
		userAgent = "rssdeck"
	}
	return &Client{client: client, baseURL: parsed, token: token, userAgent: userAgent}, nil
}

// BaseURL returns the normalized instance root, for the startup log line.
func (c *Client) BaseURL() string {
	return c.baseURL.String()
}

// Bookmark is a bookmark to create. Readeck fetches and titles the page itself,
// so the URL and its labels are all it needs.
type Bookmark struct {
	URL    string   `json:"url"`
	Labels []string `json:"labels,omitempty"`
}

// CreateBookmark asks Readeck to bookmark one URL and returns the new
// bookmark's id.
//
// Readeck accepts the request with 202 and fetches the page afterwards, so
// success here means accepted for processing, not that the article was
// retrieved. A 202 carrying no identifier still counts as success and returns an
// empty id: the bookmark exists, and failing the send would only send it again
// and leave Readeck holding two.
func (c *Client) CreateBookmark(ctx context.Context, b Bookmark) (string, error) {
	if strings.TrimSpace(b.URL) == "" {
		return "", errors.New("readeck: bookmark has no url")
	}

	body, err := json.Marshal(b)
	if err != nil {
		return "", fmt.Errorf("readeck: encode bookmark: %w", err)
	}

	resp, err := c.do(ctx, http.MethodPost, "/api/bookmarks", body)
	if err != nil {
		return "", err
	}
	defer closeBody(resp)

	if err := statusError(resp); err != nil {
		return "", err
	}
	return bookmarkID(resp), nil
}

// Probe makes one authenticated request at startup so a wrong URL or token
// fails the boot, rather than surfacing hours later as items that will not
// send.
//
// It reads the bookmark list because that is the same path the daemon posts to.
// A probe against some other endpoint could pass while the one path that
// matters is unreachable.
func (c *Client) Probe(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodGet, "/api/bookmarks?limit=1", nil)
	if err != nil {
		return err
	}
	defer closeBody(resp)
	return statusError(resp)
}

// do builds and sends one authenticated request. ref is a path, with an optional
// query, below the instance root.
func (c *Client) do(ctx context.Context, method, ref string, body []byte) (*http.Response, error) {
	endpoint := c.resolve(ref)

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, fmt.Errorf("readeck: build request for %s: %w", endpoint, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.client.Do(req)
	if err != nil {
		// Only the URL is named. A transport error never carries the token, and
		// nothing here should put it in a log.
		return nil, fmt.Errorf("readeck: %s %s: %w", method, endpoint, err)
	}
	return resp, nil
}

// resolve joins ref onto the instance root, keeping any path the root carries so
// an instance served under a reverse proxy subpath works.
func (c *Client) resolve(ref string) string {
	refPath, rawQuery, _ := strings.Cut(ref, "?")
	endpoint := *c.baseURL
	endpoint.Path = path.Join(c.baseURL.Path, refPath)
	endpoint.RawQuery = rawQuery
	return endpoint.String()
}

// statusError turns a response into an error unless it succeeded.
func statusError(resp *http.Response) error {
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%w (%d)", ErrUnauthorized, resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &StatusError{Status: resp.StatusCode, Body: excerpt(resp.Body)}
	}
	return nil
}

// bookmarkID reads the new bookmark's identifier. Readeck returns it in a
// header, and the Location URL ends with the same id, so the header is read
// first and the URL is the fallback.
func bookmarkID(resp *http.Response) string {
	if id := strings.TrimSpace(resp.Header.Get("Bookmark-Id")); id != "" {
		return id
	}
	location := strings.TrimSpace(resp.Header.Get("Location"))
	if location == "" {
		return ""
	}
	u, err := url.Parse(location)
	if err != nil {
		return ""
	}
	trimmed := strings.TrimRight(u.Path, "/")
	if trimmed == "" {
		return ""
	}
	return path.Base(trimmed)
}

// excerpt reads the start of an error body for the error message, collapsing
// whitespace so a multi-line response stays on one log line.
func excerpt(body io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(body, errorBodyLimit))
	if err != nil || len(raw) == 0 {
		return ""
	}
	return strings.Join(strings.Fields(string(raw)), " ")
}

// closeBody drains what is left so the connection can be reused, then closes it.
func closeBody(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, errorBodyLimit))
	_ = resp.Body.Close()
}
