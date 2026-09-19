package rsghttp

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/evidence"
	"github.com/lichman0405/post/internal/rsg/provenance"
	"github.com/lichman0405/post/internal/rsg/relationcatalog"
)

// The object detail page's two graph tabs (T0507). The assertions here are
// the page's own, and they come in three kinds:
//
//   - SEPARATION (acceptance: 用户不把两图混淆, docs/10 §1): each tab renders
//     its own graph and nothing of the other's — not its rows, not its
//     identities, not its relation vocabulary. Identity-level, so a
//     coincidence of wording cannot pass it and a real leak cannot hide.
//   - FAIL-CLOSED: a read that fails, and a reader that is not wired, both
//     render "could not be read" with NO rows. An empty panel would be a
//     claim about the data, and neither of those two situations knows one.
//   - THE LIST IS THE CONTENT (acceptance: a11y list 可访问, docs/06 §56,
//     docs/51 §10): the SVG is aria-hidden and every edge it draws has a
//     row with the same id in an accessible table; the state a row carries
//     is text in a cell, never colour alone; the page stays complete with
//     zero JavaScript, because there is none.

// ---------------------------------------------------------------------------
// The fixture: one version with an origin, and three assertions about it
// ---------------------------------------------------------------------------

const (
	graphObjectID  = "obj-1"
	graphVersionID = "ver-2" // the page's selected version (cannedDetail's v2)
	graphOriginID  = "ver-9"
	// graphEdgeID is the relation VERSION id of the project's one provenance
	// edge — the identity the diagram and the list must agree on.
	graphEdgeID = "relver-1"
)

// stubProvenance is T0505's read with the graph the assertions below expect:
// the page's version came from a dataset (derived_from, origin = target).
type stubProvenance struct {
	edges []provenance.Edge
	start provenance.Node
	err   error
	calls int
}

func (s *stubProvenance) ListEdges(_ context.Context, _ string) ([]provenance.Edge, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.edges, nil
}

func (s *stubProvenance) ObjectStartNode(_ context.Context, _, _ string, _ *int) (provenance.Node, error) {
	s.calls++
	if s.err != nil {
		return provenance.Node{}, s.err
	}
	return s.start, nil
}

// stubEvidence is T0506's read with three assertions about the page's
// version — one per stance the domain rule derives, deliberately on
// different evidence versions so the picture has three edges.
type stubEvidence struct {
	read  evidence.ObjectEvidence
	err   error
	calls int
}

func (s *stubEvidence) ObjectEvidence(_ context.Context, _, _ string, _ *int) (evidence.ObjectEvidence, error) {
	s.calls++
	if s.err != nil {
		return evidence.ObjectEvidence{}, s.err
	}
	return s.read, nil
}

// graphAssertion builds one assertion the way the projection does: the
// stance comes from the domain rule rather than from this fixture's opinion,
// so the fixture cannot drift from the rule it is standing in for.
func graphAssertion(id, relation, evidenceVersion string) evidence.Assertion {
	stance, _ := domain.RelationStance(domain.EvidenceRelation(relation))
	return evidence.Assertion{
		ID:                      id,
		Relation:                relation,
		Stance:                  string(stance),
		EvidenceType:            string(domain.EvidenceTypeExperimental),
		ReviewState:             string(domain.EvidenceReviewUnreviewed),
		TargetObjectVersionID:   graphVersionID,
		EvidenceObjectVersionID: evidenceVersion,
		CreatedAt:               "2026-09-18T10:00:00Z",
	}
}

func stubProvenanceReader() *stubProvenance {
	start := provenance.Node{ObjectID: graphObjectID, VersionID: graphVersionID, VersionNo: 2, ObjectType: "material", Title: "MOF-5 refined"}
	return &stubProvenance{
		start: start,
		edges: []provenance.Edge{{
			RelationID: "rel-1", RelationVersionID: graphEdgeID, RelationType: "derived_from",
			// derived_from: the source came from the target (OriginIsTarget),
			// so the page's version is the source and the dataset is its origin.
			Source: start,
			Target: provenance.Node{ObjectID: "obj-9", VersionID: graphOriginID, VersionNo: 1, ObjectType: "dataset", Title: "isotherm series"},
		}},
	}
}

