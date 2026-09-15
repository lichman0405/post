package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/states"
)

// Task T0209: RSG Query API — the required "rsg query integration" test,
// over a REAL PostgreSQL with the real stores, the real matrix engine and
// the production query port. Proves the acceptance criteria:
//
//   - the query returns a state-specific graph slice: the same project
//     queried at different pinned states renders different object/relation
//     versions (as-of semantics over the state lineage), and the branch
//     pin resolves to the head state;
//   - private relations do not leak: a non-member querying the private
//     project answers projects.ErrProjectNotFound — not-found, never
//     forbidden — and the same caller never sees the project's relations
//     through any other slice;
//   - cross-project traversal never leaks a private relation via an
//     intermediate node: an edge touching a public node whose own project
//     (or whose other endpoint's project) the caller cannot read is pruned
//     at the hop — the pinned version ids of the hidden project never
//     appear — while the same hop is included for a caller who may read
//     both projects (each hop carries the caller's own per-project
//     requireRead, not just the query project's).

// rsgQueryFixture is newRSGFixture (task-scoped DB, three users, one
// private project + one public project) — the rsg service there is wired
// with the production RSGQueryStore, the same composition cmd/api/main.go
// uses.
type rsgQueryFixture = rsgFixture

func newRSGQueryFixture(t *testing.T, ctx context.Context) *rsgQueryFixture {
	t.Helper()
	return newRSGFixture(t, ctx)
}

// queryAssertions indexes one QueryResult for set-wise checks.
type queryAssertions struct {
	t       *testing.T
	result  rsg.QueryResult
	objects map[string]map[string]bool // object id -> version ids
}

func assertQuery(t *testing.T, res rsg.QueryResult) *queryAssertions {
	t.Helper()
	objects := map[string]map[string]bool{}
	for _, o := range res.Objects {
		if objects[o.Object.ID] == nil {
			objects[o.Object.ID] = map[string]bool{}
		}
		objects[o.Object.ID][o.Version.ID] = true
	}
	return &queryAssertions{t: t, result: res, objects: objects}
}

// hasObject reports whether the slice contains the object at exactly the
// given version (nodes are version-pinned; one object may render at more
// than one version when edges pin an older version).
func (a *queryAssertions) hasObject(objectID, versionID string) bool {
	return a.objects[objectID][versionID]
}

// onlyObjects fails unless the slice's node set is exactly the given
// (object id, version id) pairs.
func (a *queryAssertions) onlyObjects(want ...string) {
	a.t.Helper()
	if len(want)%2 != 0 {
		a.t.Fatalf("onlyObjects: want pairs, got %v", want)
	}
	if len(a.result.Objects) != len(want)/2 {
		a.t.Errorf("objects = %d, want %d (%v)", len(a.result.Objects), len(want)/2, a.nodeList())
		return
	}
	for i := 0; i < len(want); i += 2 {
		if !a.hasObject(want[i], want[i+1]) {
			a.t.Errorf("missing node %s@%s in %v", want[i], want[i+1], a.nodeList())
		}
	}
}

// hasRelation reports whether the slice contains the relation id.
func (a *queryAssertions) hasRelation(relationID string) bool {
	for _, r := range a.result.Relations {
		if r.Relation.ID == relationID {
			return true
		}
	}
	return false
}

// onlyRelations fails unless the relation id set is exactly want.
func (a *queryAssertions) onlyRelations(want ...string) {
	a.t.Helper()
	if len(a.result.Relations) != len(want) {
		a.t.Errorf("relations = %d, want %d (%v)", len(a.result.Relations), len(want), a.relationList())
		return
	}
	for _, id := range want {
		if !a.hasRelation(id) {
			a.t.Errorf("missing relation %s in %v", id, a.relationList())
		}
	}
}

// versionNoOf returns the version number the slice rendered for one
// pinned version id (0 when the version is absent).
func (a *queryAssertions) versionNoOf(versionID string) int {
	for _, o := range a.result.Objects {
		if o.Version.ID == versionID {
			return o.Version.VersionNo
		}
	}
	return 0
}

func (a *queryAssertions) nodeList() string {
	out := make([]string, 0, len(a.result.Objects))
	for _, o := range a.result.Objects {
		out = append(out, fmt.Sprintf("%s@%s(v%d)", o.Object.ID, o.Version.ID, o.Version.VersionNo))
	}
	return fmt.Sprint(out)
}

