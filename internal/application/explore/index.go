package explore

import (
	"sort"
	"time"

	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/contribution"
	"github.com/lichman0405/post/internal/domain"
)

// Tab names one dimension of the Explore surface. The six values are
// docs/05 §6's dimensions, verbatim.
type Tab string

const (
	TabProjects      Tab = "projects"
	TabAssets        Tab = "assets"
	TabKnowledge     Tab = "knowledge"
	TabPeople        Tab = "people"
	TabOrganizations Tab = "organizations"
	TabContributions Tab = "contributions"
)

// Tabs is the surface's tab set in render order.
func Tabs() []Tab {
	return []Tab{TabProjects, TabAssets, TabKnowledge, TabPeople, TabOrganizations, TabContributions}
}

// SectionLimit is how many rows one tab of the index renders.
//
// The cap is the section's, not the reader's: the reused readers return
// everything they hold (none of them pages), and the index keeps the newest
// SectionLimit of each kind. Nothing is hidden by it that a reader could not
// also reach there — the rows a section drops are the OLDEST ones, and each
// tab links to the full surface where one exists (/projects, /assets). No
// total is rendered, so the cap cannot be read as a count of anything
// (docs/23 §5).
const SectionLimit = 20

// webProjectPath is the web app's route for a project
// (apps/web/app/(main)/projects/[id]), the same path lib/entity-meta.ts
// builds for the project entity.
//
// A section renders a link only where the route exists: /projects/{id} and
// /users/{id} do (T0108, T0801), /assets/{pid} comes with the asset item
// itself (T0709). There is no knowledge route and no organization route yet
// (T0805 and T0808 own them), so those two sections carry no URL at all — a
// link to a route that does not exist is a lie in the shape of an href, and
// the index renders identity (id, version, slug) instead.
func webProjectPath(id string) string { return "/projects/" + id }

// webProfilePath is the public profile route (apps/web/app/(main)/users/[id],
// T0801).
func webProfilePath(id string) string { return "/users/" + id }

// ProjectRef is the project identity a row may print.
//
// It exists only for projects the readers resolved as PUBLIC: a row whose
// project is private carries nil, and the rendering of "null" says nothing
// about whether a project exists (no count of withheld rows is rendered
// either).
type ProjectRef struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

// ProjectItem is one public project (docs/05 §6 "Projects").
type ProjectItem struct {
	ID string `json:"id"`
	// Slug and Name are the project's public identity.
	Slug string `json:"slug"`
	Name string `json:"name"`
	// Purpose is the one-sentence research goal (domain.Project.Purpose).
	Purpose string `json:"purpose"`
	// ActivityStatus is the project's lifecycle state (planning/active/
	// paused/archived). It is not a ranking input.
	ActivityStatus string `json:"activity_status"`
	// URL is the project's web page (/projects/{id}).
	URL string `json:"url"`
	// CreatedAt is when the project was created — the section's freshness
	// key. It is already public: the project read carries it
	// (cmd/api/projectshttp.projectPayload), so rendering it here is not a
	// new disclosure.
	CreatedAt time.Time `json:"created_at"`
}

// KnowledgeItem is one published knowledge object version (docs/05 §6
// "Knowledge"): a knowledge_publications row, which is what "published to
// the network" means for a scientific object.
type KnowledgeItem struct {
	// ID is the publication's own id.
	ID string `json:"id"`
	// ObjectID identifies the scientific object; PublicVersion is the
	// version label the publication pinned. The two are rendered apart so a
	// version can never be mistaken for an object.
	ObjectID      string `json:"object_id"`
	ObjectType    string `json:"object_type"`
	PublicVersion string `json:"public_version"`
	Title         string `json:"title"`
	// Project is the publishing project, nil when that project is private
	// (docs/12 §2: a private project may publish, and then the publication
	// travels without it).
	Project *ProjectRef `json:"project"`
	// PublishedAt is when the publication was made — the section's
	// freshness key.
	PublishedAt time.Time `json:"published_at"`
	// LifecycleState is the published VERSION's own state
	// (active/aborted/reopened/superseded, docs/43). It is rendered because
	// docs/43 keeps a version's content immutable while allowing a later
	// status notice: a reader of this index is entitled to see that the
	// research behind a published version was withdrawn or superseded,
	// instead of reading it as current.
	LifecycleState string `json:"lifecycle_state"`
}