func stubEvidenceReader() *stubEvidence {
	return &stubEvidence{read: evidence.ObjectEvidence{
		ProjectID: rsgTestProjectID,
		Object:    evidence.ObjectRef{ObjectID: graphObjectID, ObjectType: "material", Title: "MOF-5 refined"},
		Groups: []evidence.TargetGroup{{
			Target:     evidence.Target{ObjectVersionID: graphVersionID, VersionNo: intPtr(2), Title: "MOF-5 refined"},
			Supporting: []evidence.Assertion{graphAssertion("assert-support", "supports", "ever-20")},
			Contesting: []evidence.Assertion{graphAssertion("assert-contest", "contradicts", "ever-21")},
			Neutral:    []evidence.Assertion{graphAssertion("assert-context", "contextualizes", "ever-22")},
		}},
	}}
}

func intPtr(n int) *int { return &n }

// getGraphPage fetches one tab of the detail page with both graph readers
// wired, and returns the body.
func getGraphPage(t *testing.T, prov ProvenanceReader, ev EvidenceReader, query string) string {
	t.Helper()
	ts, _, _, _ := newRSGTestServerDeps(t, Deps{Service: &stubService{detail: cannedDetail()}, Provenance: prov, Evidence: ev})
	req, err := http.NewRequest(http.MethodGet, ts.URL+objectsBase+"/objects/"+graphObjectID+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/html")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", query, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200 (body %s)", query, resp.StatusCode, respBody(resp))
	}
	return respBody(resp)
}

// ---------------------------------------------------------------------------
// Separation
// ---------------------------------------------------------------------------

// TestGraphTabsKeepTheTwoGraphsApart is the acceptance criterion 用户不把两图
// 混淆 (docs/10 §1, CLAUDE.md §9 invariant 10), asserted by IDENTITY: the
// tab that shows one graph must not carry the other graph's row ids at all.
// Wording can be reworded; an id cannot be shared by accident.
func TestGraphTabsKeepTheTwoGraphsApart(t *testing.T) {
	prov := stubProvenanceReader()
	ev := stubEvidenceReader()

	provenanceTab := getGraphPage(t, prov, ev, "?tab=provenance")
	evidenceTab := getGraphPage(t, prov, ev, "?tab=evidence")

	for _, forbidden := range []string{
		"assert-support", "assert-contest", "assert-context", // the other graph's rows
		`data-graph="evidence"`, `data-graph-list="evidence"`, `data-graph-diagram="evidence"`,
		`data-row-stance`, // stance belongs to the evidence tab alone
	} {
		if strings.Contains(provenanceTab, forbidden) {
			t.Errorf("provenance tab carries %q — the two graphs must not be conflated", forbidden)
		}
	}
	for _, want := range []string{
		`data-graph="provenance"`, `data-graph-state="ok"`,
		`data-graph-list="provenance"`, `data-graph-diagram="provenance"`,
		`data-row-edge-id="` + graphEdgeID + `"`,
		"isotherm series", // the origin version's own title
		provenanceBoundary, "This is not the evidence graph",
		`data-graph-cross-link="evidence"`, // and where the OTHER graph is
		`tab=evidence`,
	} {
		if !strings.Contains(provenanceTab, want) {
			t.Errorf("provenance tab lacks %q", want)
		}
	}

	for _, forbidden := range []string{
		graphEdgeID, graphOriginID, // the other graph's rows and nodes
		`data-graph="provenance"`, `data-graph-list="provenance"`, `data-graph-diagram="provenance"`,
		`data-graph-direction`, // the walk control belongs to the provenance tab alone
	} {
		if strings.Contains(evidenceTab, forbidden) {
			t.Errorf("evidence tab carries %q — the two graphs must not be conflated", forbidden)
		}
	}
	for _, want := range []string{
		`data-graph="evidence"`, `data-graph-state="ok"`,
		`data-graph-list="evidence"`, `data-graph-diagram="evidence"`,
		`data-row-edge-id="assert-support"`, `data-row-edge-id="assert-contest"`, `data-row-edge-id="assert-context"`,
		evidenceBoundary, "This is not the provenance graph",
		`data-graph-cross-link="provenance"`,
		"ever-20", "ever-21", "ever-22", // the evidence versions the assertions pin
	} {
		if !strings.Contains(evidenceTab, want) {
			t.Errorf("evidence tab lacks %q", want)
		}
	}

	// Neither graph's read runs on a tab that does not render it: a page load
	// costs the queries the page renders (and the other graph's rows cannot
	// leak through a read nobody made).
	if prov.calls == 0 || ev.calls == 0 {
		t.Fatalf("reads = provenance %d, evidence %d, want both used at least once", prov.calls, ev.calls)
	}
	metadata := getGraphPage(t, prov, ev, "")
	if strings.Contains(metadata, `data-graph-list=`) {
		t.Errorf("the metadata tab renders a graph list")
	}
}

