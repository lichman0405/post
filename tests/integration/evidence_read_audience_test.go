// Task T0511 required test "evidence read audience e2e".
//
// docs/adr/ADR-024: an evidence assertion's `visibility` column says whether
// the public read may render the row, and the read over the same rows was
// carrying no reader at all — the project gate answered "may this reader see
// this PROJECT", and then every assertion of the target version was rendered
// to whoever passed it. On a PUBLIC project an anonymous reader passes, so a
// row the write path derived as `private` (a private project's judgement, or
// a draft committed on an unmerged branch) reached the network.
//
// The rule the ADR fixes, pinned here over real PostgreSQL and the whole
// composed path:
//
//  1. TWO DIRECTIONS, BOTH REQUIRED. A reader with no relation to the row —
//     anonymous, AND (the half that matters) a SIGNED-IN caller who may read
//     the public project but belongs to neither party — sees NO non-public
//     row; and every public row of the same fixture is still rendered, so a
//     fix that shows nobody anything cannot pass as this fix.
//  2. THE TWO PARTIES KEEP WHAT THEY HAVE. A member of the ASSERTING project
//     and a member of the TARGET's project each still see the private row.
//     Two separate users, so the union ADR-024 names is measured as a union
//     rather than as "the author sees her own project".
//  3. BOTH EXITS, ONE ANSWER. The object read, the hypothesis read (which
//     reads the same rows through the same query) and the object detail
//     page's evidence tab render the same set for the same reader.
//  4. THE ROW SET, NOT THE STATUS CODE. The gate's own answer is unchanged:
//     an anonymous read of a public project is still 200 (the private-project
//     half of that rule is pinned in evidence_graph_test.go). A "fix" that
//     turned the audience into a refusal would break both.
//
// The fixture's rows are written through the product's own surfaces, so the
// `private` one is the WRITE PATH's derivation and not a planted column: a
// private project asserting about a public project's published version is
// the situation the ADR's problem section names, and the rule under test is
// the read's.

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/domain"
)

// evidenceAudienceTaskID namespaces this task's test databases
// (test_T0511_<run_id>).
const evidenceAudienceTaskID = "T0511"

// --------------------------------------------------------------------------
// The fixture

// evidenceAudienceFixture is the world the cases read: a PUBLIC project
// (alpha) owning a published hypothesis, and a PRIVATE project (gamma) that
// asserts about it. Two assertions land on the SAME hypothesis version:
//
//   - the public one, committed by alpha to alpha's public branch;
//   - the private one, committed by gamma to gamma's own (private) branch —
//     cross-project, so the target publication must be network-visible (it
//     is: alpha is public and unpinned), and gamma being private is what
//     makes the row private.
//
// Five callers read it: nobody, bob (signed in, in neither party, may read
// the public project), dana (asserting project only), erin (target project
// only) and alice (both).
type evidenceAudienceFixture struct {
	w *knowledgeWorld

	alpha *knowledgeProject // public — owns the published hypothesis (the target)
	gamma *knowledgeProject // private — asserts about alpha's object

	hypothesisID      string
	hypothesisVersion string

	// alphaPub is alpha's PUBLIC branch: the surface an assertion has to be
	// committed to for the write path to derive it as public.
	alphaPub string

	publicAssertion  string
	privateAssertion string

	dana   *testUserClient // member of gamma (the asserting project) only
	erin   *testUserClient // member of alpha (the target's project) only
	danaID string
	erinID string
}

