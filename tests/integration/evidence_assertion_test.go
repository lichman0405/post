package integration

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rsg/schemareg"
)

// Task T0504: Evidence Assertion Domain.
//
// The evidence tests required by the task, against REAL PostgreSQL in a
// task-scoped namespace database (test_T0504_<run_id>), proving by making
// things fail that migration 00058's guards hold for ANY write path (raw
// SQL here — the domain services are covered by their unit tests):
//
//   - the nine canonical relations are all accepted, and nothing outside
//     them is;
//   - evidence_type / directness / inference_nature / scope enums are
//     CHECKed, value for value, against the schema;
//   - the version pins hold: both endpoints must name existing
//     scientific_object_versions, and a pinned version cannot disappear
//     while an assertion points at it (ON DELETE RESTRICT);
//   - supports and contradicts COEXIST on the same pair (task acceptance
//     criterion: 支持与反驳可同时存在) — the table must not collapse
//     them into one stance;
//   - no truth score: the table carries no score/weight/truth column
//     (docs/10 §4: V1 不自动赋数值权重; CLAUDE.md §9.13);
//   - the domain enum vocabulary and the schema enum vocabulary cannot
//     drift.

const evidenceTaskID = "T0504"

// evidenceFixture seeds the minimal raw-SQL graph the evidence tests
// need: alice → org → project → branch → state, plus a target claim and
// an evidence experiment, each with one version. Like knowledgeFixture,
// the tests write SQL directly — the guards are database constraints, and
// the invariant must hold for any write path.
type evidenceFixture struct {
	pool    *pgxpool.Pool
	alice   string
	project string
	state   string
	// targetVersion is the pinned target (a claim version);
	// evidenceVersion is the pinned evidence (an experiment version).
	targetVersion   string
	evidenceVersion string
}

func newEvidenceFixture(t *testing.T, ctx context.Context) *evidenceFixture {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), evidenceTaskID)
	uid := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("seed: %s: %v", sql, err)
		}
		return id
	}
	alice := uid(`INSERT INTO users (handle, display_name) VALUES ('alice', 'Alice') RETURNING id`)
	org := uid(`INSERT INTO organizations (slug, name) VALUES ('acme', 'Acme') RETURNING id`)
	project := uid(`INSERT INTO projects (organization_id, slug, name, purpose, visibility, created_by)
		VALUES ($1, 'ep', 'EP', 'testing evidence assertions', 'private', $2) RETURNING id`, org, alice)
	branch := uid(`INSERT INTO branches (project_id, name, visibility, git_ref, created_by)
		VALUES ($1, 'main', 'private', 'refs/heads/main', $2) RETURNING id`, project, alice)
	state := uid(`INSERT INTO project_states (project_id, branch_id, state_hash, manifest_version)
		VALUES ($1, $2, 'hash-e', 'v1') RETURNING id`, project, branch)

	mkObject := func(objectType string) (objectID, versionID string) {
		t.Helper()
		objectID = uid(`INSERT INTO scientific_objects (project_id, object_type, created_by)
			VALUES ($1, $2, $3) RETURNING id`, project, objectType, alice)
		versionID = uid(`INSERT INTO scientific_object_versions
			(object_id, version_no, state_id, schema_id, schema_version, title,
			 lifecycle_state, payload, integrity_hash, created_by)
			VALUES ($1, 1, $2, 'core/' || $3, '1', 'v1', 'active', '{}'::jsonb, 'ih-e', $4) RETURNING id`,
			objectID, state, objectType, alice)
		return objectID, versionID
	}
	_, targetVersion := mkObject("claim")
	_, evidenceVersion := mkObject("experiment")
	return &evidenceFixture{pool: pool, alice: alice, project: project, state: state,
		targetVersion: targetVersion, evidenceVersion: evidenceVersion}
}

