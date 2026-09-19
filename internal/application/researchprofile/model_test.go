package researchprofile

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The two surfaces' rules, one test per rule, each naming the document the
// rule comes from. Every one of these is a NEGATIVE assertion wherever it can
// be: what the profile must NOT contain is what this file is for, and a
// positive check on an empty answer would pass while measuring nothing.

// ---------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 10, 0, 0, 0, time.UTC)
}

func ptrTime(t time.Time) *time.Time { return &t }

const (
	aliceID   = "11111111-1111-4111-8111-111111111101"
	acmeID    = "22222222-2222-4222-8222-222222222201"
	closedID  = "22222222-2222-4222-8222-222222222202"
	openProj  = "33333333-3333-4333-8333-333333333301"
	privProj  = "33333333-3333-4333-8333-333333333302"
	openSlug  = "open-lab"
	privSlug  = "secret-lab"
	privTitle = "Secret Internal Study"
)

func visibility(v string) string { return v }

func person() PersonRow {
	return PersonRow{ID: aliceID, Handle: "alice", DisplayName: "Alice", Bio: "catalysis"}
}

// ---------------------------------------------------------------------
// The no-score rule
// ---------------------------------------------------------------------

// forbiddenKey is the vocabulary a profile payload may not use as a field
// name. docs/13 §4 ("禁止单一分数。Profile 展示多维 evidence"), docs/13 §6
// (raw commit/experiment counts are not quality proxies) and CLAUDE.md §9
// invariant 13 (no Truth Score / Research Score) are the rules; this is what
// they mean for a JSON document.
var forbiddenKey = regexp.MustCompile(`(?i)(score|rank|rating|weight|reputation|total|count|impact|percentile|level|points|karma|metric)`)

// TestPersonProfilePayloadCarriesNoScore walks EVERY key of the rendered
// profile and refuses the vocabulary above. It walks the wire shape rather
// than the Go types because the wire shape is what a client reads: a field
// added to an item struct would be caught here even if no model function
// mentions it.
func TestPersonProfilePayloadCarriesNoScore(t *testing.T) {
	profile := BuildPersonProfile(PersonInput{
		Person: person(),
		Affiliations: []AffiliationRow{{
			OrganizationID: acmeID, OrganizationSlug: "acme", OrganizationName: "Acme",
			Role: "owner", Verified: true,
		}},
		Contributions: []ContributionRow{{
			EventType: "research_state.merged", RoleCodes: []string{"author"},
			OccurredAt: day(2026, time.September, 1), Accepted: true,
			ProjectID: openProj, ProjectSlug: openSlug, ProjectName: "Open Lab",
			ProjectVisibility: visibility("public"),
		}},
		Assets: []AssetCreditRow{{
			PID: "01j9z6k3m4n5p6q7r8s9t0v1w2", Title: "Dataset", AssetType: "dataset",
			Version: "1.0", VersionVisibility: "public", Role: "creator",
			ProjectID: openProj, ProjectSlug: openSlug, ProjectName: "Open Lab",
			ProjectVisibility: "public", PublishedAt: day(2026, time.September, 2),
		}},
		Reuse: []ReuseRow{{
			PID: "01j9z6k3m4n5p6q7r8s9t0v1w2", Title: "Dataset", Version: "1.0",
			VersionVisibility: "public", VersionProjectVisibility: "public",
			UsageVisibility: "public", DependencyType: "depends_on",
			DeclaredAt: day(2026, time.September, 3),
			ProjectID:  openProj, ProjectSlug: openSlug, ProjectName: "Open Lab",
			ProjectVisibility: "public",
		}},
		Reproductions: []ReproductionRow{{
			Relation: "reproduces", ReviewState: "reviewed", CreatedAt: day(2026, time.September, 4),
			AssertionVisibility: "public",
			ProjectID:           openProj, ProjectSlug: openSlug, ProjectName: "Open Lab",
			ProjectVisibility: "public",
		}},
	})
	refuseScores(t, "person profile", profile)
}

