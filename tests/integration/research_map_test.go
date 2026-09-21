// Package integration — T1102 "research map e2e/perf": the Research Map
// exercised end to end over REAL PostgreSQL, the real auth guard, the real
// RSG service and the real HTTP surface.
//
// T0211's page test settles that the outline is question-organized and
// drills down. This file settles what the map adds, and it is written as the
// two acceptance criteria stated as negatives:
//
//  1. Seed 项目不成蜘蛛网. The seed project is the realistic MOF-shaped
//     state T0211's fixture builds: a two-level question tree with a
//     hypothesis, a finding, a claim, a material and a leftover dataset
//     under it. The map's default page must draw ONE box and no arrows, and
//     nothing may be drawn below a node that was not selected — the picture
//     of a project is bounded by the map's budgets, never by the project's
//     relation count.
//
//  2. 1000 node underlying state 仍默认粗粒度. The scale project holds 1020
//     objects — questions, sub-questions, hypotheses, findings with pinned
//     claims and materials, linked by real addresses_question relations. The
//     default page of that state must still draw the same bounded picture
//     (eight roots and one node naming the 940 roots it aggregated), the
//     outline list beside it must still carry every question, and the
//     drill-down the reader clicks must still work — all inside docs/27's
//     SLO, "Research Map initial aggregated query p95 < 1s", measured over
//     repeated real reads rather than one lucky one.
//
// Everything here is read through the links the rendered page itself
// carries: the test follows the reader's clicks (?path= coordinates), not
// coordinates it computed, so a page that draws the right boxes and links
// them nowhere fails.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The scale project's shape. Eight rounds go in through the real API — every
// object type, every relation, every pinned claim — and the rest of the
// 1000-object state is placed directly, because a fixture whose subject is a
// READ must not spend minutes measuring the write path: an object create
// costs ~3ms in a fresh project and ~68ms in one holding 600 objects (it
// grows with the project's size), so 1000 writes take minutes. The read path
// under test is untouched by that choice: the same table, the same columns,
// the same real outline query.
const (
	// mapScaleRounds is how many linked rounds are created over the wire.
	mapScaleRounds = 8
	// mapScaleBulkQuestions is how many further root questions are placed
	// directly, to carry the state past the 1000 objects the acceptance
	// criterion names. Their titles sort after the rounds' ("Round …" before
	// "Scale question …"), so the drawn column is the LINKED part of the
	// state and the drill-down assertions below have real relations to walk.
	mapScaleBulkQuestions = 940
	// mapScaleRoots is how many root questions the state ends up with: one
	// per round plus the bulk questions (each round's sub-question is a
	// child, not a root).
	mapScaleRoots = mapScaleRounds + mapScaleBulkQuestions
)

// mapScaleSeed is what the scale fixture returns: the project's coordinate
// plus what it actually placed.
type mapScaleSeed struct {
	projectID string
	branchID  string
	objects   int
	// roots is how many root questions the state holds, in title order.
	roots int
	// firstRootID is the root the map draws first (the rounds' titles sort
	// first), which is therefore the one a reader drills into first.
	firstRootID string
}

// researchMapPath is the research route for a project (the seed fixture's
// helper returns the same string; this one keeps the scale half readable).
func researchMapPath(projectID string) string {
	return "/api/v1/projects/" + projectID + "/research"
}

