package feeds

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The disclosure rules of a public feed, tested as pure functions: these are
// the tests that try to BREAK the two acceptance criteria of T1004 —
// 未登录可订阅 public feed (a public project's feed renders for a caller who
// is nobody) and private 无 feed (no target whose project is private has one,
// and no entry whose own visibility is private is ever rendered) — rather
// than the happy path that shows a feed exists.

const (
	testProject   = "3f1b0c2e-9a44-4c1d-8b7e-2f5a6d0e1c33"
	testObject    = "8c7d6e5f-1a2b-4c3d-9e8f-0a1b2c3d4e5f"
	testAssetID   = "0123456789abcdefghjkmnpqrs"
	testVersionID = "1d0e2f3a-4b5c-4d6e-8f90-a1b2c3d4e5f6"
	testPubID     = "9a8b7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d"
)

var (
	testBase    = Options{BaseURL: "https://post.example.org"}
	testInstant = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
)

func projectTarget() Target   { return Target{Kind: KindProject, ID: testProject} }
func assetTarget() Target     { return Target{Kind: KindAsset, ID: testAssetID} }
func knowledgeTarget() Target { return Target{Kind: KindKnowledge, ID: testObject} }

// publicProjectState is a public project holding the given entries.
func publicProjectState(entries ...EntryState) State {
	return State{
		Found:             true,
		Title:             "Open Photocatalysts",
		Subtitle:          "A public research project.",
		ProjectVisibility: VisibilityPublic,
		CreatedAt:         testInstant.Add(-24 * time.Hour),
		Entries:           entries,
	}
}

func assetVersion(id, version, visibility string) EntryState {
	return EntryState{
		Kind:        EntryAssetVersion,
		ID:          id,
		Version:     version,
		AssetPID:    testAssetID,
		SubjectType: "dataset",
		Title:       "Diffraction Data",
		Visibility:  visibility,
		PublishedAt: testInstant,
		Publisher:   "ada",
	}
}

// TestNoFeedWithoutAPublicProject is the second acceptance criterion. Every
// way a target can fail to have a public feed is asserted here, including the
// one a reader produces when it could not resolve a visibility at all (the
// empty string) and a visibility that is merely not the stored vocabulary
// ("PRIVATE", "Public"): the model compares against the stored value exactly,
// so anything else fails CLOSED. The entries are PUBLIC in every case, which
// is the point — nothing about the entries may rescue a target whose project
// is not public.
func TestNoFeedWithoutAPublicProject(t *testing.T) {
	public := []EntryState{assetVersion(testVersionID, "v1.0", VisibilityPublic)}
	cases := []struct {
		name       string
		target     Target
		visibility string
	}{
		{"private project", projectTarget(), VisibilityPrivate},
		{"private asset project", assetTarget(), VisibilityPrivate},
		{"private knowledge project", knowledgeTarget(), VisibilityPrivate},
		{"unresolved visibility", projectTarget(), ""},
		{"uppercase PRIVATE", projectTarget(), "PRIVATE"},
		{"mixed-case Public", projectTarget(), "Public"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := publicProjectState(public...)
			state.ProjectVisibility = tc.visibility
			feed, ok := BuildFeed(tc.target, state, testBase)
			if ok {
				t.Fatalf("BuildFeed returned a feed with %d entries, want none:\n%+v", len(feed.Entries), feed)
			}
			if feed.ID != "" || len(feed.Entries) != 0 || feed.Title != "" {
				t.Errorf("a refused feed is not zero-valued: %+v", feed)
			}
		})
	}

	// A target that does not exist has no feed either, whatever its state
	// claims about visibility.
	if _, ok := BuildFeed(projectTarget(), State{Found: false, ProjectVisibility: VisibilityPublic, Entries: public}, testBase); ok {
		t.Error("an unknown target rendered a feed")
	}
}