// TestOrganizationProfilePayloadCarriesNoScore is the same walk over the
// organization surface.
func TestOrganizationProfilePayloadCarriesNoScore(t *testing.T) {
	profile := BuildOrganizationProfile(OrganizationInput{
		Organization: OrganizationRow{ID: acmeID, Slug: "acme", Name: "Acme", Description: "institute"},
		Projects: []ProjectRow{{
			ID: openProj, Slug: openSlug, Name: "Open Lab", Purpose: "why",
			ActivityStatus: "active", Visibility: "public",
		}},
		Activity: []ContributionRow{{
			EventType: "research_state.merged", OccurredAt: day(2026, time.September, 1),
			ProjectID: openProj, ProjectSlug: openSlug, ProjectName: "Open Lab",
			ProjectVisibility: "public",
			ActorID:           aliceID, ActorHandle: "alice", ActorDisplayName: "Alice",
		}},
		Assets: []AssetCreditRow{{
			PID: "01j9z6k3m4n5p6q7r8s9t0v1w2", Title: "Dataset", AssetType: "dataset",
			Version: "1.0", VersionVisibility: "public",
			ProjectID: openProj, ProjectSlug: openSlug, ProjectName: "Open Lab",
			ProjectVisibility: "public", PublishedAt: day(2026, time.September, 2),
		}},
	})
	refuseScores(t, "organization profile", profile)
}

// refuseScores marshals v and refuses both a forbidden KEY anywhere in the
// document and a forbidden key in the payload's own text (which would catch a
// score smuggled into a field's value or a label).
func refuseScores(t *testing.T, surface string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %s: %v", surface, err)
	}
	if len(raw) < 200 {
		t.Fatalf("%s rendered %d bytes — the walk below would measure nothing:\n%s",
			surface, len(raw), raw)
	}
	for _, key := range jsonKeys(t, raw) {
		if forbiddenKey.MatchString(key) {
			t.Errorf("%s carries the key %q: docs/13 §4 forbids a single score and CLAUDE.md §9 invariant 13 forbids a Truth Score — a profile is a list of facts, not an aggregate", surface, key)
		}
	}
}

// jsonKeys returns every object key of a JSON document, at any depth.
func jsonKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the payload is not the JSON a client reads: %v\n%s", err, raw)
	}
	var keys []string
	var walk func(node any)
	walk = func(node any) {
		switch typed := node.(type) {
		case map[string]any:
			for k, v := range typed {
				keys = append(keys, k)
				walk(v)
			}
		case []any:
			for _, v := range typed {
				walk(v)
			}
		}
	}
	walk(doc)
	return keys
}

// ---------------------------------------------------------------------
// Person: which rows render
// ---------------------------------------------------------------------

// TestContributionIntoAPrivateProjectIsDropped: a ledger row about a private
// project's work is not a public object, so neither the row nor its project
// may be named on a public profile — and nothing may say how many were
// dropped.
func TestContributionIntoAPrivateProjectIsDropped(t *testing.T) {
	profile := BuildPersonProfile(PersonInput{
		Person: person(),
		Contributions: []ContributionRow{
			{
				EventType: "research_state.merged", OccurredAt: day(2026, time.September, 9),
				ProjectID: privProj, ProjectSlug: privSlug, ProjectName: privTitle,
				ProjectVisibility: "private",
			},
			{
				EventType: "research_state.merged", OccurredAt: day(2026, time.September, 1),
				ProjectID: openProj, ProjectSlug: openSlug, ProjectName: "Open Lab",
				ProjectVisibility: "public",
			},
		},
	})
	if len(profile.Contributions) != 1 {
		t.Fatalf("contributions rendered = %d, want 1 (the private one is not renderable)", len(profile.Contributions))
	}
	raw, err := json.Marshal(profile)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), privSlug) || strings.Contains(string(raw), privTitle) {
		t.Errorf("the private project's identity reached the payload:\n%s", raw)
	}
	if !strings.Contains(string(raw), openSlug) {
		t.Errorf("the public project is missing from the same bytes, so the check above proved nothing:\n%s", raw)
	}
	if len(profile.Projects) != 1 || profile.Projects[0].ID != openProj {
		t.Errorf("derived projects = %+v, want exactly the public one", profile.Projects)
	}
}

