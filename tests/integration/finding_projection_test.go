// Task T0503: Finding aggregation model.
//
// The finding tests required by the task, run against REAL PostgreSQL in a
// task-scoped namespace database (the knowledge fixture's test_T0501_<run>,
// migrated to head). They prove, by
// making things fail, that migration 00057's projection contract holds
// for every row-level write path — INSERT, UPDATE and DELETE (all raw SQL
// here; TRUNCATE is out of scope by design, it is the rebuild path):
//
//   - a findings row and its claim-version refs form one projection
//     family: refs pin real claim VERSIONS of the finding's own project,
//     a finding must pin at least one version, neither a ref-less
//     findings row nor an orphan ref can exist, a findings row's
//     object_id must be the object its own version belongs to, and
//     neither half may be left behind by a row that WALKS AWAY from it
//     (the DELETE routes and the two UPDATE re-points);
//   - finding_type/assessment are the schema enums — the claim
//     assessment "supported" and issue states are refused;
//   - the projection is rebuildable (docs/21 §5), never append-only;
//   - the acceptance criterion: a LATER claim version (a new row of the
//     append-only log) never silently rewrites what an older finding
//     aggregated — the finding's pinned version and its materialized
//     view of the claim stay the ones it pinned;
//   - the domain vocabularies cannot drift from the schema's enums.
package integration

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	"github.com/lichman0405/post/internal/rsg/semantics"
)

