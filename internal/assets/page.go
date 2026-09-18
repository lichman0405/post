package assets

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/lichman0405/post/internal/domain"
)

// The research asset page (T0709): the public/authorized read model of
// docs/42 §Asset Page, which fixes its content verbatim — "PID/version、
// type、origin、rights、creators、metadata、dependencies、lineage、
// used/derived public links、versions、network events". Eleven items, and
// this file renders one block per item, in that order:
//
//	Asset        PID, type, title, slug, the originating project  (PID/type)
//	Version      the rendered version's label and its published facts (version)
//	Origin       the version's origin refs, resolved                (origin)
//	Rights       the stored rights document, verbatim               (rights)
//	Creators     the credited parties of the version, kind and role  (creators)
//	Metadata     the manifest's metadata block                      (metadata)
//	Dependencies the manifest's dependency pins, resolved           (dependencies)
//	Lineage      the fork/derive edges of the rendered version      (lineage)
//	UsedBy       the public usages of the rendered version          (used/derived links)
//	Versions     every version visible to the reader                (versions)
//	Events       the research events recorded about the asset       (network events)
//
// # What may be rendered, and about whom
//
// An asset page is the asset network's PUBLIC surface (specs/ui/
// page-inventory.csv: "/assets/{id} … optional if public"), and a public
// page is where docs/23 §5 is strictest: "所有 query/search/export/
// download 进行 tenant/project/object policy 过滤。私有对象计数也不能
// 通过 public API 泄漏" — not the objects, and not a count that would let
// a reader infer one. The page therefore renders an entry only when the
// thing it names is public, and it omits the entry entirely rather than
// rendering a placeholder: "there is one more, you may not see it" is the
// count this rule exists to refuse.
//
// # The entry rule, and why the ref strings are entries too
//
// Five blocks can name something outside this asset, and all five follow
// one rule: an entry is rendered only when EVERY entity it names may be
// rendered, and an entry that fails is dropped rather than blanked.
//
//	used_by      a project's usage of this version   (the project's identity)
//	lineage      a fork/derive edge                  (the counterpart version)
//	dependencies a pinned version                    (the pinned version)
//	origin       an origin ref of the version        (the referenced entity)
//	asset        the asset's own originating project (the project's identity)
//
// The first three name a version or a project they resolve — those are
// dropped when the resolved side is unreadable. The last two carry a
// STRING the publisher stored — "project:<uuid>", "release:<uuid>",
// "<pid>@<version>" — and that string IS the entity's name, so it is
// disclosed by the page exactly as a title would be: publishing the ref
// bytes puts another entity's identifier on a public page. A page that
// printed "a dependency on something you may not see" (or the pin's uuid)
// would be the count, or the identity, docs/23 §5 forbids — and the
// publisher's own manifest is not a licence to hand out a third party's
// identity, because the page is served to the network, not to the
// publisher's own project.
//
// This is where the page's rule is stricter than the publish preview's,
// which renders the ref bytes beside a withheld name
// (internal/assets.previewRef) — and deliberately so: the preview's caller
// TYPED those refs into its own candidate moments earlier, so printing them
// back discloses nothing the caller does not hold, while this page's reader
// arrives holding nothing and learns the uuid from the page.
//
// For a version — the thing lineage edges and dependency pins name — "may
// be rendered" is both axes, and mayLinkVersion is the one predicate that
// decides it.
//
// The identity rule (T0712, issue #238 — the same class of leak, reached
// through a page instead of a preview): a project's id, its slug, its
// name and its visibility are rendered by exactly two readers —
//
//   - a PUBLIC project (docs/12 §2: "Public Project：accepted RSG 与公开
//     研发历史可见"). Any caller may read it through the project's own
//     routes, so printing it here discloses nothing.
//   - the asset's OWN originating project, to a caller who is a MEMBER of
//     it. A member may already read that project (that is how they got to
//     this page), so its identity is their own business; a private
//     project may explicitly publish an asset (docs/12 §2), and its own
//     members are who that publication is for.
//
// Everyone else's project is withheld — including a project the caller
// happens to belong to when it is not the asset's origin: the rule is
// about the entity being rendered, not about who is asking, which is what
// keeps one request from answering differently for two callers about the
// same third party (the preview's mayRenderProject makes the identical
// choice).
//
// An EMPTY visibility is not public. A reader that could not determine a
// project's visibility leaves the value "" and the identity is withheld,
// which is the fail-closed direction a reader that could not look must
// produce.
//
// # Blobs
//
// The page renders NO blob URL, for any blob, in any case. docs/17 §5
// makes every download check the project/object/blob policy at fetch time
// and docs/17 §3 records that public metadata is not open download; today
// the platform has no blob download route at all (cmd/api/fileshttp
// registers five reads, none of them a blob fetch), and "the bytes cannot
// be fetched" is TRUE here because there is no such route rather than
// because a page declined to print a link. A page that printed a URL
// nobody serves, or that hid a URL to bytes a route did serve, would both
// be worse than this: the first is a dead link, the second is the
// front-end hiding docs/23 §3 forbids. The manifest's blob identifiers
// appear where the publisher declared them — inside metadata, as the
// strings they are — and nowhere else.

