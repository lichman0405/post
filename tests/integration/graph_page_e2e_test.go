// Package integration — T0507 "graph e2e" (the task's required test): the
// object detail page's two graph tabs exercised end to end over REAL
// PostgreSQL, the real auth guard, the real RSG write surface (objects,
// versions and typed relations over the wire), the contract's evidence
// assertion route, a real knowledge publication, T0505's real provenance
// projection and T0506's real evidence read — then the browser read of the
// page itself.
//
// The requirements and the acceptance criteria, and where each is proven:
//
//   - Object detail 两类 graph/tab: two tabs, two panels, two identity
//     namespaces. Each tab is reached on its own (?tab=provenance,
//     ?tab=evidence) and neither carries the other's rows.
//   - list fallback: every edge the aria-hidden SVG draws has a row with the
//     same id in an accessible table beside it, in both directions (nothing
//     drawn that is not listed, nothing listed that is not drawn), and the
//     page carries no JavaScript at all — the list is the page's own content,
//     not a hydration that needs scripting to appear.
//   - relation semantics label: each row's meaning is the relation catalog's
//     own one-line Semantics for that relation type, asserted against the
//     catalog itself rather than against a copy of its wording here.
//   - 用户不把两图混淆 (docs/10 §1, CLAUDE.md §9 invariant 10): asserted by
//     ROW IDENTITY. The two graphs share the object version they are about —
//     docs/10 §1 allows shared nodes — so the check is that neither panel
//     carries the other's edges: not the evidence assertion ids on the
//     provenance panel, not the provenance edge ids on the evidence panel,
//     and not one another's relation vocabulary.
//   - a11y list 可访问: the table carries a caption and scope'd headers, the
//     stance is text in a cell (never colour alone, and the legend names the
//     strokes), and the picture is aria-hidden.
//
// The negative paths are here too:
//
//   - the weak relation on the SAME pair as the provenance edge (related_to)
//     is projected into no provenance edge (docs/07 §3, migration 00043), so
//     the provenance tab must not carry it while the relations tab still
//     does;
//   - a version nothing is recorded about says EMPTY, not "could not be
//     read" — the two are different statements about the world.
//
// The remaining fail-closed branch — a READ THAT FAILS, which this suite's
// real reads do not — is constructed in cmd/api/rsghttp/graph_test.go, where
// a failing reader can be wired.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/relationcatalog"
)

// graphPageTaskID namespaces this task's test database
// (test_T0507_<run_id>, docs/66 §3).
const graphPageTaskID = "T0507"

// --------------------------------------------------------------------------
// The fixture

// graphPageFixture is the world this suite reads: one public project whose
// claim version carries BOTH graphs — a provenance edge to the dataset it was
// derived from (plus a weak relation on the same pair, which is projected
// into no provenance edge) and two evidence assertions on one evidence
// version, one per stance.
type graphPageFixture struct {
	w *knowledgeWorld
	p *knowledgeProject

	claimID     string // the object the page shows
	claimVer    string // its published version — the two graphs' shared node
	datasetID   string
	datasetVer  string // the provenance origin
	evidenceID  string
	evidenceVer string // the version the two assertions cite

	provenanceEdge string // relation version id: claim → dataset (derived_from)
	weakEdge       string // relation version id: claim → dataset (related_to)
	supportID      string // evidence assertion: supports
	contestID      string // evidence assertion: contradicts

	// The untouched research question: a version with no relation and no
	// assertion, where both graphs must answer "nothing recorded".
	questionID  string
	questionVer string
}