// TestGraphTabsAreTwoTabs: both graph tabs are in the page's own tab strip,
// each naming itself, so a reader reaches one without a control inside the
// other (the whole point of two tabs rather than one tab with a toggle).
func TestGraphTabsAreTwoTabs(t *testing.T) {
	body := getGraphPage(t, stubProvenanceReader(), stubEvidenceReader(), "?tab=provenance")
	for _, want := range []string{
		`data-tab="provenance"`, `data-tab="evidence"`,
		`tab=provenance`, `tab=evidence`,
		">Provenance<", ">Evidence<",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("tab strip lacks %q", want)
		}
	}
}

// ---------------------------------------------------------------------------
// Fail-closed
// ---------------------------------------------------------------------------

// TestGraphPanelsFailClosed: a read that errors and a reader that is not
// wired both render "could not be read" — with no rows, no diagram and no
// empty claim. "Empty" is a statement about the data; neither case knows one.
func TestGraphPanelsFailClosed(t *testing.T) {
	cases := []struct {
		name   string
		prov   ProvenanceReader
		ev     EvidenceReader
		tab    string
		marker string
	}{
		{"provenance unwired", nil, stubEvidenceReader(), "?tab=provenance", `data-graph-unavailable="provenance"`},
		{"evidence unwired", stubProvenanceReader(), nil, "?tab=evidence", `data-graph-unavailable="evidence"`},
		{"provenance read failed", &stubProvenance{err: errors.New("projection down")}, stubEvidenceReader(), "?tab=provenance", `data-graph-unavailable="provenance"`},
		{"evidence read failed", stubProvenanceReader(), &stubEvidence{err: errors.New("assertions down")}, "?tab=evidence", `data-graph-unavailable="evidence"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := getGraphPage(t, tc.prov, tc.ev, tc.tab)
			if !strings.Contains(body, `data-graph-state="unavailable"`) || !strings.Contains(body, tc.marker) {
				t.Errorf("panel did not render the unavailable state")
			}
			for _, forbidden := range []string{
				"data-row-edge-id=", "data-graph-list=", "data-graph-diagram=", "data-graph-empty=",
			} {
				if strings.Contains(body, forbidden) {
					t.Errorf("unavailable panel carries %q — a failed read renders no rows at all", forbidden)
				}
			}
			if !strings.Contains(body, "could not be read") {
				t.Errorf("panel does not say the read failed")
			}
			// The object's own facts came from a read that succeeded, so the
			// page keeps its header — the failure is scoped to the graph.
			if !strings.Contains(body, `data-object-id="`+graphObjectID+`"`) {
				t.Errorf("the failed graph took the object header down with it")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Relation semantics
// ---------------------------------------------------------------------------

// TestRelationSemanticsAreTheCatalogsOwn: the meaning a row shows is the
// relation catalog's own one-line Semantics (docs/44, docs/19) — copied from
// the catalog, not restated here — and a type the catalog does not carry
// gets NO meaning rather than an invented one.
func TestRelationSemanticsAreTheCatalogsOwn(t *testing.T) {
	prov := provenancePanelFor(context.Background(), stubProvenanceReader(), rsgTestProjectID, graphObjectID, intPtr(2), provenance.WalkUpstream, stubTabHref)
	if len(prov.Rows) != 1 {
		t.Fatalf("provenance rows = %d, want 1", len(prov.Rows))
	}
	want, ok := relationcatalog.Lookup("derived_from")
	if !ok {
		t.Fatalf("the catalog no longer carries derived_from")
	}
	if prov.Rows[0].Semantics != want.Semantics || !prov.Rows[0].SemanticsKnown {
		t.Errorf("semantics = %q (known %v), want the catalog's %q", prov.Rows[0].Semantics, prov.Rows[0].SemanticsKnown, want.Semantics)
	}

	ev := evidencePanelFor(context.Background(), stubEvidenceReader(), rsgTestProjectID, graphObjectID, intPtr(2), stubTabHref)
	if len(ev.Rows) != 3 {
		t.Fatalf("evidence rows = %d, want 3", len(ev.Rows))
	}
	for _, row := range ev.Rows {
		entry, ok := relationcatalog.Lookup(row.Relation)
		if !ok {
			t.Fatalf("the catalog no longer carries %s", row.Relation)
		}
		if !row.SemanticsKnown || row.Semantics != entry.Semantics {
			t.Errorf("%s semantics = %q, want the catalog's %q", row.Relation, row.Semantics, entry.Semantics)
		}
		if !row.StanceKnown || row.Stance != string(stanceOf(row.Relation)) {
			t.Errorf("%s stance = %q (known %v), want the domain rule's %q", row.Relation, row.Stance, row.StanceKnown, stanceOf(row.Relation))
		}
	}

	// The fail-closed branch, which storage cannot reach (the nine evidence
	// relations are the only storable ones — T0506's integration suite pins
	// that): a relation the catalog does not define has no meaning and no
	// stance, and the page says so instead of guessing.
	if s, known := relationSemantics("not_a_catalog_relation"); known || s != "" {
		t.Errorf("relationSemantics(unknown) = %q, %v — want no semantics", s, known)
	}
	if tone := stanceTone(""); tone != "unlabeled" {
		t.Errorf("stanceTone(\"\") = %q, want the unlabeled tone", tone)
	}
}

func stanceOf(relation string) domain.EvidenceStance {
	stance, _ := domain.RelationStance(domain.EvidenceRelation(relation))
	return stance
}

// stubTabHref is the tab-href builder the panel functions take: the real page
// builds it from the request path, which these panel-level tests do not have.
func stubTabHref(tab, direction string) string {
	if direction == "" {
		return "/objects/x?tab=" + tab
	}
	return "/objects/x?tab=" + tab + "&direction=" + direction
}

// TestUnknownRelationRendersNoMeaning: the page-level branch for a relation
// outside the catalog — the semantics cell states that no meaning is
// recorded instead of repeating the raw type under a heading that promises
// its meaning (null and "no meaning recorded" must not be distinguishable
// from "the meaning is the type name").
func TestUnknownRelationRendersNoMeaning(t *testing.T) {
	var buf strings.Builder
	model := objectPageModel{
		Tab: graphEvidence,
		Evidence: evidencePanel{
			State:    graphStateOK,
			Lead:     evidenceLead,
			Boundary: evidenceBoundary,
			Rows: []evidenceRow{{
				AssertionID: "a1", Relation: "not_a_catalog_relation",
				EvidenceVersionID: "ever-1", EvidenceType: "experimental", ReviewState: "unreviewed",
			}},
		},
	}
	if err := objectDetailTemplate.Execute(&buf, model); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()
	if !strings.Contains(body, `data-row-semantics="absent"`) {
		t.Errorf("unknown relation did not render the absent semantics marker")
	}
	if !strings.Contains(body, "No meaning is recorded for this relation") {
		t.Errorf("unknown relation did not say that no meaning is recorded")
	}
	if strings.Contains(body, `<td class="semantics" data-row-semantics="absent">not_a_catalog_relation`) {
		t.Errorf("the raw type is presented as the meaning")
	}
	// No stance either: the domain rule has no answer, so the row says so and
	// is not folded into "neutral" (internal/evidence's fail-closed rule).
	if !strings.Contains(body, `data-row-stance-label="none"`) || !strings.Contains(body, "no stance label") {
		t.Errorf("a row with no derivable stance did not say so")
	}
}

// ---------------------------------------------------------------------------
// The list is the accessible form
// ---------------------------------------------------------------------------

var (
	edgeIDPattern = regexp.MustCompile(`data-edge-id="([^"]+)"`)
	rowIDPattern  = regexp.MustCompile(`data-row-edge-id="([^"]+)"`)
)

