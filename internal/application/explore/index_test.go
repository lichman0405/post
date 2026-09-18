package explore_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/application/explore"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/contribution"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rights"
)

var base = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func at(daysAgo int) time.Time { return base.AddDate(0, 0, -daysAgo) }

func publicProject(id string, age int) domain.Project {
	return domain.Project{
		ID:             id,
		Slug:           "slug-" + id,
		Name:           "Project " + id,
		Purpose:        "research goal " + id,
		ActivityStatus: "active",
		Visibility:     domain.VisibilityPublic,
		CreatedAt:      at(age),
	}
}

func privateProject(id string, age int) domain.Project {
	p := publicProject(id, age)
	p.Visibility = domain.VisibilityPrivate
	p.Name = "SECRET " + id
	p.Slug = "secret-" + id
	return p
}

// publishedKnowledge builds a row the NETWORK may see: published, from a
// public project, with the version inheriting that visibility and a rights
// declaration that defers to the project's policy.
//
// The audience inputs are not decoration since T0805. A publication exists
// the moment someone writes it, and whether the network may read it is a
// SEPARATE question the version's own visibility axis answers
// (knowledgepublish.AudienceFor) — so a fixture that left them zero would
// describe a publication that is real and not the network's, which is the
// case restrictedKnowledge below covers deliberately.
func publishedKnowledge(id, projectID string, age int) explore.KnowledgeRow {
	row := restrictedKnowledge(id, projectID, age)
	row.ProjectVisibility = "public"
	row.Rights = projectPolicyRights()
	row.RightsValid = true
	return row
}

// restrictedKnowledge builds a publication the NETWORK may not see: the row
// exists and is published, and the version's own visibility is not the
// project's. Everything the two share lives here so a change to the row's
// shape cannot leave the two fixtures disagreeing about anything but the
// audience.
func restrictedKnowledge(id, projectID string, age int) explore.KnowledgeRow {
	return explore.KnowledgeRow{
		PublicationID:  id,
		ObjectID:       "obj-" + id,
		ObjectType:     "research_question",
		PublicVersion:  "v1",
		Title:          "Knowledge " + id,
		ProjectID:      projectID,
		PublishedAt:    at(age),
		LifecycleState: "active",
	}
}

// projectPolicyRights is the rights declaration whose metadata axis is the
// fail-closed default ("whatever the owning project's policy says").
func projectPolicyRights() rights.Document {
	var doc rights.Document
	doc.Visibility.Metadata = rights.MetadataProjectPolicy
	return doc
}

func opportunity(id, projectID string, age int, vis contribution.OpportunityVisibility, state contribution.OpportunityState) contribution.ContributionOpportunity {
	pub := at(age)
	o := contribution.ContributionOpportunity{
		ID:           id,
		ProjectID:    projectID,
		TargetType:   contribution.TargetResearchQuestion,
		Title:        "Opportunity " + id,
		Description:  "context " + id,
		Difficulty:   contribution.Difficulty("beginner"),
		State:        state,
		Visibility:   vis,
		CreatedAt:    at(age + 1),
		PublicizedAt: &pub,
	}
	if vis != contribution.OpportunityVisibilityPublic {
		o.PublicizedAt = nil
	}
	return o
}

func assetItem(pid string, age int) assets.BrowseItem {
	return assets.BrowseItem{
		PID:               assets.PID(pid),
		Type:              assets.TypeDataset,
		Title:             "Asset " + pid,
		Slug:              "asset-" + pid,
		URL:               "/assets/" + pid,
		LatestVersion:     "1.0.0",
		LatestURL:         "/assets/" + pid + "/1.0.0",
		LatestPublishedAt: at(age),
	}
}

// idsOf renders a section as its identity list, in order.
func projIDs(in explore.Index) []string {
	out := make([]string, 0, len(in.Projects.Items))
	for _, p := range in.Projects.Items {
		out = append(out, p.ID)
	}
	return out
}