// TestContributionWithAnUnresolvedProjectIsDropped: the fail-closed reading
// of a row whose project the reader could not resolve (empty id and empty
// visibility). Nothing about it is renderable, and the profile says nothing.
func TestContributionWithAnUnresolvedProjectIsDropped(t *testing.T) {
	profile := BuildPersonProfile(PersonInput{
		Person: person(),
		Contributions: []ContributionRow{{
			EventType: "research_state.merged", OccurredAt: day(2026, time.September, 1),
			ProjectSlug: "ghost-lab", ProjectName: "Ghost Lab",
		}},
	})
	if len(profile.Contributions) != 0 || len(profile.Projects) != 0 {
		t.Errorf("an unresolved project rendered: %+v / %+v", profile.Contributions, profile.Projects)
	}
}

// TestDisabledAccountHasNoProfile: "exists and is disabled" is not a fact a
// public surface states, so the transport answers one 404 for it and an
// unknown id cannot be told from it (docs/45).
func TestDisabledAccountHasNoProfile(t *testing.T) {
	disabled := person()
	disabled.DisabledAt = ptrTime(day(2026, time.September, 1))
	if PersonVisible(disabled) {
		t.Error("a disabled account is visible: internal/events answers 'none' for it and explore drops it from the directory")
	}
	if !PersonVisible(person()) {
		t.Error("an active account is not visible")
	}
	if PersonVisible(PersonRow{}) {
		t.Error("an empty row is visible")
	}
}

// TestPrivateVersionIsNotRendered: a credit on a private version renders
// nothing, while the same person's public version does.
func TestPrivateVersionIsNotRendered(t *testing.T) {
	profile := BuildPersonProfile(PersonInput{
		Person: person(),
		Assets: []AssetCreditRow{
			{
				PID: "01j9z6k3m4n5p6q7r8s9t0v1w2", Title: "Hidden", AssetType: "dataset",
				Version: "0.9", VersionVisibility: "private", Role: "creator",
				ProjectID: openProj, ProjectSlug: openSlug, ProjectName: "Open Lab",
				ProjectVisibility: "public", PublishedAt: day(2026, time.September, 9),
			},
			{
				PID: "01j9z6k3m4n5p6q7r8s9t0v1w3", Title: "Shown", AssetType: "dataset",
				Version: "1.0", VersionVisibility: "public", Role: "creator",
				ProjectID: openProj, ProjectSlug: openSlug, ProjectName: "Open Lab",
				ProjectVisibility: "public", PublishedAt: day(2026, time.September, 1),
			},
		},
	})
	if len(profile.Assets) != 1 || profile.Assets[0].Title != "Shown" {
		t.Fatalf("assets rendered = %+v, want only the public version", profile.Assets)
	}
}

