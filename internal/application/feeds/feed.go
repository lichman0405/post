package feeds

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lichman0405/post/internal/application/knowledgepublish"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rights"
)

// Visibility is the stored project-visibility vocabulary (the projects
// table's CHECK, docs/12 §2). The empty string is neither value and is
// treated as not-public: a reader that could not resolve a project's
// visibility must fail closed, never guess.
const (
	VisibilityPublic  = "public"
	VisibilityPrivate = "private"
)

// Kind is the entity family a feed is about. The three values are the ones
// the requirement names ("public Project/Asset/Knowledge feeds"), and they
// are also the identity a reader subscribes to: a project uuid, an asset
// pid, a scientific-object uuid.
type Kind string

const (
	KindProject   Kind = "project"
	KindAsset     Kind = "asset"
	KindKnowledge Kind = "knowledge"
)

// parseKind maps a URL segment to a Kind.
func parseKind(segment string) (Kind, bool) {
	switch Kind(segment) {
	case KindProject, KindAsset, KindKnowledge:
		return Kind(segment), true
	default:
		return "", false
	}
}

// EntryKind distinguishes what a feed entry points at. Exactly two kinds
// exist, both immutable published versions: an asset version row and a
// knowledge publication row.
type EntryKind string

const (
	// EntryAssetVersion is one research_asset_versions row (the unit of an
	// asset's version history, T0705).
	EntryAssetVersion EntryKind = "asset_version"
	// EntryKnowledgePublication is one knowledge_publications row: what
	// "published to the network" means for a scientific object (docs/12 §2).
	EntryKnowledgePublication EntryKind = "knowledge_publication"
)

// Target addresses one feed: the family and the entity's stable identity —
// a project uuid, an asset pid (never a slug: internal/assets/url.go), or a
// scientific-object uuid.
type Target struct {
	Kind Kind
	ID   string
}

// ParseTarget validates one request's target from its URL segments. It is
// the only place a target is built from input, so an id whose shape could
// never name a stored row is refused as malformed (ErrValidation) rather
// than carried into a query that would cast it — the database would answer
// a bad uuid text with SQLSTATE 22P02, which is a 500 for what is a client
// error.
//
// A conforming uuid is lowercased: the canonical form is what the feed's own
// id is built from, and two spellings of one uuid are one entity.
func ParseTarget(kindSegment, id string) (Target, error) {
	kind, ok := parseKind(kindSegment)
	if !ok {
		return Target{}, fmt.Errorf("%w: unknown feed kind %q", ErrValidation, kindSegment)
	}
	if id == "" {
		return Target{}, fmt.Errorf("%w: missing %s id", ErrValidation, kind)
	}
	switch kind {
	case KindAsset:
		if !assets.ValidPID(id) {
			return Target{}, fmt.Errorf("%w: asset id must be a pid (26 Crockford base32 characters)", ErrValidation)
		}
		return Target{Kind: kind, ID: id}, nil
	default:
		if !domain.ValidUUID(id) {
			return Target{}, fmt.Errorf("%w: %s id must be a uuid", ErrValidation, kind)
		}
		return Target{Kind: kind, ID: strings.ToLower(id)}, nil
	}
}

// EntryState is one raw entry row as the reader found it: the version's
// identity, what it is a version of, and the fact that decides whether it
// may be rendered.
//
// It is checked HERE, and not merely trusted from the reader: the persistence
// layer's queries already filter what a public feed may render, but that
// filter is a read strategy that a query change could drop, and this is the
// rule. A private row therefore cannot reach a document by way of a query
// nobody re-read (internal/assets draws the same line for the asset page).
type EntryState struct {
	Kind EntryKind
	// ID is the row's own uuid: append-only, never reused, never edited
	// (migration 00014). It is the entry's stable identity.
	ID string
	// Version is the published version label.
	Version string
	// AssetPID is the asset's pid for an asset-version entry; empty
	// otherwise. It is what the permanent version URL is built from.
	AssetPID string
	// SubjectType is the stored asset_type / object_type (dataset, claim,
	// …); the entry's sentence names it.
	SubjectType string
	// Title is the title of the versioned thing (the asset title, the
	// object version's title).
	Title string
	// Visibility is the row's OWN stored visibility. An asset version
	// carries its own (research_asset_versions.visibility). A knowledge
	// publication has no visibility column of its own, so what it carries
	// here is the visibility of the PROJECT that owns the object — one of
	// the three inputs knowledgepublish.AudienceFor decides with, and never
	// the answer on its own (a members-only publication inside a public
	// project is legal; docs/12 §2, 发布不等于公开).
	Visibility string
	// VisibilityPolicyID is the version's own visibility axis
	// (scientific_object_versions.visibility_policy_id), nil when the
	// version inherits the project's preset. It is set for knowledge rows
	// and is one of the audience rule's three inputs; the reader reports it
	// so the model re-checks the axis rather than trusting a predicate.
	VisibilityPolicyID *string
	// Rights is the rights declaration the publication stores, and
	// RightsValid whether this build could READ it. An unreadable document
	// states no metadata token, so the audience rule refuses it — the
	// fail-closed direction, and the reason "we could not read this" is a
	// fact the struct carries instead of an accident of zero values.
	//
	// Both are meaningful for knowledge rows only: an asset version has no
	// rights declaration of its own.
	Rights      rights.Document
	RightsValid bool
	// PublishedAt is the publication's timestamp (the feed's clock).
	PublishedAt time.Time
	// Publisher is the publishing account's handle, empty when the reader
	// could not resolve it. A handle is public identity (a live profile is
	// a public read, T0102), so an entry may render it.
	Publisher string
}

