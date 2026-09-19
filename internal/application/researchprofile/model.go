package researchprofile

import (
	"sort"
	"time"

	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/domain"
)

// RenderLimit is how many rows one dimension of a profile renders.
//
// The cap is the surface's, not the reader's: the readers return up to
// FetchLimit rows each, and a dimension keeps the newest RenderLimit of them.
// Nothing is hidden by it that the reader could not also reach elsewhere —
// the rows a dimension drops are the OLDEST ones — and, exactly like the
// Explore index's section cap, NO TOTAL IS RENDERED, so the cap cannot be
// read as a count of anything (docs/23 §5: "私有对象计数也不能通过 public
// API 泄漏"; V1 takes the option that line offers, "V1 可直接不显示 private
// count").
const RenderLimit = 50

// The web paths this surface builds. Each one names the module that owns the
// route, and a link is rendered only where the route exists: /users/{id}
// (T0801) and /projects/{id} (T0108) ship, /assets/{pid} comes with the asset
// hub (T0709), and /organizations/{slug} ships with this task — the same
// "no link to a route that does not exist" rule internal/application/explore
// states for its sections.
func webProfilePath(id string) string { return "/users/" + id }

func webProjectPath(id string) string { return "/projects/" + id }

func webOrganizationPath(slug string) string { return "/organizations/" + slug }

// ProjectRef is a project identity a profile row may print.
//
// It exists only for projects the row's own visibility made printable: a
// private project carries nil, never a placeholder, and no count of withheld
// rows is rendered either — so "null" says nothing about whether a project
// exists.
type ProjectRef struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

// OrganizationRef is an organization identity a profile row may print. It is
// withheld (nil) exactly when the organization is deactivated.
type OrganizationRef struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

// PersonSubject is the person a Research Profile belongs to. email is
// deliberately absent — it is identity, not a profile field
// (internal/application/profile never renders it either) — and so is
// created_at: "when did this person register" is published on no research
// surface (the same omission internal/application/explore.PersonItem makes).
type PersonSubject struct {
	ID          string `json:"id"`
	Handle      string `json:"handle"`
	DisplayName string `json:"display_name"`
	Bio         string `json:"bio"`
	URL         string `json:"url"`
}

// AffiliationItem is one affiliation (docs/42 "Affiliations"), current or
// ended. The window is rendered as the two dates the membership row stores —
// both ends are plain calendar dates (the canonical columns are `date`, never
// `timestamptz`; internal/domain/affiliation.go) and a null end means the
// affiliation has not been ended, which is the whole of what the row knows.
// Nothing here is derived from "now": a client renders "since 2024" or
// "2024 – 2025" itself, and no ordering judgement about the person is made.
type AffiliationItem struct {
	Organization *OrganizationRef `json:"organization"`
	Role         string           `json:"role"`
	Start        *string          `json:"affiliation_start"`
	End          *string          `json:"affiliation_end"`
	// Verified is the row's own flag (organization_memberships.verified).
	Verified bool `json:"verified"`
}

// ContributionItem is one ledger row: a contribution the person made, or (on
// an organization's profile) one recorded while the actor was affiliated with
// it.
type ContributionItem struct {
	EventType string   `json:"event_type"`
	RoleCodes []string `json:"role_codes"`
	// OccurredAt is when the source research event happened.
	OccurredAt time.Time `json:"occurred_at"`
	// Accepted and Released are the two context facts docs/13 §4 makes
	// dimensions of their own — "accepted/released ... evidence" in docs/42's
	// list.
	Accepted bool `json:"accepted_context"`
	Released bool `json:"released_context"`
	// Via is the channel the contribution arrived through (docs/13 §1 "via
	// agent/client"), empty when the source event carried none.
	Via string `json:"via"`
	// Project is nil only on the organization surface, where a row may name a
	// project the organization does not own — never on a person's profile: a
	// contribution whose project is not public is DROPPED there, because the
	// event is a fact about that project's work and not a public object of
	// its own (see BuildPersonProfile).
	Project *ProjectRef `json:"project"`
	// Actor is the person the row credits. It is rendered on the organization
	// surface ("who did this here") and omitted on the person's own profile,
	// where every row is that person's by construction.
	Actor *PersonRef `json:"actor,omitempty"`
}