// TestGraphListEqualsDiagram: the SVG draws exactly the rows the table
// lists, by id, both ways. A drawn edge with no row is content a reader
// without the picture cannot reach; a row with no edge is a list that
// disagrees with the thing it claims to mirror.
func TestGraphListEqualsDiagram(t *testing.T) {
	cases := []struct {
		name string
		prov ProvenanceReader
		ev   EvidenceReader
		tab  string
	}{
		{"provenance", stubProvenanceReader(), stubEvidenceReader(), "?tab=provenance"},
		{"evidence", stubProvenanceReader(), stubEvidenceReader(), "?tab=evidence"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := getGraphPage(t, tc.prov, tc.ev, tc.tab)
			drawn := edgeIDPattern.FindAllStringSubmatch(body, -1)
			listed := rowIDPattern.FindAllStringSubmatch(body, -1)
			if len(drawn) == 0 || len(listed) == 0 {
				t.Fatalf("diagram edges = %d, list rows = %d — both must be non-empty", len(drawn), len(listed))
			}
			gotDrawn, gotListed := make([]string, 0, len(drawn)), make([]string, 0, len(listed))
			for _, m := range drawn {
				gotDrawn = append(gotDrawn, m[1])
			}
			for _, m := range listed {
				gotListed = append(gotListed, m[1])
			}
			sort.Strings(gotDrawn)
			sort.Strings(gotListed)
			if strings.Join(gotDrawn, ",") != strings.Join(gotListed, ",") {
				t.Errorf("diagram draws %v, list rows %v — the two must be the same edges", gotDrawn, gotListed)
			}
		})
	}
}