// State is one target's read: the entity, the visibility of the project
// that owns it, and the entry rows the reader returned — which are the rows
// a public feed may render, the reader's own filter applied. A private row
// that reached this struct anyway is still dropped by BuildFeed.
type State struct {
	// Found reports whether the target exists at all. False is a state, not
	// an error: the transport answers it the same 404 a non-public target
	// gets.
	Found bool
	// Title is the entity's own name: the project name, the asset title, or
	// — for a knowledge object, which has no title of its own — whatever the
	// reader resolved. BuildFeed names a knowledge feed from its own rows
	// instead (see there): the title a knowledge feed may carry belongs to
	// the newest publication it RENDERS, and which rows those are is this
	// package's decision, not the reader's.
	Title string
	// Subtitle is the entity's own one-line description, when it has one
	// (a project's purpose). Empty renders no subtitle.
	Subtitle string
	// ProjectVisibility is the visibility of the project that owns the
	// target: the project itself, the asset's origin project, or the
	// project the published object belongs to. Empty means "not resolved",
	// which BuildFeed treats as not public.
	ProjectVisibility string
	// CreatedAt is the entity's own creation time. It is the feed's updated
	// stamp when the feed has no entries, so that an empty feed renders the
	// same document on every request (a generation timestamp would not).
	CreatedAt time.Time
	// Entries are the entry rows, newest first or not — BuildFeed sorts.
	Entries []EntryState
}

// Entry is one rendered feed entry: a published version with a stable id and
// a permanent link.
type Entry struct {
	// ID is the entry's stable identity, an IRI derived from the row's
	// immutable uuid and nothing else — not the host, not the version label,
	// not a timestamp.
	ID string
	// Title is the human line a reader shows in its list.
	Title string
	// Link is the absolute URL of the version's permanent page.
	Link string
	// Version is the version label, carried apart from the title so a client
	// can machine-read it.
	Version string
	// Updated is the publication instant, in UTC.
	Updated time.Time
	// Summary is the entry's one-line description.
	Summary string
	// Author is the publishing account's handle, empty when unknown. An
	// empty author renders no author element, never an empty one.
	Author string
}

// Feed is one rendered feed document's content: the target's identity and
// its newest published versions.
type Feed struct {
	// ID is the feed's stable identity (urn:post:feed:<kind>:<target id>).
	ID string
	// Kind is the entity family, carried for the rendered document's own
	// description of itself.
	Kind Kind
	// Title is the entity's name.
	Title string
	// Subtitle is the entity's one-line description, "" when it has none.
	Subtitle string
	// Link is the absolute URL of the entity's own page — the feed's
	// alternate.
	Link string
	// Updated is the newest entry's instant, or the entity's creation time
	// when there are no entries. It is never the time of the request: two
	// requests over one state must render the same document.
	Updated time.Time
	// Entries are the renderable entries, newest first.
	Entries []Entry
}

// DefaultMaxEntries caps a feed's length. A feed is a window on the newest
// output, not an archive: the archive is the entity's own page, and an
// unbounded anonymous read is what docs/27 rules out.
const DefaultMaxEntries = 50

// Options carries what BuildFeed needs that is not the state: where the
// public pages live, and how long a feed may be.
type Options struct {
	// BaseURL is the public origin the entry links are built from, without a
	// trailing slash (e.g. https://post.example.org or
	// http://127.0.0.1:3000). It is the configured web origin — the pages
	// the links point at are the web app's — never the request's Host
	// header, which a client controls.
	BaseURL string
	// MaxEntries bounds the feed; 0 means DefaultMaxEntries.
	MaxEntries int
}