// TestPublicVersionOfAPrivateProjectRendersWithoutItsProject: docs/12 §2
// lets a private project publish, so the version travels and the project does
// not (internal/assets.BuildBrowse's rule, applied to the same two facts).
func TestPublicVersionOfAPrivateProjectRendersWithoutItsProject(t *testing.T) {
	profile := BuildPersonProfile(PersonInput{
		Person: person(),
		Assets: []AssetCreditRow{{
			PID: "01j9z6k3m4n5p6q7r8s9t0v1w2", Title: "Dataset", AssetType: "dataset",
			Version: "1.0", VersionVisibility: "public", Role: "creator",
			ProjectID: privProj, ProjectSlug: privSlug, ProjectName: privTitle,
			ProjectVisibility: "private", PublishedAt: day(2026, time.September, 1),
		}},
	})
	if len(profile.Assets) != 1 {
		t.Fatalf("the public version of a private project did not render: %+v", profile.Assets)
	}
	if profile.Assets[0].Project != nil {
		t.Errorf("the private project was named: %+v", profile.Assets[0].Project)
	}
	raw, _ := json.Marshal(profile)
	if strings.Contains(string(raw), privSlug) || strings.Contains(string(raw), privTitle) {
		t.Errorf("the private project's identity reached the payload:\n%s", raw)
	}
	if !strings.Contains(string(raw), "01j9z6k3m4n5p6q7r8s9t0v1w2") {
		t.Errorf("the version's own identity is missing, so a client cannot cite it:\n%s", raw)
	}
}

// TestReuseNeedsBothConditions: internal/assets.PageUsage's two rules — the
// declaration must be public and the USING project must be public — plus the
// used version's own visibility.
func TestReuseNeedsBothConditions(t *testing.T) {
	base := ReuseRow{
		PID: "01j9z6k3m4n5p6q7r8s9t0v1w2", Title: "Dataset", Version: "1.0",
		VersionVisibility: "public", VersionProjectVisibility: "public",
		UsageVisibility: "public", DependencyType: "depends_on",
		DeclaredAt: day(2026, time.September, 1),
		ProjectID:  openProj, ProjectSlug: openSlug, ProjectName: "Open Lab",
		ProjectVisibility: "public",
	}

	cases := map[string]struct {
		mutate func(*ReuseRow)
		render bool
	}{
		"both public":       {func(*ReuseRow) {}, true},
		"private usage":     {func(r *ReuseRow) { r.UsageVisibility = "private" }, false},
		"private user":      {func(r *ReuseRow) { r.ProjectVisibility = "private" }, false},
		"private version":   {func(r *ReuseRow) { r.VersionVisibility = "private" }, false},
		"unresolved user":   {func(r *ReuseRow) { r.ProjectID = ""; r.ProjectVisibility = "" }, false},
		"empty visibility":  {func(r *ReuseRow) { r.UsageVisibility = "" }, false},
		"private origin":    {func(r *ReuseRow) { r.VersionProjectVisibility = "private" }, false},
		"unresolved origin": {func(r *ReuseRow) { r.VersionProjectVisibility = "" }, false},
	}
	for name, tc := range cases {
		row := base
		tc.mutate(&row)
		profile := BuildPersonProfile(PersonInput{Person: person(), Reuse: []ReuseRow{row}})
		if got := len(profile.Reuse) == 1; got != tc.render {
			t.Errorf("%s: rendered = %v, want %v", name, got, tc.render)
		}
	}
	// The control: the rendering case above must actually have rendered the
	// using project, or "not rendered" and "rendered empty" would be the same
	// observation.
	profile := BuildPersonProfile(PersonInput{Person: person(), Reuse: []ReuseRow{base}})
	if profile.Reuse[0].Project.ID != openProj {
		t.Fatalf("the control row did not render its project: %+v", profile.Reuse[0])
	}
}

