package feed

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"
)

// parser is shared by every call. gofeed's format parsers are empty structs and
// its default translators are stateless, so one instance is safe for concurrent
// use and saves an allocation per poll.
var parser = gofeed.NewParser()

// Document is a parsed feed.
type Document struct {
	// Title is the feed's own title, used as the stored title and as the
	// default label when a feed is added.
	Title string

	// Items are in the order the document served them.
	Items []Item
}

// Item is one entry, reduced to what rssdeck needs.
type Item struct {
	// GUID identifies the item within its feed. Identity is per feed, so the
	// same article in two feeds becomes two bookmarks.
	GUID string

	// URL is an absolute http or https link to the article, or empty when the
	// entry offers nothing fetchable. An empty URL is not a reason to skip the
	// item: the caller records it so it is visible instead of vanishing.
	URL string

	Title string

	// PublishedAt is UTC, or nil when the entry carried no parseable date.
	PublishedAt *time.Time
}

// Parse reads a feed document. feedURL is the URL the document was fetched
// from, which is the base for resolving relative item links.
//
// A document that is not a feed, including an HTML page where a feed was
// expected, is an error. That is the signal POST /api/feeds turns into a 400,
// where it is nearly always a typo.
func Parse(body []byte, feedURL string) (*Document, error) {
	parsed, err := parser.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("feed: parse %s: %w", feedURL, err)
	}

	// gofeed detects any JSON object as a JSON feed, so a URL that answers with
	// an API response, which is what a typo usually hits, would otherwise be
	// accepted as a feed with no title and no items. JSON Feed requires a
	// version field, so its absence is the signal that this is not a feed.
	// Feed.FeedType is gofeed's own lowercase string, not one of its exported
	// detection constants.
	if parsed.FeedType == "json" && strings.TrimSpace(parsed.FeedVersion) == "" {
		return nil, fmt.Errorf("feed: parse %s: json document is not a JSON Feed, it has no version", feedURL)
	}

	// A feed URL that will not parse costs only relative-link resolution, so it
	// is not worth failing the whole document over.
	base, err := url.Parse(feedURL)
	if err != nil {
		base = nil
	}

	doc := &Document{
		Title: strings.TrimSpace(parsed.Title),
		Items: make([]Item, 0, len(parsed.Items)),
	}
	for _, entry := range parsed.Items {
		if entry == nil {
			continue
		}
		doc.Items = append(doc.Items, convert(entry, base))
	}
	return doc, nil
}

// convert reduces one gofeed entry to an Item, computing its URL first because
// identity can fall back to it.
func convert(entry *gofeed.Item, base *url.URL) Item {
	link := itemURL(entry, base)
	return Item{
		GUID:        identify(entry, link),
		URL:         link,
		Title:       strings.TrimSpace(entry.Title),
		PublishedAt: publishedAt(entry),
	}
}

// identify returns the item's identity within its feed: its GUID, else its URL,
// else a fingerprint of its title and publication date.
//
// Identity decides whether an item has already been sent, so it has to stay the
// same across polls for as long as the feed serves the item. That is why the
// second step uses the resolved absolute URL rather than the raw link: a feed
// that switches from relative to absolute links, which happens on a CMS
// upgrade, would otherwise change every identity at once and resend its whole
// window.
func identify(entry *gofeed.Item, link string) string {
	if guid := strings.TrimSpace(entry.GUID); guid != "" {
		return guid
	}
	if link != "" {
		return link
	}
	return fingerprint(entry)
}

// fingerprint is the last resort, for an entry with neither a GUID nor a usable
// link. RSS 1.0 and hand-rolled feeds do this.
//
// The date here is the publication date only, never the updated date: an edit
// bumps updated, and an identity that moves when an article is edited means
// sending it a second time. Entries with neither a title nor a date all collapse
// onto one identity, which is the right trade: such an entry has no link either,
// so it can never be sent, and collapsing leaves one visible row instead of a
// new one every poll.
func fingerprint(entry *gofeed.Item) string {
	stamp := ""
	if entry.PublishedParsed != nil {
		stamp = entry.PublishedParsed.UTC().Format(time.RFC3339)
	}
	// A separator keeps "ab" + "" from colliding with "a" + "b".
	sum := sha256.Sum256([]byte(strings.TrimSpace(entry.Title) + "\x00" + stamp))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// itemURL returns an absolute http or https URL for the entry, or empty when it
// has none. The link comes first; a GUID is used only when it is already an
// absolute URL, which is how plenty of feeds publish their permalinks.
func itemURL(entry *gofeed.Item, base *url.URL) string {
	if link := absoluteHTTP(entry.Link, base); link != "" {
		return link
	}
	// A relative GUID is meaningless as a link, so this one is not resolved.
	return absoluteHTTP(entry.GUID, nil)
}

// absoluteHTTP resolves raw against base and returns it only if the result is an
// http or https URL with a host.
//
// Everything else is dropped on purpose. Feeds carry javascript:, mailto:,
// urn:uuid: and tag: values in link and id fields, and none of them is something
// Readeck could fetch.
func absoluteHTTP(raw string, base *url.URL) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if !u.IsAbs() {
		if base == nil {
			return ""
		}
		u = base.ResolveReference(u)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	if u.Host == "" {
		return ""
	}
	return u.String()
}

// publishedAt returns the entry's date in UTC.
//
// The updated date is a fallback for formats that can carry it alone, such as a
// JSON Feed item with only date_modified. For Atom and RSS 1.0, gofeed has
// already folded updated into Published by this point.
func publishedAt(entry *gofeed.Item) *time.Time {
	for _, t := range []*time.Time{entry.PublishedParsed, entry.UpdatedParsed} {
		if t != nil {
			utc := t.UTC()
			return &utc
		}
	}
	return nil
}