func (o Options) maxEntries() int {
	if o.MaxEntries <= 0 {
		return DefaultMaxEntries
	}
	return o.MaxEntries
}

// BuildFeed decides what one target's feed is, and whether it exists at all.
//
// It returns ok=false — the transport's 404 — for exactly three states, all
// of them "there is no such public feed" from outside:
//
//	the target does not exist
//	the project that owns it is not public (including "could not be read")
//	an asset or knowledge target has nothing published
//
// The three answer alike on purpose (docs/45): telling them apart would let
// an anonymous caller enumerate private projects and private versions by
// diffing the answers. A public project with no publications is NOT one of
// them — see the package doc.
func BuildFeed(target Target, state State, opts Options) (Feed, bool) {
	if !state.Found {
		return Feed{}, false
	}
	if state.ProjectVisibility != VisibilityPublic {
		return Feed{}, false
	}

	rows := renderableRows(state)
	if len(rows) == 0 && target.Kind != KindProject {
		// Nothing published: an asset whose every version is private, a
		// knowledge object that was never published. No feed.
		return Feed{}, false
	}

	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if !a.PublishedAt.Equal(b.PublishedAt) {
			return a.PublishedAt.After(b.PublishedAt)
		}
		return entryID(a) < entryID(b)
	})
	if max := opts.maxEntries(); len(rows) > max {
		rows = rows[:max]
	}

	entries := make([]Entry, 0, len(rows))
	for _, e := range rows {
		entries = append(entries, renderEntry(e, opts))
	}

	title := state.Title
	if target.Kind == KindKnowledge && len(rows) > 0 {
		// A knowledge object has no title of its own — the title lives on
		// each version — so the feed is named by the newest publication the
		// feed actually CARRIES, which is the newest row the rule above just
		// admitted. Naming it from the newest publication instead would let a
		// members-only revision published above an open one supply the
		// <title> of a public document: the same disclosure the entry filter
		// makes, one field over.
		title = rows[0].Title
	}

	feed := Feed{
		ID:       feedID(target),
		Kind:     target.Kind,
		Title:    title,
		Subtitle: state.Subtitle,
		Link:     feedLink(target, opts.BaseURL),
		Updated:  state.CreatedAt.UTC(),
		Entries:  entries,
	}
	if len(entries) > 0 {
		feed.Updated = entries[0].Updated
	}
	return feed, true
}

// renderableRows keeps the rows a public feed may carry, in the order they
// arrived (the caller sorts). A row that cannot name itself is dropped: it
// has no stable id to publish and no version page to link to.
//
// The reader already filters, so in production this drops nothing the
// reader's SQL could decide; it is the rule, and it is what makes a query
// change unable to publish a non-public row on its own.
func renderableRows(state State) []EntryState {
	out := make([]EntryState, 0, len(state.Entries))
	for _, e := range state.Entries {
		if !rowRenderable(e) {
			continue
		}
		if e.ID == "" || e.Version == "" {
			continue
		}
		out = append(out, e)
	}
	return out
}

// rowRenderable reports whether ONE row may appear in a public feed: the
// disclosure rule, applied to whatever the reader returned.
//
// An asset version answers with its own stored visibility, and no visibility
// at all is dropped — the fail-closed direction a reader that could not
// answer must produce.
//
// A knowledge publication answers with knowledgepublish.AudienceFor over the
// three inputs the row carries: the version's visibility axis, the owning
// project's preset and the published rights document. It is the SAME rule the
// publication's own page decides with (cmd/api/knowledgehttp), so a feed and
// a page cannot come to different conclusions about one publication — which
// is what the feeds used to do, by taking "a publication's visibility IS its
// project's" for the rule and serving a members-only publication of a public
// project to the network. A declaration this build cannot read is not public
// either: RightsValid false is the model's own statement that the document
// states no token we resolve, and AudienceFor would refuse it anyway.
func rowRenderable(e EntryState) bool {
	if e.Kind == EntryKnowledgePublication {
		if !e.RightsValid {
			return false
		}
		return knowledgepublish.AudienceFor(e.Visibility, e.VisibilityPolicyID, e.Rights) ==
			knowledgepublish.AudienceNetwork
	}
	return e.Visibility == VisibilityPublic
}