// TestReproductionRelationsAreTheTwoDocumentedOnes: docs/10 §4's pair, and
// nothing else — an evidence network reads nine relations, and the profile's
// "reproduced evidence" dimension is the two that mean reproduction.
func TestReproductionRelationsAreTheTwoDocumentedOnes(t *testing.T) {
	row := func(relation string) ReproductionRow {
		return ReproductionRow{
			Relation: relation, ReviewState: "unreviewed", CreatedAt: day(2026, time.September, 1),
			AssertionVisibility: "public",
			ProjectID:           openProj, ProjectSlug: openSlug, ProjectName: "Open Lab",
			ProjectVisibility: "public",
		}
	}
	profile := BuildPersonProfile(PersonInput{Person: person(), Reproductions: []ReproductionRow{
		row("reproduces"), row("fails_to_reproduce"), row("supports"), row("contradicts"), row(""),
	}})
	if len(profile.Reproductions) != 2 {
		t.Fatalf("reproductions rendered = %d, want the 2 reproduction relations:\n%+v",
			len(profile.Reproductions), profile.Reproductions)
	}
	// Both survive, and neither is preferred or penalised: the failed
	// reproduction is a fact about the claim, not a demerit (docs/10 §4).
	relations := map[string]bool{}
	for _, item := range profile.Reproductions {
		relations[item.Relation] = true
	}
	if !relations["reproduces"] || !relations["fails_to_reproduce"] {
		t.Errorf("relations = %v, want both of docs/10 §4's reproduction relations", relations)
	}
}

// TestReproductionInAPrivateProjectKeepsTheRowAndWithholdsTheProject: unlike a
// contribution, an assertion is something the PERSON did — the row stays and
// the project it was made in is withheld. The assertion's OWN visibility is
// public here, and that is what the two axes being independent means: this
// test would fail if the model read the project's axis as the assertion's.
func TestReproductionInAPrivateProjectKeepsTheRowAndWithholdsTheProject(t *testing.T) {
	profile := BuildPersonProfile(PersonInput{Person: person(), Reproductions: []ReproductionRow{{
		Relation: "reproduces", ReviewState: "unreviewed", CreatedAt: day(2026, time.September, 1),
		AssertionVisibility: "public",
		ProjectID:           privProj, ProjectSlug: privSlug, ProjectName: privTitle,
		ProjectVisibility: "private",
	}}})
	if len(profile.Reproductions) != 1 {
		t.Fatalf("the assertion was dropped: %+v", profile.Reproductions)
	}
	if profile.Reproductions[0].Project != nil {
		t.Errorf("the private project was named: %+v", profile.Reproductions[0].Project)
	}
	raw, _ := json.Marshal(profile)
	if strings.Contains(string(raw), privSlug) || strings.Contains(string(raw), privTitle) {
		t.Errorf("the private project's identity reached the payload:\n%s", raw)
	}
}

// TestPrivateAssertionIsNotRenderedAndThePublicOneStillIs: the assertion's own
// axis, which is NOT the project's. evidence_assertions.visibility is a column
// of its own (00091, DEFAULT 'private') whose header says "an assertion
// nothing explicitly made public is not rendered anywhere", and both profile
// routes are anonymous — so an assertion its author never published is not
// this profile's to show, however public the project it was made in is.
//
// The public assertion in the same fixture is the CONTROL: the test fails just
// as hard if the model drops the dimension wholesale, or if it were to read
// the project's visibility instead of the assertion's (both assertions here
// are in the SAME public project, so only the assertion's own axis separates
// them).
func TestPrivateAssertionIsNotRenderedAndThePublicOneStillIs(t *testing.T) {
	assertion := func(visibility string, at time.Time) ReproductionRow {
		return ReproductionRow{
			Relation: "reproduces", ReviewState: "reviewed", CreatedAt: at,
			AssertionVisibility: visibility,
			ProjectID:           openProj, ProjectSlug: openSlug, ProjectName: "Open Lab",
			ProjectVisibility: "public",
		}
	}
	profile := BuildPersonProfile(PersonInput{Person: person(), Reproductions: []ReproductionRow{
		assertion("public", day(2026, time.September, 1)),
		assertion("private", day(2026, time.September, 2)),
	}})
	if len(profile.Reproductions) != 1 {
		t.Fatalf("reproductions rendered = %d, want exactly the public assertion:\n%+v",
			len(profile.Reproductions), profile.Reproductions)
	}
	if got := profile.Reproductions[0].CreatedAt; !got.Equal(day(2026, time.September, 1)) {
		t.Errorf("the row rendered is the private assertion (created_at %s); the public one is the "+
			"control and must be the one that survived", got)
	}
	// The public assertion's project is still named — the drop is per-row and
	// does not withhold anything from the rows that stay.
	ref := profile.Reproductions[0].Project
	if ref == nil || ref.Slug != openSlug {
		t.Errorf("the public assertion lost its project: %+v", ref)
	}
}

