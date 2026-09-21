package rsghttp

import (
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/application/rsg"
)

// The Research Map tests (T1102). docs/06 §5 is the acceptance criterion and
// these tests are its negatives first: the default view of a project of ANY
// size must draw a bounded number of boxes (never one node per object — the
// 蜘蛛网 the milestone forbids), nothing may be drawn below a node that was
// not selected, a bounded column must say what it aggregated, and the picture
// must be exactly the table beside it (same identities, same relations).
//
// Two of them matter more than the rest and are deliberately written as
// whole-outline properties rather than spot checks:
//
//   - TestResearchMapE2EPerf drives the REAL HTTP surface at 1000-object
//     scale: coarse by default, drill-down by following the page's own links,
//     and the render inside docs/27's p95 < 1s budget.
//   - TestResearchMapNodeBudgetAtScale asserts the ceiling for every line of
//     a 1000-object outline, at every depth — a budget that holds only for the
//     outline the test happens to be shaped around is not a budget.

const mapTestBase = researchBase

func mapTestLinks() researchMapLinks {
	return researchMapLinks{basePath: mapTestBase, projectID: rsgTestProjectID}
}

func mapTestMap(t *testing.T, out rsg.ResearchOutline, view string, path ...string) researchMap {
	t.Helper()
	if view == "" {
		view = researchViewQuestions
	}
	return buildResearchMap(out, view, path, mapTestLinks())
}

// mapScaleOutline builds an outline of a chosen size, shaped like a real
// project's state rather than like a graph: every root question carries a
// sub-question, hypotheses, findings and other objects that address it, and
// every finding pins claim versions. rounds is the number of root questions;
// the returned objectCount is how many objects the underlying state holds.
func mapScaleOutline(rounds, subs, hyps, finds, others, claimsPerFinding int) (out rsg.ResearchOutline, objectCount int) {
	out = rsg.ResearchOutline{ProjectID: rsgTestProjectID, ProjectSlug: "scale", ProjectName: "Scale"}
	counts := rsg.OutlineCounts{}
	for i := 0; i < rounds; i++ {
		q := rsg.OutlineQuestion{
			ObjectID:      fmt.Sprintf("q-%03d", i),
			VersionID:     fmt.Sprintf("q-%03d-v1", i),
			BranchID:      "branch-1",
			Title:         fmt.Sprintf("Question %03d: does the material adsorb CO2?", i),
			Statement:     "Does it, and how much?",
			QuestionState: "open",
		}
		counts.Questions++
		objectCount++
		for c := 0; c < subs; c++ {
			q.Children = append(q.Children, rsg.OutlineQuestion{
				ObjectID:      fmt.Sprintf("q-%03d-s%d", i, c),
				VersionID:     fmt.Sprintf("q-%03d-s%d-v1", i, c),
				BranchID:      "branch-1",
				Title:         fmt.Sprintf("Sub-question %03d.%d: at which pressure?", i, c),
				QuestionState: "partially_answered",
			})
			counts.Questions++
			objectCount++
		}
		for h := 0; h < hyps; h++ {
			q.Hypotheses = append(q.Hypotheses, rsg.OutlineObjectRef{
				ObjectID: fmt.Sprintf("h-%03d-%d", i, h), ObjectType: "hypothesis",
				Title:    fmt.Sprintf("Uptake scales with pressure (%03d.%d)", i, h),
				BranchID: "branch-1",
			})
			counts.Hypotheses++
			objectCount++
		}
		for o := 0; o < others; o++ {
			q.OtherObjects = append(q.OtherObjects, rsg.OutlineObjectRef{
				ObjectID: fmt.Sprintf("m-%03d-%d", i, o), ObjectType: "material",
				Title:    fmt.Sprintf("MOF-%03d-%d", i, o),
				BranchID: "branch-1",
			})
			counts.OtherObjects++
			objectCount++
		}
		for f := 0; f < finds; f++ {
			id := fmt.Sprintf("f-%03d-%d", i, f)
			title := fmt.Sprintf("Uptake peaks at %d bar (%03d.%d)", 10*f+5, i, f)
			q.Findings = append(q.Findings, rsg.OutlineObjectRef{
				ObjectID: id, ObjectType: "finding", Title: title, BranchID: "branch-1",
			})
			counts.Findings++
			objectCount++
			finding := rsg.OutlineFinding{
				ObjectID: id, VersionID: id + "-v1", BranchID: "branch-1",
				Title: title, Statement: "Peaks there.", FindingType: "trend", Assessment: "accepted",
				Questions: []rsg.OutlineObjectRef{{ObjectID: q.ObjectID, ObjectType: "research_question", Title: q.Title, BranchID: "branch-1"}},
			}
			for c := 0; c < claimsPerFinding; c++ {
				cid := fmt.Sprintf("c-%03d-%d-%d", i, f, c)
				finding.Claims = append(finding.Claims, rsg.OutlineClaimRef{
					ObjectID: cid, VersionID: cid + "-v1", BranchID: "branch-1",
					Title: fmt.Sprintf("Capacity is %d mmol/g", c+1), Resolved: true,
				})
				counts.Claims++
				objectCount++
			}
			out.Findings = append(out.Findings, finding)
		}
		out.Questions = append(out.Questions, q)
	}
	out.Counts = counts
	return out, objectCount
}

// mapNodesOf collects a column by index.
func mapNodesOf(m researchMap, col int) []researchMapNode {
	var nodes []researchMapNode
	for _, n := range m.Nodes {
		if n.Col == col {
			nodes = append(nodes, n)
		}
	}
	return nodes
}

func mapKinds(m researchMap) map[string]int {
	kinds := map[string]int{}
	for _, n := range m.Nodes {
		kinds[n.Kind]++
	}
	return kinds
}