func newEvidenceAudienceFixture(t *testing.T, ctx context.Context) *evidenceAudienceFixture {
	t.Helper()
	w := newKnowledgeWorldFor(t, ctx, evidenceAudienceTaskID)
	w.signups(t)

	f := &evidenceAudienceFixture{w: w}
	f.alpha = w.newProject(t, ctx, "ea-alpha", "public")
	f.gamma = w.newProject(t, ctx, "ea-gamma", "private")
	f.alphaPub = w.createPublicBranch(t, ctx, f.alpha, "pub")

	// The question the hypothesis answers (migration 00040's guard resolves
	// hypothesis.question_id against a research question of the same project,
	// so one has to exist first), then the hypothesis itself, published: an
	// assertion's target must be published knowledge — the write path's own
	// rule (T0806), not this suite's convenience.
	questionID, _, _ := w.seedVersion(t, ctx, f.alpha.id, f.alpha.probe, "What is the CO2 uptake of MOF-5 at 298 K")
	f.hypothesisID, f.hypothesisVersion, _ = w.seedPublishedVersion(t, ctx, f.alpha, "hypothesis",
		fmt.Sprintf(`{"statement":"MOF-5 maximizes CO2 uptake at 298 K","question_id":%q,`+
			`"hypothesis_type":"mechanistic","scope":{"conditions":"298 K, 1 bar"}}`, questionID), 1)

	// The versions each project cites as its own evidence. The write path
	// requires the cited version to belong to the ASSERTING project, so each
	// project cites its own.
	_, alphaEvidence, _ := w.seedVersion(t, ctx, f.alpha.id, f.alpha.probe, "alpha benchmark")
	_, gammaEvidence, _ := w.seedVersion(t, ctx, f.gamma.id, f.gamma.probe, "gamma internal dataset")

	// --- the two rows, in the order they become interesting --------------
	f.publicAssertion = f.assertAndCheck(t, f.alpha.id, f.alphaPub,
		evidenceAssertionBody(f.hypothesisVersion, alphaEvidence, "supports"), domain.EvidenceVisibilityPublic)
	f.privateAssertion = f.assertAndCheck(t, f.gamma.id, f.gamma.probe,
		evidenceAssertionBody(f.hypothesisVersion, gammaEvidence, "contradicts"), domain.EvidenceVisibilityPrivate)

	// The two one-sided members. V1 has no route that ADDS a membership (the
	// members route changes an existing one's role), so these rows are seeded
	// directly — the shape other suites use for a fixture fact V1 has no
	// surface for. The read under test treats them as memberships like any
	// other: nothing here is special-cased for the fixture.
	f.dana, f.danaID = signup(t, w.ts.URL, "ea-dana@example.com", "ea-dana")
	f.erin, f.erinID = signup(t, w.ts.URL, "ea-erin@example.com", "ea-erin")
	f.member(t, ctx, f.gamma.id, f.danaID)
	f.member(t, ctx, f.alpha.id, f.erinID)
	return f
}

// assertAndCheck posts one assertion through the contract's route and
// requires the write path to have derived the visibility this suite's cases
// are about: a fixture that silently produced two public rows would make
// every negative below vacuous.
func (f *evidenceAudienceFixture) assertAndCheck(t *testing.T, projectID, branchID, body string, want domain.EvidenceVisibility) string {
	t.Helper()
	resp := f.w.alice.do(t, http.MethodPost,
		"/api/v1/projects/"+projectID+"/branches/"+branchID+"/evidence-assertions", body)
	mustStatus(t, resp, http.StatusCreated)
	raw := readAll(t, resp)
	var got evidenceAssertionWire
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("evidence assertion payload: %v: %s", err, raw)
	}
	if got.Visibility != string(want) {
		t.Fatalf("the write path derived visibility %q for an assertion by project %s, want %q: %s",
			got.Visibility, projectID, want, raw)
	}
	return got.ID
}

// member seeds one membership row for a project (see the fixture note).
func (f *evidenceAudienceFixture) member(t *testing.T, ctx context.Context, projectID, userID string) {
	t.Helper()
	if _, err := f.w.pool.Exec(ctx,
		`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, 'contributor')`,
		parseUUIDOrDie(projectID), parseUUIDOrDie(userID)); err != nil {
		t.Fatalf("seed the membership of %s in %s: %v", userID, projectID, err)
	}
}

// --------------------------------------------------------------------------
// Reading, on all three exits