// mapAttrValues collects the values of one data attribute of a rendered page.
func mapAttrValues(page, attr string) []string {
	var values []string
	needle := attr + `="`
	rest := page
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

// panelHrefs collects every href the selected-node panel of a rendered page
// carries — what a reader has to work with once a node is selected.
func panelHrefs(t *testing.T, page string) []string {
	t.Helper()
	i := strings.Index(page, `data-map-detail="selected"`)
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

// selectedMapKey is the node a rendered page has selected: the identity of the
// table row carrying data-map-row-selected, or "" when nothing is selected.
func selectedMapKey(t *testing.T, page string) string {
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

// firstDrillLink is the first drill-down link of a rendered map page whose
// coordinate has the requested number of steps — the link a reader would
// click, not a coordinate the test worked out.
func firstDrillLink(t *testing.T, page, base string, steps int) string {
	t.Helper()
	needle := `href="` + base + `?path=`
	rest := page
	for {
		i := strings.Index(rest, needle)
		if i < 0 {
			t.Fatalf("the page carries no drill-down link with %d steps", steps)
		}
		rest = rest[i+len(`href="`):]
		j := strings.Index(rest, `"`)
		if j < 0 {
			t.Fatalf("truncated href in the page")
		}
		link := html.UnescapeString(rest[:j])
		rest = rest[j:]
		coordinate := link[strings.Index(link, "path=")+len("path="):]
		if i := strings.Index(coordinate, "&"); i >= 0 {
			coordinate = coordinate[:i]
		}
		if strings.Count(coordinate, "%2C") == steps-1 {
			return link
		}
	}
}

// seedMapScaleProject creates the 1000-object research state: rounds linked
// rounds through the real API (root questions, each with a sub-question, a
// hypothesis, a material, three findings and the claim versions those
// findings pin — every link a real addresses_question relation), then the
// bulk root questions placed directly in the same tables the API writes.
func seedMapScaleProject(t *testing.T, ctx context.Context, f *researchPageFixture, slug string) mapScaleSeed {
	t.Helper()

	resp := f.alice.do(t, http.MethodPost, "/api/v1/projects",
		`{"slug":"`+slug+`","name":"Scale Lab","purpose":"exercise the research map at scale","visibility":"private"}`)
	mustStatus(t, resp, http.StatusCreated)
	var project projectResponse
	if err := json.NewDecoder(resp.Body).Decode(&project); err != nil {
		t.Fatalf("create scale project payload: %v", err)
	}
	projectID := project.Project.ID

	resp = f.alice.do(t, http.MethodPost, "/api/v1/projects/"+projectID+"/branches",
		`{"name":"main","base_ref":"","visibility":"private"}`)
	mustStatus(t, resp, http.StatusCreated)
	var branch struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&branch); err != nil {
		t.Fatalf("create scale branch payload: %v", err)
	}

	seed := mapScaleSeed{projectID: projectID, branchID: branch.ID}

	// addRelation links one object's version to a question's version.
	addRelation := func(sourceVersionID, targetVersionID string) {
		t.Helper()
		resp := f.alice.do(t, http.MethodPost,
			"/api/v1/projects/"+projectID+"/branches/"+branch.ID+"/relations",
			`{"relation_type":"addresses_question","source_object_version_id":"`+sourceVersionID+
				`","target_object_version_id":"`+targetVersionID+`"}`)
		mustStatus(t, resp, http.StatusCreated)
	}

	for i := 0; i < mapScaleRounds; i++ {
		root := f.createObject(t, projectID, branch.ID, "research_question",
			fmt.Sprintf(`{"statement":"Round %03d: does the material adsorb CO2 at pressure?","question_state":"open"}`, i))
		seed.objects++
		if i == 0 {
			seed.firstRootID = root.ID
		}

		// The question tree's second level: a sub-question naming the root as
		// its parent (the tree edge is the payload's parent_question_id).
		f.createObject(t, projectID, branch.ID, "research_question",
			fmt.Sprintf(`{"statement":"Round %03d sub-question: at which pressure does uptake peak?","question_state":"partially_answered","parent_question_id":%q}`, i, root.ID))
		seed.objects++

		// The claim versions the findings pin, created first so the findings
		// can reference them.
		claimVersionIDs := make([]string, 0, 3)
		for c := 0; c < 3; c++ {
			claim := f.createObject(t, projectID, branch.ID, "claim",
				fmt.Sprintf(`{"statement":"Capacity is %d mmol/g (round %03d)","claim_type":"quantitative"}`, c+1, i))
			claimVersionIDs = append(claimVersionIDs, claim.VersionID)
			seed.objects++
		}

		hypothesis := f.createObject(t, projectID, branch.ID, "hypothesis",
			fmt.Sprintf(`{"statement":"Uptake scales with pressure (round %03d)","question_id":%q}`, i, root.ID))
		addRelation(hypothesis.VersionID, root.VersionID)
		seed.objects++

		refs, err := json.Marshal(claimVersionIDs)
		if err != nil {
			t.Fatal(err)
		}
		for k := 0; k < 3; k++ {
			finding := f.createObject(t, projectID, branch.ID, "finding",
				fmt.Sprintf(`{"statement":"Uptake peaks at %d bar (round %03d)","finding_type":"trend","assessment":"accepted","claim_version_refs":%s}`,
					10*k+5, i, string(refs)))
			addRelation(finding.VersionID, root.VersionID)
			seed.objects++
		}

		material := f.createObject(t, projectID, branch.ID, "material",
			fmt.Sprintf(`{"name":"MOF-%03d"}`, i))
		addRelation(material.VersionID, root.VersionID)
		seed.objects++
	}
	seed.roots = mapScaleRoots

	// The bulk tail: root questions with a title, a statement and a state, in
	// the same two tables the API writes, at the same schema coordinates a
	// real question carries. One row each: object + version. The ids are
	// derived from the row number so the two statements agree about which
	// version belongs to which object without a round trip per row.
	bulkID := `('00000000-0000-4000-8000-' || lpad(g::text, 12, '0'))::uuid`
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO scientific_objects (id, project_id, object_type, created_by)
		SELECT `+bulkID+`, $1, 'research_question',
		       (SELECT created_by FROM scientific_objects WHERE project_id = $1 LIMIT 1)
		FROM generate_series(1, $2) g`, projectID, mapScaleBulkQuestions); err != nil {
		t.Fatalf("place the bulk questions: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO scientific_object_versions
			(id, object_id, version_no, state_id, branch_id, schema_id, schema_version,
			 title, lifecycle_state, payload, integrity_hash, created_by)
		SELECT gen_random_uuid(), `+bulkID+`, 1,
		       (SELECT id FROM project_states WHERE project_id = $1 ORDER BY created_at ASC LIMIT 1),
		       $3, 'https://open-rd.example/schemas/research_question.schema.json', '1',
		       'Scale question ' || lpad(g::text, 4, '0'), 'active',
		       jsonb_build_object(
		           'statement', 'Scale question ' || lpad(g::text, 4, '0') || ': does the material still adsorb CO2?',
		           'question_state', 'open'),
		       'ih-map-scale',
		       (SELECT created_by FROM scientific_objects WHERE project_id = $1 LIMIT 1)
		FROM generate_series(1, $2) g`, projectID, mapScaleBulkQuestions, branch.ID); err != nil {
		t.Fatalf("place the bulk questions' versions: %v", err)
	}
	seed.objects += mapScaleBulkQuestions
	return seed
}

// TestResearchMapE2E is the required "research map e2e/perf" test, first
// half: the seed project's map over the real composition. A realistic
// project must not render as a spider web — its default page is one box and
// no arrows, and its drill-downs are the relations the project actually
// carries.
func TestResearchMapE2E(t *testing.T) {
	ctx := testCtx(t)
	f := newResearchPageFixture(t, ctx)
	base := researchMapPath(f.privateProjectID)

	resp := f.page(t, f.alice, base)
	mustStatus(t, resp, http.StatusOK)
	page := readAll(t, resp)

	// The map is on the page, and its picture is its table.
	nodes := mapAttrValues(page, "data-map-node")
	rows := mapAttrValues(page, "data-map-row")
	if len(nodes) == 0 {
		t.Fatalf("the research page drew no map at all:\n%s", page)
	}
	if len(nodes) != len(rows) {
		t.Errorf("map drew %d nodes but %d table rows: %v vs %v", len(nodes), len(rows), nodes, rows)
	}
	rowsSet := map[string]bool{}
	for _, r := range rows {
		rowsSet[r] = true
	}
	for _, n := range nodes {
		if !rowsSet[n] {
			t.Errorf("drawn node %q has no table row of the same identity", n)
		}
	}
	// A seed project of seven objects draws ONE box: its single root question.
	// The category claim under test is that the drawn size is a property of
	// the map's budgets, not of the project — so the negative is exact here:
	// one root, nothing else, no arrows.
	if len(nodes) != 1 {
		t.Errorf("default page drew %d nodes for a one-root project, want 1: %v", len(nodes), nodes)
	}
	if kinds := mapAttrValues(page, "data-map-node-kind"); len(kinds) != 1 || kinds[0] != "question" {
		t.Errorf("default page drew %v, want one question and nothing else", kinds)
	}
	if edges := mapAttrValues(page, "data-map-edge-to"); len(edges) != 0 {
		t.Errorf("default page drew %d arrows before anything was selected", len(edges))
	}
	if strings.Contains(page, `data-map-node-kind="bucket"`) {
		t.Errorf("the default page drew a rollup bucket: the map is not coarse by default")
	}

	// Follow the page's own link into the root question's rollup: the map
	// must draw the relations the project carries, one bucket per role.
	drilled := f.page(t, f.alice, firstDrillLink(t, page, base, 1))
	mustStatus(t, drilled, http.StatusOK)
	drillPage := readAll(t, drilled)
	buckets := mapAttrValues(drillPage, "data-map-node-kind")
	bucketCount := 0
	for _, k := range buckets {
		if k == "bucket" {
			bucketCount++
		}
	}
	if bucketCount != 3 {
		t.Errorf("the root question's page drew %d buckets, want the three roles the fixture's relations carry: %v", bucketCount, buckets)
	}
	// The fixture's root question is addressed by a hypothesis and a finding,
	// and has one sub-question — and each arrow must carry its target row's
	// role.
	roles := mapAttrValues(drillPage, "data-map-edge-role")
	sort.Strings(roles)
	if want := []string{"finding", "hypothesis", "sub-question"}; strings.Join(roles, ",") != strings.Join(want, ",") {
		t.Errorf("arrows = %v, want %v", roles, want)
	}
	// And the drill continues to the objects themselves, where the object
	// detail page takes over.
	members := f.page(t, f.alice, firstDrillLink(t, drillPage, base, 2))
	mustStatus(t, members, http.StatusOK)
	memberPage := readAll(t, members)
	if !strings.Contains(memberPage, "/branches/"+f.privateBranchID+"/objects/") {
		t.Errorf("the member page carries no link on to an object detail page")
	}
	// The third step reaches the object a bucket's row stands for: the
	// fixture's material addresses the SUB-question, so the sub-question's
	// own page must list it under "Other objects" and hand it a link.
	deep := f.page(t, f.alice, base+"?path="+f.rootQuestionID+",sub-question,"+f.childQuestionID+"&view=questions")
	mustStatus(t, deep, http.StatusOK)
	deepPage := readAll(t, deep)
	if !strings.Contains(deepPage, "MOF-5") {
		t.Errorf("the sub-question's map page does not list the material that addresses it:\n%s", deepPage)
	}
	if !strings.Contains(deepPage, "/branches/"+f.privateBranchID+"/objects/"+f.materialID) {
		t.Errorf("the sub-question's page carries no link to the material's object page")
	}
	// The second view is the findings axis and it opens on the finding.
	findingsView := f.page(t, f.alice, base+"?view=findings")
	mustStatus(t, findingsView, http.StatusOK)
	findingPage := readAll(t, findingsView)
	if !strings.Contains(findingPage, `data-map-node-kind="finding"`) {
		t.Errorf("the By Finding view drew no finding:\n%s", findingPage)
	}
	// Its own drill reaches the finding's pinned claims.
	findingDrill := f.page(t, f.alice, firstDrillLink(t, findingPage, base, 1))
	mustStatus(t, findingDrill, http.StatusOK)
	findingDrillPage := readAll(t, findingDrill)
	if !strings.Contains(findingDrillPage, `data-map-node="c1:claim"`) {
		t.Errorf("the finding's page drew no claim bucket:\n%s", findingDrillPage)
	}
	// Every coordinate a panel hands out is followed exactly as a reader would
	// follow it, and it must land on the node the link named. The deepest step
	// is where a link the map cannot answer would drop the reader: it lands on
	// a shallower node, or — past the map's depth — on the very page they were
	// on. The panels that DO answer keep their links, so the loop below is not
	// satisfied by a page that links nothing.
	coordinates := 0
	for _, panel := range []struct {
		name string
		body string
	}{
		{"the root question's page", drillPage},
		{"a member's page", memberPage},
		{"a sub-question's page, the map's deepest step", deepPage},
		{"the finding's page", findingDrillPage},
	} {
		for _, href := range panelHrefs(t, panel.body) {
			u, err := url.Parse(href)
			if err != nil {
				t.Fatalf("%s: the panel rendered an unparsable href %q: %v", panel.name, href, err)
			}
			coordinate := u.Query().Get("path")
			if coordinate == "" {
				continue // the object detail page: a link on purpose, not a coordinate
			}
			coordinates++
			elems := strings.Split(coordinate, ",")
			want := "c" + strconv.Itoa(len(elems)-1) + ":" + elems[len(elems)-1]
			landed := f.page(t, f.alice, u.Path+"?"+u.RawQuery)
			mustStatus(t, landed, http.StatusOK)
			if got := selectedMapKey(t, readAll(t, landed)); got != want {
				t.Errorf("%s: the panel's link %q landed on %q, want the node it named %q", panel.name, href, got, want)
			}
		}
	}
	if coordinates < 2 {
		t.Errorf("the panels emitted %d coordinates, want the answerable ones kept", coordinates)
	}
	// The page stays exactly as visible as its project: bob may not read it,
	// and the denial is the existence-hidden neutral page.
	denied := f.page(t, f.bob, base)
	if denied.StatusCode != http.StatusNotFound {
		t.Errorf("a non-member's map read = %d, want 404 (existence hiding)", denied.StatusCode)
	}
}

// TestResearchMapE2EPerfScale is the required "research map e2e/perf" test,
// second half: the 1000-object state, the coarse default, and docs/27's
// SLO, measured over real reads.
func TestResearchMapE2EPerfScale(t *testing.T) {
	ctx := testCtx(t)
	f := newResearchPageFixture(t, ctx)
	seed := seedMapScaleProject(t, ctx, f, "map-scale")
	if seed.objects < 1000 {
		t.Fatalf("fixture placed %d objects, want at least 1000", seed.objects)
	}
	base := researchMapPath(seed.projectID)

	// The underlying state really holds what the test claims: the JSON
	// contract's auxiliary counts are the same read the page renders, and
	// they count distinct objects of every type.
	req, err := http.NewRequest(http.MethodGet, f.ts.URL+base, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := f.alice.client.Do(req)
	if err != nil {
		t.Fatalf("GET %s (json): %v", base, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	mustStatus(t, resp, http.StatusOK)
	var payload struct {
		Counts struct {
			Questions    int `json:"questions"`
			Findings     int `json:"findings"`
			Hypotheses   int `json:"hypotheses"`
			Claims       int `json:"claims"`
			OtherObjects int `json:"other_objects"`
		} `json:"counts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode outline payload: %v", err)
	}
	c := payload.Counts
	readBack := c.Questions + c.Findings + c.Hypotheses + c.Claims + c.OtherObjects
	if readBack < 1000 {
		t.Fatalf("the outline read returned %d objects, want at least 1000 (counts %+v)", readBack, c)
	}
	// The counts are exact, so the state the map is reading is the state the
	// fixture placed — not a smaller one that happens to be coarse anyway.
	if want := mapScaleRoots + mapScaleRounds; c.Questions != want {
		t.Errorf("questions = %d, want %d (%d roots + %d sub-questions)", c.Questions, want, mapScaleRoots, mapScaleRounds)
	}
	if want := 3 * mapScaleRounds; c.Findings != want || c.Claims != want {
		t.Errorf("findings/claims = %d/%d, want %d each", c.Findings, c.Claims, want)
	}
	if c.Hypotheses != mapScaleRounds || c.OtherObjects != mapScaleRounds {
		t.Errorf("hypotheses/other = %d/%d, want %d each", c.Hypotheses, c.OtherObjects, mapScaleRounds)
	}
	if readBack != seed.objects {
		t.Errorf("the outline read %d objects, the fixture placed %d", readBack, seed.objects)
	}
	t.Logf("underlying state read back: %d objects (questions %d, findings %d, hypotheses %d, claims %d, other %d)",
		readBack, c.Questions, c.Findings, c.Hypotheses, c.Claims, c.OtherObjects)

	// The default page of that state is still coarse: eight roots and one
	// node naming the rest — 22 boxes is the ceiling for any project, and
	// nine is what a project with 948 roots actually costs.
	page, elapsed := f.timedPage(t, f.alice, base)
	nodes := mapAttrValues(page, "data-map-node")
	if len(nodes) != 9 {
		t.Errorf("the default page of a %d-object project drew %d nodes, want 8 roots + the remainder: %v", readBack, len(nodes), nodes)
	}
	if rows := mapAttrValues(page, "data-map-row"); len(rows) != len(nodes) {
		t.Errorf("drew %d nodes but %d rows", len(nodes), len(rows))
	}
	if edges := mapAttrValues(page, "data-map-edge-to"); len(edges) != 0 {
		t.Errorf("the default page drew %d arrows, want none before anything is selected", len(edges))
	}
	if strings.Contains(page, `data-map-node-kind="bucket"`) {
		t.Errorf("the default page of a 1000-object state drew a rollup bucket")
	}
	if !strings.Contains(page, `data-map-node-kind="remainder"`) {
		t.Errorf("the default page silently dropped the roots it could not draw — no remainder node")
	}
	// The drawn column is the linked part of the state: the fixture's titles
	// sort so that the eight rounds a reader can actually walk come first,
	// which is what makes the drill-down assertions below reachable.
	if len(nodes) > 0 && nodes[0] != "c0:"+seed.firstRootID {
		t.Errorf("the first drawn node = %q, want the first round's root %q", nodes[0], seed.firstRootID)
	}
	hidden := seed.roots - 8
	if want := fmt.Sprintf("%d questions", hidden); !strings.Contains(page, want) {
		t.Errorf("the default page does not name what it aggregated; want %q", want)
	}
	if want := fmt.Sprintf("%d rows aggregated into a remainder node", hidden); !strings.Contains(page, want) {
		t.Errorf("the default page does not state the aggregation; want %q", want)
	}
	// Nothing disappears: the outline list beside the map still carries every
	// question of the state — both levels of the tree.
	if got := strings.Count(page, `data-question-id="`); got != mapScaleRoots+mapScaleRounds {
		t.Errorf("the outline list carries %d questions, want all %d", got, mapScaleRoots+mapScaleRounds)
	}
	t.Logf("first aggregated map read at %d objects: %s", readBack, elapsed)

	// docs/27's SLO, over repeated real reads of that state.
	const reads = 20
	durations := make([]time.Duration, 0, reads)
	durations = append(durations, elapsed)
	for i := 1; i < reads; i++ {
		_, d := f.timedPage(t, f.alice, base)
		durations = append(durations, d)
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p95 := durations[(reads*95)/100-1]
	slowest := durations[reads-1]
	if p95 > time.Second {
		t.Errorf("initial aggregated query p95 = %s over %d reads of a %d-object project, docs/27 budgets < 1s (slowest %s)",
			p95, reads, readBack, slowest)
	}
	t.Logf("research map initial aggregated query at %d objects: p95 %s, slowest %s over %d reads", readBack, p95, slowest, reads)

	// The drill-down the reader clicks next is bounded the same way, and it
	// reaches the real objects the project's relations point at.
	drilled := f.page(t, f.alice, firstDrillLink(t, page, base, 1))
	mustStatus(t, drilled, http.StatusOK)
	drillPage := readAll(t, drilled)
	drillNodes := mapAttrValues(drillPage, "data-map-node")
	if len(drillNodes) > 22 {
		t.Errorf("the drilled page drew %d nodes, ceiling 22", len(drillNodes))
	}
	if edges := mapAttrValues(drillPage, "data-map-edge-to"); len(edges) != 4 {
		t.Errorf("the drilled page drew %d arrows, want one per rollup bucket of the fixture's shape", len(edges))
	}
	members := f.page(t, f.alice, firstDrillLink(t, drillPage, base, 2))
	mustStatus(t, members, http.StatusOK)
	if memberPage := readAll(t, members); !strings.Contains(memberPage, "/branches/"+seed.branchID+"/objects/") {
		t.Errorf("the member page carries no link on to an object detail page")
	}
}

// timedPage fetches the research page as a browser and reports how long the
// read and the render took.
func (f *researchPageFixture) timedPage(t *testing.T, uc *testUserClient, path string) (string, time.Duration) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/html")
	start := time.Now()
	resp, err := uc.client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", path, resp.StatusCode)
	}
	body := readAll(t, resp)
	return body, time.Since(start)
}