// PersonItem is one public research profile (docs/05 §6 "People",
// docs/02 §3 "公开 Research Profile").
type PersonItem struct {
	ID          string `json:"id"`
	Handle      string `json:"handle"`
	DisplayName string `json:"display_name"`
	Bio         string `json:"bio"`
	// URL is the profile's web page (/users/{id}).
	URL string `json:"url"`
	// The account's creation time is DELIBERATELY absent, and it is the one
	// freshness key the surface does not render: a project's created_at and
	// a publication's published_at are already public facts of those
	// entities, while "when did this person register" is not published
	// anywhere else. The section still ranks on it (newest accounts first) —
	// ordering is a property of the list, not a fact rendered per row.
}

// OrganizationItem is one active organization (docs/05 §6 "Organizations").
type OrganizationItem struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// No counts and no created_at, for the reason above: an organization's
	// membership, projects and assets are not this surface's to total
	// (docs/23 §5).
}

// ContributionItem is one open contribution opportunity (docs/05 §6 "Open
// Contributions", docs/14 §6): a publicized, still-open row — the "what can
// I do here" entry point docs/14 §6 asks the open network to offer.
type ContributionItem struct {
	ID                   string   `json:"id"`
	Title                string   `json:"title"`
	Description          string   `json:"description"`
	Difficulty           string   `json:"difficulty"`
	TargetType           string   `json:"target_type"`
	RequiredCapabilities []string `json:"required_capabilities"`
	// Project is nil when the project is private. The opportunity is public
	// (it was explicitly publicized); whose it is, may not be.
	Project *ProjectRef `json:"project"`
	// PublicizedAt is when the row entered the open network — the section's
	// freshness key. It is never nil for a rendered row (migration 00062's
	// guard stamps it as part of the publicize that makes the row public).
	PublicizedAt time.Time `json:"publicized_at"`
}

// Section is one tab's answer: the tab it belongs to and the rows it holds.
//
// A section never carries a total. The count of what a reader can see is
// len(Items) and says exactly that; a total would have to be subtracted
// from to be interesting, and the difference would be the count of public
// rows the cap dropped or of private rows behind them — the number docs/23
// §5 forbids a public API to leak.
type Section[T any] struct {
	Tab   Tab `json:"tab"`
	Items []T `json:"items"`
}

// Index is the whole answer: the six sections of the Explore surface.
type Index struct {
	Projects      Section[ProjectItem]       `json:"projects"`
	Assets        Section[assets.BrowseItem] `json:"assets"`
	Knowledge     Section[KnowledgeItem]     `json:"knowledge"`
	People        Section[PersonItem]        `json:"people"`
	Organizations Section[OrganizationItem]  `json:"organizations"`
	Contributions Section[ContributionItem]  `json:"contributions"`
}

// Input is what the readers resolved, before this package's rules are
// applied. Every slice is the reader's own public set; BuildIndex does not
// trust that, which is why each section re-checks the state it can see.
type Input struct {
	Projects      []domain.Project
	Assets        []assets.BrowseItem
	Knowledge     []KnowledgeRow
	People        []PersonRow
	Organizations []OrganizationRow
	Contributions []contribution.ContributionOpportunity
}