// TestPrivateEntriesAreNeverRendered: the reader answers rows RAW (private
// ones included), so the filter is the only thing standing between a stored
// private version and a public document. It is asserted on the model's
// entries AND on the rendered bytes, because a leak that survived Render
// would be a leak that shipped.
func TestPrivateEntriesAreNeverRendered(t *testing.T) {
	state := publicProjectState(
		assetVersion(testVersionID, "v2.0-private", VisibilityPrivate),
		assetVersion(testPubID, "v1.0", VisibilityPublic),
		EntryState{Kind: EntryKnowledgePublication, ID: testPubID, Version: "v3.0",
			Title: "Secret claim", Visibility: VisibilityPrivate, PublishedAt: testInstant},
		EntryState{Kind: EntryAssetVersion, ID: testVersionID, Version: "v4.0-unresolved",
			Title: "Unresolved", Visibility: "", PublishedAt: testInstant},
	)
	feed, ok := BuildFeed(projectTarget(), state, testBase)
	if !ok {
		t.Fatal("a public project with a public version must have a feed")
	}
	if len(feed.Entries) != 1 || feed.Entries[0].Version != "v1.0" {
		t.Fatalf("entries = %+v, want exactly the public v1.0", feed.Entries)
	}
	doc, err := Render(feed, FormatAtom)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, leak := range []string{"v2.0-private", "v4.0-unresolved", "Secret claim", testVersionID} {
		if strings.Contains(string(doc), leak) {
			t.Errorf("the rendered document contains %q — a non-public row reached the network:\n%s", leak, doc)
		}
	}
	if !strings.Contains(string(doc), "v1.0") {
		t.Errorf("the rendered document does not contain the public version:\n%s", doc)
	}
}

// TestNothingPublishedMeansNoFeedForAssetAndKnowledge: an asset whose every
// version is private, and a knowledge object that was never published, have
// no feed — while a public project with nothing published IS one (its page
// exists, so its feed may be empty; see the package doc).
func TestNothingPublishedMeansNoFeedForAssetAndKnowledge(t *testing.T) {
	privateOnly := State{
		Found:             true,
		Title:             "Diffraction Data",
		ProjectVisibility: VisibilityPublic,
		CreatedAt:         testInstant,
		Entries:           []EntryState{assetVersion(testVersionID, "v1.0", VisibilityPrivate)},
	}
	for _, target := range []Target{assetTarget(), knowledgeTarget()} {
		if _, ok := BuildFeed(target, privateOnly, testBase); ok {
			t.Errorf("%s with nothing public rendered a feed", target.Kind)
		}
	}
	// The same state, with nothing at all, is the same answer.
	for _, target := range []Target{assetTarget(), knowledgeTarget()} {
		state := privateOnly
		state.Entries = nil
		if _, ok := BuildFeed(target, state, testBase); ok {
			t.Errorf("empty %s rendered a feed", target.Kind)
		}
	}

	empty := State{Found: true, Title: "Open Photocatalysts", ProjectVisibility: VisibilityPublic, CreatedAt: testInstant}
	feed, ok := BuildFeed(projectTarget(), empty, testBase)
	if !ok {
		t.Fatal("a public project with no publications must still have a feed")
	}
	if len(feed.Entries) != 0 {
		t.Errorf("the empty feed has entries: %+v", feed.Entries)
	}
	if !feed.Updated.Equal(testInstant) {
		t.Errorf("empty feed updated = %s, want the entity's creation time %s", feed.Updated, testInstant)
	}
}