func knowledgeIDs(in explore.Index) []string {
	out := make([]string, 0, len(in.Knowledge.Items))
	for _, k := range in.Knowledge.Items {
		out = append(out, k.ID)
	}
	return out
}

func personIDs(in explore.Index) []string {
	out := make([]string, 0, len(in.People.Items))
	for _, p := range in.People.Items {
		out = append(out, p.ID)
	}
	return out
}

func orgIDs(in explore.Index) []string {
	out := make([]string, 0, len(in.Organizations.Items))
	for _, o := range in.Organizations.Items {
		out = append(out, o.ID)
	}
	return out
}

func contributionIDs(in explore.Index) []string {
	out := make([]string, 0, len(in.Contributions.Items))
	for _, c := range in.Contributions.Items {
		out = append(out, c.ID)
	}
	return out
}

func assetIDs(in explore.Index) []string {
	out := make([]string, 0, len(in.Assets.Items))
	for _, a := range in.Assets.Items {
		out = append(out, string(a.PID))
	}
	return out
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestEverySectionCarriesItsOwnTab is the surface's contract with the web
// client: six tab names, in order, and each section knows which one it is —
// a client rendering the JSON cannot mislabel a section.
func TestEverySectionCarriesItsOwnTab(t *testing.T) {
	in := explore.BuildIndex(explore.Input{})

	got := []explore.Tab{
		in.Projects.Tab, in.Assets.Tab, in.Knowledge.Tab,
		in.People.Tab, in.Organizations.Tab, in.Contributions.Tab,
	}
	want := explore.Tabs()
	if len(want) != 6 {
		t.Fatalf("the surface is defined as six dimensions, got %d", len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("section %d carries tab %q, want %q", i, got[i], want[i])
		}
	}

	// A section with nothing in it must still carry its tab: an absent tab
	// and an empty tab are different facts (docs/51), and the web client
	// renders an empty panel only for the latter.
	for _, section := range []explore.Tab{in.Projects.Tab, in.Assets.Tab, in.Knowledge.Tab, in.People.Tab, in.Organizations.Tab, in.Contributions.Tab} {
		if section == "" {
			t.Fatalf("an empty index dropped a section's tab identity")
		}
	}
}

// TestPrivateProjectIsNotListed covers acceptance "private 不出现" for the
// Projects section itself: a private row handed over by a reader is not
// rendered, and a project with no identity is not rendered either.
func TestPrivateProjectIsNotListed(t *testing.T) {
	in := explore.BuildIndex(explore.Input{
		Projects: []domain.Project{
			publicProject("pub", 1),
			privateProject("priv", 0),                     // NEWER than the public one: freshness
			{ID: "", Visibility: domain.VisibilityPublic}, // no identity
		},
	})

	if got := projIDs(in); !equalStrings(got, []string{"pub"}) {
		t.Fatalf("Projects section = %v, want only the public project", got)
	}
}

// TestRestrictedPublicationNeverReachesTheNetwork is the T0805 rule on the
// ANONYMOUS surface, and it is the one this section could get wrong in the
// direction nobody reports.
//
// This is the only reader that renders a publication to a caller who is not a
// member of the project that owns it, and before T0805 knowledge_publications
// had no writer at all — so the query here had never returned a row and its
// missing predicate had never mattered. The moment the publish path exists it
// does: 发布不等于公开 (owner ruling L3-20260916-1 #1) says a publication may
// exist and stay the project's, and a reader that keyed on the PUBLICATION's
// existence alone would print every one of them.
//
// Three rows, one per input of knowledgepublish.AudienceFor, and the public
// one is a CONTROL: without it the test would pass on a reader that renders
// nothing at all, which is the failure mode a "does it leak" test has.
func TestRestrictedPublicationNeverReachesTheNetwork(t *testing.T) {
	audience := "restricted-audience-policy"
	restrictedRights := projectPolicyRights()
	restrictedRights.Visibility.Metadata = "some-other-policy"

	in := explore.BuildIndex(explore.Input{
		Projects: []domain.Project{publicProject("pub", 1), publicProject("pub2", 1), publicProject("pub3", 1)},
		Knowledge: []explore.KnowledgeRow{
			// The version pins a visibility policy of its own. The owning
			// project is PUBLIC — this is the combination the ruling is
			// about, and the one a project-only rule gets wrong.
			func() explore.KnowledgeRow {
				row := publishedKnowledge("k-policy-pinned", "pub", 0)
				row.VisibilityPolicyID = &audience
				return row
			}(),
			// The version inherits a PRIVATE project's visibility.
			restrictedKnowledge("k-in-private-project", "priv", 0),
			// The version inherits a public project, and the published rights
			// declaration pins a metadata visibility this build cannot
			// resolve — refused rather than assumed public.
			func() explore.KnowledgeRow {
				row := publishedKnowledge("k-rights-pinned", "pub3", 0)
				row.Rights = restrictedRights
				return row
			}(),
			// The control: same shape, nothing pinned.
			publishedKnowledge("k-network", "pub2", 0),
		},
	})

	if got := knowledgeIDs(in); !equalStrings(got, []string{"k-network"}) {
		t.Fatalf("Knowledge section = %v, want only the network-visible publication", got)
	}

	// The leak assertion the section's other tests use: the rendered bytes
	// must not carry a restricted publication's identity at all. A section
	// that listed it and hid a field would still be a directory of it.
	rendered, err := json.Marshal(in.Knowledge)
	if err != nil {
		t.Fatalf("render the section: %v", err)
	}
	for _, secret := range []string{"k-policy-pinned", "k-in-private-project", "k-rights-pinned"} {
		if strings.Contains(string(rendered), secret) {
			t.Errorf("the rendered section names %q: %s", secret, rendered)
		}
	}
}

// TestPrivateProjectIsNeverNamed is the second half of "private 不出现": a
// knowledge publication and an opportunity may travel WITHOUT their project
// (docs/12 §2 lets a private project publish), but the private project's
// identity must not be printed by either section — not its name, not its
// slug, not its id.
func TestPrivateProjectIsNeverNamed(t *testing.T) {
	pub := publicProject("pub", 5)
	// The id is a distinctive string on purpose: the leak assertion below
	// searches the rendered bytes for it, and a short id would also match the
	// fixture row names ("k-from-private") — a false positive that would make
	// the check look like it works.
	priv := privateProject("9f3c", 5)

	in := explore.BuildIndex(explore.Input{
		Projects: []domain.Project{pub, priv},
		Knowledge: []explore.KnowledgeRow{
			publishedKnowledge("k-from-private", priv.ID, 0),
			publishedKnowledge("k-from-public", pub.ID, 1),
		},
		Contributions: []contribution.ContributionOpportunity{
			opportunity("o-from-private", priv.ID, 0, contribution.OpportunityVisibilityPublic, contribution.OpportunityStateOpen),
			opportunity("o-from-public", pub.ID, 1, contribution.OpportunityVisibilityPublic, contribution.OpportunityStateOpen),
		},
	})

	// The published rows themselves ARE listed: the publication is the
	// public thing (docs/12 §2/§3), and hiding it would be a second,
	// stricter visibility rule invented by this surface.
	if got := knowledgeIDs(in); !equalStrings(got, []string{"k-from-private", "k-from-public"}) {
		t.Fatalf("Knowledge section = %v, want both published rows", got)
	}
	if got := contributionIDs(in); !equalStrings(got, []string{"o-from-private", "o-from-public"}) {
		t.Fatalf("Contributions section = %v, want both publicized rows", got)
	}

	for _, k := range in.Knowledge.Items {
		if strings.HasPrefix(k.ID, "k-from-public") {
			if k.Project == nil {
				t.Fatalf("knowledge of a PUBLIC project lost its project name")
			}
			if k.Project.Name != pub.Name || k.Project.Slug != pub.Slug || k.Project.ID != pub.ID {
				t.Errorf("knowledge project = %+v, want the public project", k.Project)
			}
			if k.Project.URL != "/projects/"+pub.ID {
				t.Errorf("knowledge project url = %q, want /projects/%s", k.Project.URL, pub.ID)
			}
		}
		if strings.HasPrefix(k.ID, "k-from-private") && k.Project != nil {
			t.Errorf("knowledge row %q named its private project: %+v", k.ID, k.Project)
		}
	}

	for _, o := range in.Contributions.Items {
		switch {
		case strings.HasPrefix(o.ID, "o-from-public") && o.Project == nil:
			t.Errorf("opportunity of a PUBLIC project lost its project name")
		case strings.HasPrefix(o.ID, "o-from-private") && o.Project != nil:
			t.Errorf("opportunity %q named its private project: %+v", o.ID, o.Project)
		}
	}

	// And the strongest form, read off the bytes a client actually receives:
	// the private project's name/slug/id appears in NO byte of the payload —
	// while the public project's does, so the check is not vacuous.
	raw := marshal(t, in)
	forbidInBody(t, raw, priv.Name, priv.Slug, priv.ID)
	for _, want := range []string{pub.Name, pub.Slug, pub.ID} {
		if !strings.Contains(raw, want) {
			t.Fatalf("the check is vacuous: the PUBLIC project's %q is missing from the payload too", want)
		}
	}
}

// TestPrivateProjectHiddenByReaderIsNotCreatableByReader proves the name
// resolution does not trust the knowledge row: a row that names a project the
// index never saw listed as public renders with a nil project, so a reader
// that hands over a private project's id cannot make the index print it.
func TestPrivateProjectHiddenByReaderIsNotCreatableByReader(t *testing.T) {
	in := explore.BuildIndex(explore.Input{
		Knowledge: []explore.KnowledgeRow{
			publishedKnowledge("k", "priv-never-listed", 0),
		},
	})
	if len(in.Knowledge.Items) != 1 {
		t.Fatalf("the published row was dropped: %+v", in.Knowledge.Items)
	}
	if in.Knowledge.Items[0].Project != nil {
		t.Fatalf("a project the index never resolved as public was printed: %+v", in.Knowledge.Items[0].Project)
	}
}

// TestLifecycleStateGuardsEachSection covers the fail-closed re-checks: rows
// whose own state says "not public" do not render, even when the reader
// handed them over.
func TestLifecycleStateGuardsEachSection(t *testing.T) {
	disabledAt := at(0)
	deactivatedAt := at(0)

	in := explore.BuildIndex(explore.Input{
		// No publication id: the version is not published (docs/12 §3 —
		// visibility widening is never automatic).
		Knowledge: []explore.KnowledgeRow{
			{ObjectID: "o2", ObjectType: "research_question", Title: "unpublished", ProjectID: "pub"},
			publishedKnowledge("pub-k", "pub", 0),
		},
		People: []explore.PersonRow{
			{ID: "live", Handle: "live", DisplayName: "Live", CreatedAt: at(0)},
			{ID: "gone", Handle: "gone", DisplayName: "Gone", CreatedAt: at(0), DisabledAt: &disabledAt},
		},
		Organizations: []explore.OrganizationRow{
			{ID: "live-org", Slug: "live-org", Name: "Live Org", CreatedAt: at(0)},
			{ID: "gone-org", Slug: "gone-org", Name: "Gone Org", CreatedAt: at(0), DeactivatedAt: &deactivatedAt},
		},
		Contributions: []contribution.ContributionOpportunity{
			// internal: not on the open network at all
			opportunity("internal", "pub", 0, contribution.OpportunityVisibilityInternal, contribution.OpportunityStateOpen),
			// public but closed: history, not something to take on
			opportunity("closed", "pub", 0, contribution.OpportunityVisibilityPublic, contribution.OpportunityStateClosed),
			// public and suggested: not approved yet
			opportunity("suggested", "pub", 0, contribution.OpportunityVisibilityPublic, contribution.OpportunityStateSuggested),
			opportunity("open", "pub", 0, contribution.OpportunityVisibilityPublic, contribution.OpportunityStateOpen),
		},
	})

	if got := knowledgeIDs(in); !equalStrings(got, []string{"pub-k"}) {
		t.Errorf("Knowledge section = %v, want only the published row", got)
	}
	if got := personIDs(in); !equalStrings(got, []string{"live"}) {
		t.Errorf("People section = %v, want only the enabled account", got)
	}
	if got := orgIDs(in); !equalStrings(got, []string{"live-org"}) {
		t.Errorf("Organizations section = %v, want only the active organization", got)
	}
	if got := contributionIDs(in); !equalStrings(got, []string{"open"}) {
		t.Errorf("Contributions section = %v, want only the publicized open row", got)
	}
}

// TestSectionsAreRankedByFreshnessOnly is acceptance "无 like-based core
// ranking" in its positive form: each section's order is its freshness key,
// newest first — not the reader's order, and not any popularity input (there
// is none in the model, and a row carrying one changes nothing).
func TestSectionsAreRankedByFreshnessOnly(t *testing.T) {
	in := explore.BuildIndex(explore.Input{
		// Deliberately supplied OLDEST FIRST: the model must re-order, not
		// trust the reader (§ BuildIndex rule 3).
		Projects: []domain.Project{
			publicProject("p-old", 30), publicProject("p-mid", 20), publicProject("p-new", 10),
		},
		Assets: []assets.BrowseItem{
			assetItem("a-old", 30), assetItem("a-mid", 20), assetItem("a-new", 10),
		},
		Knowledge: []explore.KnowledgeRow{
			publishedKnowledge("k-old", "p-old", 30),
			publishedKnowledge("k-mid", "p-old", 20),
			publishedKnowledge("k-new", "p-old", 10),
		},
		People: []explore.PersonRow{
			{ID: "u-old", CreatedAt: at(30)}, {ID: "u-mid", CreatedAt: at(20)}, {ID: "u-new", CreatedAt: at(10)},
		},
		Organizations: []explore.OrganizationRow{
			{ID: "org-old", CreatedAt: at(30)}, {ID: "org-mid", CreatedAt: at(20)}, {ID: "org-new", CreatedAt: at(10)},
		},
		Contributions: []contribution.ContributionOpportunity{
			opportunity("c-old", "p-old", 30, contribution.OpportunityVisibilityPublic, contribution.OpportunityStateOpen),
			opportunity("c-mid", "p-old", 20, contribution.OpportunityVisibilityPublic, contribution.OpportunityStateOpen),
			opportunity("c-new", "p-old", 10, contribution.OpportunityVisibilityPublic, contribution.OpportunityStateOpen),
		},
	})

	cases := []struct {
		name string
		got  []string
		want []string
	}{
		{"projects", projIDs(in), []string{"p-new", "p-mid", "p-old"}},
		{"assets", assetIDs(in), []string{"a-new", "a-mid", "a-old"}},
		{"knowledge", knowledgeIDs(in), []string{"k-new", "k-mid", "k-old"}},
		{"people", personIDs(in), []string{"u-new", "u-mid", "u-old"}},
		{"organizations", orgIDs(in), []string{"org-new", "org-mid", "org-old"}},
		{"contributions", contributionIDs(in), []string{"c-new", "c-mid", "c-old"}},
	}
	for _, c := range cases {
		if !equalStrings(c.got, c.want) {
			t.Errorf("%s section = %v, want newest-first %v", c.name, c.got, c.want)
		}
	}
}

// TestEqualFreshnessBreaksTiesByIdentity pins rank stability: two rows
// published in the same instant must appear in the same order on every read,
// or a client diffing two answers sees a phantom change.
func TestEqualFreshnessBreaksTiesByIdentity(t *testing.T) {
	in := explore.BuildIndex(explore.Input{
		Projects: []domain.Project{
			publicProject("z", 5), publicProject("a", 5), publicProject("m", 5),
		},
		Knowledge: []explore.KnowledgeRow{
			publishedKnowledge("k-z", "", 5), publishedKnowledge("k-a", "", 5), publishedKnowledge("k-m", "", 5),
		},
	})
	if got := projIDs(in); !equalStrings(got, []string{"a", "m", "z"}) {
		t.Errorf("projects with equal freshness = %v, want id order [a m z]", got)
	}
	if got := knowledgeIDs(in); !equalStrings(got, []string{"k-a", "k-m", "k-z"}) {
		t.Errorf("knowledge with equal freshness = %v, want id order [k-a k-m k-z]", got)
	}
}

// TestSectionCapKeepsTheNewest pins the cap's direction. A cap that kept the
// first rows a reader returned would be a cap on an arbitrary prefix; this
// one can only ever drop the oldest.
func TestSectionCapKeepsTheNewest(t *testing.T) {
	total := explore.SectionLimit + 5
	projects := make([]domain.Project, 0, total)
	for i := 0; i < total; i++ {
		// "p000" is the NEWEST (age 0), "p024" the oldest.
		projects = append(projects, publicProject(fmt.Sprintf("p%03d", i), i))
	}

	in := explore.BuildIndex(explore.Input{Projects: projects})

	if len(in.Projects.Items) != explore.SectionLimit {
		t.Fatalf("section holds %d rows, want the cap %d", len(in.Projects.Items), explore.SectionLimit)
	}
	if in.Projects.Items[0].ID != "p000" {
		t.Errorf("first row = %q, want the newest p000", in.Projects.Items[0].ID)
	}
	if last := in.Projects.Items[len(in.Projects.Items)-1].ID; last != fmt.Sprintf("p%03d", explore.SectionLimit-1) {
		t.Errorf("last row = %q, want the oldest row inside the cap", last)
	}
	for _, p := range in.Projects.Items {
		if p.ID == fmt.Sprintf("p%03d", total-1) {
			t.Errorf("the cap kept the OLDEST row")
		}
	}

	// A section at or under the cap loses nothing.
	small := explore.BuildIndex(explore.Input{
		Organizations: []explore.OrganizationRow{{ID: "only", CreatedAt: at(0)}},
	})
	if len(small.Organizations.Items) != 1 {
		t.Errorf("a one-row section rendered %d rows", len(small.Organizations.Items))
	}
}

// TestNothingShrinksSilently checks the rows a reader hands over are all
// accounted for: within the cap, every public row renders. A section that
// quietly dropped a row would be indistinguishable from a reader that never
// returned it.
func TestNothingShrinksSilently(t *testing.T) {
	in := explore.BuildIndex(explore.Input{
		Projects: []domain.Project{publicProject("a", 0), privateProject("b", 0), publicProject("c", 0)},
		People: []explore.PersonRow{
			{ID: "u1", CreatedAt: at(0)}, {ID: "u2", CreatedAt: at(0)},
		},
		Organizations: []explore.OrganizationRow{
			{ID: "o1", CreatedAt: at(0)}, {ID: "o2", CreatedAt: at(0)}, {ID: "o3", CreatedAt: at(0)},
		},
	})

	if len(in.Projects.Items) != 2 {
		t.Errorf("2 of 3 projects are public, rendered %d", len(in.Projects.Items))
	}
	if len(in.People.Items) != 2 {
		t.Errorf("2 people, rendered %d", len(in.People.Items))
	}
	if len(in.Organizations.Items) != 3 {
		t.Errorf("3 organizations, rendered %d", len(in.Organizations.Items))
	}
}

// TestPayloadCarriesNoPopularityAndNoHiddenKeys is the acceptance criterion
// read off the wire: the JSON holds no like/star/popularity field (nothing to
// rank on), no total, and no creation time for the two sections whose
// freshness key is not a published fact.
func TestPayloadCarriesNoPopularityAndNoHiddenKeys(t *testing.T) {
	in := explore.BuildIndex(explore.Input{
		Projects:      []domain.Project{publicProject("p", 1)},
		Assets:        []assets.BrowseItem{assetItem("a", 1)},
		Knowledge:     []explore.KnowledgeRow{publishedKnowledge("k", "p", 1)},
		People:        []explore.PersonRow{{ID: "u", Handle: "u", DisplayName: "U", CreatedAt: at(1)}},
		Organizations: []explore.OrganizationRow{{ID: "org", Slug: "org", Name: "Org", CreatedAt: at(1)}},
		Contributions: []contribution.ContributionOpportunity{
			opportunity("c", "p", 1, contribution.OpportunityVisibilityPublic, contribution.OpportunityStateOpen),
		},
	})

	raw := marshal(t, in)

	forbidInBody(t, raw, "like", "likes", "star", "stars", "popularity", "score", "votes", "upvotes",
		"total", "count", "view_count", "trending")

	var decoded struct {
		People struct {
			Items []map[string]any `json:"items"`
		} `json:"people"`
		Organizations struct {
			Items []map[string]any `json:"items"`
		} `json:"organizations"`
	}
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("the payload is not the JSON object the client reads: %v", err)
	}
	if len(decoded.People.Items) != 1 || len(decoded.Organizations.Items) != 1 {
		t.Fatalf("payload sections are not populated; the key check below would be vacuous")
	}
	// created_at is a published fact of a project and of a publication, and
	// is rendered there; a person's and an organization's is the ranking
	// key only, and rendering it would publish registration dates the
	// platform never promised to publish (see PersonItem).
	for _, item := range []map[string]any{decoded.People.Items[0], decoded.Organizations.Items[0]} {
		if _, ok := item["created_at"]; ok {
			t.Errorf("a hidden ranking key was rendered: %v", item)
		}
	}
	// ...while the project DOES carry its creation time (already public).
	var proj struct {
		Projects struct {
			Items []map[string]any `json:"items"`
		} `json:"projects"`
	}
	if err := json.Unmarshal([]byte(raw), &proj); err != nil {
		t.Fatal(err)
	}
	if _, ok := proj.Projects.Items[0]["created_at"]; !ok {
		t.Errorf("the project row lost its created_at: %v", proj.Projects.Items[0])
	}
}