// BuildIndex applies the surface's rules to the rows the readers resolved
// and renders the index.
//
// Three rules, in this order:
//
//  1. A row is renderable only when its own state says public — a public
//     project, a person account that is not disabled, an organization that
//     is not deactivated, a publicized-and-open opportunity. The readers
//     filter already (their queries carry the predicate); re-checking here
//     costs nothing and means a reader regression renders a shorter index
//     rather than a leak. Asset rows are the one exception: they arrive as
//     already-built BrowseItems (T0709's BuildBrowse decided what may be
//     rendered), so there is no state left to re-read.
//
//  2. A project is named only when it is in the PUBLIC project set built in
//     step 1 — this is the rule for knowledge and for opportunities, whose
//     rows travel without their project. An id that is not in that set
//     renders as nil, and a reader that handed over a private project's row
//     cannot make it appear.
//
//  3. Each section is ordered by freshness, newest first, ties broken by
//     identity, then capped at SectionLimit. This is the whole ranking:
//     there is no popularity input, and there is no field in this payload a
//     popularity input could hide in.
func BuildIndex(in Input) Index {
	public := publicProjectSet(in.Projects)

	// The rows are filtered and ordered BEFORE they are rendered into items,
	// so no rendered item has to carry a hidden ranking key: the order of a
	// section is the order of the rows it was built from. A person's or an
	// organization's creation time therefore never reaches the payload (see
	// PersonItem) while still deciding where the row sits.
	projects := publicProjects(in.Projects)
	sortRows(projects,
		func(a, b domain.Project) bool { return a.CreatedAt.After(b.CreatedAt) },
		func(a, b domain.Project) bool { return a.ID < b.ID })

	assetItems := make([]assets.BrowseItem, len(in.Assets))
	copy(assetItems, in.Assets)
	sortRows(assetItems,
		func(a, b assets.BrowseItem) bool { return a.LatestPublishedAt.After(b.LatestPublishedAt) },
		func(a, b assets.BrowseItem) bool { return a.PID < b.PID })

	knowledgeRows := make([]KnowledgeRow, 0, len(in.Knowledge))
	for _, row := range in.Knowledge {
		if row.Published() {
			knowledgeRows = append(knowledgeRows, row)
		}
	}
	sortRows(knowledgeRows,
		func(a, b KnowledgeRow) bool { return a.PublishedAt.After(b.PublishedAt) },
		func(a, b KnowledgeRow) bool { return a.PublicationID < b.PublicationID })

	personRows := make([]PersonRow, 0, len(in.People))
	for _, row := range in.People {
		if !row.Disabled() {
			personRows = append(personRows, row)
		}
	}
	sortRows(personRows,
		func(a, b PersonRow) bool { return a.CreatedAt.After(b.CreatedAt) },
		func(a, b PersonRow) bool { return a.ID < b.ID })

	orgRows := make([]OrganizationRow, 0, len(in.Organizations))
	for _, row := range in.Organizations {
		if !row.Deactivated() {
			orgRows = append(orgRows, row)
		}
	}
	sortRows(orgRows,
		func(a, b OrganizationRow) bool { return a.CreatedAt.After(b.CreatedAt) },
		func(a, b OrganizationRow) bool { return a.ID < b.ID })

	opportunities := make([]contribution.ContributionOpportunity, 0, len(in.Contributions))
	for _, o := range in.Contributions {
		if isOpenPublicOpportunity(o) {
			opportunities = append(opportunities, o)
		}
	}
	sortRows(opportunities,
		func(a, b contribution.ContributionOpportunity) bool {
			return publicizedAt(a).After(publicizedAt(b))
		},
		func(a, b contribution.ContributionOpportunity) bool { return a.ID < b.ID })

	// Render, in the order the rows were put in.
	projectItems := make([]ProjectItem, 0, len(projects))
	for _, p := range projects {
		projectItems = append(projectItems, ProjectItem{
			ID:             p.ID,
			Slug:           p.Slug,
			Name:           p.Name,
			Purpose:        p.Purpose,
			ActivityStatus: p.ActivityStatus,
			URL:            webProjectPath(p.ID),
			CreatedAt:      p.CreatedAt.UTC(),
		})
	}

	knowledgeItems := make([]KnowledgeItem, 0, len(knowledgeRows))
	for _, row := range knowledgeRows {
		knowledgeItems = append(knowledgeItems, KnowledgeItem{
			ID:             row.PublicationID,
			ObjectID:       row.ObjectID,
			ObjectType:     row.ObjectType,
			PublicVersion:  row.PublicVersion,
			Title:          row.Title,
			Project:        public[row.ProjectID],
			PublishedAt:    row.PublishedAt.UTC(),
			LifecycleState: row.LifecycleState,
		})
	}

	peopleItems := make([]PersonItem, 0, len(personRows))
	for _, row := range personRows {
		peopleItems = append(peopleItems, PersonItem{
			ID:          row.ID,
			Handle:      row.Handle,
			DisplayName: row.DisplayName,
			Bio:         row.Bio,
			URL:         webProfilePath(row.ID),
		})
	}

	orgItems := make([]OrganizationItem, 0, len(orgRows))
	for _, row := range orgRows {
		orgItems = append(orgItems, OrganizationItem{
			ID:          row.ID,
			Slug:        row.Slug,
			Name:        row.Name,
			Description: row.Description,
		})
	}

	contributionItems := make([]ContributionItem, 0, len(opportunities))
	for _, o := range opportunities {
		contributionItems = append(contributionItems, ContributionItem{
			ID:                   o.ID,
			Title:                o.Title,
			Description:          o.Description,
			Difficulty:           string(o.Difficulty),
			TargetType:           string(o.TargetType),
			RequiredCapabilities: capabilities(o.RequiredCapabilities),
			Project:              public[o.ProjectID],
			PublicizedAt:         publicizedAt(o).UTC(),
		})
	}

	return Index{
		Projects:      Section[ProjectItem]{Tab: TabProjects, Items: capAt(projectItems)},
		Assets:        Section[assets.BrowseItem]{Tab: TabAssets, Items: capAt(assetItems)},
		Knowledge:     Section[KnowledgeItem]{Tab: TabKnowledge, Items: capAt(knowledgeItems)},
		People:        Section[PersonItem]{Tab: TabPeople, Items: capAt(peopleItems)},
		Organizations: Section[OrganizationItem]{Tab: TabOrganizations, Items: capAt(orgItems)},
		Contributions: Section[ContributionItem]{Tab: TabContributions, Items: capAt(contributionItems)},
	}
}