// TestStableIdsAndVersionLinks: the requirement's second half. An entry's id
// and its version link are derived from what never changes — the row's uuid
// and the version's own label — so they survive a rename and a new title. The
// test changes everything mutable and asserts the identities did not move.
func TestStableIdsAndVersionLinks(t *testing.T) {
	entry := assetVersion(testVersionID, "v1.0", VisibilityPublic)
	feed, ok := BuildFeed(assetTarget(), State{
		Found: true, Title: "Diffraction Data", ProjectVisibility: VisibilityPublic,
		CreatedAt: testInstant, Entries: []EntryState{entry},
	}, testBase)
	if !ok {
		t.Fatal("a public asset with a public version must have a feed")
	}
	if len(feed.Entries) != 1 {
		t.Fatalf("entries = %+v, want one", feed.Entries)
	}
	got := feed.Entries[0]
	if want := "urn:post:asset-version:" + testVersionID; got.ID != want {
		t.Errorf("entry id = %q, want %q (the row's own uuid, nothing else)", got.ID, want)
	}
	if want := "https://post.example.org/assets/" + testAssetID + "/v1.0"; got.Link != want {
		t.Errorf("entry link = %q, want %q", got.Link, want)
	}
	if want := "urn:post:feed:asset:" + testAssetID; feed.ID != want {
		t.Errorf("feed id = %q, want %q", feed.ID, want)
	}
	if want := "https://post.example.org/assets/" + testAssetID; feed.Link != want {
		t.Errorf("feed link = %q, want %q", feed.Link, want)
	}

	// The same row, described by different mutable data, renders the same
	// identities: identity does not depend on the title, the type, or the
	// publisher.
	other := entry
	other.Title = "Diffraction Data (renamed)"
	other.SubjectType = "material_collection"
	other.Publisher = "someone-else"
	rebuilt, ok := BuildFeed(assetTarget(), State{
		Found: true, Title: other.Title, ProjectVisibility: VisibilityPublic,
		CreatedAt: testInstant, Entries: []EntryState{other},
	}, testBase)
	if !ok {
		t.Fatal("the rebuilt feed vanished")
	}
	if rebuilt.Entries[0].ID != got.ID || rebuilt.Entries[0].Link != got.Link {
		t.Errorf("identity moved with mutable data: %q/%q, want %q/%q",
			rebuilt.Entries[0].ID, rebuilt.Entries[0].Link, got.ID, got.Link)
	}

	// A knowledge publication identifies itself the same way, under its own
	// urn namespace — and carries NO link, because no route renders a
	// knowledge publication (feed.go entryLink). The identity is the urn; the
	// link is the part that may not be invented. The asset assertions above
	// are the control: an implementation that simply cleared every link would
	// satisfy this half of the test and fail that one.
	knowledge, ok := BuildFeed(knowledgeTarget(), State{
		Found: true, Title: "A claim", ProjectVisibility: VisibilityPublic, CreatedAt: testInstant,
		Entries: []EntryState{{Kind: EntryKnowledgePublication, ID: testPubID, Version: "v2.0",
			Title: "A claim", Visibility: VisibilityPublic, PublishedAt: testInstant}},
	}, testBase)
	if !ok {
		t.Fatal("a public knowledge object with a publication must have a feed")
	}
	if want := "urn:post:knowledge-publication:" + testPubID; knowledge.Entries[0].ID != want {
		t.Errorf("knowledge entry id = %q, want %q", knowledge.Entries[0].ID, want)
	}
	if want := "urn:post:feed:knowledge:" + testObject; knowledge.ID != want {
		t.Errorf("knowledge feed id = %q, want %q (identity is not derived from a route)", knowledge.ID, want)
	}
	if got := knowledge.Entries[0].Link; got != "" {
		t.Errorf("knowledge entry link = %q, want none: no route renders a knowledge publication, and "+
			"a link to a route that does not exist is a lie in the shape of an href", got)
	}
	if got := knowledge.Link; got != "" {
		t.Errorf("knowledge feed link = %q, want none: there is no /knowledge route", got)
	}
}