// get sends one read, treating nil as an anonymous browser.
func (f *evidenceAudienceFixture) get(t *testing.T, uc *testUserClient, path string) *http.Response {
	t.Helper()
	if uc == nil {
		uc = newTestUserClient(f.w.ts.URL)
	}
	return uc.do(t, http.MethodGet, path, "")
}

// objectRead reads the JSON object route and returns the assertion ids that
// reader was rendered (every bucket of every group) plus the raw body.
func (f *evidenceAudienceFixture) objectRead(t *testing.T, uc *testUserClient, objectID string) (map[string]bool, string) {
	t.Helper()
	path := "/api/v1/projects/" + f.alpha.id + "/objects/" + objectID + "/evidence"
	resp := f.get(t, uc, path)
	raw := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200: %s", path, resp.StatusCode, raw)
	}
	var got evidenceGraphObjectReadWire
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("object evidence payload: %v: %s", err, raw)
	}
	seen := map[string]bool{}
	for _, g := range got.Groups {
		for _, id := range assertionIDsOf(audienceRowsOf(g)) {
			seen[id] = true
		}
	}
	return seen, raw
}

// hypothesisRead is the same for the hypothesis route — the OTHER route that
// reads this table through the same per-target query, so it must answer the
// same audience. This suite checks that rather than assuming it.
func (f *evidenceAudienceFixture) hypothesisRead(t *testing.T, uc *testUserClient, objectID string) (map[string]bool, string) {
	t.Helper()
	path := "/api/v1/projects/" + f.alpha.id + "/hypotheses/" + objectID + "/evidence"
	resp := f.get(t, uc, path)
	raw := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200: %s", path, resp.StatusCode, raw)
	}
	var got evidenceGraphHypothesisReadWire
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("hypothesis evidence payload: %v: %s", err, raw)
	}
	seen := map[string]bool{}
	collect := func(groups []evidenceGraphGroupWire) {
		for _, g := range groups {
			for _, id := range assertionIDsOf(audienceRowsOf(g)) {
				seen[id] = true
			}
		}
	}
	collect(got.Direct)
	for _, claim := range got.Claims {
		collect(claim.Evidence)
	}
	return seen, raw
}