func newGraphPageFixture(t *testing.T, ctx context.Context) *graphPageFixture {
	t.Helper()
	w := newKnowledgeWorldFor(t, ctx, graphPageTaskID)
	w.signups(t)
	p := w.newProject(t, ctx, "graph-lab", "public")
	f := &graphPageFixture{w: w, p: p}

	// The question the claim answers (migration 00040's guard resolves a
	// claim's subject against an existing object of the same project), and
	// the version this suite reads the empty state from.
	f.questionID, f.questionVer = f.seedObject(t, ctx, "research_question",
		`{"statement":"What is the CO2 uptake of MOF-5 at 298 K?","purpose":"graph page fixture","question_state":"open"}`)

	// The claim, PUBLISHED: an evidence assertion's target must be published
	// knowledge — the write path's own rule (internal/application/rsg/
	// evidence.go), not this suite's convenience.
	f.claimID, f.claimVer, _ = w.seedPublishedVersion(t, ctx, p, "claim",
		fmt.Sprintf(`{"statement":"MOF-5 takes up 5.1 mmol/g CO2 at 298 K and 1 bar",`+
			`"claim_type":"descriptive","subject_ref":%q,"property":"CO2 uptake",`+
			`"scope":{"conditions":"298 K, 1 bar"},"assessment":"preliminary"}`, f.questionID), 1)

	// The provenance origin and the evidence version (any versions of this
	// project; the evidence side need not be published).
	f.datasetID, f.datasetVer = f.seedObject(t, ctx, "dataset", `{"name":"the 298 K uptake isotherm series"}`)
	f.evidenceID, f.evidenceVer = f.seedObject(t, ctx, "material", `{"name":"an independently activated MOF-5 sample"}`)

	// The provenance edge: the claim version came from the dataset version
	// (derived_from points its origin at the TARGET, internal/rsg/provenance).
	f.provenanceEdge = f.relation(t, "derived_from", f.claimVer, f.datasetVer)
	// The weak relation on the same pair: never projected into provenance.
	f.weakEdge = f.relation(t, "related_to", f.claimVer, f.datasetVer)

	// The two evidence assertions about the claim version, on the SAME
	// (target, evidence) pair: one supporting, one contesting — the pair
	// migration 00058 deliberately leaves non-unique (docs/10 §7: a
	// contradiction stays visible next to the support it contradicts).
	f.supportID = f.assertEvidence(t, w.alice, f.claimVer, f.evidenceVer, "supports").ID
	f.contestID = f.assertEvidence(t, w.alice, f.claimVer, f.evidenceVer, "contradicts").ID
	return f
}

// seedObject creates one version of an object of the given type on the
// project's research branch, through the real RSG service.
func (f *graphPageFixture) seedObject(t *testing.T, ctx context.Context, objectType, payload string) (objectID, versionID string) {
	t.Helper()
	res, err := f.w.svc.CreateObject(ctx, domain.User{ID: f.w.aliceID}, f.p.id, f.p.probe, rsg.CreateObjectInput{
		ObjectType: objectType,
		Payload:    json.RawMessage(payload),
	})
	if err != nil {
		t.Fatalf("CreateObject(%s) on %s: %v", objectType, f.p.probe, err)
	}
	return res.Object.ID, res.Version.ID
}

// relation writes one typed relation through the product's own route and
// returns the stored relation VERSION id — the identity the page's diagram
// and list both carry.
func (f *graphPageFixture) relation(t *testing.T, relationType, sourceVersion, targetVersion string) string {
	t.Helper()
	resp := f.w.alice.do(t, http.MethodPost,
		"/api/v1/projects/"+f.p.id+"/branches/"+f.p.probe+"/relations",
		fmt.Sprintf(`{"relation_type":%q,"source_object_version_id":%q,"target_object_version_id":%q}`,
			relationType, sourceVersion, targetVersion))
	mustStatus(t, resp, http.StatusCreated)
	raw := readAll(t, resp)
	var stored struct {
		VersionID string `json:"version_id"`
	}
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		t.Fatalf("relation payload: %v: %s", err, raw)
	}
	return stored.VersionID
}