// PersonRef is the identity of one person, as a row may print it.
type PersonRef struct {
	ID          string `json:"id"`
	Handle      string `json:"handle"`
	DisplayName string `json:"display_name"`
	URL         string `json:"url"`
}

// AssetItem is one published asset version a person is credited on or an
// organization's project produced (docs/42's "assets").
//
// One item per CREDIT, not per version: asset_version_parties allows the same
// party to hold two roles on one version (UNIQUE is on
// (version, role, party)), and collapsing the two would have to choose which
// role to print. Role is empty on the organization surface, which reads a
// version's origin rather than a credit.
type AssetItem struct {
	PID       string `json:"pid"`
	Title     string `json:"title"`
	AssetType string `json:"asset_type"`
	Version   string `json:"version"`
	// URL is the version's persistent address (/assets/{pid}/{version}), the
	// same string the asset hub links to (internal/assets.AssetVersionURL).
	URL  string `json:"url"`
	Role string `json:"role,omitempty"`
	// Project is the asset's ORIGIN project, nil when it may not be named —
	// a private project may publish a public version (docs/12 §2), and then
	// the version travels without it (internal/assets.BuildBrowse).
	Project     *ProjectRef `json:"project"`
	PublishedAt time.Time   `json:"published_at"`
}

// ReuseItem is one recorded usage of a version the profile's subject is
// credited on — docs/42's "used/derived public links" read from the far side:
// "who publicly uses this version" (internal/assets/usage.go).
type ReuseItem struct {
	PID       string `json:"pid"`
	Title     string `json:"title"`
	Version   string `json:"version"`
	URL       string `json:"url"`
	AssetType string `json:"asset_type"`
	// DependencyType is the stored use (asset_dependencies.dependency_type).
	DependencyType string `json:"dependency_type"`
	// DeclaredAt is when the using project published the declaration.
	DeclaredAt time.Time `json:"declared_at"`
	// Project is the USING project. It is never nil, so it is not a pointer:
	// a usage whose project is not public is dropped rather than rendered with
	// a withheld identity (internal/assets.PageUsage's second condition), and
	// the type says so — there is no state in which this field is empty and a
	// client that renders "used by" always has a project to name. The other
	// dimensions keep a pointer because their withheld case is real: an
	// asset's origin project and a reproduction's project may be withheld
	// while the row itself is the person's own.
	Project ProjectRef `json:"project"`
}

// ReproductionItem is one evidence assertion of a reproduction relation
// (docs/10 §4's `reproduces` / `fails_to_reproduce`), recorded by the
// profile's subject.
//
// What is NOT here is the object the assertion is about. Naming it would
// require the knowledge-publish audience rule (knowledgepublish.AudienceFor),
// which is that surface's application-layer policy — the same "one rule, one
// owner" split the asset page applies to project identity. What the profile
// renders is the fact that this person recorded an assertion of this relation
// in this project, with the state the row itself carries.
type ReproductionItem struct {
	Relation string `json:"relation"`
	// ReviewState is the assertion's OWN stored state
	// (unreviewed/reviewed/rejected, 00007). It is not a classification this
	// package makes, and it is not a number.
	ReviewState string    `json:"review_state"`
	CreatedAt   time.Time `json:"created_at"`
	// Project is the asserting project; nil when it may not be named. The row
	// is rendered either way: an assertion whose project is private is still
	// an assertion the person made, and the assertion carries no other
	// project fact.
	Project *ProjectRef `json:"project"`
}

// OrgProjectItem is one of an organization's own projects (docs/42's
// "projects" on the Organization Profile).
type OrgProjectItem struct {
	ID             string `json:"id"`
	Slug           string `json:"slug"`
	Name           string `json:"name"`
	Purpose        string `json:"purpose"`
	ActivityStatus string `json:"activity_status"`
	URL            string `json:"url"`
}