// TestResearchMapDefaultIsCoarse: opening the map selects nothing, so it draws
// the roots and NOTHING else — no buckets, no members, no edges. This is the
// 默认粗粒度 half of docs/06 §5 that a 蜘蛛网 would fail first.
func TestResearchMapDefaultIsCoarse(t *testing.T) {
	m := mapTestMap(t, cannedOutline(), "")
	if len(m.Columns) != 1 {
		t.Fatalf("default draw columns = %d, want 1 (nothing below a node until it is selected)", len(m.Columns))
	}
	if len(m.Nodes) != 1 {
		t.Fatalf("default draw nodes = %d, want 1 (the project's one root question): %+v", len(m.Nodes), m.Nodes)
	}
	if got := mapKinds(m); got[mapKindBucket] != 0 || got[mapKindRemainder] != 0 {
		t.Errorf("default draw kinds = %v, want roots only", got)
	}
	if len(m.Edges) != 0 {
		t.Errorf("default draw edges = %d, want 0", len(m.Edges))
	}
	if len(m.Path) != 0 {
		t.Errorf("default draw path = %v, want empty", m.Path)
	}
	if !m.Detail.Empty {
		t.Errorf("default panel is not the empty state: %+v", m.Detail)
	}
	if !strings.Contains(m.Detail.EmptyNote, "No node selected") {
		t.Errorf("empty panel note = %q, want the how-to-select instruction", m.Detail.EmptyNote)
	}
	// The root is drawn, focusable and linked both ways.
	n := m.Nodes[0]
	if n.Elem != "q-1" || n.Kind != mapKindQuestion || n.Count != 1 {
		t.Errorf("root node = %+v, want the q-1 question", n)
	}
	if n.Href != mapTestBase+"?path=q-1&view=questions" {
		t.Errorf("root href = %q, want the drill-down coordinate", n.Href)
	}
	if n.DetailHref != objectHref(rsgTestProjectID, "branch-1", "q-1") {
		t.Errorf("root detail href = %q", n.DetailHref)
	}
}

// TestResearchMapColumnBudgetAggregatesTheRest: a column draws at most
// researchMapColumnBudget peers and gives the rest ONE remainder node carrying
// their count — aggregation, never truncation (invariant 8: nothing
// disappears; the list beside the map still has every row).
func TestResearchMapColumnBudgetAggregatesTheRest(t *testing.T) {
	const rounds = 40
	out, _ := mapScaleOutline(rounds, 1, 2, 3, 1, 3)
	m := mapTestMap(t, out, "")

	col0 := mapNodesOf(m, 0)
	if len(col0) != researchMapColumnBudget+1 {
		t.Fatalf("column 0 nodes = %d, want %d peers + 1 remainder", len(col0), researchMapColumnBudget)
	}
	last := col0[len(col0)-1]
	if last.Kind != mapKindRemainder || last.Elem != "more" {
		t.Fatalf("last column-0 node = %+v, want the remainder node", last)
	}
	wantHidden := rounds - researchMapColumnBudget
	if last.Count != wantHidden || last.Remaining != wantHidden {
		t.Errorf("remainder count = %d / remaining = %d, want %d", last.Count, last.Remaining, wantHidden)
	}
	if last.Label != pluralCount(wantHidden, "question") {
		t.Errorf("remainder label = %q, want %q", last.Label, pluralCount(wantHidden, "question"))
	}
	// The remainder is not a drill-down: there is no second page of a bounded
	// picture, so it sends the reader to the list that has every row instead.
	if last.Href != researchMapListAnchor {
		t.Errorf("remainder href = %q, want %q", last.Href, researchMapListAnchor)
	}
	if last.focusable() {
		t.Errorf("remainder node is focusable — a ?path= coordinate would be a dead end")
	}
	// The peers that ARE drawn keep their own drill-down coordinates.
	for _, n := range col0[:researchMapColumnBudget] {
		if !strings.Contains(n.Href, "path=") {
			t.Errorf("drawn root %q has no drill-down href: %q", n.Elem, n.Href)
		}
	}
	// The map says what it did: a budget that truncates silently is a lie by
	// omission.
	if want := pluralCount(wantHidden, "row") + " aggregated into a remainder node"; !strings.Contains(m.Note, want) {
		t.Errorf("map note = %q, want it to carry %q", m.Note, want)
	}
	// And the remainder's row explains where the missing rows are.
	row := m.Rows[len(m.Rows)-1]
	if !strings.Contains(row.RemainingNote, "the outline list has them all") {
		t.Errorf("remainder row note = %q", row.RemainingNote)
	}
}

// TestResearchMapDrillDownAddsOneLayerAtATime: selecting a root draws its
// rollup — one bucket per role, in docs/06 §5's own order, each carrying its
// row count — and connects the selected node to it.
func TestResearchMapDrillDownAddsOneLayerAtATime(t *testing.T) {
	m := mapTestMap(t, cannedOutline(), "", "q-1")
	if len(m.Columns) != 2 {
		t.Fatalf("columns = %d, want 2 (roots + the selected node's rollup)", len(m.Columns))
	}
	buckets := mapNodesOf(m, 1)
	wantOrder := []struct {
		role  string
		label string
		count int
	}{
		{mapRoleHypothesis, "Hypotheses", 1},
		{mapRoleFinding, "Findings", 1},
		{mapRoleAddresses, "Other objects", 1},
		{mapRoleSubQuestion, "Sub-questions", 1},
	}
	if len(buckets) != len(wantOrder) {
		t.Fatalf("buckets = %d, want %d: %+v", len(buckets), len(wantOrder), buckets)
	}
	for i, want := range wantOrder {
		b := buckets[i]
		if b.Role != want.role || b.Label != want.label {
			t.Errorf("bucket %d = (%s, %s), want (%s, %s)", i, b.Role, b.Label, want.role, want.label)
		}
		if b.Count != want.count {
			t.Errorf("bucket %s count = %d, want %d", b.Role, b.Count, want.count)
		}
		if b.Type != pluralCount(want.count, "row") {
			t.Errorf("bucket %s type = %q, want the row count %q", b.Role, b.Type, pluralCount(want.count, "row"))
		}
		if b.Href != mapTestBase+"?path=q-1%2C"+b.Role+"&view=questions" {
			t.Errorf("bucket %s href = %q", b.Role, b.Href)
		}
	}
	// Every arrow is the drill path: the selected root to each bucket, and
	// each arrow's role is the target row's own role.
	if len(m.Edges) != len(buckets) {
		t.Fatalf("edges = %d, want %d", len(m.Edges), len(buckets))
	}
	for _, e := range m.Edges {
		if e.From != researchMapKey(0, "q-1") {
			t.Errorf("edge %s → %s does not start at the selected root", e.From, e.To)
		}
		var target researchMapNode
		for _, n := range buckets {
			if n.Key == e.To {
				target = n
			}
		}
		if target.Key == "" {
			t.Fatalf("edge points at %q, which is not a drawn node", e.To)
		}
		if e.Role != target.Role {
			t.Errorf("edge role = %q, want the target row's role %q", e.Role, target.Role)
		}
	}
	// The panel lists the same rollup, with the same coordinates.
	if len(m.Detail.Buckets) != len(wantOrder) {
		t.Fatalf("panel buckets = %d, want %d", len(m.Detail.Buckets), len(wantOrder))
	}
	for i, b := range m.Detail.Buckets {
		if b.Role != wantOrder[i].role || b.Href != buckets[i].Href {
			t.Errorf("panel bucket %d = %+v, want role %s and href %s", i, b, wantOrder[i].role, buckets[i].Href)
		}
	}
	if m.Detail.Label != "Does the material adsorb CO2?" || m.Detail.Kind != mapKindQuestion {
		t.Errorf("panel head = %+v, want the selected question", m.Detail)
	}
	if m.Detail.DetailHref == "" {
		t.Errorf("panel has no object detail link for a node that has one")
	}
	if len(m.Crumbs) != 2 || !m.Crumbs[1].Current {
		t.Errorf("crumbs = %+v, want [Research map, the selected question (current)]", m.Crumbs)
	}
}