// PageViewer is who is reading the page, as the read decision needs it.
// It is deliberately one bit wide: the project read gate has already
// answered "may this caller read the asset's project at all" (a caller
// who may not is answered 404 by the transport, indistinguishable from an
// unknown pid), and the only question left is whether the caller is a
// MEMBER of it — the difference between reading the network's view of the
// asset and reading the project's own.
type PageViewer struct {
	// Member is true when the caller belongs to the asset's originating
	// project. A member sees every version of the asset, including the
	// private ones; everyone else sees the public versions only.
	Member bool
}

// PageProject is one project identity the page may print, in the shape
// the project surface publishes (cmd/api/projectshttp projectPayload) so
// a reader recognizes it.
type PageProject struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Slug       string `json:"slug"`
	Visibility string `json:"visibility"`
}

// PageAsset is the asset half: the persistent identity (docs/11 §2 —
// every asset has a persistent ID) and the display fields, plus the
// originating project when the rule above allows it.
type PageAsset struct {
	PID  PID  `json:"pid"`
	Type Type `json:"type"`
	// Title and Slug are the asset's display metadata (docs/11 §4: they
	// are revisable and are not part of the identity).
	Title string `json:"title"`
	Slug  string `json:"slug"`
	// OriginProject is nil when the project's identity is withheld —
	// never an object with empty fields, so a client cannot print the
	// difference between "no project" and "a project you may not see".
	OriginProject *PageProject `json:"origin_project"`
	CreatedAt     time.Time    `json:"created_at"`
}

// PageUser is one user identity, as the member list and the profile
// surface render it.
type PageUser struct {
	UserID      string `json:"user_id"`
	Handle      string `json:"handle"`
	DisplayName string `json:"display_name"`
}

// PageVersionFacts is the rendered version: its immutable label and the
// facts the version row carries. It is the second half of docs/42's
// "PID/version" item — the asset block carries the pid, this carries the
// version — and the two are separate fields so a reader cannot confuse
// one for the other (a version label is not an identity: it is unique per
// asset, not across the network).
type PageVersionFacts struct {
	Version string `json:"version"`
	// URL is the version's persistent web URL (internal/assets/url.go:
	// /assets/{pid}/{version}). The pid half comes from the asset block,
	// so this string is built from both and never from the label alone.
	URL string `json:"url"`
	// Visibility is the stored public/private axis of this version
	// (research_asset_versions.visibility): exactly the two values the
	// column admits (internal/assets.Visibility).
	Visibility Visibility `json:"visibility"`
	// IntegrityHash pins the published content (docs/11 §1/§3). It is a
	// hash of the version's canonical manifest bytes, so a reader can
	// check that what it was served is what was published.
	IntegrityHash string    `json:"integrity_hash"`
	PublishedAt   time.Time `json:"published_at"`
	// PublishedBy is the publishing actor's identity, nil when the user
	// row could not be resolved.
	PublishedBy *PageUser `json:"published_by"`
}

// PageCreator is one credited party of the rendered version (docs/42's
// "creators"; docs/11 §6 separates Creator, Contributor, Custodian,
// Maintainer, Rights Holder and Originating Project and keeps creator
// history forever).
//
// The rows are the version's stored credits
// (asset_version_parties, migration 00082): the ids a publish declared,
// which the gate has always validated and the repository now STORES. A
// party is an identity — a kind and an id TOGETHER, never an id alone
// (domain.Party, and 00082's party_kind beside party_id) — so Kind says
// which table PartyID is a row of, and the two display fields are read
// from that table: a user's handle and display name, or an organization's
// slug and name. There is no entry here, and no field here, that carries
// an id without the kind that says what the id is.
//
// Role is rendered rather than implied. The credit a version declares is
// a creator (or a contributor, custodian or maintainer — the four roles
// 00082 stores), and the party that PUBLISHED the version is a different
// party with a different relationship, rendered as published_by in the
// version block above. This block named the publisher under the role
// "publisher" while no table stored a creator list, and said so in so many
// words; now that the credits are stored it renders them, and a version
// published before 00082 has no credits to render at all (see
// creatorsOf).
type PageCreator struct {
	// Kind is the party's identity kind: a user or an organization. It is
	// never empty, and PartyID is meaningless without it.
	Kind string `json:"kind"`
	// PartyID is the party's row id in the table Kind names.
	PartyID string `json:"party_id"`
	// Handle is a user's handle or an organization's slug.
	Handle string `json:"handle"`
	// DisplayName is a user's display name or an organization's name.
	DisplayName string `json:"display_name"`
	// Role is the credited relationship this row states (domain.Role):
	// "creator" for the credits the publish path writes today.
	Role string `json:"role"`
}

// PageOrganization is one organization identity, resolved for a credited
// party of kind organization (00002's second identity table). It is
// deliberately NOT a PageUser: an organization has no user id, no handle
// and no display name, and rendering one in the user field — or under
// /users/{id} — is the merge those two tables exist to prevent.
type PageOrganization struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// PageMetadata is one metadata key of the manifest, with its declared
// value verbatim. docs/11 §4 keeps this block revisable without a new
// scientific version; what is rendered here is the value stored with THIS
// version, which never changes.
type PageMetadata struct {
	Key   string `json:"key"`
	Value any    `json:"value"`
}

