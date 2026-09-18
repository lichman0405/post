package feeds

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Format is a syndication format a feed can be rendered as.
type Format string

const (
	// FormatAtom is Atom 1.0 (RFC 4287) — the default.
	FormatAtom Format = "atom"
	// FormatRSS is RSS 2.0.
	FormatRSS Format = "rss"
)

// ParseFormat maps a value to a Format. The empty string is Atom, the
// default; anything else must name a format exactly — a typo is refused
// rather than silently served as Atom, because a feed reader that asked for
// RSS should be told it cannot have it, not handed a document in a format
// it did not parse.
func ParseFormat(s string) (Format, bool) {
	switch Format(strings.ToLower(strings.TrimSpace(s))) {
	case "", FormatAtom:
		return FormatAtom, true
	case FormatRSS:
		return FormatRSS, true
	default:
		return "", false
	}
}

// ParseAccept maps an HTTP Accept header to a Format. It answers
// ok=false when the header does not name exactly one of the two formats —
// including the common `*/*`, which means "anything" and therefore means
// the default. Both named at once is refused for the same reason a list is:
// the caller does not prefer one, and this surface will not pick for it
// silently (format preference in Accept is a quality-value negotiation this
// API does not implement — the documented way to choose is ?format=).
func ParseAccept(header string) (Format, bool) {
	header = strings.ToLower(header)
	atom := strings.Contains(header, string(FormatAtom)+"+xml")
	rss := strings.Contains(header, string(FormatRSS)+"+xml")
	switch {
	case atom && !rss:
		return FormatAtom, true
	case rss && !atom:
		return FormatRSS, true
	default:
		return "", false
	}
}

// Valid reports whether f is one of the two formats this package renders.
// Everything that accepts a Format from outside checks it with this before
// it reads anything: an unknown format is answerable without touching
// storage, and a caller who asked for something this platform does not speak
// must not be handed a document instead.
func (f Format) Valid() bool { return f == FormatAtom || f == FormatRSS }

// ContentType is the media type of a rendered document. The charset is
// spelled out because the XML declaration states it and a reader must not
// have to guess (the documents are UTF-8 by construction).
//
// It is defined for the two valid formats; an RSS document is the only one
// that is not Atom, so anything else reports Atom's type — callers hold a
// Format that Valid has already accepted.
func (f Format) ContentType() string {
	if f == FormatRSS {
		return "application/rss+xml; charset=utf-8"
	}
	return "application/atom+xml; charset=utf-8"
}