func (a *queryAssertions) relationList() string {
	out := make([]string, 0, len(a.result.Relations))
	for _, r := range a.result.Relations {
		out = append(out, fmt.Sprintf("%s(%s)", r.Relation.ID, r.Version.RelationType))
	}
	return fmt.Sprint(out)
}

// plantRelation writes one relation + version-1 row straight into the
// store, bypassing the service's same-project endpoint rule — the shape a
// cross-project edge would have if some writer stored it directly (the
// defensive scenario the query's per-hop authorization must survive).
func (f *rsgQueryFixture) plantRelation(ctx context.Context, t *testing.T, relationID, projectID, stateID, relationType, sourceVID, targetVID string) {
	t.Helper()
	if _, err := f.pool.Exec(ctx, `INSERT INTO relations (id, project_id) VALUES ($1, $2)`, relationID, projectID); err != nil {
		t.Fatalf("plant relation row: %v", err)
	}
	payload := json.RawMessage(`{}`)
	sum := sha256.Sum256(payload)
	// The version id is a sibling uuid derived from the relation id (the
	// planted rows must be well-formed: every id column is a uuid).
	versionID := "d0" + relationID[2:]
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO relation_versions
		  (id, relation_id, version_no, state_id, relation_type,
		   source_object_version_id, target_object_version_id,
		   payload, integrity_hash, created_by)
		VALUES ($1, $2, 1, $3, $4, $5, $6, $7::jsonb, $8, $9)`,
		versionID, relationID, stateID, relationType,
		sourceVID, targetVID, payload, hex.EncodeToString(sum[:]), f.alice.ID); err != nil {
		t.Fatalf("plant relation version row: %v", err)
	}
}

// TestRSGQueryStateSpecificSlice is the acceptance "查询可返回
// state-specific graph slice": the same project renders different
// versions at different pins. The material is versioned twice, the
// relation pins the FIRST version — so the slice's as-of nodes and the
// edge's pinned endpoint node are observably different rows.
func TestRSGQueryStateSpecificSlice(t *testing.T) {
	ctx := testCtx(t)
	f := newRSGQueryFixture(t, ctx)
	alice := projects.Reader{UserID: f.alice.ID, Authenticated: true}

	mat, err := f.svc.CreateObject(ctx, f.alice, f.project.ID, f.branch, rsg.CreateObjectInput{
		ObjectType: "material", Payload: json.RawMessage(`{"name":"MOF-5"}`),
	})
	if err != nil {
		t.Fatalf("create material: %v", err)
	}
	matV2, err := f.svc.CreateObjectVersion(ctx, f.alice, f.project.ID, f.branch, mat.Object.ID, rsg.CreateObjectVersionInput{
		ExpectedVersion: 1, Patch: json.RawMessage(`{"formula":"Zn4O(BDC)3"}`),
	})
	if err != nil {
		t.Fatalf("version material: %v", err)
	}
	// A finding must pin at least one claim version (T0503 semantics):
	// the check is format-level here — this query fixture never writes
	// the finding_claim_versions projection, so a well-formed id is the
	// rule satisfied, existence is not tested on this path.
	finding, err := f.svc.CreateObject(ctx, f.alice, f.project.ID, f.branch, rsg.CreateObjectInput{
		ObjectType: "finding", Payload: json.RawMessage(`{"name":"uptake finding","claim_version_refs":["11111111-1111-4111-8111-111111111111"]}`),
	})
	if err != nil {
		t.Fatalf("create finding: %v", err)
	}
	// The edge pins the material's FIRST version (finding -> material v1).
	rel, err := f.svc.CreateRelation(ctx, f.alice, f.project.ID, f.branch, rsg.CreateRelationInput{
		RelationType:          "derived_from",
		SourceObjectVersionID: finding.Version.ID,
		TargetObjectVersionID: mat.Version.ID,
	})
	if err != nil {
		t.Fatalf("create relation: %v", err)
	}
	s1 := mat.Version.StateID   // material v1
	s2 := matV2.Version.StateID // material v2
	s4 := rel.Version.StateID   // the relation's state (child of s2 and s3)

	// Project-wide (no pin): material at v2, finding at v1, the relation
	// with its pinned v1 endpoint node.
	res, err := f.svc.Query(ctx, alice, f.project.ID, rsg.QueryInput{})
	if err != nil {
		t.Fatalf("query (no pin): %v", err)
	}
	a := assertQuery(t, res)
	a.onlyObjects(mat.Object.ID, matV2.Version.ID, mat.Object.ID, mat.Version.ID, finding.Object.ID, finding.Version.ID)
	a.onlyRelations(rel.Relation.ID)
	if res.StateID != "" || res.BranchID != "" {
		t.Errorf("no pin: state=%q branch=%q, want empty", res.StateID, res.BranchID)
	}

	// Pinned to the material-v1 state: only the material at v1 — the v2,
	// the finding and the relation all postdate the pin.
	res, err = f.svc.Query(ctx, alice, f.project.ID, rsg.QueryInput{StateID: s1})
	if err != nil {
		t.Fatalf("query (pin s1): %v", err)
	}
	assertQuery(t, res).onlyObjects(mat.Object.ID, mat.Version.ID)
	assertQuery(t, res).onlyRelations()
	if res.StateID != s1 {
		t.Errorf("pin echo = %q, want %q", res.StateID, s1)
	}

	// Pinned to the material-v2 state: material at v2 only.
	res, err = f.svc.Query(ctx, alice, f.project.ID, rsg.QueryInput{StateID: s2})
	if err != nil {
		t.Fatalf("query (pin s2): %v", err)
	}
	assertQuery(t, res).onlyObjects(mat.Object.ID, matV2.Version.ID)

	// Pinned to the relation's state: everything — and the edge's endpoint
	// node still renders at the exact pinned v1, not the as-of v2.
	res, err = f.svc.Query(ctx, alice, f.project.ID, rsg.QueryInput{StateID: s4})
	if err != nil {
		t.Fatalf("query (pin s4): %v", err)
	}
	a = assertQuery(t, res)
	a.onlyObjects(mat.Object.ID, matV2.Version.ID, mat.Object.ID, mat.Version.ID, finding.Object.ID, finding.Version.ID)
	a.onlyRelations(rel.Relation.ID)
	if a.versionNoOf(mat.Version.ID) != 1 {
		t.Errorf("pinned endpoint rendered at version %d, want the exact pinned v1", a.versionNoOf(mat.Version.ID))
	}

	// The branch pin resolves through the head state (the relation's
	// state) and echoes both the head state and the branch.
	res, err = f.svc.Query(ctx, alice, f.project.ID, rsg.QueryInput{BranchID: f.branch})
	if err != nil {
		t.Fatalf("query (branch pin): %v", err)
	}
	if res.StateID != s4 || res.BranchID != f.branch {
		t.Errorf("branch pin = state %q branch %q, want %q/%q", res.StateID, res.BranchID, s4, f.branch)
	}
	a = assertQuery(t, res)
	a.onlyRelations(rel.Relation.ID)
	if len(res.Objects) != 3 {
		t.Errorf("branch-pinned slice objects = %d, want 3 (%v)", len(res.Objects), a.nodeList())
	}

	// A missing or foreign state answers not-found — the same outcome for
	// both, so no state existence leaks.
	_, err = f.svc.Query(ctx, alice, f.project.ID, rsg.QueryInput{StateID: "99999999-9999-4999-8999-999999999999"})
	if !errors.Is(err, states.ErrStateNotFound) {
		t.Errorf("unknown state: err = %v, want ErrStateNotFound", err)
	}
}

// TestRSGQueryFiltersAndTraversal: type filters select seeds, edges and
// endpoint nodes by the L1 rules, and the recursive traversal expands the
// slice hop by hop with the depth cap enforced.
func TestRSGQueryFiltersAndTraversal(t *testing.T) {
	ctx := testCtx(t)
	f := newRSGQueryFixture(t, ctx)
	alice := projects.Reader{UserID: f.alice.ID, Authenticated: true}

	m, err := f.svc.CreateObject(ctx, f.alice, f.project.ID, f.branch, rsg.CreateObjectInput{
		ObjectType: "material", Payload: json.RawMessage(`{"name":"MOF-5"}`),
	})
	if err != nil {
		t.Fatalf("create material: %v", err)
	}
	e, err := f.svc.CreateObject(ctx, f.alice, f.project.ID, f.branch, rsg.CreateObjectInput{
		ObjectType: "experiment", Payload: json.RawMessage(`{"name":"isotherm run"}`),
	})
	if err != nil {
		t.Fatalf("create experiment: %v", err)
	}
	q, err := f.svc.CreateObject(ctx, f.alice, f.project.ID, f.branch, rsg.CreateObjectInput{
		ObjectType: "research_question", Payload: json.RawMessage(`{"statement":"uptake?"}`),
	})
	if err != nil {
		t.Fatalf("create question: %v", err)
	}
	fnd, err := f.svc.CreateObject(ctx, f.alice, f.project.ID, f.branch, rsg.CreateObjectInput{
		ObjectType: "finding", Payload: json.RawMessage(`{"name":"uptake finding","claim_version_refs":["11111111-1111-4111-8111-111111111111"]}`),
	})
	if err != nil {
		t.Fatalf("create finding: %v", err)
	}
	r1, err := f.svc.CreateRelation(ctx, f.alice, f.project.ID, f.branch, rsg.CreateRelationInput{
		RelationType: "performed_on", SourceObjectVersionID: e.Version.ID, TargetObjectVersionID: m.Version.ID,
	})
	if err != nil {
		t.Fatalf("create performed_on: %v", err)
	}
	r2, err := f.svc.CreateRelation(ctx, f.alice, f.project.ID, f.branch, rsg.CreateRelationInput{
		RelationType: "addresses_question", SourceObjectVersionID: fnd.Version.ID, TargetObjectVersionID: q.Version.ID,
	})
	if err != nil {
		t.Fatalf("create addresses_question: %v", err)
	}
	r3, err := f.svc.CreateRelation(ctx, f.alice, f.project.ID, f.branch, rsg.CreateRelationInput{
		RelationType: "refines", SourceObjectVersionID: e.Version.ID, TargetObjectVersionID: fnd.Version.ID,
	})
	if err != nil {
		t.Fatalf("create refines: %v", err)
	}

	// Object filter at depth 0: the seed only — no induced edge connects
	// two materials here.
	res, err := f.svc.Query(ctx, alice, f.project.ID, rsg.QueryInput{ObjectTypes: []string{"material"}})
	if err != nil {
		t.Fatalf("query material depth 0: %v", err)
	}
	assertQuery(t, res).onlyObjects(m.Object.ID, m.Version.ID)
	assertQuery(t, res).onlyRelations()

	// Depth 1: one hop adds the performed_on edge and its experiment node.
	res, err = f.svc.Query(ctx, alice, f.project.ID, rsg.QueryInput{ObjectTypes: []string{"material"}, Depth: 1})
	if err != nil {
		t.Fatalf("query material depth 1: %v", err)
	}
	a := assertQuery(t, res)
	a.onlyObjects(m.Object.ID, m.Version.ID, e.Object.ID, e.Version.ID)
	a.onlyRelations(r1.Relation.ID)

	// Depth 2: through the experiment, the refines edge and the finding.
	res, err = f.svc.Query(ctx, alice, f.project.ID, rsg.QueryInput{ObjectTypes: []string{"material"}, Depth: 2})
	if err != nil {
		t.Fatalf("query material depth 2: %v", err)
	}
	a = assertQuery(t, res)
	a.onlyObjects(m.Object.ID, m.Version.ID, e.Object.ID, e.Version.ID, fnd.Object.ID, fnd.Version.ID)
	a.onlyRelations(r1.Relation.ID, r3.Relation.ID)

	// The traversal is bidirectional and capped: from the experiment, one
	// hop reaches the material and the finding but not the question.
	res, err = f.svc.Query(ctx, alice, f.project.ID, rsg.QueryInput{ObjectTypes: []string{"experiment"}, Depth: 1})
	if err != nil {
		t.Fatalf("query experiment depth 1: %v", err)
	}
	a = assertQuery(t, res)
	a.onlyObjects(e.Object.ID, e.Version.ID, m.Object.ID, m.Version.ID, fnd.Object.ID, fnd.Version.ID)
	a.onlyRelations(r1.Relation.ID, r3.Relation.ID)
	if a.hasRelation(r2.Relation.ID) {
		t.Errorf("depth 1 reached the addresses_question edge (hop 2): %v", a.relationList())
	}

	// Relation filter: the matching edges plus their endpoint nodes only.
	res, err = f.svc.Query(ctx, alice, f.project.ID, rsg.QueryInput{RelationTypes: []string{"addresses_question"}})
	if err != nil {
		t.Fatalf("query addresses_question: %v", err)
	}
	assertQuery(t, res).onlyObjects(fnd.Object.ID, fnd.Version.ID, q.Object.ID, q.Version.ID)
	assertQuery(t, res).onlyRelations(r2.Relation.ID)

	// Both filters intersect: only edges whose type matches AND whose both
	// endpoints match the object types.
	res, err = f.svc.Query(ctx, alice, f.project.ID, rsg.QueryInput{
		ObjectTypes: []string{"experiment", "material"}, RelationTypes: []string{"performed_on"},
	})
	if err != nil {
		t.Fatalf("query intersection: %v", err)
	}
	assertQuery(t, res).onlyObjects(e.Object.ID, e.Version.ID, m.Object.ID, m.Version.ID)
	assertQuery(t, res).onlyRelations(r1.Relation.ID)

	// An unknown-but-well-formed type matches nothing (never an error).
	res, err = f.svc.Query(ctx, alice, f.project.ID, rsg.QueryInput{RelationTypes: []string{"derived_from"}})
	if err != nil {
		t.Fatalf("query derived_from: %v", err)
	}
	assertQuery(t, res).onlyObjects()
	assertQuery(t, res).onlyRelations()

	// The depth cap and the pin exclusivity are enforced.
	if _, err := f.svc.Query(ctx, alice, f.project.ID, rsg.QueryInput{Depth: 6}); !errors.Is(err, rsg.ErrValidation) {
		t.Errorf("depth 6: err = %v, want ErrValidation", err)
	}
	if _, err := f.svc.Query(ctx, alice, f.project.ID, rsg.QueryInput{StateID: "s", BranchID: "b"}); !errors.Is(err, rsg.ErrValidation) {
		t.Errorf("both pins: err = %v, want ErrValidation", err)
	}
}

// TestRSGQueryAuthorizationAndNoLeak is the acceptance "private relation
// 不泄漏" + "非成员读取 private project 返回 not-found 而非 forbidden" +
// "跨 project 的 traversal 不会经由中间节点泄漏私有 relation": the entry
// gate hides the private project (not-found, never forbidden), and planted
// cross-project edges are pruned at every hop whose project the caller
// cannot read — while the same hop is included for a caller who can.
func TestRSGQueryAuthorizationAndNoLeak(t *testing.T) {
	ctx := testCtx(t)
	f := newRSGQueryFixture(t, ctx)
	carol := projects.Reader{UserID: f.carol.ID, Authenticated: true}
	bob := projects.Reader{UserID: f.bob.ID, Authenticated: true}
	anon := projects.Reader{}

	m, err := f.svc.CreateObject(ctx, f.alice, f.project.ID, f.branch, rsg.CreateObjectInput{
		ObjectType: "material", Payload: json.RawMessage(`{"name":"private MOF"}`),
	})
	if err != nil {
		t.Fatalf("create private material: %v", err)
	}
	fnd, err := f.svc.CreateObject(ctx, f.alice, f.project.ID, f.branch, rsg.CreateObjectInput{
		ObjectType: "finding", Payload: json.RawMessage(`{"name":"private finding","claim_version_refs":["11111111-1111-4111-8111-111111111111"]}`),
	})
	if err != nil {
		t.Fatalf("create private finding: %v", err)
	}
	r1, err := f.svc.CreateRelation(ctx, f.alice, f.project.ID, f.branch, rsg.CreateRelationInput{
		RelationType: "derived_from", SourceObjectVersionID: fnd.Version.ID, TargetObjectVersionID: m.Version.ID,
	})
	if err != nil {
		t.Fatalf("create private relation: %v", err)
	}
	pub, err := f.svc.CreateObject(ctx, f.alice, f.publicP.ID, f.publicRef, rsg.CreateObjectInput{
		ObjectType: "material", Payload: json.RawMessage(`{"name":"public MOF"}`),
	})
	if err != nil {
		t.Fatalf("create public material: %v", err)
	}

	// The entry gate: a non-member and an anonymous caller both get
	// not-found — never forbidden — for the private project (existence
	// hiding; its relations are hidden with it).
	if _, err := f.svc.Query(ctx, bob, f.project.ID, rsg.QueryInput{}); !errors.Is(err, projects.ErrProjectNotFound) {
		t.Errorf("non-member query: err = %v, want ErrProjectNotFound (never forbidden)", err)
	}
	if _, err := f.svc.Query(ctx, anon, f.project.ID, rsg.QueryInput{}); !errors.Is(err, projects.ErrProjectNotFound) {
		t.Errorf("anonymous query: err = %v, want ErrProjectNotFound", err)
	}
	// A viewer may read the private project (the matrix allows
	// ActionReadPrivateProject for viewers).
	res, err := f.svc.Query(ctx, carol, f.project.ID, rsg.QueryInput{})
	if err != nil {
		t.Fatalf("viewer query: %v", err)
	}
	a := assertQuery(t, res)
	if !a.hasObject(m.Object.ID, m.Version.ID) || !a.hasObject(fnd.Object.ID, fnd.Version.ID) {
		t.Errorf("viewer slice lacks the private objects: %v", a.nodeList())
	}
	a.onlyRelations(r1.Relation.ID)
	// The anonymous caller sees the public project (and only it).
	res, err = f.svc.Query(ctx, anon, f.publicP.ID, rsg.QueryInput{})
	if err != nil {
		t.Fatalf("anonymous public query: %v", err)
	}
	assertQuery(t, res).onlyObjects(pub.Object.ID, pub.Version.ID)
	assertQuery(t, res).onlyRelations()

	// Planted cross-project edges (direct store writes — the API refuses to
	// create them, the query must still survive them):
	//   rx: a PRIVATE-project edge from the private material to the public
	//       material (the private relation hangs off a public node);
	//   ry: a PUBLIC-project edge from the public material to the private
	//       material (the edge's own project is readable, its endpoint is
	//       not).
	f.plantRelation(ctx, t, "c0ffee00-0000-4000-8000-000000000001", f.project.ID, r1.Version.StateID,
		"forked_from", m.Version.ID, pub.Version.ID)
	f.plantRelation(ctx, t, "c0ffee00-0000-4000-8000-000000000002", f.publicP.ID, pub.Version.StateID,
		"forked_from", pub.Version.ID, m.Version.ID)

	// Anonymous traversal of the public project: the public node's hop
	// touches both planted edges, and BOTH are pruned — rx's own project is
	// the hidden private project, ry's target endpoint is the hidden
	// private object. No private relation, node or pinned version id
	// leaks through the intermediate public node.
	res, err = f.svc.Query(ctx, anon, f.publicP.ID, rsg.QueryInput{Depth: 1})
	if err != nil {
		t.Fatalf("anonymous public traversal: %v", err)
	}
	a = assertQuery(t, res)
	a.onlyObjects(pub.Object.ID, pub.Version.ID)
	a.onlyRelations()
	if a.hasRelation("c0ffee00-0000-4000-8000-000000000001") || a.hasRelation("c0ffee00-0000-4000-8000-000000000002") {
		t.Errorf("anonymous traversal leaked a planted edge: %v", a.relationList())
	}
	if a.hasObject(m.Object.ID, m.Version.ID) {
		t.Errorf("anonymous traversal leaked the private object: %v", a.nodeList())
	}

	// Carol may read both projects: the same traversal from the same
	// public node includes both planted edges, and through them the
	// private material and — one hop further — the private finding via the
	// private derived_from edge. Every hop ran the caller's own
	// requireRead for each project, so the authorization is per hop, not
	// per query project.
	res, err = f.svc.Query(ctx, carol, f.publicP.ID, rsg.QueryInput{Depth: 1})
	if err != nil {
		t.Fatalf("viewer public traversal: %v", err)
	}
	a = assertQuery(t, res)
	a.onlyObjects(pub.Object.ID, pub.Version.ID, m.Object.ID, m.Version.ID, fnd.Object.ID, fnd.Version.ID)
	a.onlyRelations("c0ffee00-0000-4000-8000-000000000001", "c0ffee00-0000-4000-8000-000000000002", r1.Relation.ID)

	// And the mirror direction: carol traversing the PRIVATE project sees
	// the planted private edge rx and reaches the public node (the private
	// slice owns rx — a relation of the query project — and its public
	// endpoint node enters), plus the public edge ry hanging off it —
	// cross-project traversal exists and is authorized hop by hop.
	res, err = f.svc.Query(ctx, carol, f.project.ID, rsg.QueryInput{Depth: 1})
	if err != nil {
		t.Fatalf("viewer private traversal: %v", err)
	}
	a = assertQuery(t, res)
	a.onlyObjects(m.Object.ID, m.Version.ID, fnd.Object.ID, fnd.Version.ID, pub.Object.ID, pub.Version.ID)
	a.onlyRelations(r1.Relation.ID, "c0ffee00-0000-4000-8000-000000000001", "c0ffee00-0000-4000-8000-000000000002")
}