// PageOrigin is one resolved origin ref of the rendered version: where
// the published content came from (docs/11 §3 provenance pin; the four
// V1 kinds are internal/assets.OriginKind).
//
// Ref is the canonical string the immutable version row carries, and it
// carries the referenced entity's uuid — "project:<uuid>",
// "release:<uuid>" — so the ref itself is rendered only when that
// entity's project may be rendered, and the whole entry is dropped when
// it may not (see the entry rule above). A ref the platform cannot
// resolve is dropped too: an unresolvable ref is a defect in the
// repository rather than a public fact, and printing a speculative one
// would invite a reader to fetch something that is not there.
//
// The four fields therefore appear together or not at all, and the block
// is empty when every ref points somewhere the reader may not look — a
// stated absence rather than a redacted list, which is the difference
// between "no origins you may see" and a count.
type PageOrigin struct {
	Ref       string     `json:"ref"`
	Kind      OriginKind `json:"kind"`
	Resolved  bool       `json:"resolved"`
	Title     string     `json:"title,omitempty"`
	ProjectID string     `json:"project_id,omitempty"`
	// Link is a web URL when the platform has a public page for the
	// entity: a project (its Overview) or a release (its Release page,
	// which is project-scoped). States and object versions have no
	// public page in V1, so they carry no link rather than a guess.
	Link string `json:"link,omitempty"`
}

// PageDependency is one dependency pin of the rendered version (docs/11
// §5: a dependency is an exact published version, docs/42's
// "dependencies").
//
// The pin STRING is "<pid>@<version>" (internal/assets.DependencyPin), so
// it names the pinned asset and one of its versions — two identifiers of
// something that is not this asset. It is therefore rendered only when
// that version is one the network can open (mayLinkVersion), and a pin
// that is not is dropped from the block entirely: an entry saying "there is
// a dependency here, on something you may not see" is the count docs/23 §5
// forbids, and the pin's own bytes are the identity.
//
// So this block has no "hidden" case at all — every rendered entry names
// something a reader can go and read, and its Title, Type and URL come with
// it. Resolved reports whether the pin named a stored version at all,
// which is a fact about the publisher's document; Public reports whether
// the reader may open it. The two differ only in that a pin which resolves
// to nothing is still rendered (its own bytes name an asset the platform
// has no row for, which discloses nothing).
type PageDependency struct {
	Pin      string `json:"pin"`
	Resolved bool   `json:"resolved"`
	Public   bool   `json:"public"`
	Title    string `json:"title,omitempty"`
	Type     Type   `json:"type,omitempty"`
	URL      string `json:"url,omitempty"`
}

// Lineage direction, as a reader of the rendered version sees the edge.
const (
	// LineageParent: the rendered version derives from the named version
	// (relation forked_from / derived_from / supersedes, docs/11 §5).
	LineageParent = "parent"
	// LineageChild: the named version derives from the rendered version
	// (the reverse reading of the same edge) — a supersede chain read
	// downwards.
	LineageChild = "child"
)

// PageLineage is one fork/derive edge of the rendered version (docs/42's
// "lineage"; asset_lineage, migration 00010: forked_from / derived_from /
// supersedes).
//
// Only edges whose OTHER END is a version the network can open are
// rendered (mayLinkVersion: public, in a public project), and the
// direction says which end that is. An edge to a version the network
// cannot see is omitted entirely — the same fail-closed rule the used_by
// block follows, and for the same reason: an entry here would name another
// project's private asset, and a "hidden" placeholder would leak its
// count.
type PageLineage struct {
	// Relation is the stored relation type, verbatim.
	Relation string `json:"relation"`
	// Direction is LineageParent or LineageChild.
	Direction string `json:"direction"`
	// Version is the named version's label and PID its asset's persistent
	// identifier; URL is its persistent web URL.
	PID     PID    `json:"pid"`
	Version string `json:"version"`
	URL     string `json:"url"`
	// Title is the named asset's title — rendered because the edge is
	// rendered only for a public counterpart.
	Title string `json:"title"`
}

// PageUsage is one PUBLIC usage of the rendered version (docs/42's
// "used/derived public links"): a project that depends on this version
// (asset_dependencies, migration 00010).
//
// Two conditions must hold, and both are the fail-closed reading:
//
//   - the usage row itself declares visibility_of_usage = 'public' — the
//     column docs/23 §5's "只显示公开使用" rule is about.
//   - the USING project is public. The row's own declaration is about the
//     usage, not about the project's identity: a private project may use
//     a public asset, and printing its name, its slug or its id beside
//     this asset would disclose the existence of that project — the
//     disclosure issue #238 closed on the preview surface.
//
// A usage that fails either condition is not rendered and not counted.
// The asset page never prints "N private projects use this": docs/23 §5
// allows such an aggregate only "在产品策略明确允许的匿名聚合阈值下", and
// V1's choice is the one that line offers as sufficient — "V1 可直接不显
// 示 private count".
type PageUsage struct {
	ProjectID      string    `json:"project_id"`
	ProjectName    string    `json:"project_name"`
	ProjectSlug    string    `json:"project_slug"`
	DependencyType string    `json:"dependency_type"`
	CreatedAt      time.Time `json:"created_at"`
}

