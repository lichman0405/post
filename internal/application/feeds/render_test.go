package feeds

import (
	"bytes"
	"encoding/xml"
	"errors"
	"strings"
	"testing"
	"time"
)

// The serializer's contract: well-formed XML a reader can parse, the shape
// each format's own specification requires, nothing but the model's data in
// the document, and the same bytes every time.
//
// Every check here parses the document back with encoding/xml rather than
// grepping it, because "the reader can parse this" is the property that
// matters — a test that looked for a substring would pass on a document no
// feed reader would accept.

func sampleFeed() Feed {
	return Feed{
		ID:       "urn:post:feed:project:" + testProject,
		Kind:     KindProject,
		Title:    "Open Photocatalysts",
		Subtitle: "A public research project.",
		Link:     "https://post.example.org/projects/" + testProject,
		Updated:  testInstant,
		Entries: []Entry{
			{
				ID:      "urn:post:asset-version:" + testVersionID,
				Title:   "Diffraction Data v1.0",
				Link:    "https://post.example.org/assets/" + testAssetID + "/v1.0",
				Version: "v1.0",
				Updated: testInstant,
				Summary: "Published dataset version v1.0 “Diffraction Data”. Published by ada.",
				Author:  "ada",
			},
			{
				ID:    "urn:post:knowledge-publication:" + testPubID,
				Title: "A claim v2.0",
				// Empty on purpose: no route renders a knowledge publication,
				// and the model leaves the link empty rather than pointing at
				// a page that does not exist (feed.go entryLink). The
				// serializer must render NO element for it, never an empty
				// href — and the entry above is the fixture's control that
				// links are still emitted when there IS one.
				Link:    "",
				Version: "v2.0",
				Updated: testInstant.Add(-time.Hour),
				Summary: "Published claim version v2.0 “A claim”.",
			},
		},
	}
}

// TestAtomDocumentShape: RFC 4287 §4 requires a feed to carry id, title,
// updated and at least one rel="alternate" link, and each entry to carry id,
// title, updated and a link. The document parses as XML and into the Atom
// model the package declares.
func TestAtomDocumentShape(t *testing.T) {
	doc, err := Render(sampleFeed(), FormatAtom)
	if err != nil {
		t.Fatalf("Render(atom): %v", err)
	}
	if !bytes.HasPrefix(doc, []byte(xml.Header)) {
		t.Errorf("document does not start with the XML declaration: %q", firstLine(doc))
	}
	if !strings.HasSuffix(string(doc), "\n") {
		t.Error("document does not end with a newline")
	}

	var parsed atomFeed
	if err := xml.Unmarshal(doc, &parsed); err != nil {
		t.Fatalf("the rendered Atom document does not parse: %v\n%s", err, doc)
	}
	if parsed.XMLName.Space != "http://www.w3.org/2005/Atom" {
		t.Errorf("root namespace = %q, want the Atom namespace", parsed.XMLName.Space)
	}
	if parsed.ID == "" || parsed.Title == "" || parsed.Updated == "" {
		t.Errorf("feed is missing a required element: %+v", parsed)
	}
	if parsed.Link == nil || parsed.Link.Rel != "alternate" || parsed.Link.Type != "text/html" || parsed.Link.Href == "" {
		t.Errorf("feed link = %+v, want one rel=alternate text/html link", parsed.Link)
	}
	if parsed.Author.Name == "" {
		t.Error("feed has no author (RFC 4287 requires one when an entry has none)")
	}
	if len(parsed.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(parsed.Entries))
	}
	for i, e := range parsed.Entries {
		if e.ID == "" || e.Title == "" || e.Updated == "" {
			t.Errorf("entry %d is missing a required element: %+v", i, e)
		}
		if _, err := time.Parse(time.RFC3339, e.Updated); err != nil {
			t.Errorf("entry %d updated = %q is not RFC 3339: %v", i, e.Updated, err)
		}
	}
	// Entry 0 (an asset version) has a page, so its link IS emitted and points
	// at it — the control. Entry 1 (a knowledge publication) has none, so there
	// is no element at all: a link element with an empty href is a link to
	// nothing, which is the same lie as a link to a 404.
	if got, want := parsed.Entries[0].Link, sampleFeed().Entries[0].Link; got == nil || got.Rel != "alternate" || got.Href != want {
		t.Errorf("asset entry link = %+v, want one rel=alternate link to %q", got, want)
	}
	if got := parsed.Entries[1].Link; got != nil {
		t.Errorf("knowledge entry link = %+v, want no link element (no route renders a knowledge publication)", got)
	}
	// The order the model produced is the order the document carries: a
	// serializer that reordered would break "newest first" silently.
	if parsed.Entries[0].ID != sampleFeed().Entries[0].ID {
		t.Errorf("first entry = %q, want the model's first", parsed.Entries[0].ID)
	}
	// An entry with no author renders NO author element — never an empty one
	// (an empty name is a lie about who published it).
	if parsed.Entries[1].Author != nil {
		t.Errorf("entry with no publisher rendered an author: %+v", parsed.Entries[1].Author)
	}
	if strings.Contains(string(doc), "<author></author>") {
		t.Error("document contains an empty author element")
	}
}