// publicProjects keeps only the projects that may be listed, in no
// particular order (the caller sorts).
func publicProjects(projects []domain.Project) []domain.Project {
	out := make([]domain.Project, 0, len(projects))
	for _, p := range projects {
		if isPublicProject(p) {
			out = append(out, p)
		}
	}
	return out
}

// isPublicProject is the one reading of "this project may be listed".
func isPublicProject(p domain.Project) bool {
	return p.ID != "" && p.Visibility == domain.VisibilityPublic
}

// publicProjectSet is the set of projects the index may NAME. It is built
// from the same public rows the Projects section renders, so the two can
// never disagree about which project is public.
func publicProjectSet(projects []domain.Project) map[string]*ProjectRef {
	set := make(map[string]*ProjectRef, len(projects))
	for _, p := range projects {
		if !isPublicProject(p) {
			continue
		}
		set[p.ID] = &ProjectRef{ID: p.ID, Slug: p.Slug, Name: p.Name, URL: webProjectPath(p.ID)}
	}
	return set
}

// isOpenPublicOpportunity is the one reading of "this opportunity is part
// of the open network": publicized by the explicit action (docs/12 §3) and
// still open. A closed row stays as history (invariant 8), but it is not
// something a contributor can take on, and an internal row is not on the
// network at all.
func isOpenPublicOpportunity(o contribution.ContributionOpportunity) bool {
	return o.ID != "" &&
		o.Visibility == contribution.OpportunityVisibilityPublic &&
		o.State == contribution.OpportunityStateOpen
}

// publicizedAt is the freshness key of a publicized row. Migration 00062's
// guard stamps it as part of the publicize that makes a row public, so a
// rendered row always has one; the zero time is used only by the
// impossible case, and such a row still sorts (last) rather than dropping,
// so a reader defect cannot silently shrink the index.
func publicizedAt(o contribution.ContributionOpportunity) time.Time {
	if o.PublicizedAt == nil {
		return time.Time{}
	}
	return *o.PublicizedAt
}

// capabilities copies the required-capability tags in canonical (sorted)
// order, so the rendering is stable and no caller's order leaks into an
// answer.
func capabilities(tags []string) []string {
	out := make([]string, 0, len(tags))
	out = append(out, tags...)
	sort.Strings(out)
	return out
}

// sortRows orders a section's rows by their freshness key (newest first),
// breaking ties by identity so two reads of one state render the same list.
func sortRows[T any](rows []T, newer func(a, b T) bool, lessID func(a, b T) bool) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if !newer(a, b) && !newer(b, a) {
			// Neither is newer: the rows are equal on the ranking key, so
			// identity decides.
			return lessID(a, b)
		}
		return newer(a, b)
	})
}

// capAt keeps the newest SectionLimit rows of a section.
func capAt[T any](items []T) []T {
	if len(items) > SectionLimit {
		return items[:SectionLimit]
	}
	return items
}