// PageVersionSummary is one row of the versions block (docs/42's
// "versions"): every version of the asset the reader may see, so that two
// versions of the same asset are distinguishable on the page. Current
// marks the version the rest of the page renders.
type PageVersionSummary struct {
	Version       string     `json:"version"`
	URL           string     `json:"url"`
	Visibility    Visibility `json:"visibility"`
	IntegrityHash string     `json:"integrity_hash"`
	PublishedAt   time.Time  `json:"published_at"`
	Current       bool       `json:"current"`
}

// PageEvent is one research event recorded about the asset (docs/42's
// "network events"): the version-published events the publish command
// writes (internal/persistence.AssetPublishStore, event type
// research_asset.version_published, specs/events/event-types.yaml).
//
// An event is rendered only when its own stored visibility is public, for
// a caller who is not a member: the publish command stores the project's
// visibility at publish time on the event precisely so that an event
// about a private project's work stays private, and the asset page obeys
// the event's own answer rather than the asset's.
type PageEvent struct {
	Type       string    `json:"type"`
	OccurredAt time.Time `json:"occurred_at"`
	// Version is the version label the event is about, when its payload
	// names one.
	Version string `json:"version,omitempty"`
	// Actor is the acting user's identity, nil when the event has no actor
	// or the user row could not be resolved.
	Actor *PageUser `json:"actor"`
}

// AssetPage is one asset page's data: the eleven blocks of docs/42 §Asset
// Page, in the order that line lists them. Every block is rendered, empty
// ones as empty lists, so a client renders a stated absence ("no public
// usage", "no lineage") instead of guessing at a missing field.
type AssetPage struct {
	Asset        PageAsset            `json:"asset"`
	Version      PageVersionFacts     `json:"version"`
	Origin       []PageOrigin         `json:"origin"`
	Rights       json.RawMessage      `json:"rights"`
	Creators     []PageCreator        `json:"creators"`
	Metadata     []PageMetadata       `json:"metadata"`
	Dependencies []PageDependency     `json:"dependencies"`
	Lineage      []PageLineage        `json:"lineage"`
	UsedBy       []PageUsage          `json:"used_by"`
	Versions     []PageVersionSummary `json:"versions"`
	Events       []PageEvent          `json:"events"`
}

// ---------------------------------------------------------------------
// The reader's answer
//
// PageState is what a reader (the Postgres adapter in
// internal/persistence) resolved for one asset, and BuildPage is the
// pure half that decides what may be rendered. The split is the same one
// internal/assets draws for the publish preview: the adapter answers
// questions about rows, the model owns every rule about disclosure, and
// the rules are therefore testable without a database (page_test.go).
//
// Everything here is RAW: the rows as stored, including the private ones.
// A state that pre-filtered would move the decision into the adapter,
// where the tests that name docs/23 §5 could not reach it.
// ---------------------------------------------------------------------

// PageAssetState is the research_assets row.
type PageAssetState struct {
	ID              string
	PID             PID
	Type            Type
	Title           string
	Slug            string
	OriginProjectID string
	CreatedAt       time.Time
}

// PageProjectState is the origin project row: the identity the page may
// or may not print, with the visibility that decides it.
type PageProjectState struct {
	ID         string
	Name       string
	Slug       string
	Visibility Visibility
}

// PageVersionState is one research_asset_versions row, with the stored
// documents as bytes (the manifest and rights documents are read by their
// own parsers, never by this file).
type PageVersionState struct {
	ID            string
	Version       string
	Visibility    Visibility
	IntegrityHash string
	PublishedBy   string
	PublishedAt   time.Time
	Manifest      json.RawMessage
	RightsJSON    json.RawMessage
	OriginRefs    []string
}

// PagePartyState is one credited party of ONE version, as stored in
// asset_version_parties: the role the version credits it in, and the
// (kind, id) pair that names it. The pair is kept whole — the kind is why
// the column exists beside the id (00082) — and the version it belongs to
// is the key it is filed under, so a reader cannot apply one version's
// credits to another.
type PagePartyState struct {
	Role    string
	Kind    string
	PartyID string
}

// PagePinState is one dependency pin, resolved against the stored
// versions. Resolved is false when the pin names no stored version — the
// document may pin anything, and a pin that resolves to nothing is a fact
// about the pinned document rather than an error here.
type PagePinState struct {
	Pin      DependencyPin
	Resolved bool
	// Visibility is the pinned VERSION's stored visibility and
	// ProjectVisibility is its asset's originating project's — both axes of
	// mayLinkVersion, which is what the block is allowed to render from.
	Visibility        Visibility
	ProjectVisibility Visibility
	Title             string
	Type              Type
	// PID and Version are the pinned version's own identity, split back out
	// of the pin so the block can build its URL. They are read from the pin
	// (ParseDependencyPin) rather than carried beside it, so the two cannot
	// disagree.
	PID     PID
	Version string
}

// PageLineageEnd is one end of a lineage edge, with what the page needs
// to decide whether the edge may be rendered.
type PageLineageEnd struct {
	// VersionID is the version ROW's internal id. It is the identity this
	// model matches an edge against, and it is never rendered: a version
	// label is unique per asset and NOT across the network, so matching on
	// the label could pair two different assets' versions — the row id
	// cannot.
	VersionID string
	PID       PID
	Version   string
	// Visibility is this version's stored visibility and ProjectVisibility
	// its asset's project's: both are needed, because an edge may be
	// rendered only when the reader could open the other end
	// (mayLinkVersion).
	Visibility        Visibility
	ProjectVisibility Visibility
	Title             string
}