// OrganizationSubject is the organization an Organization Profile belongs to
// (docs/05 §4 "Organization Research Profile": persistent identity, and never
// a total of anything).
type OrganizationSubject struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
	URL         string `json:"url"`
}

// PersonProfile is the whole Research Profile answer (docs/42, docs/05 §4).
//
// Every dimension is a LIST OF FACTS and the payload contains no aggregate of
// any kind — no total, no per-dimension count, no weight, no rank, no
// ordering key. docs/13 §4 ("禁止单一分数。Profile 展示多维 evidence"),
// docs/13 §6 (raw counts are not quality proxies) and CLAUDE.md §9 invariant
// 13 (no Truth Score / Research Score) are why, and
// TestPersonProfilePayloadCarriesNoScore asserts it on the wire shape rather
// than trusting this comment.
type PersonProfile struct {
	Person        PersonSubject      `json:"person"`
	Affiliations  []AffiliationItem  `json:"affiliations"`
	Contributions []ContributionItem `json:"contributions"`
	Assets        []AssetItem        `json:"assets"`
	Reuse         []ReuseItem        `json:"reuse"`
	Reproductions []ReproductionItem `json:"reproductions"`
	// Projects is derived from the rows above — the public projects the
	// rendered contributions name and the public origin projects of the
	// rendered asset credits — rather than from a second read:
	// project_memberships is a governance relation (who may write), and a
	// profile publishes a person's WORK, not their project ACL. The order is
	// id ascending and carries NO meaning.
	Projects []ProjectRef `json:"projects"`
}

// OrganizationProfile is the whole Organization Profile answer: the
// institution's public research identity (docs/05 §4) as activity it can be
// held to.
type OrganizationProfile struct {
	Organization OrganizationSubject `json:"organization"`
	Projects     []OrgProjectItem    `json:"projects"`
	// Activity is what the organization's affiliated people did while they
	// were affiliated: the ledger rows whose organization_id_at_time is this
	// organization (00011's column, resolved at event time by the T0807
	// projection).
	Activity []ContributionItem `json:"activity"`
	// Assets are the published versions the organization's own PUBLIC
	// projects produced. A private project's versions are not drawn on here
	// even when the version itself is public: the asset hub already lists
	// that version without its project, and attaching it to the organization
	// from this surface would publish a link between the two that no rule
	// has decided (docs/23 §5).
	Assets []AssetItem `json:"assets"`
}

// PersonInput is what the readers resolved for one person, before this
// package's rules are applied. Every slice is the reader's answer for this
// person, bounded by researchprofile.FetchLimit and already narrowed by the
// query's render predicate (the SQL file's header records that split) — and
// still carrying rows this package will drop, because a read strategy and a
// disclosure rule are not the same thing and the predicates here are the
// second one.
type PersonInput struct {
	Person        PersonRow
	Affiliations  []AffiliationRow
	Contributions []ContributionRow
	Assets        []AssetCreditRow
	Reuse         []ReuseRow
	Reproductions []ReproductionRow
}

// OrganizationInput is the same for one organization.
type OrganizationInput struct {
	Organization OrganizationRow
	Projects     []ProjectRow
	Activity     []ContributionRow
	Assets       []AssetCreditRow
}

// PersonVisible reports whether this person has a public research profile at
// all. The transport answers the same existence-hiding 404 for a disabled
// account as for an unknown id (docs/45), because "this account exists but is
// disabled" is a fact about it that no public surface states.
//
// The rule is not invented here: internal/events' audience rule answers
// 'none' for a disabled account to everyone but the account itself ("WHEN
// u.disabled_at IS NULL THEN 'public' ELSE 'none'"), and
// internal/application/explore drops disabled accounts from the people
// directory. A profile is the same kind of public surface.
func PersonVisible(p PersonRow) bool { return p.ID != "" && p.DisabledAt == nil }