// evidencePanel reads the object detail page's evidence tab as one caller
// and returns the panel cut out of the page.
func (f *evidenceAudienceFixture) evidencePanel(t *testing.T, uc *testUserClient, objectID string) string {
	t.Helper()
	path := "/api/v1/projects/" + f.alpha.id + "/branches/" + f.alphaPub +
		"/objects/" + objectID + "?tab=evidence"
	req, err := http.NewRequest(http.MethodGet, f.w.ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The Accept header is what selects the HTML representation on this
	// route; without it the same URL answers the JSON detail document.
	req.Header.Set("Accept", "text/html")
	client := http.DefaultClient
	if uc != nil {
		req.Header.Set("X-CSRF-Token", uc.csrf)
		client = uc.client
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200 (the evidence tab is as visible as its project)", path, resp.StatusCode)
	}
	return panelOf(t, readAll(t, resp), "evidence")
}

// audienceRowsOf walks one group's four buckets.
func audienceRowsOf(g evidenceGraphGroupWire) []evidenceGraphAssertionWire {
	out := make([]evidenceGraphAssertionWire, 0, len(g.Supporting)+len(g.Contesting)+len(g.Neutral)+len(g.Unlabeled))
	out = append(out, g.Supporting...)
	out = append(out, g.Contesting...)
	out = append(out, g.Neutral...)
	out = append(out, g.Unlabeled...)
	return out
}

// --------------------------------------------------------------------------
// The required test

// TestEvidenceReadAudienceIsTheTwoPartiesUnion: one fixture, five readers,
// three exits, one row set each.
func TestEvidenceReadAudienceIsTheTwoPartiesUnion(t *testing.T) {
	ctx := testCtx(t)
	f := newEvidenceAudienceFixture(t, ctx)

	// The fixture's own facts, read off the TABLE rather than off the write
	// responses: the negative assertions below are only meaningful while the
	// private row really is there, with the visibility this suite assumes.
	t.Run("the fixture carries both shapes", func(t *testing.T) {
		for _, tc := range []struct {
			id        string
			want      string
			asserting string
		}{
			{f.publicAssertion, "public", f.alpha.id},
			{f.privateAssertion, "private", f.gamma.id},
		} {
			var visibility, asserting string
			if err := f.w.pool.QueryRow(ctx,
				`SELECT visibility, project_id::text FROM evidence_assertions WHERE id = $1`,
				parseUUIDOrDie(tc.id)).Scan(&visibility, &asserting); err != nil {
				t.Fatalf("read the assertion %s back: %v", tc.id, err)
			}
			if visibility != tc.want {
				t.Fatalf("assertion %s has visibility %q, want %q — the cases below would be vacuous",
					tc.id, visibility, tc.want)
			}
			if asserting != tc.asserting {
				t.Fatalf("assertion %s was asserted by project %s, want %s", tc.id, asserting, tc.asserting)
			}
		}
		// The target version carries exactly these two rows, so a later
		// change to the fixture cannot silently add a third.
		if n := countRows(t, ctx, f.w.pool,
			`SELECT count(*) FROM evidence_assertions WHERE target_object_version_id = $1`,
			parseUUIDOrDie(f.hypothesisVersion)); n != 2 {
			t.Fatalf("the target version carries %d assertions, want the fixture's two", n)
		}
	})

	for _, c := range []struct {
		name     string
		uc       *testUserClient
		seesPriv bool
	}{
		{name: "anonymous", uc: nil},
		{name: "signed-in non-member", uc: f.w.bob},
		{name: "asserting-project member", uc: f.dana, seesPriv: true},
		{name: "target-project member", uc: f.erin, seesPriv: true},
		{name: "member of both", uc: f.w.alice, seesPriv: true},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			// --- the JSON object route ----------------------------------
			seen, raw := f.objectRead(t, c.uc, f.hypothesisID)
			if !seen[f.publicAssertion] {
				t.Errorf("the public assertion is missing from the answer — rendering nobody anything is not this fix: %s", raw)
			}
			if seen[f.privateAssertion] != c.seesPriv {
				t.Errorf("the object route renders the private assertion = %v, want %v for the %s reader: %s",
					seen[f.privateAssertion], c.seesPriv, c.name, raw)
			}

			// --- the OTHER JSON route over the same rows ----------------
			hypSeen, hypRaw := f.hypothesisRead(t, c.uc, f.hypothesisID)
			if !hypSeen[f.publicAssertion] {
				t.Errorf("the hypothesis route dropped the public assertion: %s", hypRaw)
			}
			if hypSeen[f.privateAssertion] != c.seesPriv {
				t.Errorf("the hypothesis route renders the private assertion = %v, want %v for the %s reader: %s",
					hypSeen[f.privateAssertion], c.seesPriv, c.name, hypRaw)
			}

			// --- the page's evidence tab --------------------------------
			panel := f.evidencePanel(t, c.uc, f.hypothesisID)
			if !strings.Contains(panel, `data-row-edge-id="`+f.publicAssertion+`"`) {
				t.Errorf("the evidence tab dropped the public row for the %s reader:\n%s", c.name, panel)
			}
			if got := strings.Contains(panel, f.privateAssertion); got != c.seesPriv {
				t.Errorf("the evidence tab carries the private assertion = %v, want %v for the %s reader:\n%s",
					got, c.seesPriv, c.name, panel)
			}
		})
	}
}