// TestGraphListIsAccessible: the SVG is hidden from assistive technology and
// the table beside it is the content — a caption, our own column headers with
// scope, and the state of every row as text in a cell.
func TestGraphListIsAccessible(t *testing.T) {
	provenanceTab := getGraphPage(t, stubProvenanceReader(), stubEvidenceReader(), "?tab=provenance")
	for _, want := range []string{
		`aria-hidden="true"`, `focusable="false"`, // the picture carries no content of its own
		`<table class="graph-list" data-graph-list="provenance">`,
		"<caption>", `<th scope="col">Relation</th>`, `<th scope="col">What the relation means</th>`,
		`<th scope="row">`, // each row names its own relation as its header
	} {
		if !strings.Contains(provenanceTab, want) {
			t.Errorf("provenance list lacks %q", want)
		}
	}
	if strings.Contains(provenanceTab, "<script") {
		t.Errorf("the page carries script — the list must be the page's own content, not a hydration of one")
	}

	evidenceTab := getGraphPage(t, stubProvenanceReader(), stubEvidenceReader(), "?tab=evidence")
	for _, want := range []string{
		`<table class="graph-list" data-graph-list="evidence">`,
		`<th scope="col">Stance</th>`,
		`data-row-stance-label="supporting"`, `data-row-stance-label="contesting"`, `data-row-stance-label="neutral"`,
		// The colour a stroke uses is named in words too: the legend says what
		// each stroke means, so the stance is never conveyed by colour alone.
		`data-graph-legend="evidence"`, "supporting — ", "contesting — ", "neutral — ", "no stance label — ",
	} {
		if !strings.Contains(evidenceTab, want) {
			t.Errorf("evidence list lacks %q", want)
		}
	}
	// Both stances are listed as their own rows, in the read's own order, and
	// never netted into one: the supporting row does not stand in for the
	// contesting one.
	support := strings.Index(evidenceTab, `data-row-edge-id="assert-support"`)
	contest := strings.Index(evidenceTab, `data-row-edge-id="assert-contest"`)
	if support < 0 || contest < 0 || support == contest {
		t.Fatalf("the supporting and contesting assertions are not two distinct rows")
	}
}