// assert inserts one evidence assertion row; it returns the insert error
// (nil when the guard let it pass) so the tests can probe violations.
func (f *evidenceFixture) assert(ctx context.Context, relation, evidenceType, directness, inferenceNature string, scope string) error {
	_, err := f.pool.Exec(ctx, `INSERT INTO evidence_assertions
		(project_id, state_id, target_object_version_id, evidence_object_version_id,
		 relation_type, evidence_type, scope, directness, inference_nature, reasoning_note, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9, 'n', $10)`,
		f.project, f.state, f.targetVersion, f.evidenceVersion,
		relation, evidenceType, scope, directness, inferenceNature, f.alice)
	return err
}

func wantPGState(t *testing.T, stmt string, err error, wantState string) {
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

func TestEvidenceAssertionContract(t *testing.T) {
	ctx := testCtx(t)
	f := newEvidenceFixture(t, ctx)

	count := func() int {
		t.Helper()
		var n int
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM evidence_assertions`).Scan(&n); err != nil {
			t.Fatalf("count assertions: %v", err)
		}
		return n
	}

	// --- all nine canonical relations land --------------------------------
	for _, rel := range domain.CanonicalEvidenceRelations() {
		if err := f.assert(ctx, rel, "experimental", "direct", "observational", `{}`); err != nil {
			t.Errorf("assertion with canonical relation %q rejected: %v", rel, err)
		}
	}
	if n := count(); n != 9 {
		t.Fatalf("assertions after the nine canonical relations = %d, want 9", n)
	}

	// --- supports and contradicts coexist on the same pair ---------------
	// Task acceptance criterion (支持与反驳可同时存在): the SAME evidence
	// version may carry a supporting AND a contradicting assertion on the
	// SAME target version, side by side — no unique constraint on the
	// pair, no stance collapsing, no winner.
	if err := f.assert(ctx, "supports", "experimental", "direct", "causal", `{}`); err != nil {
		t.Fatalf("supports assertion rejected: %v", err)
	}
	if err := f.assert(ctx, "contradicts", "experimental", "direct", "causal", `{}`); err != nil {
		t.Fatalf("contradicts assertion rejected: %v", err)
	}
	type stanceRow struct {
		ID           string
		RelationType string
	}
	var stances []stanceRow
	rows, err := f.pool.Query(ctx, `SELECT id, relation_type FROM evidence_assertions
		WHERE target_object_version_id = $1 AND evidence_object_version_id = $2
		ORDER BY created_at, id`, f.targetVersion, f.evidenceVersion)
	if err != nil {
		t.Fatalf("query assertions for the pair: %v", err)
	}
	for rows.Next() {
		var s stanceRow
		if err := rows.Scan(&s.ID, &s.RelationType); err != nil {
			t.Fatalf("scan assertion: %v", err)
		}
		stances = append(stances, s)
	}
	rows.Close()
	got := map[string]int{}
	for _, s := range stances {
		got[s.RelationType]++
	}
	if got["supports"] < 1 || got["contradicts"] < 1 {
		t.Errorf("pair holds %v — both stances must coexist as separate rows", got)
	}

	// --- non-canonical relation refused (23514) ---------------------------
	wantPGState(t, `INSERT with relation_type 'correlates'`,
		f.assert(ctx, "correlates", "experimental", "direct", "observational", `{}`), "23514")
	// --- non-canonical evidence_type refused (23514) ----------------------
	wantPGState(t, `INSERT with evidence_type 'anecdote'`,
		f.assert(ctx, "supports", "anecdote", "direct", "observational", `{}`), "23514")
	// --- non-canonical directness refused (23514) -------------------------
	wantPGState(t, `INSERT with directness 'semi-direct'`,
		f.assert(ctx, "supports", "experimental", "semi-direct", "observational", `{}`), "23514")
	// --- non-canonical inference_nature refused (23514) -------------------
	wantPGState(t, `INSERT with inference_nature 'correlational'`,
		f.assert(ctx, "supports", "experimental", "direct", "correlational", `{}`), "23514")
	// --- non-object scope refused (23514) ---------------------------------
	wantPGState(t, `INSERT with a string scope`,
		f.assert(ctx, "supports", "experimental", "direct", "observational", `"298 K"`), "23514")

	// --- self-assertion refused by the database (23514) -------------------
	// domain.EvidenceAssertion.Validate hard-rejects "the same version on
	// both ends" on the happy path; migration 00058's pins-differ CHECK
	// makes that rule unconditional, so this bare-SQL insert — which never
	// touches the domain layer — must be refused by the database itself.
	_, err = f.pool.Exec(ctx, `INSERT INTO evidence_assertions
		(project_id, state_id, target_object_version_id, evidence_object_version_id,
		 relation_type, evidence_type, scope, directness, inference_nature, reasoning_note, created_by)
		VALUES ($1, $2, $3, $3, 'supports', 'experimental', '{}'::jsonb, 'direct', 'observational', 'n', $4)`,
		f.project, f.state, f.targetVersion, f.alice)
	wantPGState(t, "INSERT naming the same version on both ends", err, "23514")

	// --- version pins hold: both endpoints must exist ---------------------
	// A fabricated target pin is refused by the FK (23503).
	_, err = f.pool.Exec(ctx, `INSERT INTO evidence_assertions
		(project_id, state_id, target_object_version_id, evidence_object_version_id,
		 relation_type, evidence_type, scope, directness, inference_nature, reasoning_note, created_by)
		VALUES ($1, $2, gen_random_uuid(), $3, 'supports', 'experimental', '{}'::jsonb, 'direct', 'observational', 'n', $4)`,
		f.project, f.state, f.evidenceVersion, f.alice)
	wantPGState(t, "INSERT with a fabricated target version pin", err, "23503")
	// A fabricated evidence pin is refused too (23503).
	_, err = f.pool.Exec(ctx, `INSERT INTO evidence_assertions
		(project_id, state_id, target_object_version_id, evidence_object_version_id,
		 relation_type, evidence_type, scope, directness, inference_nature, reasoning_note, created_by)
		VALUES ($1, $2, $3, gen_random_uuid(), 'supports', 'experimental', '{}'::jsonb, 'direct', 'observational', 'n', $4)`,
		f.project, f.state, f.targetVersion, f.alice)
	wantPGState(t, "INSERT with a fabricated evidence version pin", err, "23503")

	// A pinned version cannot disappear while an assertion points at it.
	// The DELETE is refused — the version log's append-only guard (00014)
	// fires first (P0001), and the FK's ON DELETE RESTRICT is the
	// declarative second layer (asserted structurally in the migration
	// catalog fixture, fk target/evidence → scientific_object_versions
	// RESTRICT). The assertion keeps meaning after its objects move on,
	// and the exact version it saw is immutable while it lives.
	_, err = f.pool.Exec(ctx, `DELETE FROM scientific_object_versions WHERE id = $1`, f.evidenceVersion)
	wantPGState(t, "DELETE the pinned evidence version", err, "P0001")

	// --- no truth score surface -------------------------------------------
	// Acceptance criterion (不生成 Truth Score): the table carries no
	// score/weight/truth column, and neither does anything else in the
	// catalog — the assertion data can produce labels, never numbers
	// (docs/10 §4, §8; CLAUDE.md §9.13).
	rows, err = f.pool.Query(ctx, `SELECT table_name, column_name FROM information_schema.columns
		WHERE (column_name ILIKE '%score%' OR column_name ILIKE '%weight%' OR column_name ILIKE '%truth%')
		AND table_schema = 'public'`)
	if err != nil {
		t.Fatalf("catalog scan for score columns: %v", err)
	}
	var offenders []string
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			t.Fatalf("scan catalog row: %v", err)
		}
		offenders = append(offenders, table+"."+column)
	}
	rows.Close()
	if len(offenders) != 0 {
		t.Errorf("catalog carries score/weight/truth surfaces: %v — none may exist (docs/10 §4: V1 不自动赋数值权重)", offenders)
	}

	// The listing stays per-relation: counts are data, never a net
	// position. (The query exists to prove the index-backed read path.)
	var n int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM evidence_assertions WHERE target_object_version_id = $1`, f.targetVersion).Scan(&n); err != nil {
		t.Fatalf("list assertions for target: %v", err)
	}
	if n < 11 {
		t.Errorf("assertions for the target = %d, want at least 11 (9 canonical + supports + contradicts)", n)
	}
}

// TestEvidenceAssertionEnumsMatchSchema pins the domain's evidence
// vocabulary to the runtime schema registry in BOTH directions: every
// canonical value of every enum (relation, evidence_type, directness,
// inference_nature, review_state) validates, AND every value the schema
// declares is a value the domain knows. The schema side of that second
// direction is read out of the registered schema document itself
// (Schema.Raw), never out of a hand-written copy of its enums — so a
// value added on either side fails this test instead of drifting.
func TestEvidenceAssertionEnumsMatchSchema(t *testing.T) {
	ctx := testCtx(t)
	_ = newEvidenceFixture(t, ctx) // runs in a real migrated T0504 database, like the rest
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	ref := schemareg.Ref{
		ID:      "https://open-rd.example/schemas/evidence-assertion.schema.json",
		Version: schemareg.CanonicalV1,
	}
	ea, ok := reg.Get(ref)
	if !ok {
		t.Fatalf("evidence-assertion schema not registered under %s", ref)
	}

	// schemaEnumValues reads properties.<field>.enum straight out of the
	// schema document the registry actually compiled — the authority this
	// test pins the domain to, not a copy of it kept here.
	schemaEnumValues := func(field string) []string {
		t.Helper()
		var parsed struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(ea.Raw(), &parsed); err != nil {
			t.Fatalf("registered evidence-assertion schema does not decode: %v", err)
		}
		prop, ok := parsed.Properties[field]
		if !ok {
			t.Fatalf("registered schema declares no properties.%s — this test's premise is gone", field)
		}
		if len(prop.Enum) == 0 {
			t.Fatalf("registered schema's properties.%s carries no enum — this test's premise is gone", field)
		}
		return prop.Enum
	}

	doc := func(relation, evidenceType, directness, inferenceNature, reviewState string) []byte {
		t.Helper()
		raw, err := json.Marshal(map[string]any{
			"id":                   "ev-00000001",
			"target_version_ref":   "obj-00000001",
			"evidence_version_ref": "obj-00000002",
			"relation":             relation,
			"evidence_type":        evidenceType,
			"scope":                map[string]any{"population": "bulk"},
			"directness":           directness,
			"inference_nature":     inferenceNature,
			"reasoning_note":       "n",
			"review_state":         reviewState,
			"created_by":           "user-00000001",
			"created_at":           time.Now().UTC().Format(time.RFC3339),
		})
		if err != nil {
			t.Fatalf("marshal evidence assertion doc: %v", err)
		}
		return raw
	}

	base := func() (relation, evidenceType, directness, inferenceNature, reviewState string) {
		return "supports", "experimental", "direct", "causal", "unreviewed"
	}

	// Every canonical value validates, in its own enum's slot.
	for _, rel := range domain.CanonicalEvidenceRelations() {
		_, et, d, in, rs := base()
		if err := ea.Validate(doc(rel, et, d, in, rs)); err != nil {
			t.Errorf("schema refuses canonical relation %q: %v", rel, err)
		}
	}
	for _, et := range domain.CanonicalEvidenceTypes() {
		r, _, d, in, rs := base()
		if err := ea.Validate(doc(r, et, d, in, rs)); err != nil {
			t.Errorf("schema refuses canonical evidence_type %q: %v", et, err)
		}
	}
	for _, d := range domain.CanonicalEvidenceDirectness() {
		r, et, _, in, rs := base()
		if err := ea.Validate(doc(r, et, d, in, rs)); err != nil {
			t.Errorf("schema refuses canonical directness %q: %v", d, err)
		}
	}
	for _, in := range domain.CanonicalEvidenceInferenceNatures() {
		r, et, d, _, rs := base()
		if err := ea.Validate(doc(r, et, d, in, rs)); err != nil {
			t.Errorf("schema refuses canonical inference_nature %q: %v", in, err)
		}
	}
	for _, rs := range domain.CanonicalEvidenceReviewStates() {
		r, et, d, in, _ := base()
		if err := ea.Validate(doc(r, et, d, in, rs)); err != nil {
			t.Errorf("schema refuses canonical review_state %q: %v", rs, err)
		}
	}

	// The other direction, and the one a list of known-bad values cannot
	// cover: every value the SCHEMA declares must be a value the domain
	// knows. Read from the schema document itself, so a 10th relation (or a
	// new directness, inference nature, evidence type, review state) added
	// on the schema side fails here until the domain is taught it — the
	// domain's Valid* predicate and its canonical list must agree with the
	// schema's enum as a set, never merely contain it.
	enums := []struct {
		field     string
		canonical []string
		known     func(string) bool
	}{
		{"relation", domain.CanonicalEvidenceRelations(), domain.ValidEvidenceRelation},
		{"evidence_type", domain.CanonicalEvidenceTypes(), domain.ValidEvidenceType},
		{"directness", domain.CanonicalEvidenceDirectness(), domain.ValidEvidenceDirectness},
		{"inference_nature", domain.CanonicalEvidenceInferenceNatures(), domain.ValidEvidenceInferenceNature},
		{"review_state", domain.CanonicalEvidenceReviewStates(), domain.ValidEvidenceReviewState},
	}
	for _, e := range enums {
		schemaValues := schemaEnumValues(e.field)
		domainSet := map[string]bool{}
		for _, v := range e.canonical {
			domainSet[v] = true
		}
		schemaSet := map[string]bool{}
		var schemaOnly []string
		for _, v := range schemaValues {
			schemaSet[v] = true
			if !domainSet[v] {
				schemaOnly = append(schemaOnly, v)
			}
			if !e.known(v) {
				t.Errorf("schema's %s enum declares %q, which domain.Valid* does not recognize — teach the domain the value in the same commit", e.field, v)
			}
		}
		var domainOnly []string
		for _, v := range e.canonical {
			if !schemaSet[v] {
				domainOnly = append(domainOnly, v)
			}
		}
		sort.Strings(schemaOnly)
		sort.Strings(domainOnly)
		if len(schemaOnly) > 0 {
			t.Errorf("schema's %s enum declares %v, which the domain's canonical list does not carry — the domain and the schema drifted apart", e.field, schemaOnly)
		}
		if len(domainOnly) > 0 {
			t.Errorf("domain's canonical %s list carries %v, which the schema's enum does not declare — the domain and the schema drifted apart", e.field, domainOnly)
		}
	}

	// Values outside the schema's enums never validate. This is the other
	// half of the pin: the sets above prove the two vocabularies agree, and
	// this proves the schema's enum is actually ENFORCED (a declared enum
	// the validator ignored would accept these).
	reject := []struct {
		field, value string
	}{
		{"relation", "causes"},
		{"relation", "disproves"},
		{"evidence_type", "anecdote"},
		{"evidence_type", "doi"},
		{"directness", "semi-direct"},
		{"inference_nature", "correlational"},
		{"review_state", "accepted"},
	}
	for _, tc := range reject {
		r, et, d, in, rs := base()
		switch tc.field {
		case "relation":
			r = tc.value
		case "evidence_type":
			et = tc.value
		case "directness":
			d = tc.value
		case "inference_nature":
			in = tc.value
		case "review_state":
			rs = tc.value
		}
		if err := ea.Validate(doc(r, et, d, in, rs)); err == nil {
			t.Errorf("schema ACCEPTS %s %q, which no enum of its own declares — the enum is not being enforced", tc.field, tc.value)
		}
	}
}