// OrganizationActive reports whether this organization has a public profile.
// Deactivation is the only "delete" this domain has (CLAUDE.md §9.8) and a
// deactivated organization has no public surface: internal/events gives it
// 'none' for non-members, and explore leaves it out of the directory.
func OrganizationActive(o OrganizationRow) bool { return o.ID != "" && o.DeactivatedAt == nil }

// BuildPersonProfile applies the surface's rules to the rows the readers
// resolved and renders the profile.
//
// Three rules, in this order:
//
//  1. A row is renderable only when its own state says public, re-checked
//     here even though every store query already carries the predicate — the
//     shape internal/application/explore documents: a reader regression
//     renders a SHORTER profile, never a leak.
//
//  2. An identity is named only when it is public: a project a row points
//     into renders as nil unless it is public, and an organization an
//     affiliation names renders as nil when it is deactivated. The row
//     survives; the identity does not. (An affiliation is the person's own
//     history — docs/04 §6 "组织不能删除个人历史贡献；离职只终止
//     affiliation/role" — so a deactivated employer leaves the role, the
//     dates and the verification in place.)
//
//  3. A withheld row is DROPPED, not counted. docs/23 §5 forbids a private
//     count on a public API, and V1's option there is to render none at all:
//     the bytes that leave this function hold no placeholder, no gap and no
//     difference between two numbers from which the count could be
//     reconstructed.
func BuildPersonProfile(in PersonInput) PersonProfile {
	affiliations := make([]AffiliationItem, 0, len(in.Affiliations))
	for _, row := range in.Affiliations {
		if row.OrganizationID == "" && row.Role == "" {
			// A membership row always names an organization and a role
			// (00002: both NOT NULL). A row missing them is a reader defect,
			// and dropping it is the fail-closed answer — an affiliation with
			// no employer is not a fact this profile can render.
			continue
		}
		item := AffiliationItem{
			Role:     row.Role,
			Start:    calendarDate(row.AffiliationStart),
			End:      calendarDate(row.AffiliationEnd),
			Verified: row.Verified,
		}
		if row.OrganizationID != "" && row.OrganizationDeactivatedAt == nil {
			item.Organization = &OrganizationRef{
				ID:   row.OrganizationID,
				Slug: row.OrganizationSlug,
				Name: row.OrganizationName,
				URL:  webOrganizationPath(row.OrganizationSlug),
			}
		}
		affiliations = append(affiliations, item)
	}
	sortStable(affiliations, func(a, b AffiliationItem) bool {
		return affiliationKey(a) > affiliationKey(b)
	})

	contributions := make([]ContributionItem, 0, len(in.Contributions))
	projects := map[string]ProjectRef{}
	for _, row := range in.Contributions {
		project := publicProject(row.ProjectID, row.ProjectSlug, row.ProjectName, row.ProjectVisibility)
		if project == nil {
			// The contribution happened in a project this surface may not
			// name, so it may not state the contribution either: the event is
			// a fact about that project's work rather than a public object of
			// its own (docs/12 §2 — a private project's activity is private).
			continue
		}
		projects[project.ID] = *project
		contributions = append(contributions, ContributionItem{
			EventType:  row.EventType,
			RoleCodes:  roleCodes(row.RoleCodes),
			OccurredAt: row.OccurredAt.UTC(),
			Accepted:   row.Accepted,
			Released:   row.Released,
			Via:        row.Via,
			Project:    project,
		})
	}
	sortStable(contributions, func(a, b ContributionItem) bool {
		return a.OccurredAt.After(b.OccurredAt)
	})

	assetItems := make([]AssetItem, 0, len(in.Assets))
	for _, row := range in.Assets {
		if !publicVersion(row.VersionVisibility) {
			continue
		}
		item := AssetItem{
			PID:         row.PID,
			Title:       row.Title,
			AssetType:   row.AssetType,
			Version:     row.Version,
			URL:         assets.AssetVersionURL(assets.PID(row.PID), row.Version),
			Role:        row.Role,
			Project:     publicProject(row.ProjectID, row.ProjectSlug, row.ProjectName, row.ProjectVisibility),
			PublishedAt: row.PublishedAt.UTC(),
		}
		if item.Project != nil {
			projects[item.Project.ID] = *item.Project
		}
		assetItems = append(assetItems, item)
	}
	sortStable(assetItems, func(a, b AssetItem) bool { return a.PublishedAt.After(b.PublishedAt) })

	reuse := make([]ReuseItem, 0, len(in.Reuse))
	for _, row := range in.Reuse {
		// PageUsage's two conditions — the declaration must be public and the
		// USING project must be public (a private project may use a public
		// asset, and printing its name would disclose its existence) — plus
		// mayLinkVersion on the USED version: the URL this row prints points
		// at a version other than the profile's own asset, and that address
		// answers the existence-hiding 404 outside its project
		// (internal/assets/page.go: "a link that answers 404 is the same
		// disclosure as a bad link with a worse outcome").
		//
		// The assets dimension above applies the HUB's rule instead, because
		// its row is about the asset itself rather than a link into it, so a
		// public version of a private project is listed there without its
		// project (internal/assets.BuildBrowse does the same). Two rules,
		// each borrowed from the surface that already decided it.
		if row.UsageVisibility != string(assets.VisibilityPublic) {
			continue
		}
		if !mayLinkVersion(row.VersionVisibility, row.VersionProjectVisibility) {
			continue
		}
		project := publicProject(row.ProjectID, row.ProjectSlug, row.ProjectName, row.ProjectVisibility)
		if project == nil {
			continue
		}
		reuse = append(reuse, ReuseItem{
			PID:            row.PID,
			Title:          row.Title,
			AssetType:      row.AssetType,
			Version:        row.Version,
			URL:            assets.AssetVersionURL(assets.PID(row.PID), row.Version),
			DependencyType: row.DependencyType,
			DeclaredAt:     row.DeclaredAt.UTC(),
			Project:        *project,
		})
	}
	sortStable(reuse, func(a, b ReuseItem) bool { return a.DeclaredAt.After(b.DeclaredAt) })

	reproductions := make([]ReproductionItem, 0, len(in.Reproductions))
	for _, row := range in.Reproductions {
		// Two rules, and they are independent of each other.
		//
		// The relation: only docs/10 §4's two reproduction relations are this
		// dimension's.
		if !reproductionRelation(row.Relation) {
			continue
		}
		// The assertion's OWN visibility (00091): evidence_assertions carries
		// its own axis, separate from the project's, and its header is explicit
		// that "an assertion nothing explicitly made public is not rendered
		// anywhere" — the column's DEFAULT is 'private'. Both profile surfaces
		// are anonymous, so an assertion its author never published is not this
		// profile's to show, whatever the project's visibility says.
		//
		// The project's visibility is NOT this rule: a private project's
		// assertion is still the person's own act, and publicProject below
		// withholds the project's identity rather than the row (docs/12 §2 —
		// what is withheld is the project's existence, not the person's
		// history).
		if !isPublicVisibility(row.AssertionVisibility) {
			continue
		}
		reproductions = append(reproductions, ReproductionItem{
			Relation:    row.Relation,
			ReviewState: row.ReviewState,
			CreatedAt:   row.CreatedAt.UTC(),
			Project:     publicProject(row.ProjectID, row.ProjectSlug, row.ProjectName, row.ProjectVisibility),
		})
	}
	sortStable(reproductions, func(a, b ReproductionItem) bool { return a.CreatedAt.After(b.CreatedAt) })

	projectList := make([]ProjectRef, 0, len(projects))
	for _, p := range projects {
		projectList = append(projectList, p)
	}
	sort.SliceStable(projectList, func(i, j int) bool { return projectList[i].ID < projectList[j].ID })

	return PersonProfile{
		Person: PersonSubject{
			ID:          in.Person.ID,
			Handle:      in.Person.Handle,
			DisplayName: in.Person.DisplayName,
			Bio:         in.Person.Bio,
			URL:         webProfilePath(in.Person.ID),
		},
		Affiliations:  capAt(affiliations),
		Contributions: capAt(contributions),
		Assets:        capAt(assetItems),
		Reuse:         capAt(reuse),
		Reproductions: capAt(reproductions),
		Projects:      projectList,
	}
}