// TestRSSDocumentShape: RSS 2.0 requires channel title, link and description,
// and an item guid; the guid is the stable urn, marked as not a link.
func TestRSSDocumentShape(t *testing.T) {
	doc, err := Render(sampleFeed(), FormatRSS)
	if err != nil {
		t.Fatalf("Render(rss): %v", err)
	}
	var parsed rssDocument
	if err := xml.Unmarshal(doc, &parsed); err != nil {
		t.Fatalf("the rendered RSS document does not parse: %v\n%s", err, doc)
	}
	if parsed.XMLName.Local != "rss" || parsed.Version != "2.0" {
		t.Errorf("root = %q version %q, want rss 2.0", parsed.XMLName.Local, parsed.Version)
	}
	if parsed.Channel.Title == "" || parsed.Channel.Link == "" || parsed.Channel.Description == "" {
		t.Errorf("channel is missing a required element: %+v", parsed.Channel)
	}
	if _, err := time.Parse(time.RFC1123Z, parsed.Channel.LastBuildDate); err != nil {
		t.Errorf("lastBuildDate = %q is not RFC 1123 with a zone: %v", parsed.Channel.LastBuildDate, err)
	}
	if len(parsed.Channel.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(parsed.Channel.Items))
	}
	for i, item := range parsed.Channel.Items {
		if item.Title == "" || item.Description == "" {
			t.Errorf("item %d is missing a required element: %+v", i, item)
		}
		if item.GUID.IsPermaLink != "false" {
			t.Errorf("item %d guid isPermaLink = %q, want false (the guid is a urn, not a URL)", i, item.GUID.IsPermaLink)
		}
		if !strings.HasPrefix(item.GUID.Value, "urn:post:") {
			t.Errorf("item %d guid = %q, want the stable urn", i, item.GUID.Value)
		}
		if _, err := time.Parse(time.RFC1123Z, item.PubDate); err != nil {
			t.Errorf("item %d pubDate = %q is not RFC 1123 with a zone: %v", i, item.PubDate, err)
		}
	}
	// The asset item keeps its link and the knowledge item has none — the
	// same pairing the Atom half asserts, for the same reason.
	if got, want := parsed.Channel.Items[0].Link, sampleFeed().Entries[0].Link; got != want {
		t.Errorf("asset item link = %q, want %q", got, want)
	}
	if got := parsed.Channel.Items[1].Link; got != "" {
		t.Errorf("knowledge item link = %q, want none (no route renders a knowledge publication)", got)
	}
	// RSS 2.0 defines <author> as an email address; the platform publishes
	// none, so it must not appear at all (the handle is in the description).
	if strings.Contains(string(doc), "<author>") {
		t.Error("the RSS document carries an <author> element, which RSS defines as an email address")
	}
	// A channel with no entries still has its required description.
	empty := sampleFeed()
	empty.Entries = nil
	empty.Subtitle = ""
	emptyDoc, err := Render(empty, FormatRSS)
	if err != nil {
		t.Fatalf("Render(empty rss): %v", err)
	}
	var emptyParsed rssDocument
	if err := xml.Unmarshal(emptyDoc, &emptyParsed); err != nil {
		t.Fatalf("the empty RSS document does not parse: %v", err)
	}
	if emptyParsed.Channel.Description != emptyParsed.Channel.Title || emptyParsed.Channel.Description == "" {
		t.Errorf("empty channel description = %q, want the entity's own name %q",
			emptyParsed.Channel.Description, emptyParsed.Channel.Title)
	}
	if len(emptyParsed.Channel.Items) != 0 {
		t.Errorf("empty channel has %d items", len(emptyParsed.Channel.Items))
	}
}