// PageLineageState is one asset_lineage row, both ends resolved.
type PageLineageState struct {
	Relation string
	Parent   PageLineageEnd
	Child    PageLineageEnd
}

// PageUsageState is one asset_dependencies row with its project's own
// visibility, which is the second condition PageUsage states.
type PageUsageState struct {
	AssetVersionID    string
	ProjectID         string
	ProjectName       string
	ProjectSlug       string
	ProjectVisibility Visibility
	VisibilityOfUsage Visibility
	DependencyType    string
	CreatedAt         time.Time
}

// PageEventState is one research_events row about the asset.
type PageEventState struct {
	Type         string
	Visibility   Visibility
	ActorID      string
	AssetVersion string
	OccurredAt   time.Time
}

// PageState is the whole raw answer for one asset.
type PageState struct {
	Asset    PageAssetState
	Project  PageProjectState
	Versions []PageVersionState
	// Users are the identities of the actors the page renders (published_by,
	// event actors), by user id. A missing entry renders as nil rather than
	// as an id-only entry: an unresolved user is a row this reader could not
	// answer for, not an identity to print.
	Users map[string]PageUser
	// Parties are the credited parties of EVERY version of the asset, as
	// stored (asset_version_parties), keyed by the version ROW id. Every
	// version's rows are carried, not just the rendered one's, for the same
	// reason every version's pins are: the model picks the version it
	// renders, and a reader that resolved only the version it guessed would
	// starve the model of the state it needs for any other choice.
	Parties map[string][]PagePartyState
	// Organizations resolves the identities of the organization-kind
	// credited parties, by organization id — the second table a party id can
	// name (00002). A missing entry renders no identity, which is the rule
	// Users follows too.
	Organizations map[string]PageOrganization
	// Pins are the dependency pins of the rendered version's manifest,
	// resolved, by canonical pin.
	Pins map[DependencyPin]PagePinState
	// Refs are the origin refs of the rendered version, resolved by the
	// reader, one entry per canonical ref.
	Refs map[OriginRef]StoredRef
	// Lineage, Usages and Events are every row the reader found for the
	// asset — private ones included; BuildPage filters.
	Lineage []PageLineageState
	Usages  []PageUsageState
	Events  []PageEventState
}

// BuildPage renders one asset page from the reader's answer. ok is false
// when no version of the asset may be shown to this viewer at all — an
// asset whose every version is private, read by a non-member, or a requested
// version the viewer may not see: the transport answers every one of those
// the same 404 an unknown pid gets, so neither the page's existence nor a
// version's is an oracle.
//
// want is the version to render, and it is a REQUEST rather than a
// selection: "" asks for the newest version the viewer may see, and a label
// asks for that exact version — the version URL /assets/{pid}/{version}
// (internal/assets/url.go) is a persistent address, so the page it names has
// to render that version. A requested version the viewer may not see (a
// private one, read by a non-member) is refused with the same ok=false as an
// asset with nothing visible: "that version exists and you may not see it"
// is a disclosure, and a page that answered it differently from a version
// that does not exist would hand one out.
//
// Without want the rendered version is the NEWEST version the viewer may see
// (published_at descending, then the label ascending for rows published in
// the same transaction — a total order, so two reads of one state render the
// same page). A member sees private versions and therefore renders the
// newest of all; everyone else renders the newest public one.
func BuildPage(state PageState, viewer PageViewer, want string) (AssetPage, bool) {
	current, ok := currentVersion(state.Versions, viewer, want)
	if !ok {
		return AssetPage{}, false
	}

	page := AssetPage{
		Asset: PageAsset{
			PID:       state.Asset.PID,
			Type:      state.Asset.Type,
			Title:     state.Asset.Title,
			Slug:      state.Asset.Slug,
			CreatedAt: state.Asset.CreatedAt.UTC(),
		},
		Version: PageVersionFacts{
			Version:       current.Version,
			URL:           AssetVersionURL(state.Asset.PID, current.Version),
			Visibility:    current.Visibility,
			IntegrityHash: current.IntegrityHash,
			PublishedAt:   current.PublishedAt.UTC(),
			PublishedBy:   userOf(state.Users, current.PublishedBy),
		},
		Origin:       []PageOrigin{},
		Rights:       rightsOf(current),
		Creators:     creatorsOf(state, current),
		Metadata:     metadataOf(current),
		Dependencies: []PageDependency{},
		Lineage:      []PageLineage{},
		UsedBy:       []PageUsage{},
		Versions:     versionSummaries(state, viewer, current),
		Events:       eventsOf(state, viewer),
	}
	page.Asset.OriginProject = renderProject(state, viewer)
	page.Origin = originsOf(state, viewer, current)
	page.Dependencies = dependenciesOf(state, current)
	page.Lineage = lineageOf(state, current)
	page.UsedBy = usagesOf(state, current)
	return page, true
}