// TestResearchMapBucketOrderIsFixedNotBySize: bucket order is the docs' own
// line, never an order by count. Ranking a project's groups by how many rows
// they hold is arithmetic the domain refuses (CLAUDE.md §9.13), and a bucket
// list that reordered itself by size would be a de facto ranking.
func TestResearchMapBucketOrderIsFixedNotBySize(t *testing.T) {
	out, _ := mapScaleOutline(1, 0, 1, 9, 2, 1)
	m := mapTestMap(t, out, "", "q-000")
	got := []string{}
	for _, b := range mapNodesOf(m, 1) {
		got = append(got, b.Role)
	}
	want := []string{mapRoleHypothesis, mapRoleFinding, mapRoleAddresses}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("bucket order = %v, want the fixed order %v (findings outnumber hypotheses 9:1 and must still follow them)", got, want)
	}
}

// TestResearchMapMemberBudgetAggregatesTheRest: the leaf column obeys the same
// budget as the roots, and its rows are the objects themselves — each with its
// own object detail link, which is where its provenance and evidence live.
func TestResearchMapMemberBudgetAggregatesTheRest(t *testing.T) {
	out, _ := mapScaleOutline(1, 0, 0, 9, 0, 1)
	m := mapTestMap(t, out, "", "q-000", mapRoleFinding)
	col := mapNodesOf(m, 2)
	if len(col) != researchMapColumnBudget+1 {
		t.Fatalf("member column = %d nodes, want %d + 1 remainder", len(col), researchMapColumnBudget)
	}
	for _, n := range col[:researchMapColumnBudget] {
		if n.Kind != "finding" {
			t.Errorf("member %q kind = %q, want the object type", n.Elem, n.Kind)
		}
		if !strings.HasPrefix(n.DetailHref, "/api/v1/projects/"+rsgTestProjectID+"/branches/branch-1/objects/") {
			t.Errorf("member %q has no object detail link: %q", n.Elem, n.DetailHref)
		}
		if !strings.Contains(n.Href, "path=q-000%2Cfinding%2C") {
			t.Errorf("member %q href = %q, want the member coordinate", n.Elem, n.Href)
		}
	}
	last := col[len(col)-1]
	if last.Kind != mapKindRemainder || last.Count != 1 || last.Label != "1 row" {
		t.Errorf("member remainder = %+v, want one aggregated row", last)
	}
}

// TestResearchMapDepthIsThree: the map answers three steps and then hands off
// to the object page (docs/06 §5: drill-down 到 Claim/Evidence/Object). A
// coordinate deeper than that is not a fourth column — it is the same map.
func TestResearchMapDepthIsThree(t *testing.T) {
	out, _ := mapScaleOutline(1, 0, 0, 3, 0, 1)
	deep := mapTestMap(t, out, "", "q-000", mapRoleFinding, "f-000-0", "one-more")
	if len(deep.Columns) > researchMapDepth {
		t.Errorf("columns = %d, want at most %d", len(deep.Columns), researchMapDepth)
	}
	for _, n := range deep.Nodes {
		if n.Col >= researchMapDepth {
			t.Errorf("drew a node in column %d: %+v", n.Col, n)
		}
	}
	// The ?path= parser caps the coordinate at the same depth.
	req := httptest.NewRequest("GET", mapTestBase+"?path=a,b,c,d,e", nil)
	if got := researchMapPath(req); len(got) != researchMapDepth {
		t.Errorf("researchMapPath = %v, want %d elements", got, researchMapDepth)
	}
	// An empty or blank parameter is the default coordinate, not an error.
	for _, raw := range []string{"", "?", "?path=", "?path=%20"} {
		req := httptest.NewRequest("GET", mapTestBase+raw, nil)
		if got := researchMapPath(req); len(got) != 0 {
			t.Errorf("researchMapPath(%q) = %v, want empty", raw, got)
		}
	}
}

// TestResearchMapPathResolvesLongestValidPrefix: a coordinate that names
// nothing focusable ends there and the map falls back to the deepest level it
// can prove — never an empty frame, never an error page. A remainder node
// cannot be selected at all.
func TestResearchMapPathResolvesLongestValidPrefix(t *testing.T) {
	out, _ := mapScaleOutline(40, 0, 1, 2, 0, 1)
	cases := []struct {
		path []string
		want []string
	}{
		{nil, nil},
		{[]string{"nope"}, nil},
		{[]string{"q-000"}, []string{"q-000"}},
		{[]string{"q-000", "nope"}, []string{"q-000"}},
		{[]string{"q-000", mapRoleFinding}, []string{"q-000", mapRoleFinding}},
		{[]string{"q-000", mapRoleFinding, "f-000-0"}, []string{"q-000", mapRoleFinding, "f-000-0"}},
		{[]string{"q-000", mapRoleFinding, "nope"}, []string{"q-000", mapRoleFinding}},
		// "more" names the remainder node of column 0, which is not focusable.
		{[]string{"more"}, nil},
	}
	for _, tc := range cases {
		m := mapTestMap(t, out, "", tc.path...)
		if strings.Join(m.Path, ",") != strings.Join(tc.want, ",") {
			t.Errorf("path %v resolved to %v, want %v", tc.path, m.Path, tc.want)
		}
	}
}