// TestEvidenceReadAudienceIsNotMerelyAnonymous is the criterion the ADR calls
// out by name: a SIGNED-IN caller who may read the public project but belongs
// to neither party gets the public rows only. It is separate from the table
// above so "we blocked anonymous" cannot be mistaken for the rule — and it
// asserts the positive half too, because a caller who sees nothing proves
// nothing about a filter.
func TestEvidenceReadAudienceIsNotMerelyAnonymous(t *testing.T) {
	ctx := testCtx(t)
	f := newEvidenceAudienceFixture(t, ctx)

	// bob is signed in and a member of nothing here; the public project's
	// gate lets him through, which is exactly the situation ADR-024's
	// "尚未决定" #1 is about (a network account that may read the project but
	// is party to none of its assertions).
	if n := countRows(t, ctx, f.w.pool,
		`SELECT count(*) FROM project_memberships WHERE user_id = $1 AND project_id IN ($2, $3)`,
		parseUUIDOrDie(f.w.bobID), parseUUIDOrDie(f.alpha.id), parseUUIDOrDie(f.gamma.id)); n != 0 {
		t.Fatalf("the fixture's non-member holds %d memberships in the two parties: the case would be vacuous", n)
	}

	seen, raw := f.objectRead(t, f.w.bob, f.hypothesisID)
	if !seen[f.publicAssertion] {
		t.Errorf("the signed-in non-member lost the PUBLIC row, so nothing below measures an audience: %s", raw)
	}
	if seen[f.privateAssertion] {
		t.Errorf("a signed-in caller unrelated to the row was rendered the private one: %s", raw)
	}

	hypSeen, hypRaw := f.hypothesisRead(t, f.w.bob, f.hypothesisID)
	if !hypSeen[f.publicAssertion] {
		t.Errorf("the hypothesis route lost the public row for the signed-in non-member: %s", hypRaw)
	}
	if hypSeen[f.privateAssertion] {
		t.Errorf("the hypothesis route rendered the private row to a signed-in non-member: %s", hypRaw)
	}

	panel := f.evidencePanel(t, f.w.bob, f.hypothesisID)
	if !strings.Contains(panel, `data-row-edge-id="`+f.publicAssertion+`"`) {
		t.Errorf("the page dropped the public row for a signed-in non-member:\n%s", panel)
	}
	if strings.Contains(panel, f.privateAssertion) {
		t.Errorf("the page rendered the private row to a signed-in non-member:\n%s", panel)
	}
}

// TestEvidenceReadAudienceKeepsTheGateAnswer: fixing which ROWS a reader sees
// must not change what the project gate ANSWERS, and must not turn the read
// into an error for the readers who now see less. An anonymous read of a
// public project is still 200 carrying the full document shape — its own
// rows, in the projection's buckets — and not an unavailability envelope.
func TestEvidenceReadAudienceKeepsTheGateAnswer(t *testing.T) {
	ctx := testCtx(t)
	f := newEvidenceAudienceFixture(t, ctx)

	for _, c := range []struct {
		name string
		uc   *testUserClient
	}{
		{name: "anonymous", uc: nil},
		{name: "signed-in non-member", uc: f.w.bob},
		{name: "asserting-project member", uc: f.dana},
		{name: "target-project member", uc: f.erin},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			path := "/api/v1/projects/" + f.alpha.id + "/objects/" + f.hypothesisID + "/evidence"
			resp := f.get(t, c.uc, path)
			raw := readAll(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET %s = %d, want 200: %s", path, resp.StatusCode, raw)
			}
			var got evidenceGraphObjectReadWire
			if err := json.Unmarshal([]byte(raw), &got); err != nil {
				t.Fatalf("object evidence payload: %v: %s", err, raw)
			}
			if got.ProjectID != f.alpha.id {
				t.Errorf("project_id = %q, want the path project %q", got.ProjectID, f.alpha.id)
			}
			if len(got.Groups) != 1 || got.Groups[0].Target.ObjectVersionID != f.hypothesisVersion {
				t.Errorf("groups = %+v, want the one version carrying evidence", got.Groups)
			}
			if strings.Contains(raw, "EVIDENCE_UNAVAILABLE") {
				t.Errorf("the reader's answer is an unavailability envelope: %s", raw)
			}
		})
	}
}