// currentVersion picks the version the page renders: the one the viewer
// asked for when it may be seen, and otherwise the newest one the viewer may
// see. The order is the storage order inverted — published_at descending,
// then the label ascending — and both halves are needed: two versions
// published in the same statement share a timestamp, and a page that
// rendered a different one of them on each read would be a page nobody could
// cite.
func currentVersion(versions []PageVersionState, viewer PageViewer, want string) (PageVersionState, bool) {
	var best PageVersionState
	found := false
	for _, v := range versions {
		if !memberMaySee(v, viewer) {
			continue
		}
		if want != "" {
			if v.Version == want {
				return v, true
			}
			continue
		}
		if !found || newerVersion(v, best) {
			best, found = v, true
		}
	}
	return best, found
}

// memberMaySee reports whether this viewer may be shown one version: a
// member of the asset's own project sees every version of it, and everyone
// else sees the public ones. It is the single rule the three version-aware
// blocks (the rendered version, the versions list and the refusal of a
// requested version) all read, so they cannot disagree about what is
// visible.
func memberMaySee(v PageVersionState, viewer PageViewer) bool {
	return viewer.Member || v.Visibility == VisibilityPublic
}

// newerVersion reports whether a sorts before b in the page's order.
func newerVersion(a, b PageVersionState) bool {
	if !a.PublishedAt.Equal(b.PublishedAt) {
		return a.PublishedAt.After(b.PublishedAt)
	}
	return a.Version < b.Version
}

// renderProject returns the asset's originating project when the identity
// rule in the file comment allows it, and nil otherwise.
func renderProject(state PageState, viewer PageViewer) *PageProject {
	if !mayRenderOwnProject(state.Project, viewer) {
		return nil
	}
	return &PageProject{
		ID:         state.Project.ID,
		Name:       state.Project.Name,
		Slug:       state.Project.Slug,
		Visibility: string(state.Project.Visibility),
	}
}

// mayRenderOwnProject reports whether the page may print the asset's own
// originating project: it is public, or the caller is one of its members.
func mayRenderOwnProject(project PageProjectState, viewer PageViewer) bool {
	if project.ID == "" {
		// Nothing resolved a project row, so there is nothing the page may
		// print about one.
		return false
	}
	return project.Visibility == VisibilityPublic || viewer.Member
}

// mayLinkVersion reports whether the page may name one version of some
// asset — as a lineage counterpart or as a dependency pin, the two blocks
// that link to a version other than the rendered one.
//
// Both axes are required. The version itself must be public, and its
// asset's project must be public too, because the URL this block would
// print (/assets/{pid}/{version}) is served through that project's read
// gate: a public version of an asset in a private project answers the
// existence-hiding 404 to everybody outside that project. "Public version"
// alone would render a link that the reader it was rendered for cannot
// open, and a link that answers 404 is the same disclosure as a bad link
// with a worse outcome — a reader who fetches it learns that the pid
// exists.
//
// Membership does not enter into it, and cannot: PageViewer carries the
// caller's standing in the RENDERED asset's project only, and a page that
// asked about the caller's membership of every project a row points into
// would answer differently for two readers about a third party's state.
func mayLinkVersion(version, project Visibility) bool {
	return version == VisibilityPublic && project == VisibilityPublic
}

// mayRenderForeignProject reports whether the page may print a project
// that is NOT the asset's origin — the project a usage, a lineage edge or
// an origin ref points into. Only a public one qualifies: membership in
// the asset's own project says nothing about somebody else's, and asking
// about the caller's memberships per rendered row would make one page
// answer differently for two readers about a third party's state.
func mayRenderForeignProject(visibility Visibility) bool {
	return visibility == VisibilityPublic
}

// rightsOf returns the stored rights document verbatim — the document the
// rights panel renders (apps/web/app/components/rights-panel.tsx reads it
// through lib/rights.ts). It is echoed as stored bytes rather than
// re-encoded, so what the page shows is what the publisher published. An
// EMPTY document renders as null: the column is NOT NULL, so an empty one
// means the reader did not resolve it, and the panel's own "no
// declaration" state is the honest rendering of that.
func rightsOf(v PageVersionState) json.RawMessage {
	if len(v.RightsJSON) == 0 {
		return nil
	}
	return v.RightsJSON
}

// creatorsOf renders the credited parties of the version, in the order the
// reader returned them: grouped by role, and within a role in the order the
// publish declared them (asset_version_parties.position). The order is
// total, so two reads of one state render the same block.
//
// A party whose identity the reader did not resolve renders NO entry. That
// is the rule the whole file follows — an id is not an identity, and an
// entry carrying one would be a half-answer a client renders as a name —
// and it is also what happens to a version published before 00082: it has
// no credit rows, so it renders no credited party, and nothing stands in
// for one. The publisher is NOT substituted here: published_by is a
// different relationship on a different party, and it is rendered as
// itself in the version block (PageVersionFacts.PublishedBy). See
// PageCreator.
func creatorsOf(state PageState, v PageVersionState) []PageCreator {
	parties := state.Parties[v.ID]
	out := make([]PageCreator, 0, len(parties))
	for _, p := range parties {
		switch p.Kind {
		case string(domain.PartyUser):
			user, ok := state.Users[p.PartyID]
			if !ok {
				continue
			}
			out = append(out, PageCreator{
				Kind:        p.Kind,
				PartyID:     user.UserID,
				Handle:      user.Handle,
				DisplayName: user.DisplayName,
				Role:        p.Role,
			})
		case string(domain.PartyOrganization):
			org, ok := state.Organizations[p.PartyID]
			if !ok {
				continue
			}
			out = append(out, PageCreator{
				Kind:        p.Kind,
				PartyID:     org.ID,
				Handle:      org.Slug,
				DisplayName: org.Name,
				Role:        p.Role,
			})
		}
		// A kind with no identity table resolves to nothing and renders
		// nothing: the page has no name to put beside it, and inventing one
		// from the other kind's shape is what the kind column prevents.
	}
	return out
}