// TestRenderedKnowledgeFeedHasNoURL: the end of the road for the rule the
// model applies — what a subscriber actually downloads. A knowledge feed is
// built by BuildFeed (so this reads the model's real output, not a fixture)
// and rendered in both formats; no document may carry a link element or a
// /knowledge/ URL, because no such route exists, while the entry's urn — the
// thing that identifies it — is still there.
//
// The asset feed below is the control. Without it a serializer that dropped
// every link, or a model that cleared every link, would pass this test.
func TestRenderedKnowledgeFeedHasNoURL(t *testing.T) {
	knowledge, ok := BuildFeed(knowledgeTarget(), State{
		Found: true, ProjectVisibility: VisibilityPublic, CreatedAt: testInstant,
		Entries: []EntryState{knowledgePublication(testPubID, "v2.0", "A claim")},
	}, testBase)
	if !ok {
		t.Fatal("a public knowledge object with a publication must have a feed")
	}
	for _, format := range []Format{FormatAtom, FormatRSS} {
		doc, err := Render(knowledge, format)
		if err != nil {
			t.Fatalf("Render(%s): %v", format, err)
		}
		if bytes.Contains(doc, []byte("<link")) {
			t.Errorf("%s: a knowledge feed rendered a link element:\n%s", format, doc)
		}
		if bytes.Contains(doc, []byte("/knowledge/")) {
			t.Errorf("%s: the document carries a /knowledge/ URL, and there is no such route:\n%s", format, doc)
		}
		if !bytes.Contains(doc, []byte(knowledge.Entries[0].ID)) {
			t.Errorf("%s: the document lost the entry's urn id:\n%s", format, doc)
		}
	}

	asset, ok := BuildFeed(assetTarget(), State{
		Found: true, Title: "Diffraction Data", ProjectVisibility: VisibilityPublic, CreatedAt: testInstant,
		Entries: []EntryState{assetVersion(testVersionID, "v1.0", VisibilityPublic)},
	}, testBase)
	if !ok {
		t.Fatal("a public asset with a public version must have a feed")
	}
	for _, format := range []Format{FormatAtom, FormatRSS} {
		doc, err := Render(asset, format)
		if err != nil {
			t.Fatalf("Render(%s): %v", format, err)
		}
		for _, want := range []string{asset.Link, asset.Entries[0].Link} {
			if !bytes.Contains(doc, []byte(want)) {
				t.Errorf("%s: the asset document is missing the URL %q:\n%s", format, want, doc)
			}
		}
	}
}

// TestAtomEmptyFeedHasNoEntries: a public project that has published nothing
// still has a feed (see the package doc), and that feed carries no entries —
// not an empty <entry/> placeholder, which a reader would show as a blank
// line.
func TestAtomEmptyFeedHasNoEntries(t *testing.T) {
	feed := sampleFeed()
	feed.Entries = nil
	doc, err := Render(feed, FormatAtom)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(string(doc), "<entry") {
		t.Errorf("an entry-less feed rendered an entry element:\n%s", doc)
	}
	var parsed atomFeed
	if err := xml.Unmarshal(doc, &parsed); err != nil {
		t.Fatalf("does not parse: %v", err)
	}
	if len(parsed.Entries) != 0 || parsed.ID == "" || parsed.Title == "" || parsed.Updated == "" {
		t.Errorf("empty feed = %+v, want the required elements and no entries", parsed)
	}
}

