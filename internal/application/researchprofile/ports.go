package researchprofile

import (
	"context"
	"time"
)

// Reader is the two surfaces' read port (docs/52: the application
// orchestrates against ports; adapters live with whoever owns the data).
//
// Ten reads, and every one of them answers a question the platform never
// asked before this task: "everything about THIS person" and "everything
// about THIS organization". Each row carries the facts the rules need — the
// project's and the version's own visibility, the organization's deactivation
// instant and the account's disabled instant — and the rules about what may be
// rendered live in model.go, where they have unit tests that name the document
// each one comes from.
//
// Each read states the render predicate of the dimension it feeds in SQL, with
// the LIMIT after it, so a bounded window is counted in rows this surface may
// actually RENDER (internal/persistence/queries/research_profile.sql; the same
// arrangement T1004 gave the feed reads). The model re-checks every row anyway
// — the query's predicate is up to the query, and the value it filtered on
// comes back with the row so the re-check does not have to trust it. A read
// whose predicate were dropped costs entries, not correctness.
type Reader interface {
	// GetPerson returns one account with its profile content. ok is false
	// for an unknown id (the transport answers the same existence-hiding
	// 404 an unknown handle gets, docs/45).
	GetPerson(ctx context.Context, userID string) (PersonRow, bool, error)
	// ListAffiliations returns every membership of the person — current AND
	// ended, because docs/04 §6 keeps a person's history when an affiliation
	// ends ("组织不能删除个人历史贡献；离职只终止 affiliation/role").
	ListAffiliations(ctx context.Context, userID string) ([]AffiliationRow, error)
	// ListContributions returns the ledger rows one actor wrote, newest
	// first, at most FetchLimit of them.
	ListContributions(ctx context.Context, userID string) ([]ContributionRow, error)
	// ListPersonAssets returns the asset versions one person is credited on.
	ListPersonAssets(ctx context.Context, userID string) ([]AssetCreditRow, error)
	// ListReuses returns the recorded usages of the versions one person is
	// credited on: "who publicly uses this version".
	ListReuses(ctx context.Context, userID string) ([]ReuseRow, error)
	// ListReproductions returns the evidence assertions one person created
	// with a reproduction relation (docs/10 §4). Assertions are read on their
	// own visibility axis (00091): an assertion nothing explicitly made public
	// is not rendered anywhere, and both surfaces are anonymous.
	ListReproductions(ctx context.Context, userID string) ([]ReproductionRow, error)

	// GetOrganizationBySlug returns one organization addressed by its slug —
	// the stable public identity (internal/application/orgs: the slug is not
	// mutable). ok is false for an unknown slug.
	GetOrganizationBySlug(ctx context.Context, slug string) (OrganizationRow, bool, error)
	// ListOrganizationProjects returns the projects an organization owns.
	ListOrganizationProjects(ctx context.Context, organizationID string) ([]ProjectRow, error)
	// ListOrganizationAssets returns the asset versions whose asset
	// originates from one of the organization's projects.
	ListOrganizationAssets(ctx context.Context, organizationID string) ([]AssetCreditRow, error)
	// ListOrganizationActivity returns the ledger rows recorded while their
	// actor was affiliated with one organization
	// (contribution_events.organization_id_at_time, docs/13 §1 "affiliation
	// at time"), newest first, at most FetchLimit of them.
	ListOrganizationActivity(ctx context.Context, organizationID string) ([]ContributionRow, error)
}

// FetchLimit bounds what each read returns (see RenderLimit for what is
// rendered). It is a read bound, not a rendered count, so it is never
// disclosed: it caps the memory one profile view can cost on an append-only
// table that only grows.
const FetchLimit = 200

// PersonRow is one account with its profile content, as the reader resolved
// it. users.email is never read (it is identity, not a profile field —
// internal/application/profile).
type PersonRow struct {
	ID          string
	Handle      string
	DisplayName string
	Bio         string
	// DisabledAt is set when the account is disabled (users.disabled_at).
	// A disabled account is not part of the network: internal/events'
	// audience rule answers 'none' for everyone but the account itself
	// ("WHEN u.disabled_at IS NULL THEN 'public' ELSE 'none'"), and this
	// profile follows it rather than publishing the record of an account
	// the platform has taken out of the directory.
	DisabledAt *time.Time
}

// AffiliationRow is one membership row with its organization's public state.
type AffiliationRow struct {
	OrganizationID   string
	OrganizationSlug string
	OrganizationName string
	// OrganizationDeactivatedAt is the organization's soft-delete instant
	// (00018). A deactivated organization's identity is not rendered —
	// internal/events' audience rule gives it 'none' for non-members, and
	// internal/application/explore leaves it out of the network directory —
	// while the person's own row (role, dates, verification) stays.
	OrganizationDeactivatedAt *time.Time
	Role                      string
	// AffiliationStart/End are calendar dates (the canonical columns are
	// date, never timestamptz — internal/domain/affiliation.go). A nil
	// start means "since always"; a nil end means the affiliation has not
	// been ended.
	AffiliationStart *time.Time
	AffiliationEnd   *time.Time
	Verified         bool
}