// metadataOf renders the manifest's metadata block, ascending by key —
// the order the canonical manifest bytes sort it in (Manifest.
// CanonicalJSON), so the page reads the way the stored document does. A
// manifest that does not parse renders no metadata: guessing at the
// fields of a document the platform cannot read would be a second, more
// permissive parser.
func metadataOf(v PageVersionState) []PageMetadata {
	manifest, err := ParseManifest(v.Manifest)
	if err != nil {
		return []PageMetadata{}
	}
	keys := sortedKeys(manifest.Metadata)
	out := make([]PageMetadata, 0, len(keys))
	for _, k := range keys {
		out = append(out, PageMetadata{Key: k, Value: manifest.Metadata[k]})
	}
	return out
}

// originsOf renders the version's origin refs, in the order the immutable
// row declares them. A ref the reader did not answer for, or whose entity
// it could not resolve, is omitted — see PageOrigin.
func originsOf(state PageState, viewer PageViewer, v PageVersionState) []PageOrigin {
	out := make([]PageOrigin, 0, len(v.OriginRefs))
	for _, raw := range v.OriginRefs {
		ref := OriginRef(raw)
		kind, value, ok := ParseOriginRef(raw)
		if !ok {
			// Not a canonical ref: the row holds something this platform
			// cannot resolve, and the page has nothing to say about it.
			continue
		}
		st, answered := state.Refs[ref]
		if !answered || !st.Resolved {
			continue
		}
		// The ref string names the referenced entity, so the entry is
		// rendered only when that entity's project may be rendered —
		// otherwise it is dropped whole (the entry rule in the file
		// comment). Rendering "ref, kind, resolved" without the identity
		// would still spell the uuid.
		if st.ProjectID == state.Asset.OriginProjectID {
			if !mayRenderOwnProject(state.Project, viewer) {
				continue
			}
		} else if !mayRenderForeignProject(st.ProjectVisibility) {
			continue
		}
		entry := PageOrigin{
			Ref:       raw,
			Kind:      kind,
			Resolved:  true,
			ProjectID: st.ProjectID,
			Link:      originLink(kind, value, st),
		}
		if st.Object != nil {
			entry.Title = st.Object.Title
		}
		out = append(out, entry)
	}
	return out
}

// originLink is the public web page of one resolved origin entity, or ""
// when V1 has none: a project's Overview, a release's Release page
// (project-scoped, docs/42 §Release). States and object versions have no
// public page yet, and a guessed URL would be a link to 404.
func originLink(kind OriginKind, value string, st StoredRef) string {
	switch kind {
	case KindProject:
		return "/projects/" + st.ProjectID
	case KindRelease:
		return "/projects/" + st.ProjectID + "/releases/" + value
	}
	return ""
}

// dependenciesOf renders the pins the rendered version's manifest
// declares, in the document's own order. A pin whose version the network
// cannot open is dropped (see PageDependency).
func dependenciesOf(state PageState, v PageVersionState) []PageDependency {
	manifest, err := ParseManifest(v.Manifest)
	if err != nil {
		return []PageDependency{}
	}
	out := make([]PageDependency, 0, len(manifest.DependencyPins))
	for _, pin := range manifest.DependencyPins {
		resolved, known := state.Pins[pin]
		if !known || !resolved.Resolved {
			// The pin names a version the platform has no row for. Its own
			// bytes are rendered: they spell an asset identity the
			// repository does not answer for, which tells a reader nothing
			// about anything that exists.
			out = append(out, PageDependency{Pin: string(pin)})
			continue
		}
		if !mayLinkVersion(resolved.Visibility, resolved.ProjectVisibility) {
			continue
		}
		out = append(out, PageDependency{
			Pin:      string(pin),
			Resolved: true,
			Public:   true,
			Title:    resolved.Title,
			Type:     resolved.Type,
			URL:      AssetVersionURL(resolved.PID, resolved.Version),
		})
	}
	return out
}

// lineageOf renders the fork/derive edges of the rendered version. Only
// edges whose other end is public are rendered (see PageLineage).
func lineageOf(state PageState, v PageVersionState) []PageLineage {
	out := []PageLineage{}
	for _, edge := range state.Lineage {
		switch {
		case edge.Parent.is(v):
			if !mayLinkVersion(edge.Child.Visibility, edge.Child.ProjectVisibility) {
				continue
			}
			out = append(out, lineageEntry(edge.Relation, LineageChild, edge.Child))
		case edge.Child.is(v):
			if !mayLinkVersion(edge.Parent.Visibility, edge.Parent.ProjectVisibility) {
				continue
			}
			out = append(out, lineageEntry(edge.Relation, LineageParent, edge.Parent))
		}
	}
	return out
}

