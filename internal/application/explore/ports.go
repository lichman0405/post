package explore

import (
	"context"
	"errors"
	"time"

	"github.com/lichman0405/post/internal/application/knowledgepublish"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/contribution"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rights"
)

// ErrStore reports a reader that failed. The transport answers 503 (docs/45:
// the wire carries a code, never dependency detail).
var ErrStore = errors.New("explore: store failure")

// Reader is the index's read port (docs/52: the application orchestrates
// against ports; the adapters live with whoever owns the data).
//
// Six reads, one per dimension. Three of them are the platform's EXISTING
// public reads, reused whole rather than re-derived — a second
// implementation of a disclosure rule in a layer nobody tests is exactly
// what T0709 warned about:
//
//   - ListPublicProjects is projects.Service.List's public half
//     (ListPublicProjects in the project store; docs/12 §2, T0106).
//   - ListPublicAssets is the asset hub's browse list, already built by
//     internal/assets.BuildBrowse (T0709), so the Explore assets tab and
//     /assets can never disagree.
//   - ListPublicContributions is contribution.Service.ListPublic
//     (T0803): publicized AND open rows only.
//
// The other three had no read at all before this task; the PostgreSQL
// adapter is Store (store.go), and each of those row types carries the
// lifecycle fact its section re-checks, so the model fails closed even if an
// adapter forgets its predicate.
type Reader interface {
	// ListPublicProjects returns the public projects, in any order.
	ListPublicProjects(ctx context.Context) ([]domain.Project, error)
	// ListPublicAssets returns the asset hub's public browse items, in any
	// order.
	ListPublicAssets(ctx context.Context) ([]assets.BrowseItem, error)
	// ListPublishedKnowledge returns published knowledge object versions —
	// one row per knowledge_publications row, with the object version it
	// pinned. Rows are RAW: the project is named by ID only, and BuildIndex
	// decides whether that project may be printed.
	ListPublishedKnowledge(ctx context.Context) ([]KnowledgeRow, error)
	// ListPublicPeople returns the network's accounts.
	ListPublicPeople(ctx context.Context) ([]PersonRow, error)
	// ListPublicOrganizations returns the organizations of the network.
	ListPublicOrganizations(ctx context.Context) ([]OrganizationRow, error)
	// ListPublicContributions returns the publicized, open contribution
	// opportunities across all projects.
	ListPublicContributions(ctx context.Context) ([]contribution.ContributionOpportunity, error)
}

// KnowledgeRow is one published knowledge object version as the reader
// resolved it.
//
// It is deliberately shaped so the store CANNOT leak a private project
// identity into the index: the row names its project by id, never by name
// or slug (see store.go's query), and BuildIndex resolves that id against
// the public project set it built from the Projects section. There is no
// field here for a private name to arrive in.
type KnowledgeRow struct {
	// PublicationID is the knowledge_publications row that put this version
	// on the network. EMPTY means the version is not published, and a row
	// without a publication renders nothing — the fail-closed reading of
	// "a scientific object is public knowledge only once someone published
	// it" (docs/12 §3: visibility widening is never automatic).
	PublicationID string
	// ObjectID is the scientific object; PublicVersion is the version label
	// the publication pinned.
	ObjectID      string
	ObjectType    string
	PublicVersion string
	Title         string
	// ProjectID is the publishing project's id — an IDENTIFIER ONLY. The
	// project's name, slug and visibility are not read here at all, so
	// there is no path by which a private project's identity could reach
	// the answer.
	ProjectID string
	// PublishedAt is when the publication was made (zero when unpublished).
	PublishedAt time.Time
	// LifecycleState is the published version's own state (docs/43).
	LifecycleState string
	// AudienceFor's three inputs, carried RAW so Published can apply the
	// rule itself rather than trust a reader's filter.
	//
	// This section is the one that made them necessary (T0805): before the
	// publish path existed this query had no writer and read nothing, and
	// the moment it does, an unfiltered read here is an anonymous read of
	// every publication — including one whose version is visibility
	// restricted in a PUBLIC project, which the publication decision
	// deliberately admits (发布不等于公开). The audience rule is
	// knowledgepublish.AudienceFor and it lives in Go; the adapter reads
	// these three columns and applies IT, and Published re-applies it, so
	// there is one definition of "may the network see this" and a reader
	// regression renders a shorter index rather than a leak.
	//
	// VisibilityPolicyID is the version's OWN axis
	// (scientific_object_versions.visibility_policy_id): nil means it
	// inherits the project's visibility.
	VisibilityPolicyID *string
	// ProjectVisibility is projects.visibility ('public' or 'private').
	ProjectVisibility string
	// Rights is the publication's stored rights declaration, parsed.
	// RightsValid is false when the stored bytes are not a document this
	// build can read; AudienceFor refuses an unreadable declaration rather
	// than treating it as a licence to print.
	Rights      rights.Document
	RightsValid bool
}

// Published reports whether this row is a publication the NETWORK may see —
// two questions, and the second is not a formality: a publication exists
// (PublicationID is set) as soon as someone published it, and a version can
// be published and still not be the network's (owner ruling L3-20260916-1
// #1: publishing records a state; visibility is the version's own axis).
//
// The audience decision is knowledgepublish.AudienceFor — the same function
// the publish command decides with and the public read route applies — so
// this anonymous surface cannot become a wider reader than the publication's
// own page.
func (r KnowledgeRow) Published() bool {
	if r.PublicationID == "" {
		return false
	}
	if !r.RightsValid {
		return false
	}
	return knowledgepublish.AudienceFor(r.ProjectVisibility, r.VisibilityPolicyID, r.Rights) ==
		knowledgepublish.AudienceNetwork
}

// PersonRow is one account with a research profile as the reader resolved
// it. It carries identity and profile fields only: users.email is never
// read by the adapter (email is identity, not a profile field —
// internal/application/profile).
type PersonRow struct {
	ID          string
	Handle      string
	DisplayName string
	Bio         string
	// CreatedAt is the section's freshness key. It is not rendered
	// (PersonItem): see the note there.
	CreatedAt time.Time
	// DisabledAt is set when the account was disabled (users.disabled_at).
	// A disabled account is not part of the network's directory.
	DisabledAt *time.Time
}

// Disabled reports whether the account is disabled.
func (r PersonRow) Disabled() bool { return r.DisabledAt != nil }

// OrganizationRow is one organization as the reader resolved it.
type OrganizationRow struct {
	ID          string
	Slug        string
	Name        string
	Description string
	// CreatedAt is the section's freshness key, not rendered
	// (OrganizationItem).
	CreatedAt time.Time
	// DeactivatedAt is set when the organization was deactivated —
	// migration 00018's soft delete, the only "delete" this domain has
	// (CLAUDE.md §9.8). A deactivated organization is not listed.
	DeactivatedAt *time.Time
}

// Deactivated reports whether the organization is deactivated.
func (r OrganizationRow) Deactivated() bool { return r.DeactivatedAt != nil }
