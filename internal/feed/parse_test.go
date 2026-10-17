package feed

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Fingerprints are pinned rather than recomputed from the code under test. The
// identity formula must not change quietly: a new formula gives every
// fingerprinted item a new identity, which resends the feed's whole window.
const (
	fpMinisterOct15 = "sha256:44bb033d1fc8366c5fd2d22e23836eb3e01ca10c3bc71d22aa604e3105db1167"
	fpMinisterOct16 = "sha256:6f28ef0678189113ef2f4800a360a5c0070426bca45a87e6017b307265167ea3"
	fpEmpty         = "sha256:6e340b9cffb37a989ca544e6bb780a2c78901d3fb33738768511a30617afa01d"
	fpScriptLink    = "sha256:bd141824e60fc0322ad444da70c100fe72a38ff55c854d14aa7d829933456b01"
)

func utc(s string) *time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	t = t.UTC()
	return &t
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return body
}

type wantItem struct {
	guid      string
	url       string
	title     string
	published *time.Time
}

// The corpus is deliberately ugly: CDATA, prefixed namespaces, relative links,
// unparseable dates, raw control bytes, and entries with nothing to link to.
func TestParseCorpus(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		feedURL string
		title   string
		items   []wantItem
	}{
		{
			name:    "wordpress rss 2.0 with CDATA and a non-permalink guid",
			fixture: "wordpress_rss2.xml",
			feedURL: "https://notebook.example/feed/",
			title:   "Örjan’s Notebook",
			items: []wantItem{
				{
					guid:      "https://notebook.example/?p=1482",
					url:       "https://notebook.example/2026/10/shipping-v2/",
					title:     "What I learned shipping <em>v2</em>",
					published: utc("2026-10-15T14:30:00Z"),
				},
				{
					guid:      "https://notebook.example/?p=1475",
					url:       "https://notebook.example/2026/10/weeknotes-41/",
					title:     "Weeknotes — week 41",
					published: utc("2026-10-13T08:00:00Z"),
				},
			},
		},
		{
			// The first entry has only <updated>, the second has both, and
			// <published> has to win where it exists.
			name:    "atom where entries carry only updated",
			fixture: "atom_updated_only.xml",
			feedURL: "https://changelog.example/atom.xml",
			title:   "Infra Changelog",
			items: []wantItem{
				{
					guid:      "tag:changelog.example,2026:entry-8821",
					url:       "https://changelog.example/postgres-17",
					title:     "Postgres 17 rollout",
					published: utc("2026-10-16T11:45:00Z"),
				},
				{
					guid:      "tag:changelog.example,2026:entry-8807",
					url:       "https://changelog.example/retire-lb",
					title:     "Retire the old load balancer",
					published: utc("2026-10-13T22:00:00Z"),
				},
			},
		},
		{
			name:    "relative links resolve against the feed url",
			fixture: "relative_links.xml",
			feedURL: "https://dept.example/bulletin/feed.xml",
			title:   "Department Bulletin",
			items: []wantItem{
				{
					guid:      "https://dept.example/bulletin/parking-closure",
					url:       "https://dept.example/bulletin/parking-closure",
					title:     "Parking closure",
					published: utc("2026-10-14T10:00:00Z"),
				},
				{
					guid:      "https://dept.example/bulletin/cafeteria-hours.html",
					url:       "https://dept.example/bulletin/cafeteria-hours.html",
					title:     "Cafeteria hours",
					published: utc("2026-10-13T10:00:00Z"),
				},
				{
					guid:      "https://dept.example/announcements/fire-drill",
					url:       "https://dept.example/announcements/fire-drill",
					title:     "Fire drill",
					published: utc("2026-10-12T10:00:00Z"),
				},
			},
		},
		{
			name:    "entries with no guid and no link fall back to a fingerprint",
			fixture: "no_guid_no_link.xml",
			feedURL: "https://headlines.example/rss",
			title:   "Headlines Only",
			items: []wantItem{
				{guid: fpMinisterOct15, title: "Minister resigns", published: utc("2026-10-15T06:00:00Z")},
				{guid: fpMinisterOct16, title: "Minister resigns", published: utc("2026-10-16T06:00:00Z")},
				{guid: fpEmpty},
			},
		},
		{
			name:    "a repeated guid stays one identity",
			fixture: "duplicate_guids.xml",
			feedURL: "https://repeater.example/feed",
			title:   "Repeater",
			items: []wantItem{
				{
					guid:      "https://repeater.example/a",
					url:       "https://repeater.example/a",
					title:     "First copy",
					published: utc("2026-10-15T06:00:00Z"),
				},
				{
					guid:      "https://repeater.example/a",
					url:       "https://repeater.example/a?utm_source=rss",
					title:     "Second copy, same guid",
					published: utc("2026-10-15T06:00:00Z"),
				},
				{
					guid:      "https://repeater.example/b",
					url:       "https://repeater.example/b",
					title:     "Distinct",
					published: utc("2026-10-14T06:00:00Z"),
				},
			},
		},
		{
			name:    "unparseable dates leave the date empty, not the item dropped",
			fixture: "bad_dates.xml",
			feedURL: "https://dates.example/feed",
			title:   "Date Soup",
			items: []wantItem{
				{
					guid: "https://dates.example/1", url: "https://dates.example/1",
					title: "Two digit year", published: utc("2026-10-15T06:00:00Z"),
				},
				{
					guid: "https://dates.example/2", url: "https://dates.example/2",
					title: "ISO in an RSS feed", published: utc("2026-10-14T06:00:00Z"),
				},
				{guid: "https://dates.example/3", url: "https://dates.example/3", title: "Not a date at all"},
				{guid: "https://dates.example/4", url: "https://dates.example/4", title: "Empty date element"},
				{guid: "https://dates.example/5", url: "https://dates.example/5", title: "No date element"},
			},
		},
		{
			name:    "schemes Readeck cannot fetch are dropped",
			fixture: "unsafe_links.xml",
			feedURL: "https://unsafe.example/feed",
			title:   "Nothing Fetchable",
			items: []wantItem{
				{guid: fpScriptLink, title: "Script link", published: utc("2026-10-15T06:00:00Z")},
				{
					guid:      "urn:uuid:60a76c80-d399-11d9-b93C-0003939e0af6",
					title:     "Mail link with a urn guid",
					published: utc("2026-10-14T06:00:00Z"),
				},
				{
					guid:      "https://unsafe.example/real-post",
					url:       "https://unsafe.example/real-post",
					title:     "No link, but the guid is a permalink",
					published: utc("2026-10-13T06:00:00Z"),
				},
				{
					guid:      "ftp://unsafe.example/pub/file.txt",
					title:     "FTP is not fetchable by Readeck",
					published: utc("2026-10-12T06:00:00Z"),
				},
			},
		},
		{
			name:    "json feed",
			fixture: "jsonfeed.json",
			feedURL: "https://micro.example/feed.json",
			title:   "Micro Posts",
			items: []wantItem{
				// An untitled item is normal in a microblog feed.
				{guid: "4821", url: "https://micro.example/4821", published: utc("2026-10-15T22:04:00Z")},
				{
					guid: "https://micro.example/4815", url: "https://micro.example/4815",
					title: "Titled post", published: utc("2026-10-14T12:00:00Z"),
				},
				{guid: "4810"},
			},
		},
		{
			// RSS 1.0 gives gofeed no guid at all, so identity comes from the
			// link.
			name:    "rss 1.0 over rdf",
			fixture: "rdf_rss1.xml",
			feedURL: "https://wiki.example/feed.rdf",
			title:   "Wiki Recent Changes",
			items: []wantItem{
				{
					guid: "https://wiki.example/page/Kettle", url: "https://wiki.example/page/Kettle",
					title: "Kettle", published: utc("2026-10-16T07:14:00Z"),
				},
				{
					guid: "https://wiki.example/page/Teapot", url: "https://wiki.example/page/Teapot",
					title: "Teapot", published: utc("2026-10-15T19:40:00Z"),
				},
			},
		},
		{
			name:    "atom under a namespace prefix with media extensions",
			fixture: "namespace_soup.xml",
			feedURL: "https://prefixed.example/feed",
			title:   "Prefixed Atom",
			items: []wantItem{
				{
					guid: "https://prefixed.example/ep/12", url: "https://prefixed.example/ep/12",
					title: "Episode 12", published: utc("2026-10-16T09:30:00Z"),
				},
			},
		},
		{
			name:    "raw control bytes in element text",
			fixture: "control_chars.xml",
			feedURL: "https://control.example/feed",
			title:   "Control Characters",
			items: []wantItem{
				{
					guid: "https://control.example/1", url: "https://control.example/1",
					title: "Formfeed in a title", published: utc("2026-10-15T06:00:00Z"),
				},
			},
		},
		{
			name:    "a feed that has published nothing",
			fixture: "empty_feed.xml",
			feedURL: "https://empty.example/feed",
			title:   "Nothing Yet",
			items:   []wantItem{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc, err := Parse(readFixture(t, tt.fixture), tt.feedURL)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if doc.Title != tt.title {
				t.Errorf("title = %q, want %q", doc.Title, tt.title)
			}
			if len(doc.Items) != len(tt.items) {
				t.Fatalf("got %d items, want %d", len(doc.Items), len(tt.items))
			}
			for i, want := range tt.items {
				got := doc.Items[i]
				if got.GUID != want.guid {
					t.Errorf("item %d guid = %q, want %q", i, got.GUID, want.guid)
				}
				if got.URL != want.url {
					t.Errorf("item %d url = %q, want %q", i, got.URL, want.url)
				}
				if got.Title != want.title {
					t.Errorf("item %d title = %q, want %q", i, got.Title, want.title)
				}
				switch {
				case want.published == nil && got.PublishedAt != nil:
					t.Errorf("item %d published = %v, want nil", i, got.PublishedAt)
				case want.published != nil && got.PublishedAt == nil:
					t.Errorf("item %d published = nil, want %v", i, want.published)
				case want.published != nil && !got.PublishedAt.Equal(*want.published):
					t.Errorf("item %d published = %v, want %v", i, got.PublishedAt, want.published)
				}
				if got.PublishedAt != nil && got.PublishedAt.Location() != time.UTC {
					t.Errorf("item %d published is in %v, want UTC", i, got.PublishedAt.Location())
				}
			}
		})
	}
}