// TestEntriesAreNewestFirstAndTotallyOrdered: a feed is read newest first,
// and two entries published in the same instant still have one order — the
// property the transport's ETag and every reader's "what is new" depend on.
func TestEntriesAreNewestFirstAndTotallyOrdered(t *testing.T) {
	older := assetVersion(testVersionID, "v0.9", VisibilityPublic)
	older.PublishedAt = testInstant.Add(-time.Hour)
	tieA := assetVersion("aaaaaaaa-2222-4333-8444-555566667777", "v1.0", VisibilityPublic)
	tieB := assetVersion("bbbbbbbb-2222-4333-8444-555566667777", "v1.1", VisibilityPublic)
	tieA.PublishedAt, tieB.PublishedAt = testInstant, testInstant

	feed, ok := BuildFeed(projectTarget(), publicProjectState(older, tieB, tieA), testBase)
	if !ok {
		t.Fatal("no feed")
	}
	got := []string{feed.Entries[0].Version, feed.Entries[1].Version, feed.Entries[2].Version}
	want := []string{"v1.0", "v1.1", "v0.9"} // the tie broken by id ASC, then the older row
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
	if !feed.Updated.Equal(testInstant) {
		t.Errorf("feed updated = %s, want the newest entry's instant %s", feed.Updated, testInstant)
	}
	// The same state in a different input order renders the same order:
	// nothing here may depend on how the reader happened to return rows.
	shuffled, ok := BuildFeed(projectTarget(), publicProjectState(tieA, older, tieB), testBase)
	if !ok {
		t.Fatal("no feed")
	}
	for i := range want {
		if shuffled.Entries[i].ID != feed.Entries[i].ID {
			t.Errorf("the input order changed the rendered order at position %d", i)
		}
	}
}

// TestFeedLengthIsBounded: an anonymous read may not be an unbounded one
// (docs/27 §"unbounded anonymous read"). The cap keeps the NEWEST entries.
func TestFeedLengthIsBounded(t *testing.T) {
	entries := make([]EntryState, 0, 120)
	for i := 0; i < 120; i++ {
		e := assetVersion(fmt.Sprintf("00000000-0000-4000-8000-%012x", i), fmt.Sprintf("v1.%d", i), VisibilityPublic)
		e.PublishedAt = testInstant.Add(-time.Duration(i) * time.Minute)
		entries = append(entries, e)
	}
	feed, ok := BuildFeed(projectTarget(), publicProjectState(entries...), testBase)
	if !ok {
		t.Fatal("no feed")
	}
	if len(feed.Entries) != DefaultMaxEntries {
		t.Fatalf("entries = %d, want the default cap %d", len(feed.Entries), DefaultMaxEntries)
	}
	if feed.Entries[0].Version != "v1.0" {
		t.Errorf("the capped feed dropped the newest entry: first is %q", feed.Entries[0].Version)
	}
	small, ok := BuildFeed(projectTarget(), publicProjectState(entries...),
		Options{BaseURL: testBase.BaseURL, MaxEntries: 3})
	if !ok {
		t.Fatal("no feed")
	}
	if len(small.Entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(small.Entries))
	}
	if small.Entries[0].Version != "v1.0" {
		t.Errorf("the 3-entry feed dropped the newest entry: first is %q", small.Entries[0].Version)
	}
}

// TestEntriesWithoutIdentityAreDropped: a row that cannot name itself gets no
// entry — there is no stable id to publish and no version page to link to,
// and inventing one (an index, a timestamp) would be a link that rots.
func TestEntriesWithoutIdentityAreDropped(t *testing.T) {
	noID := assetVersion("", "v1.0", VisibilityPublic)
	noVersion := assetVersion(testVersionID, "", VisibilityPublic)
	good := assetVersion(testPubID, "v1.0", VisibilityPublic)
	feed, ok := BuildFeed(projectTarget(), publicProjectState(noID, noVersion, good), testBase)
	if !ok {
		t.Fatal("no feed")
	}
	if len(feed.Entries) != 1 || feed.Entries[0].ID != "urn:post:asset-version:"+testPubID {
		t.Fatalf("entries = %+v, want only the row that has an id and a version", feed.Entries)
	}
}