// TestTextEscapingAndHygiene: the values in a feed come from user data
// (titles, purposes, handles), so they may contain anything a text column can
// hold — including characters XML cannot represent at all. The document must
// stay PARSEABLE and must not lose the characters that are merely special.
func TestTextEscapingAndHygiene(t *testing.T) {
	dirty := "Photocatalysts <TiO2> & \"oxides\" 'at' 100% — \x00\x01\x07\x1f ok"
	feed := sampleFeed()
	feed.Title = dirty
	feed.Subtitle = dirty
	feed.Entries[0].Title = dirty
	feed.Entries[0].Summary = dirty
	feed.Entries[0].Author = "a\x0bb" // a vertical tab: XML forbids it
	feed.Entries[1].Title = string([]byte{0xff, 0xfe}) + "invalid utf8"

	for _, format := range []Format{FormatAtom, FormatRSS} {
		doc, err := Render(feed, format)
		if err != nil {
			t.Fatalf("Render(%s): %v", format, err)
		}
		if bytes.Contains(doc, []byte{0x00}) || bytes.Contains(doc, []byte{0x07}) || bytes.Contains(doc, []byte{0x1f}) {
			t.Errorf("%s: the document contains a character XML 1.0 forbids", format)
		}
		if !bytes.Contains(doc, []byte("&lt;TiO2&gt;")) || !bytes.Contains(doc, []byte("&amp;")) {
			t.Errorf("%s: the document does not escape the text it carries:\n%s", format, doc)
		}
		var parsed atomFeed
		if format == FormatAtom {
			if err := xml.Unmarshal(doc, &parsed); err != nil {
				t.Fatalf("atom does not parse with hostile text: %v", err)
			}
			if !strings.Contains(parsed.Title, "<TiO2> &") || !strings.Contains(parsed.Title, "100%") {
				t.Errorf("atom title = %q, want the readable text preserved", parsed.Title)
			}
		} else {
			var rss rssDocument
			if err := xml.Unmarshal(doc, &rss); err != nil {
				t.Fatalf("rss does not parse with hostile text: %v", err)
			}
			if !strings.Contains(rss.Channel.Title, "<TiO2> &") {
				t.Errorf("rss channel title = %q, want the readable text preserved", rss.Channel.Title)
			}
		}
	}
}

// TestRenderIsDeterministic: the transport derives an ETag from these bytes,
// so two renders of one feed must be byte-identical — including an instant
// that arrives in another zone and a feed whose entries were handed over in
// another order. A clock read anywhere in here would make the ETag a lie.
func TestRenderIsDeterministic(t *testing.T) {
	feed := sampleFeed()
	// The same instant, expressed in a zone that is not UTC.
	zone := time.FixedZone("UTC+9", 9*3600)
	feed.Updated = feed.Updated.In(zone)
	feed.Entries[0].Updated = feed.Entries[0].Updated.In(zone)

	for _, format := range []Format{FormatAtom, FormatRSS} {
		first, err := Render(feed, format)
		if err != nil {
			t.Fatalf("Render(%s): %v", format, err)
		}
		time.Sleep(2 * time.Millisecond) // a clock that leaked would move
		second, err := Render(feed, format)
		if err != nil {
			t.Fatalf("Render(%s): %v", format, err)
		}
		if !bytes.Equal(first, second) {
			t.Errorf("%s: two renders of one feed differ:\n%s\n---\n%s", format, first, second)
		}
		if strings.Contains(string(first), "+09:00") {
			t.Errorf("%s: the document carries a non-UTC offset; every instant is rendered in UTC", format)
		}
		if !strings.Contains(string(first), "2026-03-04") && !strings.Contains(string(first), "04 Mar 2026") {
			t.Errorf("%s: the instant is not in the document at all:\n%s", format, first)
		}
	}
}

// TestRenderRefusesAnUnknownFormat: a serializer that answered Atom to a
// caller who asked for something else would undo ParseFormat's refusal one
// layer down.
func TestRenderRefusesAnUnknownFormat(t *testing.T) {
	for _, format := range []Format{"", "json", "xml", "ATOM"} {
		if _, err := Render(sampleFeed(), format); err == nil {
			t.Errorf("Render(%q) succeeded, want a refusal", format)
		} else if !errors.Is(err, ErrValidation) {
			t.Errorf("Render(%q) error = %v, want ErrValidation", format, err)
		}
	}
}

func firstLine(doc []byte) string {
	if i := bytes.IndexByte(doc, '\n'); i >= 0 {
		return string(doc[:i])
	}
	return string(doc)
}