func TestParseRejectsWhatIsNotAFeed(t *testing.T) {
	tests := []struct {
		name string
		body []byte
	}{
		{"an html page", readFixtureBytes("not_a_feed.html")},
		{"empty", nil},
		{"whitespace", []byte("   \n\t ")},
		{"json that is not a json feed", []byte(`{"hello":"world"}`)},
		{"truncated xml", []byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>Cut`)},
		{"plain text", []byte("not a feed, just words")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse(tt.body, "https://example.com/feed.xml"); err == nil {
				t.Fatal("Parse succeeded, want an error")
			}
		})
	}
}

func readFixtureBytes(name string) []byte {
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		panic(err)
	}
	return body
}

// The error has to name the feed, since it reaches the operator as a 400 from
// POST /api/feeds or as a recorded poll failure.
func TestParseErrorNamesTheFeed(t *testing.T) {
	_, err := Parse(readFixture(t, "not_a_feed.html"), "https://example.com/feed.xml")
	if err == nil {
		t.Fatal("Parse succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "https://example.com/feed.xml") {
		t.Errorf("error = %v, want it to name the feed url", err)
	}
}

// Identity has to survive re-parsing the same bytes, which is the whole basis of
// not resending an item.
func TestParseIsDeterministic(t *testing.T) {
	for _, fixture := range []string{
		"wordpress_rss2.xml", "no_guid_no_link.xml", "unsafe_links.xml", "jsonfeed.json",
	} {
		t.Run(fixture, func(t *testing.T) {
			body := readFixture(t, fixture)
			first, err := Parse(body, "https://example.com/feed.xml")
			if err != nil {
				t.Fatalf("first Parse: %v", err)
			}
			second, err := Parse(body, "https://example.com/feed.xml")
			if err != nil {
				t.Fatalf("second Parse: %v", err)
			}
			if len(first.Items) != len(second.Items) {
				t.Fatalf("item counts differ: %d then %d", len(first.Items), len(second.Items))
			}
			for i := range first.Items {
				if first.Items[i].GUID != second.Items[i].GUID {
					t.Errorf("item %d identity changed between parses: %q then %q",
						i, first.Items[i].GUID, second.Items[i].GUID)
				}
			}
		})
	}
}

// Relative links need a base. Without a usable feed URL they are dropped rather
// than stored as something unfetchable.
func TestParseWithAnUnusableFeedURL(t *testing.T) {
	doc, err := Parse(readFixture(t, "relative_links.xml"), "://not a url")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(doc.Items) != 3 {
		t.Fatalf("got %d items, want 3", len(doc.Items))
	}
	for i, item := range doc.Items {
		if item.URL != "" {
			t.Errorf("item %d url = %q, want empty with no base to resolve against", i, item.URL)
		}
		if !strings.HasPrefix(item.GUID, "sha256:") {
			t.Errorf("item %d guid = %q, want a fingerprint", i, item.GUID)
		}
	}
}

func TestAbsoluteHTTP(t *testing.T) {
	base := mustParseURL(t, "https://example.com/blog/feed.xml")
	tests := []struct {
		name string
		raw  string
		base *url.URL
		want string
	}{
		{"absolute https", "https://other.example/a", base, "https://other.example/a"},
		{"absolute http", "http://other.example/a", base, "http://other.example/a"},
		{"uppercase scheme", "HTTPS://other.example/a", base, "https://other.example/a"},
		{"root relative", "/a", base, "https://example.com/a"},
		{"document relative", "a.html", base, "https://example.com/blog/a.html"},
		{"parent relative", "../a", base, "https://example.com/a"},
		{"protocol relative", "//other.example/a", base, "https://other.example/a"},
		{"query only", "?page=2", base, "https://example.com/blog/feed.xml?page=2"},
		{"surrounding whitespace", "  https://other.example/a\n", base, "https://other.example/a"},
		{"empty", "", base, ""},
		{"whitespace only", "   ", base, ""},
		{"javascript", "javascript:void(0)", base, ""},
		{"mailto", "mailto:a@example.com", base, ""},
		{"urn", "urn:uuid:60a76c80-d399-11d9-b93C-0003939e0af6", base, ""},
		{"tag uri", "tag:example.com,2026:1", base, ""},
		{"ftp", "ftp://example.com/f", base, ""},
		{"data uri", "data:text/plain,hello", base, ""},
		{"relative with no base", "/a", nil, ""},
		{"absolute with no base", "https://other.example/a", nil, "https://other.example/a"},
		{"scheme with no host", "https://", base, ""},
		{"unparseable", "http://[::1", base, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := absoluteHTTP(tt.raw, tt.base); got != tt.want {
				t.Errorf("absoluteHTTP(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

// gofeed accepts any JSON object as a JSON feed. A feed URL that has been typo'd
// into an API endpoint must fail at add time, not become a feed that never
// produces an item.
func TestParseRejectsJSONThatIsNotAJSONFeed(t *testing.T) {
	tests := []struct {
		name string
		body string
		ok   bool
	}{
		{name: "api error response", body: `{"error":"not found","code":404}`},
		{name: "empty object", body: `{}`},
		{name: "items but no version", body: `{"items":[{"id":"1","url":"https://e.example/1"}]}`},
		{
			name: "a real json feed",
			body: `{"version":"https://jsonfeed.org/version/1.1","title":"T","items":[]}`,
			ok:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.body), "https://example.com/feed.json")
			if tt.ok && err != nil {
				t.Fatalf("Parse: %v, want it accepted", err)
			}
			if !tt.ok && err == nil {
				t.Fatal("Parse succeeded, want an error")
			}
		})
	}
}