// TestResearchMapFindingsView: the second view opens on the findings axis and
// drills into what a finding stands on — its claims and the questions it
// addresses (provenance is not evidence, §9.10: the claims are the finding's
// own pinned references, the addressed questions are its relations).
func TestResearchMapFindingsView(t *testing.T) {
	out := cannedOutline()
	m := mapTestMap(t, out, researchViewFindings)
	if len(m.Nodes) != 1 || m.Nodes[0].Kind != mapKindFinding || m.Nodes[0].Elem != "f-1" {
		t.Fatalf("findings view roots = %+v, want the project's one finding", m.Nodes)
	}
	if !strings.Contains(m.Nodes[0].Href, "view="+researchViewFindings) {
		t.Errorf("root href = %q, want the view kept in the coordinate", m.Nodes[0].Href)
	}
	drilled := mapTestMap(t, out, researchViewFindings, "f-1")
	got := []string{}
	for _, b := range mapNodesOf(drilled, 1) {
		got = append(got, b.Role)
	}
	want := []string{mapRoleClaim, mapRoleAddresses}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("finding buckets = %v, want %v", got, want)
	}
	// The claim bucket's rows are the finding's pinned claim versions.
	claims := mapTestMap(t, out, researchViewFindings, "f-1", mapRoleClaim)
	claimRows := mapNodesOf(claims, 2)
	if len(claimRows) != 2 {
		t.Fatalf("claim rows = %d, want the finding's 2 pinned claims: %+v", len(claimRows), claimRows)
	}
	pinned := false
	for _, n := range claimRows {
		if n.Elem == "c-ext-v9" {
			pinned = true
			if n.DetailHref != "" {
				t.Errorf("a claim the project cannot resolve got an object link: %q", n.DetailHref)
			}
			if n.Label != "c-ext-v9" {
				t.Errorf("pinned claim label = %q, want its pinned version id", n.Label)
			}
		}
	}
	if !pinned {
		t.Errorf("the externally pinned claim is missing from the claim column: %+v", claimRows)
	}
}

// TestResearchMapNodeBudgetAtScale: the ceiling is a property of the map, not
// of the outline it was handed. Every drawn node of a 1000-object outline, at
// every depth, in both views, is counted against it — and the object count is
// asserted too, so the test cannot pass by measuring an outline that is not
// actually big.
func TestResearchMapNodeBudgetAtScale(t *testing.T) {
	out, objects := mapScaleOutline(60, 1, 2, 3, 1, 3)
	if objects < 1000 {
		t.Fatalf("fixture holds %d objects, want at least 1000 — a budget proven on a small outline proves nothing", objects)
	}
	root := out.Questions[0].ObjectID
	coordinates := [][]string{
		nil,
		{root},
		{root, mapRoleFinding},
		{root, mapRoleSubQuestion},
	}
	views := []string{researchViewQuestions, researchViewFindings}
	for _, view := range views {
		for _, path := range coordinates {
			m := buildResearchMap(out, view, path, mapTestLinks())
			if len(m.Nodes) > researchMapNodeBudget {
				t.Errorf("view %s path %v drew %d nodes, ceiling %d", view, path, len(m.Nodes), researchMapNodeBudget)
			}
			if len(m.Nodes) != len(m.Rows) {
				t.Errorf("view %s path %v: %d nodes but %d rows", view, path, len(m.Nodes), len(m.Rows))
			}
			if len(m.Edges) > researchMapEdgeBudget {
				t.Errorf("view %s path %v drew %d arrows, ceiling %d", view, path, len(m.Edges), researchMapEdgeBudget)
			}
			for _, col := range m.Columns {
				if len(col) > researchMapColumnBudget+1 {
					t.Errorf("view %s path %v: a column drew %d boxes", view, path, len(col))
				}
			}
		}
	}
}

// TestResearchMapEdgeBudgetIsReached: the arrow ceiling is a property of the
// code, so it has to be reachable — a bound nothing can reach proves nothing
// and hides a wrong number. One question with all four roles and one bucket
// with more rows than a column draws is exactly the widest drill the map
// answers, and it draws the whole budget.
func TestResearchMapEdgeBudgetIsReached(t *testing.T) {
	out, _ := mapScaleOutline(1, 1, 1, researchMapColumnBudget+4, 1, 1)
	root := out.Questions[0].ObjectID
	m := mapTestMap(t, out, "", root, mapRoleFinding)
	if got := len(m.Edges); got != researchMapEdgeBudget {
		t.Errorf("arrows = %d, want the full budget %d: the widest drill is %d buckets plus %d drawn rows and their remainder",
			got, researchMapEdgeBudget, researchMapBucketBudget, researchMapColumnBudget)
	}
}

// TestResearchMapRowsMirrorNodes: the picture carries no content of its own.
// Every drawn node is a row (same identity), every arrow names a row that
// exists and repeats that row's relation, and the arrow set is exactly the
// drill path from one selected node to the next column.
func TestResearchMapRowsMirrorNodes(t *testing.T) {
	out, _ := mapScaleOutline(3, 0, 2, 3, 0, 1)
	m := mapTestMap(t, out, "", out.Questions[0].ObjectID, mapRoleFinding)

	byKey := map[string]researchMapRow{}
	for _, r := range m.Rows {
		byKey[r.Key] = r
	}
	if len(byKey) != len(m.Nodes) || len(m.Rows) != len(m.Nodes) {
		t.Fatalf("nodes %d, rows %d, distinct row keys %d — a row without a picture or a picture without a row",
			len(m.Nodes), len(m.Rows), len(byKey))
	}
	for _, n := range m.Nodes {
		row, ok := byKey[n.Key]
		if !ok {
			t.Errorf("drawn node %q has no table row", n.Key)
			continue
		}
		if row.Label != n.Label || row.Kind != n.Kind || row.Href != n.Href {
			t.Errorf("row %q = %+v, node = %+v — the row must be the node", n.Key, row, n)
		}
	}
	edges := map[string]bool{}
	for _, e := range m.Edges {
		edges[e.To] = true
		row, ok := byKey[e.To]
		if !ok {
			t.Errorf("arrow points at %q, which is not a row", e.To)
			continue
		}
		if e.Role != row.Role {
			t.Errorf("arrow to %q has role %q, row says %q", e.To, e.Role, row.Role)
		}
	}
	// Nothing is connected that was not selected-into: every node outside
	// column 0 hangs off the selected node of the column before it, and the
	// edge count is the target column's size.
	for col := 1; col < len(m.Columns); col++ {
		for _, n := range mapNodesOf(m, col) {
			if !edges[n.Key] {
				t.Errorf("node %q in column %d has no arrow — the picture would show it as unrelated", n.Key, col)
			}
		}
	}
	if want := len(mapNodesOf(m, 1)) + len(mapNodesOf(m, 2)); len(m.Edges) != want {
		t.Errorf("arrows = %d, want one per node outside column 0 = %d", len(m.Edges), want)
	}
}