// TestGraphTabReadsOnlyItsOwnGraph: each tab reads its own graph and not the
// other's — the page costs the queries it renders, and a graph nobody asked
// for cannot leak onto the page it was not read for.
func TestGraphTabReadsOnlyItsOwnGraph(t *testing.T) {
	prov, ev := stubProvenanceReader(), stubEvidenceReader()
	getGraphPage(t, prov, ev, "?tab=provenance")
	if ev.calls != 0 {
		t.Errorf("the provenance tab read the evidence graph (%d calls)", ev.calls)
	}
	prov, ev = stubProvenanceReader(), stubEvidenceReader()
	getGraphPage(t, prov, ev, "?tab=evidence")
	if prov.calls != 0 {
		t.Errorf("the evidence tab read the provenance graph (%d calls)", prov.calls)
	}
	prov, ev = stubProvenanceReader(), stubEvidenceReader()
	getGraphPage(t, prov, ev, "")
	if prov.calls != 0 || ev.calls != 0 {
		t.Errorf("the metadata tab read a graph (provenance %d, evidence %d)", prov.calls, ev.calls)
	}
}

// ---------------------------------------------------------------------------
// The walk the picture and the list both come from
// ---------------------------------------------------------------------------

// TestProvenancePanelFollowsTheWalk: both the rows and the diagram come from
// T0505's walk, so the direction switch changes both together — upstream
// shows the origin, downstream shows nothing for an origin-side start (the
// page's version is nobody's dependent here).
func TestProvenancePanelFollowsTheWalk(t *testing.T) {
	up := provenancePanelFor(context.Background(), stubProvenanceReader(), rsgTestProjectID, graphObjectID, intPtr(2), provenance.WalkUpstream, stubTabHref)
	if up.Direction != "upstream" || !up.UpstreamCurrent || up.DownCurrent {
		t.Errorf("upstream panel = %q (up current %v, down current %v)", up.Direction, up.UpstreamCurrent, up.DownCurrent)
	}
	if len(up.Rows) != 1 || up.Rows[0].SourceLabel != "MOF-5 refined" || up.Rows[0].TargetLabel != "isotherm series" {
		t.Errorf("upstream rows = %+v, want the derived_from edge from the version to its origin", up.Rows)
	}
	if !strings.Contains(up.DirectionNote, "Origins") {
		t.Errorf("upstream note = %q", up.DirectionNote)
	}
	if strings.Contains(up.DownHref, "direction=") == false || !strings.Contains(up.UpstreamHref, "direction=upstream") {
		t.Errorf("direction hrefs = %q / %q", up.UpstreamHref, up.DownHref)
	}

	down := provenancePanelFor(context.Background(), stubProvenanceReader(), rsgTestProjectID, graphObjectID, intPtr(2), provenance.WalkDownstream, stubTabHref)
	if down.Direction != "downstream" || down.UpstreamCurrent || !down.DownCurrent {
		t.Errorf("downstream panel = %q (up current %v, down current %v)", down.Direction, down.UpstreamCurrent, down.DownCurrent)
	}
	if len(down.Rows) != 0 {
		t.Errorf("downstream rows = %+v, want none: nothing depends on this version", down.Rows)
	}
	if !strings.Contains(down.DirectionNote, "Dependents") {
		t.Errorf("downstream note = %q", down.DirectionNote)
	}
}

// TestGraphPanelIsDeterministic: the same read renders the same bytes, so a
// reviewer (and a reviewer's diff) can compare two runs of this page.
func TestGraphPanelIsDeterministic(t *testing.T) {
	first := getGraphPage(t, stubProvenanceReader(), stubEvidenceReader(), "?tab=evidence")
	second := getGraphPage(t, stubProvenanceReader(), stubEvidenceReader(), "?tab=evidence")
	if first != second {
		t.Errorf("two renders of the same read differ")
	}
}