// renderEntry renders one admitted row as the entry a feed carries.
func renderEntry(e EntryState, opts Options) Entry {
	return Entry{
		ID:      entryID(e),
		Title:   entryTitle(e),
		Link:    entryLink(e, opts.BaseURL),
		Version: e.Version,
		Updated: e.PublishedAt.UTC(),
		Summary: entrySummary(e),
		Author:  e.Publisher,
	}
}

// entryID is the stable IRI of one published version. It names the row's
// immutable uuid and nothing else.
func entryID(e EntryState) string {
	switch e.Kind {
	case EntryKnowledgePublication:
		return "urn:post:knowledge-publication:" + e.ID
	default:
		return "urn:post:asset-version:" + e.ID
	}
}

// feedID is the stable IRI of one feed. Like an entry id it is host-free:
// two callers on two hosts read the same feed, and it keeps its identity.
func feedID(t Target) string {
	return "urn:post:feed:" + string(t.Kind) + ":" + t.ID
}

// entryTitle is the line a reader's list shows: the thing's title and the
// version label it was published under.
func entryTitle(e EntryState) string {
	title := strings.TrimSpace(e.Title)
	if title == "" {
		// A version whose subject could not be named still has an honest
		// title: the version label itself.
		return e.Version
	}
	return title + " " + e.Version
}

// entrySummary is the entry's one-line description. It says what was
// published, in the platform's own vocabulary, and nothing that is not
// already public: the version label, the subject's title and type, and the
// publisher's handle when the reader resolved one.
func entrySummary(e EntryState) string {
	what := "version " + e.Version
	if e.SubjectType != "" {
		what = humanType(e.SubjectType) + " version " + e.Version
	}
	summary := "Published " + what
	if title := strings.TrimSpace(e.Title); title != "" {
		summary += " “" + title + "”"
	}
	summary += "."
	if e.Publisher != "" {
		summary += " Published by " + e.Publisher + "."
	}
	return summary
}

// humanType renders a stored type slug ("research_question",
// "material_collection") as the words it is made of. The stored value is a
// machine identifier and stays one everywhere else; a feed's sentence is
// read by people.
func humanType(t string) string {
	return strings.ReplaceAll(t, "_", " ")
}

// feedLink is the absolute URL of the entity's own public page: the feed's
// alternate. The asset path is internal/assets' own builder (the single
// source of the persistent URL scheme); the project path is the web app's
// route (apps/web/lib/entity-meta.ts, docs/05 §3).
//
// The knowledge feed's link is EMPTY, and deliberately so: there is no
// /knowledge route. T0805 owns the published knowledge object's surface and
// its id scheme ("fixed version/rights/PID-like id"), and the repository says
// so in as many words — apps/web/app/sitemap.ts ("there is no knowledge route
// at all yet") and internal/application/explore/index.go ("a link to a route
// that does not exist is a lie in the shape of an href"). A feed that pointed
// there would hand every subscriber a 404 dressed as an address, so the
// document carries the feed's IDENTITY instead — the urn feedID builds from
// the object's uuid — and no URL at all. When T0805 mints the public id, the
// link is what moves; nothing that identifies a feed or an entry does,
// because neither is derived from a route.
func feedLink(t Target, base string) string {
	path := entityPath(t)
	if path == "" {
		return ""
	}
	return base + path
}

// entryLink is the absolute URL of one entry's version page, or "" when the
// platform has no page for it.
//
// An asset version has a permanent address by construction
// (assets.AssetVersionURL: pid + the immutable version label). A knowledge
// publication has NO address: no route renders one, and the obvious guess
// would not even be unambiguous — public_version is unique per object VERSION,
// not per object (UNIQUE(object_version_id, public_version), migration
// 00010), so /knowledge/{object}/{version} could name two different rows.
// Rather than mint a link that is either a 404 or a guess, a knowledge entry
// is identified by its urn and carries no link (see feedLink).
func entryLink(e EntryState, base string) string {
	switch e.Kind {
	case EntryKnowledgePublication:
		return ""
	default:
		return base + assets.AssetVersionURL(assets.PID(e.AssetPID), e.Version)
	}
}

// entityPath is the web path of the target's own page, or "" when the
// platform renders no such page — which the caller must treat as "there is
// no URL", never as a path that begins with a slash.
func entityPath(t Target) string {
	switch t.Kind {
	case KindAsset:
		return assets.AssetURL(assets.PID(t.ID))
	case KindKnowledge:
		// No /knowledge route exists (see feedLink).
		return ""
	default:
		return "/projects/" + t.ID
	}
}