// TestResearchMapLabelIsFullInTheTable: truncation is a property of the
// illustration, never of the content. A 300-character title is drawn short and
// listed whole.
func TestResearchMapLabelIsFullInTheTable(t *testing.T) {
	long := strings.Repeat("吸附容量随压力上升", 40)
	out := rsg.ResearchOutline{
		ProjectID: rsgTestProjectID,
		Questions: []rsg.OutlineQuestion{{
			ObjectID: "q-long", VersionID: "q-long-v1", BranchID: "branch-1",
			Title: long, QuestionState: "open",
		}},
	}
	m := mapTestMap(t, out, "")
	if m.Nodes[0].Label != long {
		t.Errorf("node label was truncated: %q", m.Nodes[0].Label)
	}
	if got := m.Nodes[0].ShortLabel(); got == long {
		t.Errorf("the diagram drew the full %d-rune title", len([]rune(long)))
	} else if len([]rune(got)) != researchMapLabelRunes {
		t.Errorf("short label = %d runes, want %d", len([]rune(got)), researchMapLabelRunes)
	}
	if m.Rows[0].Label != long {
		t.Errorf("the table row truncated the title: %q", m.Rows[0].Label)
	}
}

// TestResearchMapEmptyProject: an empty project gets an explanation and no
// picture — never an empty frame, never a fabricated node.
func TestResearchMapEmptyProject(t *testing.T) {
	m := mapTestMap(t, rsg.ResearchOutline{ProjectID: rsgTestProjectID}, "")
	if len(m.Nodes) != 0 || len(m.Rows) != 0 || len(m.Edges) != 0 || len(m.Columns) != 0 {
		t.Fatalf("empty project drew %d nodes, %d rows, %d edges", len(m.Nodes), len(m.Rows), len(m.Edges))
	}
	if !strings.Contains(m.Empty, "No research questions yet") {
		t.Errorf("empty note = %q", m.Empty)
	}
	if m.Note != "0 nodes drawn · the outline list beside the map has every row." {
		t.Errorf("empty map note = %q, want it to say it drew nothing", m.Note)
	}
	findings := mapTestMap(t, rsg.ResearchOutline{ProjectID: rsgTestProjectID}, researchViewFindings)
	if !strings.Contains(findings.Empty, "No findings yet") {
		t.Errorf("findings empty note = %q", findings.Empty)
	}
}

// TestResearchMapUnknownViewFallsBack: a navigation is never an error. An
// unknown ?view= draws the default axis, and the map says which axis it drew.
func TestResearchMapUnknownViewFallsBack(t *testing.T) {
	req := httptest.NewRequest("GET", mapTestBase+"?view=by-anything", nil)
	if got := researchView(req); got != researchViewQuestions {
		t.Errorf("researchView = %q, want %q", got, researchViewQuestions)
	}
	m := mapTestMap(t, cannedOutline(), "by-anything")
	if len(m.Nodes) != 1 || m.Nodes[0].Kind != mapKindQuestion {
		t.Errorf("unknown view drew %+v, want the question axis", m.Nodes)
	}
}

// ---------------------------------------------------------------------------
// The page: the map as the reader actually receives it
// ---------------------------------------------------------------------------

// mapAttrValues collects the values of one data attribute in the rendered body.
func mapAttrValues(body, attr string) []string {
	var values []string
	needle := attr + `="`
	rest := body
	for {
		i := strings.Index(rest, needle)
		if i < 0 {
			return values
		}
		rest = rest[i+len(needle):]
		j := strings.Index(rest, `"`)
		if j < 0 {
			return values
		}
		values = append(values, rest[:j])
		rest = rest[j:]
	}
}