// BuildOrganizationProfile applies the same rules to an organization's rows.
//
// Two differences from the person surface, both stated where they are decided
// rather than left to the reader:
//
//   - The identities a row points into ARE dropped row-and-all here, because
//     an organization's activity is made of OTHER people's work: a
//     contribution into a private project, or by an account that is no longer
//     part of the network, is not this institution's public record. A person
//     surface renders those rows with a withheld identity because the row is
//     that person's own; an institution's surface has no such claim on it.
//   - An asset is drawn only from one of the organization's PUBLIC projects,
//     so this surface never publishes a link between itself and a project it
//     may not name (see OrganizationProfile.Assets).
func BuildOrganizationProfile(in OrganizationInput) OrganizationProfile {
	publicProjectIDs := map[string]bool{}
	projects := make([]OrgProjectItem, 0, len(in.Projects))
	for _, p := range in.Projects {
		if !isPublicVisibility(p.Visibility) || p.ID == "" {
			continue
		}
		publicProjectIDs[p.ID] = true
		projects = append(projects, OrgProjectItem{
			ID:             p.ID,
			Slug:           p.Slug,
			Name:           p.Name,
			Purpose:        p.Purpose,
			ActivityStatus: p.ActivityStatus,
			URL:            webProjectPath(p.ID),
		})
	}
	// Order: slug ascending, an organization's own naming of its work. It
	// carries no ranking — nothing here is sorted by how much anything got.
	sort.SliceStable(projects, func(i, j int) bool {
		if projects[i].Slug != projects[j].Slug {
			return projects[i].Slug < projects[j].Slug
		}
		return projects[i].ID < projects[j].ID
	})

	activity := make([]ContributionItem, 0, len(in.Activity))
	for _, row := range in.Activity {
		if row.ActorID == "" || row.ActorDisabledAt != nil {
			// The row credits an account that is not part of the network (or
			// names none at all). An institution's public activity is
			// attributed work, and an unattributable row is not renderable
			// here — dropped, never shown as "someone".
			continue
		}
		project := publicProject(row.ProjectID, row.ProjectSlug, row.ProjectName, row.ProjectVisibility)
		if project == nil {
			continue
		}
		activity = append(activity, ContributionItem{
			EventType:  row.EventType,
			RoleCodes:  roleCodes(row.RoleCodes),
			OccurredAt: row.OccurredAt.UTC(),
			Accepted:   row.Accepted,
			Released:   row.Released,
			Via:        row.Via,
			Project:    project,
			Actor: &PersonRef{
				ID:          row.ActorID,
				Handle:      row.ActorHandle,
				DisplayName: row.ActorDisplayName,
				URL:         webProfilePath(row.ActorID),
			},
		})
	}
	sortStable(activity, func(a, b ContributionItem) bool { return a.OccurredAt.After(b.OccurredAt) })

	assetItems := make([]AssetItem, 0, len(in.Assets))
	for _, row := range in.Assets {
		if !publicVersion(row.VersionVisibility) {
			continue
		}
		if !publicProjectIDs[row.ProjectID] {
			continue
		}
		assetItems = append(assetItems, AssetItem{
			PID:       row.PID,
			Title:     row.Title,
			AssetType: row.AssetType,
			Version:   row.Version,
			URL:       assets.AssetVersionURL(assets.PID(row.PID), row.Version),
			Project: publicProject(row.ProjectID, row.ProjectSlug, row.ProjectName,
				row.ProjectVisibility),
			PublishedAt: row.PublishedAt.UTC(),
		})
	}
	sortStable(assetItems, func(a, b AssetItem) bool { return a.PublishedAt.After(b.PublishedAt) })

	return OrganizationProfile{
		Organization: OrganizationSubject{
			ID:          in.Organization.ID,
			Slug:        in.Organization.Slug,
			Name:        in.Organization.Name,
			Description: in.Organization.Description,
			URL:         webOrganizationPath(in.Organization.Slug),
		},
		Projects: projects,
		Activity: capAt(activity),
		Assets:   capAt(assetItems),
	}
}