// assertEvidence posts one assertion through the contract's route and returns
// the stored row as the wire renders it.
func (f *graphPageFixture) assertEvidence(t *testing.T, uc *testUserClient, targetVersion, evidenceVersion, relation string) evidenceAssertionWire {
	t.Helper()
	resp := uc.do(t, http.MethodPost,
		"/api/v1/projects/"+f.p.id+"/branches/"+f.p.probe+"/evidence-assertions",
		evidenceAssertionBody(targetVersion, evidenceVersion, relation))
	mustStatus(t, resp, http.StatusCreated)
	raw := readAll(t, resp)
	var got evidenceAssertionWire
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("evidence assertion payload: %v: %s", err, raw)
	}
	return got
}

// pagePath is the object's canonical detail URL on the branch the fixture
// wrote to.
func (f *graphPageFixture) pagePath(objectID, query string) string {
	return "/api/v1/projects/" + f.p.id + "/branches/" + f.p.probe + "/objects/" + objectID + query
}

// graphTab fetches one tab of an object's detail page as alice, and returns
// the whole page. The Accept header is what selects the HTML representation:
// the same route answers JSON to a JSON client (T0210's negotiation).
func (f *graphPageFixture) graphTab(t *testing.T, objectID, query string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.w.ts.URL+f.pagePath(objectID, query), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/html")
	req.Header.Set("X-CSRF-Token", f.w.alice.csrf)
	resp, err := f.w.alice.client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", query, err)
	}
	defer resp.Body.Close()
	mustStatus(t, resp, http.StatusOK)
	return readAll(t, resp)
}

// panelOf cuts one graph panel out of the page: from the section carrying
// that graph's identity to its closing tag. Assertions about what a tab does
// NOT carry must be scoped to the panel, because the tab strip on every page
// names both tabs — that is navigation, not content.
func panelOf(t *testing.T, body, graph string) string {
	t.Helper()
	marker := `data-graph="` + graph + `"`
	start := strings.Index(body, marker)
	if start < 0 {
		t.Fatalf("the page carries no %s panel (%s)", graph, marker)
	}
	end := strings.Index(body[start:], "</section>")
	if end < 0 {
		t.Fatalf("the %s panel is not closed", graph)
	}
	return body[start : start+end]
}

// idsBetween collects the values of one attribute inside a fragment.
func idsBetween(fragment, attr string) []string {
	var ids []string
	for rest := fragment; ; {
		i := strings.Index(rest, attr+`="`)
		if i < 0 {
			return ids
		}
		rest = rest[i+len(attr)+2:]
		j := strings.Index(rest, `"`)
		if j < 0 {
			return ids
		}
		ids = append(ids, rest[:j])
		rest = rest[j:]
	}
}

// --------------------------------------------------------------------------
// The required test