// ---------------------------------------------------------------------
// Person: affiliations keep their history
// ---------------------------------------------------------------------

// TestEndedAffiliationKeepsThePersonHistory is acceptance "离职后个人历史
// 保留", at the model: docs/04 §6 keeps the row — role, dates, verification —
// when the employment ends, and the dates are rendered as the calendar days
// the columns hold.
func TestEndedAffiliationKeepsThePersonHistory(t *testing.T) {
	start := day(2024, time.January, 15)
	end := day(2025, time.June, 30)
	profile := BuildPersonProfile(PersonInput{
		Person: person(),
		Affiliations: []AffiliationRow{
			{
				OrganizationID: acmeID, OrganizationSlug: "acme", OrganizationName: "Acme",
				Role: "contributor", AffiliationStart: &start, AffiliationEnd: &end, Verified: true,
			},
			{
				OrganizationID: closedID, OrganizationSlug: "closed", OrganizationName: "Closed",
				Role: "viewer", AffiliationStart: &start,
			},
		},
	})
	if len(profile.Affiliations) != 2 {
		t.Fatalf("affiliations = %d, want both the ended and the open one:\n%+v",
			len(profile.Affiliations), profile.Affiliations)
	}
	var ended *AffiliationItem
	for i := range profile.Affiliations {
		if profile.Affiliations[i].Organization != nil && profile.Affiliations[i].Organization.Slug == "acme" {
			ended = &profile.Affiliations[i]
		}
	}
	if ended == nil {
		t.Fatalf("the ended affiliation is missing:\n%+v", profile.Affiliations)
	}
	if ended.Start == nil || *ended.Start != "2024-01-15" {
		t.Errorf("start = %v, want the calendar day 2024-01-15", ended.Start)
	}
	if ended.End == nil || *ended.End != "2025-06-30" {
		t.Errorf("end = %v, want the calendar day 2025-06-30", ended.End)
	}
	if !ended.Verified || ended.Role != "contributor" {
		t.Errorf("role/verified lost: %+v", ended)
	}
	// The open affiliation renders a null end, which is the whole of what the
	// row knows — nothing here computes "today".
	for _, item := range profile.Affiliations {
		if item.Organization != nil && item.Organization.Slug == "closed" && item.End != nil {
			t.Errorf("an open affiliation rendered an end date: %+v", item)
		}
	}
}

// TestDeactivatedEmployerKeepsTheRowAndWithholdsTheIdentity: the person's own
// history survives the employer's deactivation, and the employer's identity
// does not surface.
func TestDeactivatedEmployerKeepsTheRowAndWithholdsTheIdentity(t *testing.T) {
	start := day(2024, time.January, 15)
	profile := BuildPersonProfile(PersonInput{
		Person: person(),
		Affiliations: []AffiliationRow{{
			OrganizationID: closedID, OrganizationSlug: "wound-down-lab",
			OrganizationName:          "Wound Down Lab",
			OrganizationDeactivatedAt: ptrTime(day(2026, time.August, 1)),
			Role:                      "maintainer", AffiliationStart: &start, Verified: true,
		}},
	})
	if len(profile.Affiliations) != 1 {
		t.Fatalf("the affiliation was dropped: %+v", profile.Affiliations)
	}
	if profile.Affiliations[0].Organization != nil {
		t.Errorf("a deactivated employer was named: %+v", profile.Affiliations[0].Organization)
	}
	if profile.Affiliations[0].Role != "maintainer" || profile.Affiliations[0].Start == nil {
		t.Errorf("the person's own history was lost with the employer: %+v", profile.Affiliations[0])
	}
	raw, _ := json.Marshal(profile)
	if strings.Contains(string(raw), "wound-down-lab") || strings.Contains(string(raw), "Wound Down Lab") {
		t.Errorf("the deactivated employer's identity reached the payload:\n%s", raw)
	}
}