// Render serializes a feed. It is deterministic: the same Feed renders the
// same bytes on every call, so the transport can derive an ETag from the
// body, and two readers asking the same question get the same document
// (BuildFeed is what keeps time out of the model — Render never reads the
// clock).
//
// A format that is neither of the two is refused rather than defaulted:
// ParseFormat already refuses it ("a typo is refused rather than silently
// served as Atom"), and a serializer that answered Atom to a caller who
// asked for something else would undo that one layer down.
func Render(feed Feed, format Format) ([]byte, error) {
	if !format.Valid() {
		return nil, fmt.Errorf("%w: unknown feed format %q", ErrValidation, format)
	}
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	var err error
	switch format {
	case FormatRSS:
		err = xml.NewEncoder(&buf).Encode(rssFromFeed(feed))
	default:
		err = xml.NewEncoder(&buf).Encode(atomFromFeed(feed))
	}
	if err != nil {
		return nil, fmt.Errorf("feeds: render %s: %w", format, err)
	}
	// encoding/xml ends a document without a trailing newline; one is added
	// so the document is a well-formed text file (and so a diff of two
	// responses has a final line).
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

// ---------------------------------------------------------------------
// Atom 1.0 (RFC 4287)
//
// The document declares the Atom namespace as its default, so element names
// carry no prefix. RFC 4287 requires a feed to have id, title, updated and
// at least one link with rel="alternate"; every entry has id, title, updated
// and link. The feed's alternate is the entity's own page: the feed's HTML
// version is the page it describes.
//
// There is deliberately NO rel="self" link. Its URL would have to come from
// the request (the Host header, which the client controls), and a public
// document that echoes a caller-supplied host is a cache-poisoning vector
// for no gain — every URL in these documents is built from the configured
// public origin instead.

type atomFeed struct {
	XMLName  xml.Name    `xml:"http://www.w3.org/2005/Atom feed"`
	ID       string      `xml:"id"`
	Title    string      `xml:"title"`
	Subtitle string      `xml:"subtitle,omitempty"`
	Updated  string      `xml:"updated"`
	Author   atomAuthor  `xml:"author"`
	Link     *atomLink   `xml:"link,omitempty"`
	Entries  []atomEntry `xml:"entry"`
}

type atomLink struct {
	Rel  string `xml:"rel,attr"`
	Type string `xml:"type,attr"`
	Href string `xml:"href,attr"`
}

type atomAuthor struct {
	Name string `xml:"name"`
}

type atomEntry struct {
	ID        string      `xml:"id"`
	Title     string      `xml:"title"`
	Updated   string      `xml:"updated"`
	Published string      `xml:"published"`
	Link      *atomLink   `xml:"link,omitempty"`
	Author    *atomAuthor `xml:"author,omitempty"`
	Summary   string      `xml:"summary"`
}

// atomAlternateLink is the entry/feed's HTML alternate, or nil when there is
// no URL to point at. A link element is emitted only when its href EXISTS:
// an omitted element says "this document has no page to offer", while an
// element with an empty href is a link to nothing — and a link to a route the
// platform does not serve is a lie in the shape of an href
// (internal/application/explore/index.go). The feed's identity does not
// depend on this element: it is atom:id, which is the urn.
func atomAlternateLink(href string) *atomLink {
	if href == "" {
		return nil
	}
	return &atomLink{Rel: "alternate", Type: "text/html", Href: href}
}

func atomFromFeed(feed Feed) atomFeed {
	out := atomFeed{
		ID:       feed.ID,
		Title:    text(feed.Title),
		Subtitle: text(feed.Subtitle),
		Updated:  atomTime(feed.Updated),
		// The feed-level author is the platform: an entry's author is the
		// account that published the version, and the feed itself is
		// published by POST. RFC 4287 requires a feed author when an entry
		// has none, and this one is true for every entry.
		Author: atomAuthor{Name: "POST"},
		Link:   atomAlternateLink(feed.Link),
	}
	out.Entries = make([]atomEntry, 0, len(feed.Entries))
	for _, e := range feed.Entries {
		entry := atomEntry{
			ID:        e.ID,
			Title:     text(e.Title),
			Updated:   atomTime(e.Updated),
			Published: atomTime(e.Updated),
			Link:      atomAlternateLink(e.Link),
			Summary:   text(e.Summary),
		}
		if name := text(e.Author); name != "" {
			entry.Author = &atomAuthor{Name: name}
		}
		out.Entries = append(out.Entries, entry)
	}
	return out
}

// atomTime renders an instant as RFC 3339 in UTC (RFC 4287 §3.3: date-time
// with a numeric timezone; UTC is the platform's canonical zone).
func atomTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// ---------------------------------------------------------------------
// RSS 2.0
//
// channel has the four required elements (title, link, description) plus
// lastBuildDate, and each item has title, link, description, guid and
// pubDate. The guid is the entry's urn with isPermaLink="false" — RSS's own
// way of saying "this identifies the item and is not a URL" (the urn IS the
// stable identity, and the entry's link is a URL that could legitimately
// change with the deployment's origin; the guid may not).
//
// No <author> is emitted: RSS 2.0 defines it as an email address, and the
// platform does not publish addresses. The publisher's handle is in the
// item's description, where it is a name rather than a fake address.

type rssDocument struct {
	XMLName xml.Name   `xml:"rss"`
	Version string     `xml:"version,attr"`
	Channel rssChannel `xml:"channel"`
}

type rssChannel struct {
	Title string `xml:"title"`
	// Link is omitted when there is no URL to point at. RSS 2.0 lists link as
	// required, but an empty <link/> is a link to nothing, and the rule this
	// package renders under is the same as Atom's: no route, no element
	// (atomAlternateLink). A required element that would have to be a lie is
	// absent, not faked.
	Link          string    `xml:"link,omitempty"`
	Description   string    `xml:"description"`
	LastBuildDate string    `xml:"lastBuildDate"`
	Items         []rssItem `xml:"item"`
}

type rssItem struct {
	Title string `xml:"title"`
	// Link is omitted for the same reason as the channel's: a knowledge
	// publication has no page, so the item carries no URL and keeps its
	// identity in <guid>.
	Link        string  `xml:"link,omitempty"`
	Description string  `xml:"description"`
	GUID        rssGUID `xml:"guid"`
	PubDate     string  `xml:"pubDate"`
}

type rssGUID struct {
	IsPermaLink string `xml:"isPermaLink,attr"`
	Value       string `xml:",chardata"`
}

func rssFromFeed(feed Feed) rssDocument {
	description := text(feed.Subtitle)
	if description == "" {
		// RSS 2.0 requires a channel description. The entity's own name is
		// the one true sentence available about it that is not invented.
		description = text(feed.Title)
	}
	out := rssDocument{
		Version: "2.0",
		Channel: rssChannel{
			Title:         text(feed.Title),
			Link:          feed.Link,
			Description:   description,
			LastBuildDate: rssTime(feed.Updated),
		},
	}
	out.Channel.Items = make([]rssItem, 0, len(feed.Entries))
	for _, e := range feed.Entries {
		out.Channel.Items = append(out.Channel.Items, rssItem{
			Title:       text(e.Title),
			Link:        e.Link,
			Description: text(e.Summary),
			GUID:        rssGUID{IsPermaLink: "false", Value: e.ID},
			PubDate:     rssTime(e.Updated),
		})
	}
	return out
}

// rssTime renders an instant in RFC 1123 with a numeric zone (RFC 822's
// date-time as RSS 2.0 §item pubDate specifies it), in UTC.
func rssTime(t time.Time) string {
	return t.UTC().Format(time.RFC1123Z)
}

// ---------------------------------------------------------------------
// Text hygiene

// text makes a string safe for an XML document. encoding/xml escapes the
// five predefined entities and rejects nothing else, but XML 1.0 forbids
// most C0 control characters outright — a title containing a NUL or a bare
// ESC (both of which PostgreSQL text accepts) would produce a document no
// reader can parse. They are dropped, not escaped, because there is no
// escape for them.
//
// Invalid UTF-8 is replaced rather than dropped: a byte that cannot decode
// is a broken character in the source data, and dropping it would silently
// merge the two halves of the word it sat in.
func text(s string) string {
	if s == "" {
		return ""
	}
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if !validXMLRune(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// validXMLRune reports whether r is a character XML 1.0 permits:
//
//	#x9 | #xA | #xD | [#x20-#xD7FF] | [#xE000-#xFFFD] | [#x10000-#x10FFFF]
//
// The excluded tails are the surrogate range (which a Go rune cannot hold
// anyway) and #xFFFE/#xFFFF.
func validXMLRune(r rune) bool {
	switch {
	case r == '\t' || r == '\n' || r == '\r':
		return true
	case r >= 0x20 && r <= 0xD7FF:
		return true
	case r >= 0xE000 && r <= 0xFFFD:
		return true
	case r >= 0x10000 && r <= 0x10FFFF:
		return true
	default:
		return false
	}
}
