package integration

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/lichman0405/post/internal/persistence/testdb"
)

// Claim projection contract (T0502, migration 00041): the claims table is
// the structured face of a claim object version — claim type, subject,
// property, value, scope (structured, with normalized conditions), causal
// basis and assessment as columns. This test drives the table against
// real PostgreSQL and tries to make it fail: every CHECK, FK and PK the
// projection promises is probed with a forbidden statement and the
// SQLSTATE is asserted (not merely "the statement errored").

func TestClaimProjectionContract(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), "T0502")

	mustQueryUUID := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("setup query failed: %s: %v", sql, err)
		}
		return id
	}

	// Minimal RSG graph: user → project → branch → state → claim object
	// → version (the same shape the object write path produces).
	u1 := mustQueryUUID(`INSERT INTO users (handle, display_name) VALUES ('alice', 'Alice') RETURNING id`)
	o1 := mustQueryUUID(`INSERT INTO organizations (slug, name) VALUES ('acme', 'Acme') RETURNING id`)
	p1 := mustQueryUUID(`INSERT INTO projects (organization_id, slug, name, purpose, visibility, created_by)
		VALUES ($1, 'p1', 'P1', 'claim projection test', 'private', $2) RETURNING id`, o1, u1)
	b1 := mustQueryUUID(`INSERT INTO branches (project_id, name, visibility, git_ref, created_by)
		VALUES ($1, 'main', 'private', 'refs/heads/main', $2) RETURNING id`, p1, u1)
	s1 := mustQueryUUID(`INSERT INTO project_states (project_id, branch_id, state_hash, manifest_version)
		VALUES ($1, $2, 'hash-1', 'v1') RETURNING id`, p1, b1)
	co1 := mustQueryUUID(`INSERT INTO scientific_objects (project_id, object_type, created_by)
		VALUES ($1, 'claim', $2) RETURNING id`, p1, u1)
	sov := func(versionNo int) string {
		t.Helper()
		return mustQueryUUID(`INSERT INTO scientific_object_versions
			(object_id, version_no, state_id, schema_id, schema_version, title,
			 lifecycle_state, payload, integrity_hash, created_by)
			VALUES ($1, $2, $3, 'https://open-rd.example/schemas/claim.schema.json', '1',
			        'Claim', 'active', '{}'::jsonb, 'ih-1', $4) RETURNING id`,
			co1, versionNo, s1, u1)
	}
	v1 := sov(1)

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

	// --- a well-formed projection row lands and round-trips --------------
	const insert = `INSERT INTO claims
		(version_id, object_id, claim_type, subject_ref, property, value, scope, scope_conditions, basis, assessment)
		VALUES ($1, $2, 'quantitative', 'obj-MOF-5', 'BET surface area',
		        '{"value":3800,"unit":"m2/g"}'::jsonb,
		        '{"population":"as-synthesized powder"}'::jsonb,
		        '[{"name":"temperature","value":77,"unit":"K"}]'::jsonb,
		        '[]'::jsonb, 'preliminary')`
	if _, err := pool.Exec(ctx, insert, v1, co1); err != nil {
		t.Fatalf("valid claim projection insert: %v", err)
	}
	var (
		gotType, gotSubject, gotAssessment string
		gotBasis                           json.RawMessage
		gotConditions                      json.RawMessage
	)
	if err := pool.QueryRow(ctx,
		`SELECT claim_type, subject_ref, assessment, basis, scope_conditions FROM claims WHERE version_id = $1`, v1).
		Scan(&gotType, &gotSubject, &gotAssessment, &gotBasis, &gotConditions); err != nil {
		t.Fatalf("read claim projection: %v", err)
	}
	if gotType != "quantitative" || gotSubject != "obj-MOF-5" || gotAssessment != "preliminary" {
		t.Errorf("row = %s/%s/%s, want quantitative/obj-MOF-5/preliminary", gotType, gotSubject, gotAssessment)
	}
	if string(gotBasis) != "[]" {
		t.Errorf("basis = %s, want []", gotBasis)
	}
	if string(gotConditions) != `[{"name": "temperature", "unit": "K", "value": 77}]` {
		t.Errorf("scope_conditions = %s, want the normalized condition (jsonb key order is canonical)", gotConditions)
	}

	// A second claim version is a second row — the projection is
	// per-version, so a Finding pinning v1 never sees v2's content
	// (T0503 acceptance: a new claim version must not silently rewrite
	// what an older Finding saw).
	v2 := sov(2)
	if _, err := pool.Exec(ctx, insert, v2, co1); err != nil {
		t.Fatalf("second claim version projection insert: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM claims WHERE object_id = $1`, co1).Scan(&n); err != nil {
		t.Fatalf("count claim projections: %v", err)
	}
	if n != 2 {
		t.Errorf("projection rows for object = %d, want 2 (one per version)", n)
	}

	// --- the projection is rebuildable, not append-only -------------------
	// docs/21 §5: projections may be rebuilt; the version logs (00014)
	// are immutable, their derived rows are not. An UPDATE here must be
	// ALLOWED — the contrast with scientific_object_versions is the
	// point.
	if _, err := pool.Exec(ctx, `UPDATE claims SET assessment = 'supported' WHERE version_id = $1`, v1); err != nil {
		t.Fatalf("projection rebuild update: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT assessment FROM claims WHERE version_id = $1`, v1).Scan(&gotAssessment); err != nil || gotAssessment != "supported" {
		t.Fatalf("assessment after rebuild = %q, err %v; want supported", gotAssessment, err)
	}

	// --- every constraint the projection promises rejects its violation --
	// claim_type is the schema's canonical enum (23514).
	_, err := pool.Exec(ctx, `UPDATE claims SET claim_type = 'correlational' WHERE version_id = $1`, v1)
	wantErr("UPDATE claims SET claim_type = 'correlational'", err, "23514")
	// assessment is the docs/43 enum, not a free string (23514).
	_, err = pool.Exec(ctx, `UPDATE claims SET assessment = 'accepted' WHERE version_id = $1`, v1)
	wantErr("UPDATE claims SET assessment = 'accepted'", err, "23514")
	// scope must be an object (23514) — a string scope has no structure.
	_, err = pool.Exec(ctx, `UPDATE claims SET scope = '"298 K"'::jsonb WHERE version_id = $1`, v1)
	wantErr(`UPDATE claims SET scope = '"298 K"'`, err, "23514")
	// scope_conditions must be an array of the normalized shape (23514).
	_, err = pool.Exec(ctx, `UPDATE claims SET scope_conditions = '{}'::jsonb WHERE version_id = $1`, v1)
	wantErr("UPDATE claims SET scope_conditions = '{}'", err, "23514")
	// basis must be an array of entries (23514).
	_, err = pool.Exec(ctx, `UPDATE claims SET basis = '"dose-response"'::jsonb WHERE version_id = $1`, v1)
	wantErr(`UPDATE claims SET basis = '"dose-response"'`, err, "23514")
	// version_id must name a real scientific object version (23503).
	_, err = pool.Exec(ctx, `INSERT INTO claims (version_id, object_id, claim_type)
		VALUES (gen_random_uuid(), $1, 'descriptive')`, co1)
	wantErr("INSERT claims with a fabricated version_id", err, "23503")
	// one row per version (23505).
	_, err = pool.Exec(ctx, `INSERT INTO claims (version_id, object_id, claim_type)
		VALUES ($1, $2, 'descriptive')`, v1, co1)
	wantErr("INSERT claims duplicating version_id", err, "23505")

	// A causal basis declaration round-trips as a structured entry, so
	// the validation check has a real field to read (T0502 acceptance:
	// the basis is data, not prose).
	if _, err := pool.Exec(ctx, `UPDATE claims SET
		claim_type = 'causal',
		basis = '[{"type":"controlled_intervention","detail":"pressure swing 1-10 bar"}]'::jsonb
		WHERE version_id = $1`, v2); err != nil {
		t.Fatalf("causal basis update: %v", err)
	}
	var basis []struct {
		Type   string `json:"type"`
		Detail string `json:"detail"`
	}
	if err := pool.QueryRow(ctx, `SELECT basis FROM claims WHERE version_id = $1`, v2).Scan(&gotBasis); err != nil {
		t.Fatalf("read basis: %v", err)
	}
	if err := json.Unmarshal(gotBasis, &basis); err != nil {
		t.Fatalf("basis is not a JSON array of entries: %v", err)
	}
	if len(basis) != 1 || basis[0].Type != "controlled_intervention" || basis[0].Detail != "pressure swing 1-10 bar" {
		t.Errorf("basis = %+v, want one controlled_intervention entry with detail", basis)
	}
}