// TestEmptyGraphSaysSo: a read that succeeded and found nothing renders the
// empty state and no diagram — the page does not draw an empty picture with
// no explanation, and it does not claim the graph is unavailable either.
func TestEmptyGraphSaysSo(t *testing.T) {
	empty := &stubProvenance{start: provenance.Node{ObjectID: graphObjectID, VersionID: graphVersionID, ObjectType: "material"}}
	body := getGraphPage(t, empty, stubEvidenceReader(), "?tab=provenance")
	if !strings.Contains(body, `data-graph-state="ok"`) || !strings.Contains(body, `data-graph-empty="provenance"`) {
		t.Errorf("an empty graph did not render the empty state")
	}
	if strings.Contains(body, "data-graph-diagram=") || strings.Contains(body, "data-row-edge-id=") {
		t.Errorf("an empty graph drew a picture or rows")
	}
	if !strings.Contains(body, "nothing is RECORDED") {
		t.Errorf("the empty state does not say what empty means")
	}
}

// TestEvidenceDiagramPointsEveryEdgeAtItsOwnTarget: the drawing model takes
// the read's OWN grouping rather than assuming one box. The page pins one
// version, so today's read answers one group — but if a read ever answered
// several, every assertion must still be drawn at the version it is about
// instead of stacking all of them on the first one. That is the difference
// between a picture of the answer and a picture of part of it.
func TestEvidenceDiagramPointsEveryEdgeAtItsOwnTarget(t *testing.T) {
	const (
		targetA = graphVersionID // ver-2, the page's version
		targetB = "ver-3"        // a second version of the same object
	)
	reader := &stubEvidence{read: evidence.ObjectEvidence{
		ProjectID: rsgTestProjectID,
		Object:    evidence.ObjectRef{ObjectID: graphObjectID, ObjectType: "material", Title: "MOF-5 refined"},
		Groups: []evidence.TargetGroup{
			{
				Target:     evidence.Target{ObjectVersionID: targetA, VersionNo: intPtr(2), Title: "MOF-5 refined"},
				Supporting: []evidence.Assertion{graphAssertion("assert-a", "supports", "ever-20")},
			},
			{
				Target:     evidence.Target{ObjectVersionID: targetB, VersionNo: intPtr(3), Title: "MOF-5 refined again"},
				Contesting: []evidence.Assertion{graphAssertion("assert-b", "contradicts", "ever-21")},
			},
		},
	}}
	panel := evidencePanelFor(context.Background(), reader, rsgTestProjectID, graphObjectID, nil,
		func(tab, direction string) string { return "" })
	if len(panel.Rows) != 2 {
		t.Fatalf("rows = %d, want 2 (both groups' assertions)", len(panel.Rows))
	}
	// The box each target version got, by the identity the diagram draws.
	boxOf := map[string]graphNode{}
	for _, n := range panel.Diagram.Nodes {
		if n.Kind == "target" {
			boxOf[n.ID] = n
		}
	}
	want := map[string]string{"assert-a": targetA, "assert-b": targetB}
	if len(panel.Diagram.Edges) != 2 {
		t.Fatalf("edges = %d, want 2", len(panel.Diagram.Edges))
	}
	for _, e := range panel.Diagram.Edges {
		target, ok := want[e.ID]
		if !ok {
			t.Fatalf("the diagram drew an edge for an assertion no group carries: %s", e.ID)
		}
		box, ok := boxOf[target]
		if !ok {
			t.Fatalf("no target box for %s (boxes: %v)", target, boxOf)
		}
		if e.X2 != box.X || e.Y2 != box.Y+box.H/2 {
			t.Errorf("edge %s ends at (%d,%d) — want the %s box's left-middle (%d,%d)",
				e.ID, e.X2, e.Y2, target, box.X, box.Y+box.H/2)
		}
	}
}

// TestObjectDetailTemplateParsesEveryBranch: the template is parsed once at
// init, so a broken branch is a build-time failure — this asserts the whole
// tab set renders rather than trusting that.
func TestObjectDetailTemplateParsesEveryBranch(t *testing.T) {
	for _, tab := range []string{"metadata", "relations", graphProvenance, graphEvidence, "history", "files", "bogus"} {
		var buf strings.Builder
		model := objectPageModel{Tab: tab, Title: "t", ObjectType: "material"}
		if err := objectDetailTemplate.Execute(&buf, model); err != nil {
			t.Errorf("tab %q: %v", tab, err)
		}
	}
	if objectDetailTemplate.Lookup("graphDiagram") == nil {
		t.Errorf("the diagram template is not defined")
	}
}