// TestCapabilitiesAreCanonical pins that a client's tag control gets a stable
// order: the same opportunity renders the same tag list however the reader
// happened to order it.
func TestCapabilitiesAreCanonical(t *testing.T) {
	o := opportunity("c", "p", 1, contribution.OpportunityVisibilityPublic, contribution.OpportunityStateOpen)
	o.RequiredCapabilities = []string{"python", "dft", "benchmarking"}

	in := explore.BuildIndex(explore.Input{Contributions: []contribution.ContributionOpportunity{o}})
	if len(in.Contributions.Items) != 1 {
		t.Fatalf("opportunity vanished: %+v", in.Contributions.Items)
	}
	got := in.Contributions.Items[0].RequiredCapabilities
	if !equalStrings(got, []string{"benchmarking", "dft", "python"}) {
		t.Errorf("capabilities = %v, want sorted", got)
	}
	if !strings.Contains(marshal(t, in), `"required_capabilities":["benchmarking","dft","python"]`) {
		t.Errorf("capabilities are not rendered in canonical order: %s", marshal(t, in))
	}
}

func marshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// forbidInBody fails when any needle appears in the response body. It exists
// so a leak assertion reads the bytes a client actually receives rather than
// the struct a test happens to hold.
func forbidInBody(t *testing.T, body string, needles ...string) {
	t.Helper()
	lower := strings.ToLower(body)
	for _, n := range needles {
		if strings.Contains(lower, strings.ToLower(n)) {
			t.Errorf("response body contains %q; body = %s", n, body)
		}
	}
}