// TestResearchPageMapIsTheTable: over the HTTP surface, the page's picture and
// its table carry the same identities — data-map-node values are exactly the
// data-map-row values, the SVG is aria-hidden, and every arrow names a drawn
// row and repeats that row's relation.
func TestResearchPageMapIsTheTable(t *testing.T) {
	stub := &stubService{outline: cannedOutline()}
	resp := getResearchPage(t, stub, researchBase+"?path=q-1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (body %s)", resp.StatusCode, respBody(resp))
	}
	body := respBody(resp)
	for _, want := range []string{
		`data-map-table="research"`,
		`data-map-diagram="research-map"`,
		`role="presentation" aria-hidden="true"`,
		`data-map-detail="selected"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	nodes := mapAttrValues(body, "data-map-node")
	rows := mapAttrValues(body, "data-map-row")
	if len(nodes) == 0 {
		t.Fatalf("the page drew no map nodes")
	}
	rowSet := map[string]bool{}
	for _, r := range rows {
		rowSet[r] = true
	}
	if len(nodes) != len(rows) {
		t.Errorf("%d drawn nodes but %d table rows: %v vs %v", len(nodes), len(rows), nodes, rows)
	}
	for _, n := range nodes {
		if !rowSet[n] {
			t.Errorf("drawn node %q has no table row of the same identity", n)
		}
	}
	// Every arrow repeats a row's relation and points at a drawn row.
	edgeRoles := mapAttrValues(body, "data-map-edge-role")
	edgeTos := mapAttrValues(body, "data-map-edge-to")
	if len(edgeRoles) != len(edgeTos) || len(edgeTos) == 0 {
		t.Fatalf("arrows = %d roles / %d targets, want one role each and at least one arrow", len(edgeTos), len(edgeRoles))
	}
	for i, to := range edgeTos {
		if !rowSet[to] {
			t.Errorf("arrow %d points at %q, which has no row", i, to)
		}
		if !strings.Contains(body, `data-map-row="`+to+`" data-map-row-kind=`) {
			t.Errorf("arrow %d target %q is not a rendered row", i, to)
		}
	}
}

// mapDeepOutline is the fixture the panel-link test needs: a project in which
// the deepest column holds a node that has something below it. A root question
// carries a finding; its sub-question carries a finding and a material of its
// own, so the sub-question's page has a rollup to list while the map's depth is
// already spent. That is the shape in which a panel link must either reach a
// drawn node or stop being a link.
func mapDeepOutline() rsg.ResearchOutline {
	child := rsg.OutlineQuestion{
		ObjectID: "q-2", VersionID: "q-2-v1", BranchID: "branch-1",
		Title: "At what pressure?", QuestionState: "partially_answered",
		Findings:     []rsg.OutlineObjectRef{{ObjectID: "f-2", ObjectType: "finding", Title: "Uptake peaks at 40 bar", BranchID: "branch-1"}},
		OtherObjects: []rsg.OutlineObjectRef{{ObjectID: "m-2", ObjectType: "material", Title: "MOF-5", BranchID: "branch-1"}},
	}
	root := rsg.OutlineQuestion{
		ObjectID: "q-1", VersionID: "q-1-v1", BranchID: "branch-1",
		Title: "Does the material adsorb CO2?", Statement: "Does it, under pressure?",
		QuestionState: "open",
		Findings:      []rsg.OutlineObjectRef{{ObjectID: "f-1", ObjectType: "finding", Title: "Uptake peaks at 30 bar", BranchID: "branch-1"}},
		Children:      []rsg.OutlineQuestion{child},
	}
	return rsg.ResearchOutline{
		ProjectID:   rsgTestProjectID,
		ProjectSlug: "rsg-project",
		ProjectName: "RSG Project",
		Questions:   []rsg.OutlineQuestion{root},
		Findings: []rsg.OutlineFinding{{
			ObjectID: "f-1", VersionID: "f-1-v1", BranchID: "branch-1",
			Title: "Uptake peaks at 30 bar", Statement: "Peaks at 30 bar.",
			FindingType: "trend", Assessment: "accepted",
			Claims:    []rsg.OutlineClaimRef{{ObjectID: "c-1", VersionID: "c-1-v1", Title: "Capacity is 2 mmol/g", BranchID: "branch-1", Resolved: true}},
			Questions: []rsg.OutlineObjectRef{{ObjectID: "q-1", ObjectType: "research_question", Title: root.Title, BranchID: "branch-1"}},
		}},
		Counts: rsg.OutlineCounts{Questions: 2, Findings: 1, Claims: 1, OtherObjects: 1},
	}
}

// mapPanelHrefs collects every href the selected-node panel renders — the
// links a reader has to work with once a node is selected.
func mapPanelHrefs(t *testing.T, page string) []string {
	t.Helper()
	const open = `data-map-detail="selected"`
	i := strings.Index(page, open)
	if i < 0 {
		t.Fatalf("the page has no selected-node panel")
	}
	panel := page[i:]
	if j := strings.Index(panel, `</aside>`); j >= 0 {
		panel = panel[:j]
	}
	var hrefs []string
	rest := panel
	for {
		k := strings.Index(rest, `href="`)
		if k < 0 {
			return hrefs
		}
		rest = rest[k+len(`href="`):]
		j := strings.Index(rest, `"`)
		if j < 0 {
			t.Fatalf("truncated href in the panel")
		}
		hrefs = append(hrefs, html.UnescapeString(rest[:j]))
		rest = rest[j:]
	}
}

// mapSelectedKey is the identity of the node a rendered page has selected —
// the table row carrying data-map-row-selected, "" when the page selected
// nothing. It is read from the row, not from the picture, because the row is
// the form the map promises the picture is equivalent to.
func mapSelectedKey(t *testing.T, page string) string {
	t.Helper()
	const attr = `data-map-row="`
	key, found := "", 0
	rest := page
	for {
		i := strings.Index(rest, attr)
		if i < 0 {
			break
		}
		rest = rest[i+len(attr):]
		j := strings.Index(rest, `"`)
		if j < 0 {
			t.Fatalf("truncated data-map-row in the page")
		}
		candidate := rest[:j]
		rest = rest[j:]
		end := strings.Index(rest, ">")
		if end < 0 {
			t.Fatalf("truncated table row in the page")
		}
		if strings.Contains(rest[:end], "data-map-row-selected") {
			key, found = candidate, found+1
		}
		rest = rest[end:]
	}
	if found > 1 {
		t.Errorf("the page marks %d rows selected, want at most one", found)
	}
	return key
}

// TestResearchMapPanelLinksResolve: every coordinate the selected-node panel
// renders is followed exactly as a reader would follow it, and the page it
// lands on must have selected the node the link named. A coordinate the map
// cannot answer must not be a link at all — a link is a promise, and the map
// resolves a coordinate by keeping its longest valid prefix, so an
// unanswerable link does not fail loudly: it silently drops the reader on a
// shallower node (or, past the depth cap, on the page they were already on).
//
// The deepest column is where this bites: a node there has rows below it that
// the map's depth cannot express, and the panel used to hand out coordinates
// for them anyway.
func TestResearchMapPanelLinksResolve(t *testing.T) {
	stub := &stubService{outline: mapDeepOutline()}
	ts, _, _, _ := newRSGTestServer(t, stub)
	handler := ts.Config.Handler

	fetch := func(path string) string {
		t.Helper()
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Accept", "text/html")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, rec.Code)
		}
		return rec.Body.String()
	}

	starts := []struct{ name, path string }{
		{"the root question's page", researchBase + "?path=q-1&view=questions"},
		{"a sub-question's page, the map's deepest step", researchBase + "?path=q-1%2Csub-question%2Cq-2&view=questions"},
		{"a finding's page", researchBase + "?path=f-1&view=findings"},
		{"a question reached from the findings axis, the deepest step", researchBase + "?path=f-1%2Caddresses%2Cq-1&view=findings"},
	}
	coordinates := 0
	for _, start := range starts {
		page := fetch(start.path)
		for _, href := range mapPanelHrefs(t, page) {
			u, err := url.Parse(href)
			if err != nil {
				t.Fatalf("%s: the panel rendered an unparsable href %q: %v", start.name, href, err)
			}
			coordinate := u.Query().Get("path")
			if coordinate == "" {
				// Not a map coordinate: the object detail page, or the outline
				// list's anchor. Those are links on purpose.
				continue
			}
			coordinates++
			elems := strings.Split(coordinate, ",")
			want := researchMapKey(len(elems)-1, elems[len(elems)-1])
			landed := fetch(u.Path + "?" + u.RawQuery)
			if got := mapSelectedKey(t, landed); got != want {
				t.Errorf("%s: the panel's link %q landed on %q, want the node it named %q — an unanswerable coordinate is not a link",
					start.name, href, got, want)
			}
		}
	}
	// The links the map CAN answer must still be links: a panel that emits no
	// coordinates at all would pass the loop above by promising nothing.
	if coordinates < 3 {
		t.Errorf("the panels emitted %d coordinates over %d pages, want the answerable ones kept", coordinates, len(starts))
	}
	// And a row whose coordinate was suppressed is still reachable: the label
	// stays on the page and the object detail page is still linked.
	deep := fetch(researchBase + "?path=q-1%2Csub-question%2Cq-2&view=questions")
	for _, want := range []string{"MOF-5", "Uptake peaks at 40 bar", objectHref(rsgTestProjectID, "branch-1", "m-2")} {
		if !strings.Contains(deep, want) {
			t.Errorf("the sub-question's panel no longer states %q:\n%s", want, deep)
		}
	}
	// The suppressed bucket is stated, with its count in the reader's words and
	// no link around it: the count is one row, so it reads "1 row".
	if want := `<li><span>Findings</span> <span class="muted">1 row</span></li>`; !strings.Contains(deep, want) {
		t.Errorf("the deepest step's rollup is not stated as plain text; want %s", want)
	}
	// The panel says why there is no link there — an aggregate that stops being
	// a link without saying so is the silent drop this test exists for.
	if want := `data-map-depth-note`; !strings.Contains(deep, want) {
		t.Errorf("the deepest step's panel does not say why its rows are not links")
	}
	if want := pluralCount(researchMapDepth, "step"); !strings.Contains(deep, want) {
		t.Errorf("the depth note does not name the map's depth (%q)", want)
	}
	// A panel that CAN answer states the same bucket as a link, so the text
	// form above is the exception and not the panel's only rendering.
	root := fetch(researchBase + "?path=q-1&view=questions")
	if want := `<li><a href="` + researchBase + `?path=q-1%2Cfinding&amp;view=questions">Findings</a>`; !strings.Contains(root, want) {
		t.Errorf("the root's panel states its findings bucket as text, want the link %s", want)
	}
}