// TestGraphPageE2E is the required "graph e2e".
func TestGraphPageE2E(t *testing.T) {
	ctx := testCtx(t)
	f := newGraphPageFixture(t, ctx)

	provenanceTab := f.graphTab(t, f.claimID, "?tab=provenance")
	evidenceTab := f.graphTab(t, f.claimID, "?tab=evidence")
	provenancePanel := panelOf(t, provenanceTab, "provenance")
	evidencePanel := panelOf(t, evidenceTab, "evidence")

	t.Run("two tabs, two graphs", func(t *testing.T) {
		for _, want := range []string{`data-tab="provenance"`, `data-tab="evidence"`, ">Provenance<", ">Evidence<"} {
			if !strings.Contains(provenanceTab, want) {
				t.Errorf("the page's tab strip lacks %q", want)
			}
		}
		if !strings.Contains(provenancePanel, `data-graph-state="ok"`) || !strings.Contains(evidencePanel, `data-graph-state="ok"`) {
			t.Fatalf("a graph panel did not render the ok state")
		}
		// The other graph is not on this tab at all — not its panel, not its
		// rows, not its direction control.
		for _, forbidden := range []string{`data-graph="evidence"`, `data-graph-list="evidence"`, `data-graph-diagram="evidence"`} {
			if strings.Contains(provenanceTab, forbidden) {
				t.Errorf("the provenance tab carries %q", forbidden)
			}
		}
		for _, forbidden := range []string{`data-graph="provenance"`, `data-graph-list="provenance"`, `data-graph-diagram="provenance"`, `data-graph-direction`} {
			if strings.Contains(evidenceTab, forbidden) {
				t.Errorf("the evidence tab carries %q", forbidden)
			}
		}
		// Each names the other, so a reader on one tab is told what it is
		// not without having to visit the other one first.
		if !strings.Contains(provenancePanel, "This is not the evidence graph") || !strings.Contains(provenancePanel, `data-graph-cross-link="evidence"`) {
			t.Errorf("the provenance panel does not name the evidence graph as the other one")
		}
		if !strings.Contains(evidencePanel, "This is not the provenance graph") || !strings.Contains(evidencePanel, `data-graph-cross-link="provenance"`) {
			t.Errorf("the evidence panel does not name the provenance graph as the other one")
		}
	})

	t.Run("the two graphs share no row", func(t *testing.T) {
		// The seeded world puts both graphs on ONE object version, so a
		// conflation would show up here as one graph's EDGE id appearing in
		// the other's panel.
		for _, other := range []string{f.supportID, f.contestID, f.evidenceVer} {
			if strings.Contains(provenancePanel, other) {
				t.Errorf("the provenance panel carries the evidence id %q", other)
			}
		}
		for _, other := range []string{f.provenanceEdge, f.weakEdge} {
			if strings.Contains(evidencePanel, other) {
				t.Errorf("the evidence panel carries the provenance edge id %q", other)
			}
		}
		for _, relation := range []string{"supports", "contradicts", "contextualizes"} {
			if strings.Contains(provenancePanel, `data-row-relation="`+relation+`"`) {
				t.Errorf("the provenance list carries the evidence relation %q", relation)
			}
		}
		for _, relation := range []string{"derived_from", "related_to", "contains", "produces"} {
			if strings.Contains(evidencePanel, `data-row-relation="`+relation+`"`) {
				t.Errorf("the evidence list carries the provenance relation %q", relation)
			}
		}
		if strings.Contains(provenancePanel, "data-row-stance") {
			t.Errorf("the provenance panel carries a stance — a provenance edge has none")
		}
		if strings.Contains(evidencePanel, `data-graph-direction`) {
			t.Errorf("the evidence panel carries the provenance walk's direction control")
		}
	})

	t.Run("the provenance list is the projected edge, with the catalog's meaning", func(t *testing.T) {
		if !strings.Contains(provenancePanel, `data-row-edge-id="`+f.provenanceEdge+`"`) {
			t.Fatalf("the provenance list lacks the projected edge %s:\n%s", f.provenanceEdge, provenancePanel)
		}
		entry, ok := relationcatalog.Lookup("derived_from")
		if !ok {
			t.Fatalf("the catalog no longer carries derived_from")
		}
		if !strings.Contains(provenancePanel, `data-row-relation="derived_from"`) || !strings.Contains(provenancePanel, entry.Semantics) {
			t.Errorf("the provenance list does not carry derived_from with the catalog's meaning %q", entry.Semantics)
		}
		if !strings.Contains(provenancePanel, `data-row-semantics="catalog"`) {
			t.Errorf("the provenance row does not mark its meaning as the catalog's")
		}
		// The weak relation on the same pair is projected into no edge
		// (docs/07 §3, migration 00043), so the provenance tab must not carry
		// it — while the relations tab still shows it, which is what makes
		// this a filter and not a hole.
		if strings.Contains(provenanceTab, f.weakEdge) || strings.Contains(provenancePanel, "related_to") {
			t.Errorf("the provenance tab carries the weak related_to relation")
		}
		relationsTab := f.graphTab(t, f.claimID, "?tab=relations")
		if !strings.Contains(relationsTab, "related_to") {
			t.Errorf("the relations tab does not carry related_to — the weak relation vanished instead of staying weak")
		}
	})

	t.Run("the evidence list keeps both stances apart, with the catalog's meaning", func(t *testing.T) {
		for _, id := range []string{f.supportID, f.contestID} {
			if !strings.Contains(evidencePanel, `data-row-edge-id="`+id+`"`) {
				t.Errorf("the evidence list lacks assertion %s", id)
			}
		}
		if !strings.Contains(evidencePanel, `data-row-stance-label="supporting"`) || !strings.Contains(evidencePanel, `data-row-stance-label="contesting"`) {
			t.Errorf("the evidence list does not carry both stances as their own rows")
		}
		support := strings.Index(evidencePanel, `data-row-edge-id="`+f.supportID+`"`)
		contest := strings.Index(evidencePanel, `data-row-edge-id="`+f.contestID+`"`)
		if support < 0 || contest < 0 || support == contest {
			t.Errorf("the supporting and contesting assertions are not two distinct rows")
		}
		for _, relation := range []string{"supports", "contradicts"} {
			entry, ok := relationcatalog.Lookup(relation)
			if !ok {
				t.Fatalf("the catalog no longer carries %s", relation)
			}
			if !strings.Contains(evidencePanel, `data-row-relation="`+relation+`"`) || !strings.Contains(evidencePanel, entry.Semantics) {
				t.Errorf("the evidence list does not carry %s with the catalog's meaning %q", relation, entry.Semantics)
			}
		}
		if !strings.Contains(evidencePanel, f.evidenceVer) {
			t.Errorf("the evidence list does not name the evidence version the assertions pin")
		}
	})

	t.Run("the list equals the diagram, both ways", func(t *testing.T) {
		for _, tc := range []struct{ graph, panel string }{
			{"provenance", provenancePanel},
			{"evidence", evidencePanel},
		} {
			drawn := idsBetween(tc.panel, "data-edge-id")
			listed := idsBetween(tc.panel, "data-row-edge-id")
			if len(drawn) == 0 || len(listed) == 0 {
				t.Fatalf("%s: diagram edges = %d, list rows = %d — the tab must draw and list its rows", tc.graph, len(drawn), len(listed))
			}
			sort.Strings(drawn)
			sort.Strings(listed)
			if strings.Join(drawn, ",") != strings.Join(listed, ",") {
				t.Errorf("%s: the diagram draws %v but the list rows are %v", tc.graph, drawn, listed)
			}
			// The identities are the wire's own — the relation version id
			// (provenance) and the assertion id (evidence) the JSON reads
			// return — not a display-only counter invented by the page.
			switch tc.graph {
			case "provenance":
				if len(listed) != 1 || listed[0] != f.provenanceEdge {
					t.Errorf("provenance list ids = %v, want just %s", listed, f.provenanceEdge)
				}
			case "evidence":
				want := []string{f.contestID, f.supportID}
				sort.Strings(want)
				if strings.Join(listed, ",") != strings.Join(want, ",") {
					t.Errorf("evidence list ids = %v, want %v", listed, want)
				}
			}
		}
	})

	t.Run("the list is accessible and the page needs no script", func(t *testing.T) {
		for _, tc := range []struct{ graph, panel string }{
			{"provenance", provenancePanel},
			{"evidence", evidencePanel},
		} {
			for _, want := range []string{
				`<table class="graph-list" data-graph-list="` + tc.graph + `">`,
				"<caption>",
				`<th scope="col">Relation</th>`,
				`<th scope="col">What the relation means</th>`,
				`<th scope="row">`,
				`aria-hidden="true"`,
				`focusable="false"`,
				`data-graph-diagram="` + tc.graph + `"`,
			} {
				if !strings.Contains(tc.panel, want) {
					t.Errorf("%s list lacks %q", tc.graph, want)
				}
			}
		}
		if !strings.Contains(evidencePanel, `data-graph-legend="evidence"`) {
			t.Errorf("the evidence diagram has no legend naming its strokes in words")
		}
		for _, tab := range []string{provenanceTab, evidenceTab} {
			if strings.Contains(tab, "<script") {
				t.Errorf("a graph tab carries script — the list must be the page's own content")
			}
		}
	})

	t.Run("a version with no graph says empty, not unavailable", func(t *testing.T) {
		// The untouched question carries no relation and no assertion: both
		// reads succeed and answer nothing, which is a different statement
		// from "the graph could not be read".
		for _, tab := range []string{"provenance", "evidence"} {
			panel := panelOf(t, f.graphTab(t, f.questionID, "?tab="+tab), tab)
			if !strings.Contains(panel, `data-graph-state="ok"`) {
				t.Errorf("%s tab on a version with no graph: state is not ok", tab)
			}
			if !strings.Contains(panel, `data-graph-empty="`+tab+`"`) {
				t.Errorf("%s tab on a version with no graph does not say it is empty", tab)
			}
			if strings.Contains(panel, "data-graph-diagram=") || strings.Contains(panel, "data-row-edge-id=") {
				t.Errorf("%s tab on a version with no graph drew a picture or rows", tab)
			}
			if strings.Contains(panel, "could not be read") {
				t.Errorf("%s tab claims the graph could not be read when the read succeeded", tab)
			}
		}
	})
}