// TestParseTargetShapes: the one place a target is built from input. A shape
// that could never name a stored row is refused here, before a query could
// cast it (a bad uuid reaching PostgreSQL is SQLSTATE 22P02 — a 500 for what
// is a client's mistake).
func TestParseTargetShapes(t *testing.T) {
	good := []struct {
		kind, id string
		want     Target
	}{
		{"project", testProject, Target{Kind: KindProject, ID: testProject}},
		{"project", strings.ToUpper(testProject), Target{Kind: KindProject, ID: testProject}},
		{"asset", testAssetID, Target{Kind: KindAsset, ID: testAssetID}},
		{"knowledge", testObject, Target{Kind: KindKnowledge, ID: testObject}},
	}
	for _, tc := range good {
		got, err := ParseTarget(tc.kind, tc.id)
		if err != nil {
			t.Errorf("ParseTarget(%q, %q) = error %v", tc.kind, tc.id, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseTarget(%q, %q) = %+v, want %+v", tc.kind, tc.id, got, tc.want)
		}
	}

	bad := []struct{ kind, id string }{
		{"project", "not-a-uuid"},
		{"project", ""},
		{"knowledge", "1f"},
		{"asset", testProject},                  // a uuid is not a pid
		{"asset", ""},                           // no id at all
		{"asset", strings.ToUpper(testAssetID)}, // a pid is lowercase by construction
		{"asset", testAssetID + "s"},            // the wrong length
		{"asset", "0123456789abcdefghjkmnpqru"}, // 'u' is not Crockford base32
		{"feed", testProject},                   // a kind this surface does not have
		{"", testProject},                       // no kind at all
		{"Project", testProject},                // the kind is a path segment, not prose
	}
	for _, tc := range bad {
		got, err := ParseTarget(tc.kind, tc.id)
		if err == nil {
			t.Errorf("ParseTarget(%q, %q) = %+v, want an error", tc.kind, tc.id, got)
			continue
		}
		if !errors.Is(err, ErrValidation) {
			t.Errorf("ParseTarget(%q, %q) error = %v, want ErrValidation", tc.kind, tc.id, err)
		}
	}
}

// TestParseFormatAndAccept: the two ways a format is chosen. A typo is
// refused — the caller is told it cannot have what it asked for — while an
// absent or wildcard Accept means the default.
func TestParseFormatAndAccept(t *testing.T) {
	for _, in := range []string{"", "atom", "ATOM", " atom "} {
		if got, ok := ParseFormat(in); !ok || got != FormatAtom {
			t.Errorf("ParseFormat(%q) = (%q, %v), want atom", in, got, ok)
		}
	}
	if got, ok := ParseFormat("rss"); !ok || got != FormatRSS {
		t.Errorf("ParseFormat(rss) = (%q, %v)", got, ok)
	}
	for _, in := range []string{"json", "xml", "rss2", "atom,rss"} {
		if got, ok := ParseFormat(in); ok {
			t.Errorf("ParseFormat(%q) = %q, want a refusal", in, got)
		}
	}

	accepts := []struct {
		header string
		want   Format
		ok     bool
	}{
		{"application/atom+xml", FormatAtom, true},
		{"application/rss+xml", FormatRSS, true},
		{"application/rss+xml;q=0.9, application/xml;q=0.5", FormatRSS, true},
		{"application/atom+xml,application/xml;q=0.9", FormatAtom, true},
		{"text/html,*/*", "", false},
		{"", "", false},
		{"application/atom+xml, application/rss+xml", "", false}, // no preference is not a preference
	}
	for _, tc := range accepts {
		got, ok := ParseAccept(tc.header)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ParseAccept(%q) = (%q, %v), want (%q, %v)", tc.header, got, ok, tc.want, tc.ok)
		}
	}

	for _, f := range []Format{FormatAtom, FormatRSS} {
		if !f.Valid() {
			t.Errorf("%q is not Valid", f)
		}
		if ct := f.ContentType(); !strings.Contains(ct, "charset=utf-8") {
			t.Errorf("%q ContentType = %q, want a stated charset", f, ct)
		}
	}
	if Format("json").Valid() {
		t.Error(`Format("json").Valid() = true`)
	}
}