// ContributionRow is one ledger row as the reader resolved it, with the
// project it belongs to and (for the organization surface) the actor's
// identity.
type ContributionRow struct {
	EventType  string
	RoleCodes  []string
	OccurredAt time.Time
	// Accepted/Released are the two context facts docs/13 §4 makes dimensions
	// of their own (contribution_events.accepted_context / released_context).
	Accepted bool
	Released bool
	// Via is the channel the contribution arrived through (docs/13 §1 "via
	// agent/client"), empty when the source event carried none — never a
	// guessed default (00087 states the same thing on the column).
	Via string
	// Project is the research boundary the event names. Its visibility
	// decides whether the row may be rendered at all.
	ProjectID         string
	ProjectSlug       string
	ProjectName       string
	ProjectVisibility string
	// Actor is the person the row credits; the organization surface renders
	// it, the person surface does not (the profile already names them).
	ActorID          string
	ActorHandle      string
	ActorDisplayName string
	// ActorDisabledAt is the actor's disabled instant: a contribution by an
	// account that is no longer part of the network is not rendered on an
	// organization's public activity either.
	ActorDisabledAt *time.Time
}

// AssetCreditRow is one published asset version a person is credited on or
// an organization's project produced.
type AssetCreditRow struct {
	PID       string
	Title     string
	AssetType string
	Version   string
	// VersionVisibility is the VERSION's own axis (research_asset_versions
	// .visibility, 00010). A private version is not rendered — the asset hub's
	// browse rule is the same one ("an asset appears when at least one of its
	// versions is public"), applied here to the exact version the profile
	// names.
	VersionVisibility string
	// Role is the credited relationship (asset_version_parties.role:
	// creator / contributor / custodian / maintainer, docs/11 §6). Empty for
	// the organization surface, which reads a version's origin rather than a
	// credit.
	Role string
	// Project is the asset's ORIGIN project (research_assets.origin_project_id)
	// — whose the asset is, not who is credited on the version. A private
	// project may publish a public version (docs/12 §2), so the version
	// renders and the project is withheld (the asset hub's rule,
	// internal/assets.BuildBrowse).
	ProjectID         string
	ProjectSlug       string
	ProjectName       string
	ProjectVisibility string
	PublishedAt       time.Time
}

// ReuseRow is one recorded usage of an asset version (asset_dependencies,
// 00010), read from the far side: the row names the version that was USED
// and the project that declared the use.
type ReuseRow struct {
	PID       string
	Title     string
	AssetType string
	Version   string
	// VersionVisibility is the USED version's own axis and
	// VersionProjectVisibility its asset's origin project's — the two facts
	// mayLinkVersion reads (internal/assets/page.go). A usage of a private
	// version is not rendered.
	VersionVisibility        string
	VersionProjectVisibility string
	// UsageVisibility is visibility_of_usage — the using project's own
	// declaration, derived at publish time from the VERSION's visibility
	// (internal/assets/usage.go).
	UsageVisibility string
	DependencyType  string
	DeclaredAt      time.Time
	// Project is the USING project: the one whose declaration this row is. It
	// must be public for the row to be rendered at all (PageUsage's second
	// condition) — a private project may use a public version, and naming it
	// would disclose that project's existence.
	ProjectID         string
	ProjectSlug       string
	ProjectName       string
	ProjectVisibility string
}

// ReproductionRow is one evidence assertion of a reproduction relation, as
// the reader resolved it.
type ReproductionRow struct {
	// Relation is the stored relation type: reproduces or fails_to_reproduce
	// (docs/10 §4). Both are rendered and neither is preferred — docs/10 §4
	// ends with "V1 不自动赋数值权重", and a failed reproduction is evidence
	// about the claim, not a demerit for the person who recorded it.
	Relation string
	// ReviewState is the assertion's own review state (00007). It is not a
	// classification this package makes; docs/10 §8's descriptive labels are
	// deliberately not produced anywhere in this repository yet.
	ReviewState string
	CreatedAt   time.Time
	// AssertionVisibility is the ASSERTION's own axis (evidence_assertions
	// .visibility, 00091: public/private, DEFAULT 'private') — not the
	// project's. The two are independent: an assertion may be public while the
	// project it was made in is private (it then renders with the project
	// withheld), and 00091's header is explicit that "an assertion nothing
	// explicitly made public is not rendered anywhere". The reader filters on
	// it and the model re-checks it; the value has to come back for the
	// re-check to be independent of the query.
	AssertionVisibility string
	// Project is the asserting project.
	ProjectID         string
	ProjectSlug       string
	ProjectName       string
	ProjectVisibility string
}

// OrganizationRow is one organization, addressed by slug.
type OrganizationRow struct {
	ID          string
	Slug        string
	Name        string
	Description string
	// DeactivatedAt is the soft-delete instant (00018): the only "delete"
	// this domain has (CLAUDE.md §9.8). A deactivated organization has no
	// public profile — internal/events' audience rule and explore's
	// directory agree — and the transport answers it the same
	// existence-hiding 404 an unknown slug gets.
	DeactivatedAt *time.Time
}

// ProjectRow is one project as the organization surface resolves it.
type ProjectRow struct {
	ID             string
	Slug           string
	Name           string
	Purpose        string
	ActivityStatus string
	Visibility     string
}