func TestFindingProjectionContract(t *testing.T) {
	ctx := testCtx(t)
	f := newKnowledgeFixture(t, ctx)

	// --- the minimal graph: claim v1 + finding v1 pinning it ------------
	insertObject := func(objectType string) string {
		t.Helper()
		var id string
		if err := f.pool.QueryRow(ctx, `INSERT INTO scientific_objects (project_id, object_type, created_by)
			VALUES ($1, $2, $3) RETURNING id`, f.project, objectType, f.alice).Scan(&id); err != nil {
			t.Fatalf("seed %s object: %v", objectType, err)
		}
		return id
	}
	insertVersion := func(objectID, objectType string, versionNo int, payload string) string {
		t.Helper()
		var versionID string
		if err := f.pool.QueryRow(ctx, `INSERT INTO scientific_object_versions
			(object_id, version_no, state_id, schema_id, schema_version, title,
			 lifecycle_state, payload, integrity_hash, created_by)
			VALUES ($1, $2, $3, $4, '1', $5, 'active', $6::jsonb, 'ih-f', $7) RETURNING id`,
			objectID, versionNo, f.state, "core/"+objectType, objectType+" title", payload, f.alice).Scan(&versionID); err != nil {
			t.Fatalf("seed %s version %d: %v", objectType, versionNo, err)
		}
		return versionID
	}
	claimRow := func(versionID, objectID, valueJSON, assessment string) {
		t.Helper()
		if _, err := f.pool.Exec(ctx, `INSERT INTO claims
			(version_id, object_id, claim_type, subject_ref, property, value, scope, scope_conditions, basis, assessment)
			VALUES ($1, $2, 'quantitative', 'MOF-5', 'BET surface area', $3::jsonb, '{}'::jsonb, '[]'::jsonb, '[]'::jsonb, $4)`,
			versionID, objectID, valueJSON, assessment); err != nil {
			t.Fatalf("seed claims projection row for %s: %v", versionID, err)
		}
	}
	wantErr := func(stmt string, err error, wantState string) {
		t.Helper()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Fatalf("%s: expected a PostgreSQL error (SQLSTATE %s), got %v", stmt, wantState, err)
		}
		if pgErr.Code != wantState {
			t.Errorf("%s: SQLSTATE = %s, want %s", stmt, pgErr.Code, wantState)
		} else {
			t.Logf("%s → rejected with SQLSTATE %s, as required", stmt, pgErr.Code)
		}
	}

	co1 := insertObject("claim")
	cv1 := insertVersion(co1, "claim", 1, `{"statement":"MOF-5 BET surface area is 3800 m2/g.","claim_type":"quantitative"}`)
	claimRow(cv1, co1, `{"value":3800,"unit":"m2/g"}`, "preliminary")

	fo1 := insertObject("finding")
	fv1 := insertVersion(fo1, "finding", 1, fmt.Sprintf(
		`{"statement":"The 3800 m2/g value was not reproduced under the stated conditions.","finding_type":"negative_finding","assessment":"accepted","claim_version_refs":["%s"]}`, cv1))

	// --- happy path: findings row + refs in ONE transaction --------------
	// The presence/ref guards are deferred to COMMIT, so a projection
	// writer may write the two rows in any order — findings first here.
	tx1, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx1.Exec(ctx, `INSERT INTO findings (version_id, object_id, finding_type, assessment)
		VALUES ($1, $2, 'negative_finding', 'accepted')`, fv1, fo1); err != nil {
		t.Fatalf("insert findings row (tx, findings first): %v", err)
	}
	if _, err := tx1.Exec(ctx, `INSERT INTO finding_claim_versions (finding_version_id, claim_version_id, position)
		VALUES ($1, $2, 0)`, fv1, cv1); err != nil {
		t.Fatalf("insert claim ref (tx, refs second): %v", err)
	}
	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("commit findings row + ref: %v", err)
	}

	// The negative finding round-trips as data: its type and the finding
	// assessment enum ("accepted", the human-confirmed position) are
	// first-class columns, not prose.
	var gotType, gotAssessment string
	if err := f.pool.QueryRow(ctx, `SELECT finding_type, assessment FROM findings WHERE version_id = $1`, fv1).
		Scan(&gotType, &gotAssessment); err != nil || gotType != "negative_finding" || gotAssessment != "accepted" {
		t.Fatalf("finding projection = (%q, %q), err %v; want (negative_finding, accepted)", gotType, gotAssessment, err)
	}
	var gotRef string
	var gotPosInt int
	if err := f.pool.QueryRow(ctx, `SELECT claim_version_id, position FROM finding_claim_versions WHERE finding_version_id = $1`, fv1).
		Scan(&gotRef, &gotPosInt); err != nil || gotRef != cv1 || gotPosInt != 0 {
		t.Fatalf("claim ref = (%q, %d), err %v; want (%s, 0)", gotRef, gotPosInt, err, cv1)
	}

	// --- happy path in the other order: refs first, findings row second --
	co2 := insertObject("claim")
	cv2 := insertVersion(co2, "claim", 1, `{"statement":"MOF-5 BET surface area is 3500 m2/g.","claim_type":"quantitative"}`)
	claimRow(cv2, co2, `{"value":3500,"unit":"m2/g"}`, "preliminary")
	fo2 := insertObject("finding")
	fv2 := insertVersion(fo2, "finding", 1, fmt.Sprintf(
		`{"statement":"The two values differ by 300 m2/g.","finding_type":"comparison","assessment":"preliminary","claim_version_refs":["%s","%s"]}`, cv1, cv2))

	tx2, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx2.Exec(ctx, `INSERT INTO finding_claim_versions (finding_version_id, claim_version_id, position)
		VALUES ($1, $2, 0)`, fv2, cv2); err != nil {
		t.Fatalf("insert claim ref (tx, refs first): %v", err)
	}
	if _, err := tx2.Exec(ctx, `INSERT INTO findings (version_id, object_id, finding_type, assessment)
		VALUES ($1, $2, 'comparison', 'preliminary')`, fv2, fo2); err != nil {
		t.Fatalf("insert findings row (tx, findings second): %v", err)
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatalf("commit ref + findings row: %v", err)
	}

	// --- ACCEPTANCE: a later claim version must not silently rewrite -----
	// the older finding. The claim is edited: version 2 is a NEW row of
	// the append-only log (00014), with its own claims projection row.
	cv1b := insertVersion(co1, "claim", 2, `{"statement":"MOF-5 BET surface area is 3500 m2/g.","claim_type":"quantitative"}`)
	claimRow(cv1b, co1, `{"value":3500,"unit":"m2/g"}`, "preliminary")

	// The finding's pinned ref still names version 1 — exactly one ref,
	// and it is not the new version.
	if err := f.pool.QueryRow(ctx, `SELECT claim_version_id FROM finding_claim_versions WHERE finding_version_id = $1`, fv1).
		Scan(&gotRef); err != nil || gotRef != cv1 {
		t.Fatalf("pinned ref after claim edit = %q, err %v; want %s (the OLD version)", gotRef, err, cv1)
	}
	// The finding's materialized view of the claim joins through its
	// pinned version: it still sees version 1's content, not version 2's.
	var gotValue json.RawMessage
	if err := f.pool.QueryRow(ctx, `SELECT c.value FROM finding_claim_versions fcv
		JOIN claims c ON c.version_id = fcv.claim_version_id
		WHERE fcv.finding_version_id = $1`, fv1).Scan(&gotValue); err != nil {
		t.Fatalf("materialized claim value: %v", err)
	}
	if !strings.Contains(string(gotValue), "3800") {
		t.Errorf("finding's materialized claim value = %s, want the version-1 value (3800)", gotValue)
	}
	// And the evolution is a new row, not an overwrite: the projection
	// holds BOTH versions side by side.
	var n int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM claims WHERE object_id = $1`, co1).Scan(&n); err != nil || n != 2 {
		t.Fatalf("claim projection rows = %d, err %v; want 2 (one per version)", n, err)
	}

	// --- the guards reject every forbidden shape -------------------------
	// A findings row with no refs (the schema's claim_version_refs
	// minItems 1 as a commit-time invariant).
	fo3 := insertObject("finding")
	fv3 := insertVersion(fo3, "finding", 1, `{"statement":"orphan finding.","finding_type":"observation","claim_version_refs":["`+cv1+`"]}`)
	_, err = f.pool.Exec(ctx, `INSERT INTO findings (version_id, object_id, finding_type, assessment)
		VALUES ($1, $2, 'observation', 'preliminary')`, fv3, fo3)
	wantGuardErr(t, "ref-less findings row", err, "must pin at least one claim version")

	// A findings row must name the object its OWN version belongs to:
	// object_id is denormalized from version_id, and a mismatched pair
	// would mis-attribute the finding in every object_id-keyed lookup
	// (findings_object_idx first). Re-pointing fv1's row at fo2 — a
	// different finding object — is refused at commit.
	_, err = f.pool.Exec(ctx, `UPDATE findings SET object_id = $1 WHERE version_id = $2`, fo2, fv1)
	wantGuardErr(t, "object_id re-pointed at another object", err, "is not the object of version")

	// A ref must name a CLAIM version — a dataset version is refused.
	do1 := insertObject("dataset")
	dv1 := insertVersion(do1, "dataset", 1, `{"name":"isotherm run"}`)
	_, err = f.pool.Exec(ctx, `INSERT INTO finding_claim_versions (finding_version_id, claim_version_id, position)
		VALUES ($1, $2, 1)`, fv1, dv1)
	wantGuardErr(t, "ref to a dataset version", err, "not a claim version")

	// A ref must name a claim of the finding's OWN project.
	var otherProject string
	if err := f.pool.QueryRow(ctx, `INSERT INTO projects (organization_id, slug, name, purpose, visibility, created_by)
		VALUES ((SELECT id FROM organizations WHERE slug = 'acme'), 'kp2', 'KP2', 'other project', 'private', $1) RETURNING id`,
		f.alice).Scan(&otherProject); err != nil {
		t.Fatalf("seed other project: %v", err)
	}
	var co4, cv4 string
	if err := f.pool.QueryRow(ctx, `INSERT INTO scientific_objects (project_id, object_type, created_by)
		VALUES ($1, 'claim', $2) RETURNING id`, otherProject, f.alice).Scan(&co4); err != nil {
		t.Fatalf("seed other-project claim: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `INSERT INTO scientific_object_versions
		(object_id, version_no, state_id, schema_id, schema_version, title,
		 lifecycle_state, payload, integrity_hash, created_by)
		VALUES ($1, 1, $2, 'core/claim', '1', 'claim title', 'active', '{}'::jsonb, 'ih-f', $3) RETURNING id`,
		co4, f.state, f.alice).Scan(&cv4); err != nil {
		t.Fatalf("seed other-project claim version: %v", err)
	}
	fo4 := insertObject("finding")
	fv4 := insertVersion(fo4, "finding", 1, fmt.Sprintf(
		`{"statement":"cross-project aggregation.","finding_type":"trend","claim_version_refs":["%s"]}`, cv4))
	tx4, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx4.Exec(ctx, `INSERT INTO findings (version_id, object_id, finding_type, assessment)
		VALUES ($1, $2, 'trend', 'preliminary')`, fv4, fo4); err != nil {
		t.Fatalf("insert cross-project findings row: %v", err)
	}
	if _, err := tx4.Exec(ctx, `INSERT INTO finding_claim_versions (finding_version_id, claim_version_id, position)
		VALUES ($1, $2, 0)`, fv4, cv4); err != nil {
		t.Fatalf("insert cross-project ref: %v", err)
	}
	wantGuardErr(t, "ref to another project's claim", tx4.Commit(ctx), "different project")

	// A ref must name an existing version row (the FK itself, 23503).
	_, err = f.pool.Exec(ctx, `INSERT INTO finding_claim_versions (finding_version_id, claim_version_id, position)
		VALUES ($1, gen_random_uuid(), 1)`, fv1)
	wantErr("INSERT ref with a fabricated claim version", err, "23503")

	// finding_type is the schema enum, not a free string (23514).
	_, err = f.pool.Exec(ctx, `UPDATE findings SET finding_type = 'conjecture' WHERE version_id = $1`, fv1)
	wantErr("UPDATE findings SET finding_type = 'conjecture'", err, "23514")
	// assessment is the FINDING enum — the claim assessment "supported"
	// and issue states must never validate (23514).
	_, err = f.pool.Exec(ctx, `UPDATE findings SET assessment = 'supported' WHERE version_id = $1`, fv1)
	wantErr("UPDATE findings SET assessment = 'supported'", err, "23514")
	_, err = f.pool.Exec(ctx, `UPDATE findings SET assessment = 'closed' WHERE version_id = $1`, fv1)
	wantErr("UPDATE findings SET assessment = 'closed'", err, "23514")

	// One pinned version, one row — a duplicate ref is refused (23505),
	// and so is a second ref at an occupied position (23505).
	_, err = f.pool.Exec(ctx, `INSERT INTO finding_claim_versions (finding_version_id, claim_version_id, position)
		VALUES ($1, $2, 1)`, fv1, cv1)
	wantErr("INSERT ref duplicating the pinned claim version", err, "23505")
	_, err = f.pool.Exec(ctx, `INSERT INTO finding_claim_versions (finding_version_id, claim_version_id, position)
		VALUES ($1, $2, 0)`, fv1, cv2)
	wantErr("INSERT ref at an occupied position", err, "23505")
	// positions carry the array order and start at 0 (23514).
	_, err = f.pool.Exec(ctx, `INSERT INTO finding_claim_versions (finding_version_id, claim_version_id, position)
		VALUES ($1, $2, -1)`, fv1, cv2)
	wantErr("INSERT ref at position -1", err, "23514")

	// The projection is rebuildable, not append-only (docs/21 §5): the
	// version log is immutable, its derived rows are not. An UPDATE of
	// the projection row must be ALLOWED.
	if _, err := f.pool.Exec(ctx, `UPDATE findings SET assessment = 'contested' WHERE version_id = $1`, fv1); err != nil {
		t.Fatalf("projection rebuild update: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT assessment FROM findings WHERE version_id = $1`, fv1).Scan(&gotAssessment); err != nil || gotAssessment != "contested" {
		t.Fatalf("assessment after rebuild = %q, err %v; want contested", gotAssessment, err)
	}

	// Deleting the only pinned ref leaves the finding ref-less — refused
	// at commit; deleting the findings row while refs remain orphans the
	// refs — refused too. The projection family lives and dies together.
	_, err = f.pool.Exec(ctx, `DELETE FROM finding_claim_versions WHERE finding_version_id = $1 AND claim_version_id = $2`, fv1, cv1)
	wantGuardErr(t, "delete the only pinned ref", err, "without claim refs")
	_, err = f.pool.Exec(ctx, `DELETE FROM findings WHERE version_id = $1`, fv1)
	wantGuardErr(t, "delete the findings row while refs remain", err, "must be removed before")

	// The ref guard fires on UPDATE too: re-pointing the ref at a
	// non-claim version is refused, re-pointing it within the claim
	// family (a legitimate projection rebuild) is allowed.
	_, err = f.pool.Exec(ctx, `UPDATE finding_claim_versions SET claim_version_id = $1
		WHERE finding_version_id = $2 AND claim_version_id = $3`, dv1, fv1, cv1)
	wantGuardErr(t, "re-point a ref at a dataset version", err, "not a claim version")
	if _, err := f.pool.Exec(ctx, `UPDATE finding_claim_versions SET claim_version_id = $1
		WHERE finding_version_id = $2 AND claim_version_id = $3`, cv1b, fv1, cv1); err != nil {
		t.Fatalf("re-point a ref at the newer claim version (rebuild): %v", err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT claim_version_id FROM finding_claim_versions WHERE finding_version_id = $1`, fv1).
		Scan(&gotRef); err != nil || gotRef != cv1b {
		t.Fatalf("ref after rebuild = %q, err %v; want %s", gotRef, err, cv1b)
	}
	// Sensitivity of the acceptance assertion, through the same join: with
	// the pin rebuilt onto cv1b the materialized view NOW reads that
	// version. The assertion is made twice over — identity AND content —
	// because the content alone would not discriminate: cv2's value is
	// 3500 too, so a finding that pinned a different 3500 claim would
	// satisfy a content-only check. What is asserted is the PIN: the ref
	// is cv1b, and the value read through it is cv1b's. A finding that
	// pinned the claim OBJECT (or always took the newest version) would
	// have read 3500 all along, which is what makes the 3800 above a
	// statement about the pin rather than a property of the query.
	var rebuiltRef string
	var rebuiltValue json.RawMessage
	if err := f.pool.QueryRow(ctx, `SELECT fcv.claim_version_id, c.value FROM finding_claim_versions fcv
		JOIN claims c ON c.version_id = fcv.claim_version_id
		WHERE fcv.finding_version_id = $1`, fv1).Scan(&rebuiltRef, &rebuiltValue); err != nil {
		t.Fatalf("materialized claim after rebuild: %v", err)
	}
	if rebuiltRef != cv1b {
		t.Errorf("rebuilt pin = %s, want %s: the join must read through the version the finding pins", rebuiltRef, cv1b)
	}
	if !strings.Contains(string(rebuiltValue), "3500") {
		t.Errorf("rebuilt pin reads %s, want the version-2 value (3500): the acceptance join cannot distinguish the versions and the 3800 assertion proves nothing", rebuiltValue)
	}

	// --- the two loss routes that are UPDATEs, not DELETEs ---------------
	// The mirror image of the two refused deletes above: here both rows
	// survive and it is an identity column that moves. These are why the
	// two guards carry an UPDATE OF event (00057's header), and they are
	// refused for the same reason as the deletes — a finding may not be
	// walked away from.
	//
	// (1) A ref re-pointed at ANOTHER finding strips the one it named.
	// position 5 keeps the move clear of fv2's occupied position 0, so
	// the ref guard is satisfied (fv2 is a real findings row, cv1b is a
	// claim of the same project) and the refs-remain guard is the only
	// thing that can refuse the write.
	_, err = f.pool.Exec(ctx, `UPDATE finding_claim_versions SET finding_version_id = $1, position = 5
		WHERE finding_version_id = $2 AND claim_version_id = $3`, fv2, fv1, cv1b)
	wantGuardErr(t, "re-point the only ref at another finding", err, "without claim refs")

	// (2) A findings row re-pointed at ANOTHER version of the same object.
	// The construction is deliberately one that breaks ONLY the orphan
	// invariant: the target belongs to fo1 (so the pairing guard passes)
	// and its refs are written in the same transaction (so the ref guard
	// and the refs-present guard pass), which leaves the refs of the
	// version being left behind as the single broken thing. Deferred
	// guards judge the state at COMMIT, not the statement, so this is the
	// same shape a rebuild that rewrites both halves would take.
	fv1b := insertVersion(fo1, "finding", 2, fmt.Sprintf(
		`{"statement":"The 3800 m2/g value reproduces at 25 C.","finding_type":"integrated_conclusion","assessment":"accepted","claim_version_refs":["%s"]}`, cv1b))
	tx5, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx5.Exec(ctx, `INSERT INTO finding_claim_versions (finding_version_id, claim_version_id, position)
		VALUES ($1, $2, 0)`, fv1b, cv1b); err != nil {
		t.Fatalf("insert ref for the re-point target: %v", err)
	}
	if _, err := tx5.Exec(ctx, `UPDATE findings SET version_id = $1 WHERE version_id = $2`, fv1b, fv1); err != nil {
		t.Fatalf("re-point the findings row at another version of the object: %v", err)
	}
	wantGuardErr(t, "re-point the findings row at another version (the old version keeps its refs)", tx5.Commit(ctx), "must be removed before")
}

// assembledFindingDoc builds the full document the validator assembles
// (server-authoritative fields + payload), so the schema's own enums can
// be interrogated through the public registry API — the same
// assembledQuestionDoc/HypothesisDoc shape (T0501).
func assembledFindingDoc(findingType, assessment string) map[string]any {
	return map[string]any{
		"id": "11111111-1111-4111-8111-111111111111", "type": "finding",
		"version": 1, "project_id": "22222222-2222-4222-8222-222222222222",
		"title": "f", "lifecycle_state": "active",
		"schema_ref":   map[string]any{"id": "https://open-rd.example/schemas/finding.schema.json", "version": "1"},
		"created_by":   "33333333-3333-4333-8333-333333333333",
		"created_at":   time.Now().UTC().Format(time.RFC3339),
		"statement":    "The claimed uptake was not reproduced under the stated conditions.",
		"finding_type": findingType, "assessment": assessment,
		"claim_version_refs": []any{"44444444-4444-4444-8444-444444444444"},
	}
}

// TestFindingSchemaEnumsMatchDomainTypes pins the domain's finding-type
// and assessment vocabularies to the runtime schema registry: every
// canonical value must validate, and the claim assessment "supported" and
// the issue states must never validate — the finding assessment is its
// own vocabulary, not a claim or issue one.
func TestFindingSchemaEnumsMatchDomainTypes(t *testing.T) {
	ctx := testCtx(t)
	_ = newKnowledgeFixture(t, ctx) // runs in a real migrated T0501-shaped database, like the rest
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	fs, ok := reg.Get(schemareg.Ref{ID: "https://open-rd.example/schemas/finding.schema.json", Version: "1"})
	if !ok {
		t.Fatal("finding schema not registered")
	}

	for _, typ := range domain.CanonicalFindingTypes() {
		doc, err := json.Marshal(assembledFindingDoc(typ, "accepted"))
		if err != nil {
			t.Fatalf("marshal finding doc: %v", err)
		}
		if err := fs.Validate(doc); err != nil {
			t.Errorf("schema refuses canonical finding_type %q: %v", typ, err)
		}
	}
	for _, typ := range []string{"conjecture", "summary", "positive_finding", "negative"} {
		doc, err := json.Marshal(assembledFindingDoc(typ, "accepted"))
		if err != nil {
			t.Fatalf("marshal finding doc: %v", err)
		}
		if err := fs.Validate(doc); err == nil {
			t.Errorf("schema ACCEPTS non-canonical finding_type %q — the domain/schema vocabulary drifted", typ)
		}
	}
	for _, a := range domain.CanonicalFindingAssessments() {
		doc, err := json.Marshal(assembledFindingDoc("observation", a))
		if err != nil {
			t.Fatalf("marshal finding doc: %v", err)
		}
		if err := fs.Validate(doc); err != nil {
			t.Errorf("schema refuses canonical finding assessment %q: %v", a, err)
		}
	}
	for _, a := range []string{"supported", "open", "closed", "in_progress", "proposed", "rejected", "Accepted"} {
		doc, err := json.Marshal(assembledFindingDoc("observation", a))
		if err != nil {
			t.Fatalf("marshal finding doc: %v", err)
		}
		if err := fs.Validate(doc); err == nil {
			t.Errorf("schema ACCEPTS non-canonical finding assessment %q — the domain/schema vocabulary drifted", a)
		}
	}
}

// TestFindingDraftWithoutRefsIsAcceptedWithHint is the platform-level half
// of the T0503 draft rule, run through the real service composition (the
// T0208 rsgFixture: semantics check + schema registry + commit guard +
// real stores). docs/08 §CoreScientificObject line 9 — "Draft 可缺部分
// domain field；进入 PR/main/release 时按 validation gate 逐步增强" — and
// the gate ladder make the draft allowance the GATE's business:
// CheckSchemaTyped reports an incomplete payload at draft and blocks it at
// PR (internal/rsg/validation/spec.go:90 SeverityWarning, :100
// SeverityBlocking). So the write must LAND, carrying
// FINDING_MISSING_CLAIM_VERSION_REFS as a hint — while the storage guard
// still refuses to materialize a ref-less projection row for it. The
// refusal moves to the gate that owns it; it is not deleted.
func TestFindingDraftWithoutRefsIsAcceptedWithHint(t *testing.T) {
	ctx := testCtx(t)
	f := newRSGFixture(t, ctx)

	res, err := f.svc.CreateObject(ctx, f.alice, f.project.ID, f.branch, rsg.CreateObjectInput{
		ObjectType: "finding",
		Payload:    json.RawMessage(`{"statement":"The claimed uptake has not been reproduced under the stated conditions (claim versions still being gathered).","finding_type":"observation"}`),
	})
	if err != nil {
		t.Fatalf("a ref-less draft finding must be accepted (spec.go:90 reports the schema gap, spec.go:100 is what blocks it — at PR): %v", err)
	}
	seen := false
	for _, h := range res.Hints {
		if h.Code == semantics.HintFindingClaimVersionRefs {
			seen = true
			t.Logf("draft finding accepted with hint %s: %s", h.Code, h.Message)
			if !strings.Contains(h.Message, "claim_version_refs") {
				t.Errorf("hint %q does not name the field the finding is missing", h.Message)
			}
		}
	}
	if !seen {
		t.Errorf("hints = %+v, want %s: the omission must be REPORTED to the caller, not swallowed", res.Hints, semantics.HintFindingClaimVersionRefs)
	}

	// The storage half is untouched: the draft lives in the append-only
	// version log (its payload is the historical truth, and a later
	// version that pins its refs is the sanctioned way to complete it),
	// while the projection row for it cannot be written at all — the
	// guard, not the semantics hint, is what makes a ref-less finding
	// impossible to materialize.
	_, err = f.pool.Exec(ctx, `INSERT INTO findings (version_id, object_id, finding_type, assessment)
		VALUES ($1, $2, 'observation', 'preliminary')`, res.Version.ID, res.Object.ID)
	wantGuardErr(t, "projection row for a ref-less draft finding", err, "must pin at least one claim version")

	// And the upper rung is intact — the SAME check, one gate up. The
	// assembled finding doc without claim_version_refs does not satisfy
	// the finding schema; that is the failure CheckSchemaTyped reports at
	// draft (spec.go:90) and BLOCKS on at PR (spec.go:100), so a ref-less
	// finding can be drafted and can never be merged. The generic
	// draft→PR escalation of this check is proven end to end in
	// validation_gates_test.go:191-212; what is pinned here is that the
	// finding payload is one of the payloads it refuses.
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	fs, ok := reg.Get(schemareg.Ref{ID: "https://open-rd.example/schemas/finding.schema.json", Version: "1"})
	if !ok {
		t.Fatal("finding schema not registered")
	}
	refLess := assembledFindingDoc("observation", "preliminary")
	delete(refLess, "claim_version_refs")
	doc, err := json.Marshal(refLess)
	if err != nil {
		t.Fatalf("marshal ref-less finding doc: %v", err)
	}
	if err := fs.Validate(doc); err == nil {
		t.Error("the finding schema ACCEPTS a payload with no claim_version_refs — then nothing blocks it at PR and the draft hint would be a permanent allowance instead of a draft one")
	}
}