// ---------------------------------------------------------------------
// Organization
// ---------------------------------------------------------------------

// TestDeactivatedOrganizationHasNoProfile: deactivation is the only delete
// this domain has (CLAUDE.md §9.8), and a deactivated organization has no
// public surface.
func TestDeactivatedOrganizationHasNoProfile(t *testing.T) {
	closed := OrganizationRow{ID: acmeID, Slug: "acme", DeactivatedAt: ptrTime(day(2026, time.August, 1))}
	if OrganizationActive(closed) {
		t.Error("a deactivated organization is active: internal/events gives it 'none' and explore drops it")
	}
	if !OrganizationActive(OrganizationRow{ID: acmeID, Slug: "acme"}) {
		t.Error("an active organization is not active")
	}
	if OrganizationActive(OrganizationRow{}) {
		t.Error("an empty row is active")
	}
}

// TestOrganizationAssetsComeOnlyFromPublicProjects: this surface never
// publishes a link between the institution and a project it may not name.
func TestOrganizationAssetsComeOnlyFromPublicProjects(t *testing.T) {
	asset := func(projectID, slug, name, vis, title string) AssetCreditRow {
		return AssetCreditRow{
			PID: "01j9z6k3m4n5p6q7r8s9t0v1w" + title[:1], Title: title, AssetType: "dataset",
			Version: "1.0", VersionVisibility: "public",
			ProjectID: projectID, ProjectSlug: slug, ProjectName: name, ProjectVisibility: vis,
			PublishedAt: day(2026, time.September, 1),
		}
	}
	profile := BuildOrganizationProfile(OrganizationInput{
		Organization: OrganizationRow{ID: acmeID, Slug: "acme", Name: "Acme"},
		Projects: []ProjectRow{
			{ID: openProj, Slug: openSlug, Name: "Open Lab", Visibility: "public"},
			{ID: privProj, Slug: privSlug, Name: privTitle, Visibility: "private"},
		},
		Assets: []AssetCreditRow{
			asset(openProj, openSlug, "Open Lab", "public", "Shown"),
			asset(privProj, privSlug, privTitle, "private", "Hidden"),
		},
	})
	if len(profile.Projects) != 1 || profile.Projects[0].ID != openProj {
		t.Fatalf("projects = %+v, want only the public one", profile.Projects)
	}
	if len(profile.Assets) != 1 || profile.Assets[0].Title != "Shown" {
		t.Fatalf("assets = %+v, want only the public project's version", profile.Assets)
	}
	raw, _ := json.Marshal(profile)
	if strings.Contains(string(raw), privSlug) || strings.Contains(string(raw), privTitle) {
		t.Errorf("the private project's identity reached the payload:\n%s", raw)
	}
}

// TestOrganizationActivityDropsUnattributableRows: an institution's public
// activity is attributed work — a contribution by a disabled account, or by
// no account at all, is dropped rather than shown as "someone".
func TestOrganizationActivityDropsUnattributableRows(t *testing.T) {
	row := func(actorID string, disabled *time.Time) ContributionRow {
		return ContributionRow{
			EventType: "research_state.merged", OccurredAt: day(2026, time.September, 1),
			ProjectID: openProj, ProjectSlug: openSlug, ProjectName: "Open Lab",
			ProjectVisibility: "public",
			ActorID:           actorID, ActorHandle: "alice", ActorDisplayName: "Alice",
			ActorDisabledAt: disabled,
		}
	}
	profile := BuildOrganizationProfile(OrganizationInput{
		Organization: OrganizationRow{ID: acmeID, Slug: "acme", Name: "Acme"},
		Activity: []ContributionRow{
			row(aliceID, nil),
			row(aliceID, ptrTime(day(2026, time.September, 2))),
			row("", nil),
		},
	})
	if len(profile.Activity) != 1 {
		t.Fatalf("activity = %d rows, want the one attributable, active account:\n%+v",
			len(profile.Activity), profile.Activity)
	}
	if profile.Activity[0].Actor == nil || profile.Activity[0].Actor.ID != aliceID {
		t.Errorf("the actor is not rendered on the organization surface: %+v", profile.Activity[0])
	}
}