// publicProject is the one reading of "this project may be named": it must
// have an id AND be public. The id requirement is the fail-closed half — a
// row whose project the reader could not resolve has an empty visibility,
// which is not public either, so both conditions usually agree; stating both
// means a reader that filled in a name but not a visibility still gets nil.
func publicProject(id, slug, name, visibility string) *ProjectRef {
	if id == "" || !isPublicVisibility(visibility) {
		return nil
	}
	return &ProjectRef{ID: id, Slug: slug, Name: name, URL: webProjectPath(id)}
}

func isPublicVisibility(v string) bool { return v == string(domain.VisibilityPublic) }

func publicVersion(v string) bool { return v == string(assets.VisibilityPublic) }

// mayLinkVersion is internal/assets' rule for naming a version OTHER than the
// one a page is about, restated here because this package cannot import the
// unexported original: both axes must be public, the version's and its
// asset's origin project's, or the address answers the existence-hiding 404
// to everyone outside that project. It is the same predicate, spelled out
// where the citations to it are.
func mayLinkVersion(version, project string) bool {
	return publicVersion(version) && isPublicVisibility(project)
}

// reproductionRelation is the pair of relations docs/10 §4 gives the
// reproduction dimension. Both are rendered and neither is preferred: docs/10
// §4 ends with "V1 不自动赋数值权重", and a failed reproduction is evidence
// about a claim, not a demerit for the person who recorded it.
func reproductionRelation(relation string) bool {
	return relation == "reproduces" || relation == "fails_to_reproduce"
}