// is reports whether one lineage end names the given version: the version
// row's own id, not its label (see PageLineageEnd.VersionID).
func (e PageLineageEnd) is(v PageVersionState) bool {
	return e.VersionID != "" && e.VersionID == v.ID
}

// lineageEntry renders one renderable lineage edge.
func lineageEntry(relation, direction string, end PageLineageEnd) PageLineage {
	return PageLineage{
		Relation:  relation,
		Direction: direction,
		PID:       end.PID,
		Version:   end.Version,
		URL:       AssetVersionURL(end.PID, end.Version),
		Title:     end.Title,
	}
}

// usagesOf renders the public usages of the rendered version, oldest
// first. See PageUsage for the two conditions.
func usagesOf(state PageState, v PageVersionState) []PageUsage {
	out := []PageUsage{}
	for _, usage := range state.Usages {
		if usage.AssetVersionID != v.ID {
			continue
		}
		if usage.VisibilityOfUsage != VisibilityPublic {
			continue
		}
		if !mayRenderForeignProject(usage.ProjectVisibility) {
			continue
		}
		out = append(out, PageUsage{
			ProjectID:      usage.ProjectID,
			ProjectName:    usage.ProjectName,
			ProjectSlug:    usage.ProjectSlug,
			DependencyType: usage.DependencyType,
			CreatedAt:      usage.CreatedAt.UTC(),
		})
	}
	return out
}

// versionSummaries renders every version the viewer may see, newest
// first, with the rendered one marked. The public/private distinction is
// the stored visibility verbatim: a member sees the private ones too, and
// the label says so rather than hiding them, because a member reading
// their own project's page is exactly who that value is for.
func versionSummaries(state PageState, viewer PageViewer, current PageVersionState) []PageVersionSummary {
	visible := make([]PageVersionState, 0, len(state.Versions))
	for _, v := range state.Versions {
		if !memberMaySee(v, viewer) {
			continue
		}
		visible = append(visible, v)
	}
	sort.SliceStable(visible, func(i, j int) bool { return newerVersion(visible[i], visible[j]) })
	out := make([]PageVersionSummary, 0, len(visible))
	for _, v := range visible {
		out = append(out, PageVersionSummary{
			Version:       v.Version,
			URL:           AssetVersionURL(state.Asset.PID, v.Version),
			Visibility:    v.Visibility,
			IntegrityHash: v.IntegrityHash,
			PublishedAt:   v.PublishedAt.UTC(),
			Current:       v.ID == current.ID,
		})
	}
	return out
}

// eventsOf renders the research events about the asset, newest first.
//
// Two questions decide one row, and both must be answered yes.
//
// May this caller see the EVENT? A non-member sees the events whose own
// stored visibility is public, and only those: the event's visibility is
// the publishing project's at publish time
// (internal/persistence.AssetPublishStore), so an event about a private
// project's publication stays out of the network's page whatever the
// asset's own visibility is.
//
// May this caller see the VERSION the event names? An event that names a
// version is a statement about that version — its label, when it was
// published, who published it — so it may be rendered only when the
// VERSION is one this caller may see. That is the second guard below, and
// it is the same predicate the versions block renders a version from
// (memberMaySee), not a rule of this block's own: an event may name a
// version only if the versions block would name it, so this page cannot
// answer "no such version" in one block and print the label in another.
//
// The event's own visibility cannot stand in for the version's, which is
// why both guards are needed. A public project may publish a PRIVATE
// version (internal/application/assetpublish: a private publication widens
// nothing, so it is permitted and no policy is read), and the event's
// stored visibility is that project's at publish time — PUBLIC. The event
// axis passes on its own, and what it would render is a private version's
// label, publish instant and publisher. docs/12 §2 and docs/23 §4 are the
// rule this keeps: a private thing does not become visible by sitting next
// to a public one.
//
// An event whose version label resolves to no version of this asset at all
// is withheld too. The label cannot be judged, and a thing that cannot be
// judged is not rendered (fail closed) — a stored label always names a row,
// version rows being append-only and never deleted, so this guard costs an
// unreadable row its symptom rather than its effect.
func eventsOf(state PageState, viewer PageViewer) []PageEvent {
	// The versions this caller may see, by label: the versions block's own
	// answer, asked once and reused for every event.
	visible := make(map[string]bool, len(state.Versions))
	for _, v := range state.Versions {
		if memberMaySee(v, viewer) {
			visible[v.Version] = true
		}
	}
	out := []PageEvent{}
	for _, e := range state.Events {
		if !viewer.Member && e.Visibility != VisibilityPublic {
			continue
		}
		if e.AssetVersion != "" && !visible[e.AssetVersion] {
			continue
		}
		out = append(out, PageEvent{
			Type:       e.Type,
			OccurredAt: e.OccurredAt.UTC(),
			Version:    e.AssetVersion,
			Actor:      userOf(state.Users, e.ActorID),
		})
	}
	return out
}

// userOf resolves one user id to its identity, or nil when the reader
// could not answer for it.
func userOf(users map[string]PageUser, userID string) *PageUser {
	if userID == "" {
		return nil
	}
	user, ok := users[userID]
	if !ok {
		return nil
	}
	return &user
}