// TestGraphPageRendersTheSameReadsAsTheJSONRoutes is the anti-vacuity check
// for the suite above: the page shows a row exactly when the JSON read of the
// SAME adapter returns it. Without this, every assertion above could be
// passing because the fixture never put anything in the database — a check
// that measures nothing reads as green as one that measures something.
func TestGraphPageRendersTheSameReadsAsTheJSONRoutes(t *testing.T) {
	ctx := testCtx(t)
	f := newGraphPageFixture(t, ctx)

	// T0505's own route: the project's provenance graph.
	resp := f.w.alice.do(t, http.MethodGet, "/api/v1/projects/"+f.p.id+"/provenance/graph", "")
	mustStatus(t, resp, http.StatusOK)
	provenanceJSON := readAll(t, resp)
	if !strings.Contains(provenanceJSON, f.provenanceEdge) || !strings.Contains(provenanceJSON, "derived_from") {
		t.Fatalf("the provenance JSON graph does not carry the seeded edge: %s", provenanceJSON)
	}
	if strings.Contains(provenanceJSON, f.weakEdge) {
		t.Fatalf("the provenance JSON graph carries the weak relation — the projection filter is not running")
	}

	// T0506's own route: the object's evidence.
	resp = f.w.alice.do(t, http.MethodGet, "/api/v1/projects/"+f.p.id+"/objects/"+f.claimID+"/evidence", "")
	mustStatus(t, resp, http.StatusOK)
	evidenceJSON := readAll(t, resp)
	for _, id := range []string{f.supportID, f.contestID} {
		if !strings.Contains(evidenceJSON, id) {
			t.Fatalf("the evidence JSON read does not carry assertion %s: %s", id, evidenceJSON)
		}
	}
	if !strings.Contains(evidenceJSON, `"stance":"supporting"`) || !strings.Contains(evidenceJSON, `"stance":"contesting"`) {
		t.Fatalf("the evidence JSON read does not carry both stances: %s", evidenceJSON)
	}

	// And the page carries the same identities — the page and the API cannot
	// disagree, because there is one adapter per graph and both go through it.
	page := f.graphTab(t, f.claimID, "?tab=provenance")
	if !strings.Contains(panelOf(t, page, "provenance"), f.provenanceEdge) {
		t.Errorf("the page does not show the edge the JSON read returned")
	}
	page = f.graphTab(t, f.claimID, "?tab=evidence")
	panel := panelOf(t, page, "evidence")
	for _, id := range []string{f.supportID, f.contestID} {
		if !strings.Contains(panel, id) {
			t.Errorf("the page does not show the assertion %s the JSON read returned", id)
		}
	}
}