// mapPageCoordinates collects every ?path= coordinate a whole page emits — the
// table's rows, the breadcrumb and the panel — as (href, coordinate) pairs.
func mapPageCoordinates(t *testing.T, page string) [][2]string {
	t.Helper()
	var found [][2]string
	rest := page
	for {
		k := strings.Index(rest, `href="`)
		if k < 0 {
			return found
		}
		rest = rest[k+len(`href="`):]
		j := strings.Index(rest, `"`)
		if j < 0 {
			t.Fatalf("truncated href in the page")
		}
		href := html.UnescapeString(rest[:j])
		rest = rest[j:]
		u, err := url.Parse(href)
		if err != nil || u.Path != researchBase {
			// Not this page's coordinate space: the outline list's object
			// detail links, the remainder's in-page anchor, or the view links.
			continue
		}
		if coordinate := u.Query().Get("path"); coordinate != "" {
			found = append(found, [2]string{href, coordinate})
		}
	}
}

// TestResearchMapEveryPageLinkResolves is the panel test's property one level
// up: EVERY ?path= coordinate any page of the map emits — a table row, a
// breadcrumb, a panel entry, in either view — is followed as a reader would
// follow it and must land on the node it named. Nothing in the map may hand
// out a coordinate the map would not answer, whoever produced it.
func TestResearchMapEveryPageLinkResolves(t *testing.T) {
	stub := &stubService{outline: mapDeepOutline()}
	ts, _, _, _ := newRSGTestServer(t, stub)
	handler := ts.Config.Handler

	fetch := func(path string) string {
		t.Helper()
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Accept", "text/html")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, rec.Code)
		}
		return rec.Body.String()
	}

	pages := []string{
		researchBase,
		researchBase + "?path=q-1&view=questions",
		researchBase + "?path=q-1%2Cfinding&view=questions",
		researchBase + "?path=q-1%2Csub-question&view=questions",
		researchBase + "?path=q-1%2Csub-question%2Cq-2&view=questions",
		researchBase + "?view=findings",
		researchBase + "?path=f-1&view=findings",
		researchBase + "?path=f-1%2Cclaim&view=findings",
		researchBase + "?path=f-1%2Caddresses&view=findings",
		researchBase + "?path=f-1%2Caddresses%2Cq-1&view=findings",
	}
	followed := 0
	for _, page := range pages {
		body := fetch(page)
		for _, pair := range mapPageCoordinates(t, body) {
			href, coordinate := pair[0], pair[1]
			followed++
			elems := strings.Split(coordinate, ",")
			want := researchMapKey(len(elems)-1, elems[len(elems)-1])
			landed := fetch(href[strings.Index(href, "/"):])
			if got := mapSelectedKey(t, landed); got != want {
				t.Errorf("page %s emits %q, which lands on %q, want the node it named %q", page, href, got, want)
			}
		}
	}
	if followed < 10 {
		t.Errorf("the pages emitted %d coordinates in all, want the map's links to exist", followed)
	}
	t.Logf("followed %d coordinates over %d pages, each landing on the node it named", followed, len(pages))
}

// TestResearchPageMapDrillDownIsALink: every drill-down is a plain <a href>
// with a ?path= coordinate — no canvas, no script, no client state — so the
// keyboard and the screen reader get the whole map and a reader without
// JavaScript can still walk it. This test follows the page's own links back
// through the HTTP surface, one step at a time.
func TestResearchPageMapDrillDownIsALink(t *testing.T) {
	stub := &stubService{outline: cannedOutline()}
	ts, _, _, _ := newRSGTestServer(t, stub)

	fetch := func(path string) string {
		t.Helper()
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Accept", "text/html")
		rec := httptest.NewRecorder()
		ts.Config.Handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, rec.Code)
		}
		return rec.Body.String()
	}

	// Step 1: the coarse page names the root and links to its rollup.
	first := fetch(researchBase)
	if !strings.Contains(first, `data-map-node="c0:q-1"`) {
		t.Fatalf("the default page did not draw the root question:\n%s", first)
	}
	if strings.Contains(first, `data-map-node-kind="bucket"`) {
		t.Fatalf("the default page drew a bucket — the map is not coarse by default")
	}
	// Step 2: the root's table row links to the selected coordinate.
	if !strings.Contains(first, researchBase+"?path=q-1&amp;view=questions") {
		t.Fatalf("the default page has no drill-down link for the root")
	}
	second := fetch(researchBase + "?path=q-1&view=questions")
	if !strings.Contains(second, `data-map-node-kind="bucket"`) {
		t.Fatalf("the selected question's page drew no buckets:\n%s", second)
	}
	if !strings.Contains(second, `data-map-node="c1:finding"`) {
		t.Fatalf("the selected question's page drew no finding bucket")
	}
	// Step 3: the bucket links to its own rows, which are the objects — and
	// each row carries the object detail link, where its provenance and its
	// evidence are.
	third := fetch(researchBase + "?path=q-1,finding&view=questions")
	if !strings.Contains(third, `data-map-node="c2:f-1"`) {
		t.Fatalf("the bucket page drew no member row:\n%s", third)
	}
	if !strings.Contains(third, objectHref(rsgTestProjectID, "branch-1", "f-1")) {
		t.Fatalf("the member row offers no way on to the object detail page")
	}
	// Step 4: a coordinate that names nothing falls back to the deepest level
	// it can prove, and never renders an error page.
	fallback := fetch(researchBase + "?path=q-1,nope&view=questions")
	if !strings.Contains(fallback, `data-map-node="c0:q-1"`) || !strings.Contains(fallback, `data-map-node-kind="bucket"`) {
		t.Fatalf("a broken coordinate did not fall back to the deepest valid level")
	}
	// The map is built from the page's ONE outline read — never a second one.
	if stub.researchOutlineCalls != 4 {
		t.Errorf("outline reads = %d, want 4 (one per page, no extra read for the map)", stub.researchOutlineCalls)
	}
}