// calendarDate renders a date column as the calendar day it is, in the one
// text form the rest of the platform uses (domain.AffiliationDayText), or nil
// when the column is NULL. The value is a PostgreSQL `date`; rendering it
// through a timestamp would attach a time of day the column does not have.
func calendarDate(d *time.Time) *string {
	if d == nil {
		return nil
	}
	text := domain.AffiliationDayText(domain.AffiliationDay(*d))
	return &text
}

// affiliationKey is the ordering key of an affiliation list: the start date
// descending (an undated affiliation sorts last), then the employer's id. It
// is a display order, not a rank — no affiliation is "better" than another,
// and nothing is dropped by it.
func affiliationKey(a AffiliationItem) string {
	start := ""
	if a.Start != nil {
		start = *a.Start
	}
	if a.Organization != nil {
		return start + "\x00" + a.Organization.ID
	}
	return start + "\x00"
}

// roleCodes copies the stored role codes verbatim, deduplicated and sorted:
// the ledger column is a set (contribution_events.role_codes is text[]), and
// rendering a caller's array order as if it meant something — first author,
// say — would be a ranking this platform does not assign.
func roleCodes(codes []string) []string {
	out := make([]string, 0, len(codes))
	seen := make(map[string]bool, len(codes))
	for _, c := range codes {
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// sortStable orders a dimension newest first. The readers already order their
// answers deterministically (each query ends in a unique tiebreaker), and a
// stable sort keeps that order for rows equal on the freshness key — so two
// reads of one state render the same list.
func sortStable[T any](rows []T, newer func(a, b T) bool) {
	sort.SliceStable(rows, func(i, j int) bool { return newer(rows[i], rows[j]) })
}

// capAt keeps the newest RenderLimit rows of a dimension.
func capAt[T any](items []T) []T {
	if len(items) > RenderLimit {
		return items[:RenderLimit]
	}
	return items
}