// TestOrganizationActivityNeverCarriesTheActorScore is a direct check on the
// organization surface's actor block, whose key set is small enough to state.
func TestOrganizationActivityNeverCarriesTheActorScore(t *testing.T) {
	profile := BuildOrganizationProfile(OrganizationInput{
		Organization: OrganizationRow{ID: acmeID, Slug: "acme", Name: "Acme"},
		Activity: []ContributionRow{{
			EventType: "research_state.merged", OccurredAt: day(2026, time.September, 1),
			ProjectID: openProj, ProjectSlug: openSlug, ProjectName: "Open Lab",
			ProjectVisibility: "public",
			ActorID:           aliceID, ActorHandle: "alice", ActorDisplayName: "Alice",
		}},
	})
	raw, _ := json.Marshal(profile)
	for _, key := range jsonKeys(t, raw) {
		if forbiddenKey.MatchString(key) {
			t.Errorf("organization payload carries the key %q", key)
		}
	}
}

// ---------------------------------------------------------------------
// The cap
// ---------------------------------------------------------------------

// TestRenderLimitCapsWithoutTellingHowManyWereDropped: the cap is the
// surface's, and a reader that handed over more rows must not make the
// profile longer — nor make the payload say how many it dropped.
func TestRenderLimitCapsWithoutTellingHowManyWereDropped(t *testing.T) {
	contributions := make([]ContributionRow, 0, RenderLimit+7)
	for i := 0; i < RenderLimit+7; i++ {
		contributions = append(contributions, ContributionRow{
			EventType: "research_state.merged",
			// Newest first: row i is older than row i+1 by construction, so
			// the cap must keep the LAST-created ones.
			OccurredAt: day(2026, time.January, 1).Add(time.Duration(i) * time.Hour),
			ProjectID:  openProj, ProjectSlug: openSlug, ProjectName: "Open Lab",
			ProjectVisibility: "public",
		})
	}
	profile := BuildPersonProfile(PersonInput{Person: person(), Contributions: contributions})
	if len(profile.Contributions) != RenderLimit {
		t.Fatalf("contributions rendered = %d, want the cap %d", len(profile.Contributions), RenderLimit)
	}
	// Newest first: the newest instant in the fixture is the last one written.
	newest := day(2026, time.January, 1).Add(time.Duration(RenderLimit+6) * time.Hour)
	if !profile.Contributions[0].OccurredAt.Equal(newest) {
		t.Errorf("the first rendered contribution is %s, want the newest %s",
			profile.Contributions[0].OccurredAt, newest)
	}
}

// ---------------------------------------------------------------------
// Vocabulary
// ---------------------------------------------------------------------

// TestRoleCodesAreASetInCanonicalOrder: an array column is a set, and
// rendering a caller's order as if it meant something (first author, say)
// would be a ranking this platform does not assign.
func TestRoleCodesAreASetInCanonicalOrder(t *testing.T) {
	got := roleCodes([]string{"author", "reviewer", "author", "", "maintainer"})
	want := []string{"author", "maintainer", "reviewer"}
	if len(got) != len(want) {
		t.Fatalf("roleCodes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("roleCodes = %v, want %v", got, want)
		}
	}
	if roleCodes(nil) == nil {
		t.Error("roleCodes(nil) returned nil; a rendered list is an empty array, never null")
	}
}