// TestResearchMapE2EPerf is the required "research map e2e/perf" test: the map
// end to end over the HTTP surface at the size the acceptance criterion names.
//
// Two claims, both asserted against a 1000-object outline — not against a
// small one that happens to behave:
//
//  1. 1000 node underlying state 仍默认粗粒度 — the default page draws a
//     bounded picture (no bucket, no member, no arrow) whatever the state
//     holds, and it says how many rows it aggregated.
//  2. docs/27's SLO for the map's own work, "Research Map initial aggregated
//     query p95 < 1s": the default page's build and render at that size stays
//     inside it, measured over repeated reads rather than one lucky one.
//
// The store read behind the outline is the page's existing project-wide read
// (budgeted by docs/27 for the Research page and exercised against real
// PostgreSQL by tests/integration's research map e2e); what is measured here
// is the map's own cost, which the constants bound independently of the
// project's size.
func TestResearchMapE2EPerf(t *testing.T) {
	const roots = 60
	out, objects := mapScaleOutline(roots, 1, 2, 3, 1, 3)
	if objects < 1000 {
		t.Fatalf("fixture holds %d objects, want at least 1000", objects)
	}
	stub := &stubService{outline: out}
	ts, _, _, _ := newRSGTestServer(t, stub)
	handler := ts.Config.Handler

	fetch := func(path string) (string, time.Duration) {
		t.Helper()
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Accept", "text/html")
		rec := httptest.NewRecorder()
		start := time.Now()
		handler.ServeHTTP(rec, req)
		elapsed := time.Since(start)
		if rec.Code != 200 {
			t.Fatalf("GET %s = %d", path, rec.Code)
		}
		return rec.Body.String(), elapsed
	}

	body, _ := fetch(researchBase)
	// 1. Coarse by default, at 1000 objects: the roots and their remainder,
	// and nothing else.
	nodes := mapAttrValues(body, "data-map-node")
	rows := mapAttrValues(body, "data-map-row")
	if len(nodes) != researchMapColumnBudget+1 {
		t.Errorf("default page drew %d nodes for a %d-object project, want %d + the remainder: %v",
			len(nodes), objects, researchMapColumnBudget, nodes)
	}
	if len(rows) != len(nodes) {
		t.Errorf("default page drew %d nodes but %d rows", len(nodes), len(rows))
	}
	if strings.Contains(body, `data-map-node-kind="`+mapKindBucket+`"`) {
		t.Errorf("the default page of a 1000-object project drew a bucket")
	}
	if edges := mapAttrValues(body, "data-map-edge-to"); len(edges) != 0 {
		t.Errorf("the default page drew %d arrows, want none before anything is selected", len(edges))
	}
	if !strings.Contains(body, `data-map-node-kind="remainder"`) {
		t.Errorf("the default page silently dropped the roots it could not draw — no remainder node")
	}
	hidden := roots - researchMapColumnBudget
	if want := pluralCount(hidden, "row") + " aggregated into a remainder node"; !strings.Contains(body, want) {
		t.Errorf("the default page does not say what it aggregated; want %q", want)
	}
	// The whole 1000-object state is still reachable by keyboard: the outline
	// list beside the map carries every row (the accessible fallback).
	if got := strings.Count(body, `data-question-id="`); got < roots {
		t.Errorf("the outline list carries %d questions, want all %d", got, roots)
	}

	// 2. The SLO, measured over repeated default reads of the same 1000-object
	// outline: p95 < 1s. Timing one request would measure luck.
	const reads = 40
	durations := make([]time.Duration, 0, reads)
	for i := 0; i < reads; i++ {
		_, elapsed := fetch(researchBase)
		durations = append(durations, elapsed)
	}
	sortDurations(durations)
	p95 := durations[(reads*95)/100-1]
	slowest := durations[reads-1]
	if p95 > time.Second {
		t.Errorf("initial aggregated map p95 = %s over %d reads, docs/27 budgets < 1s (slowest %s)", p95, reads, slowest)
	}
	t.Logf("research map initial read at %d objects: p95 %s, slowest %s over %d reads", objects, p95, slowest, reads)

	// And the drill-down the reader reaches for next is bounded the same way:
	// selecting a root draws its rollup and stops there.
	drilled, _ := fetch(researchBase + "?path=" + out.Questions[0].ObjectID + "&view=questions")
	drillNodes := mapAttrValues(drilled, "data-map-node")
	if len(drillNodes) > researchMapNodeBudget {
		t.Errorf("drilled page drew %d nodes, ceiling %d", len(drillNodes), researchMapNodeBudget)
	}
	drillEdges := mapAttrValues(drilled, "data-map-edge-to")
	if len(drillEdges) > researchMapBucketBudget {
		t.Errorf("drilled page drew %d arrows to a question's rollup, ceiling %d", len(drillEdges), researchMapBucketBudget)
	}
}

// sortDurations sorts ascending (a tiny insertion sort keeps the test free of
// an import for four lines of arithmetic).
func sortDurations(d []time.Duration) {
	for i := 1; i < len(d); i++ {
		for j := i; j > 0 && d[j] < d[j-1]; j-- {
			d[j], d[j-1] = d[j-1], d[j]
		}
	}
}
