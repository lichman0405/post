package integration

// External Reference live identity + snapshot gates (task T0508,
// migration 00045). These are the "external ref tests" required by the
// task package: the four requirements (unique live identity, snapshot
// metadata/hash/accessed_at, manual refresh semantics, citation/
// dependency relations) and the two acceptance criteria (旧 snapshot 固定
// — a pinned snapshot row never changes; upstream refresh 不改历史 — a
// refresh only ever appends) are proven against REAL PostgreSQL, through
// raw SQL, so every guard fires on the exact write paths the database
// polices (docs/66 §3: no mocks).
//
// The Go-side pieces these pins mirror: internal/domain
// (NormalizeExternalIdentifier, CanonicalExternalReferenceSourceTypes),
// internal/rsg/relationcatalog (ExternalRefTargetTypes) and
// internal/rsg/externalref (the refresh adapter, unit-tested in its own
// package — the acceptance criteria live there at unit level).

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rsg/relationcatalog"
	"github.com/lichman0405/post/internal/rsg/schemareg"
)

// extRefFixture is the minimal user → org → project → branch → state
// graph (two projects: A hosts the relations under test, B is the
// foreign project for the cross-project negatives), plus the write
// helpers every test uses.
type extRefFixture struct {
	pool   *pgxpool.Pool
	userID string
	projA  string
	projB  string
	stateA string
	stateB string
}

func newExternalRefFixture(t *testing.T, ctx context.Context) *extRefFixture {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), "T0508")

	mustID := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("external ref fixture: %s: %v", sql, err)
		}
		return id
	}
	userID := mustID(`INSERT INTO users (handle, display_name) VALUES ('extref', 'ExtRef') RETURNING id`)
	orgID := mustID(`INSERT INTO organizations (slug, name) VALUES ('extref-org', 'ExtRef Org') RETURNING id`)
	f := &extRefFixture{pool: pool, userID: userID}
	for i, proj := range []struct {
		slug    string
		projOut *string
		stateID *string
	}{
		{"extref-a", &f.projA, &f.stateA},
		{"extref-b", &f.projB, &f.stateB},
	} {
		*proj.projOut = mustID(`INSERT INTO projects (organization_id, slug, name, purpose, visibility, created_by)
			VALUES ($1, $2, $2, 'testing external references', 'private', $3) RETURNING id`, orgID, proj.slug, userID)
		branchID := mustID(`INSERT INTO branches (project_id, name, visibility, git_ref, created_by)
			VALUES ($1, 'main', 'private', 'refs/heads/main', $2) RETURNING id`, *proj.projOut, userID)
		_ = i
		*proj.stateID = mustID(`INSERT INTO project_states (project_id, branch_id, state_hash, manifest_version)
			VALUES ($1, $2, 'hash-extref', 'v1') RETURNING id`, *proj.projOut, branchID)
	}
	return f
}

// addObjectVersion writes one scientific object of the given type and its
// version 1, with the payload fields jsonb-encoded. Returns both ids.
func (f *extRefFixture) addObjectVersion(t *testing.T, ctx context.Context, projectID, stateID, objectType string, payload map[string]any) (objectID, versionID string) {
	t.Helper()
	var err error
	err = f.pool.QueryRow(ctx,
		`INSERT INTO scientific_objects (project_id, object_type, created_by)
		 VALUES ($1, $2, $3) RETURNING id`, projectID, objectType, f.userID).Scan(&objectID)
	if err != nil {
		t.Fatalf("external ref fixture: seed object: %v", err)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("external ref fixture: marshal payload: %v", err)
	}
	err = f.pool.QueryRow(ctx,
		`INSERT INTO scientific_object_versions
			(object_id, version_no, state_id, schema_id, schema_version, title,
			 lifecycle_state, payload, integrity_hash, created_by)
		 VALUES ($1, 1, $2, 'core/external_reference', '1.0', 'Ref', 'active',
		         $3::jsonb, 'ih-extref', $4) RETURNING id`,
		objectID, stateID, string(raw), f.userID).Scan(&versionID)
	if err != nil {
		t.Fatalf("external ref fixture: seed version: %v", err)
	}
	return objectID, versionID
}

// addExternalRef is the common case: an external_reference object whose
// payload carries the identity fields.
func (f *extRefFixture) addExternalRef(t *testing.T, ctx context.Context, projectID, stateID string, sourceType, externalIdentifier, canonicalURL string) (objectID, versionID string) {
	t.Helper()
	payload := map[string]any{"source_type": sourceType, "external_identifier": externalIdentifier}
	if canonicalURL != "" {
		payload["canonical_url"] = canonicalURL
	}
	return f.addObjectVersion(t, ctx, projectID, stateID, "external_reference", payload)
}

// addRelation writes one relation and its version 1.
func (f *extRefFixture) addRelation(t *testing.T, ctx context.Context, projectID, stateID, relationType, sourceVersionID, targetVersionID string) (relationID, versionID string) {
	t.Helper()
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO relations (project_id) VALUES ($1) RETURNING id`, projectID).Scan(&relationID); err != nil {
		t.Fatalf("external ref fixture: seed relation: %v", err)
	}
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO relation_versions
			(relation_id, version_no, state_id, relation_type,
			 source_object_version_id, target_object_version_id,
			 payload, integrity_hash, created_by)
		 VALUES ($1, 1, $2, $3, $4, $5, '{}'::jsonb, 'ih-extref', $6) RETURNING id`,
		relationID, stateID, relationType, sourceVersionID, targetVersionID, f.userID).Scan(&versionID); err != nil {
		t.Fatalf("external ref fixture: seed relation version: %v", err)
	}
	return relationID, versionID
}

// tryRelation writes one relation and its version 1 and returns the
// write error, for the negative cells (the deferred endpoint guard
// raises at the Exec's commit).
func (f *extRefFixture) tryRelation(ctx context.Context, projectID, stateID, relationType, sourceVersionID, targetVersionID string) error {
	var relationID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO relations (project_id) VALUES ($1) RETURNING id`, projectID).Scan(&relationID); err != nil {
		return err
	}
	_, err := f.pool.Exec(ctx, `INSERT INTO relation_versions
		(relation_id, version_no, state_id, relation_type,
		 source_object_version_id, target_object_version_id,
		 payload, integrity_hash, created_by)
		VALUES ($1, 1, $2, $3, $4, $5, '{}'::jsonb, 'ih-extref', $6)`,
		relationID, stateID, relationType, sourceVersionID, targetVersionID, f.userID)
	return err
}

// addIdentity inserts an external_references row directly (the path the
// identity sync upsert and any future refresh wiring also use).
func (f *extRefFixture) addIdentity(t *testing.T, ctx context.Context, sourceType, externalIdentifier string) string {
	t.Helper()
	var id string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO external_references (source_type, external_identifier)
		 VALUES ($1, $2) RETURNING id`, sourceType, externalIdentifier).Scan(&id); err != nil {
		t.Fatalf("external ref fixture: seed identity: %v", err)
	}
	return id
}

// wantPgErr asserts err carries the expected SQLSTATE.
func wantPgErr(t *testing.T, stmt string, err error, wantState string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("%s: expected a PostgreSQL error (SQLSTATE %s), got %v", stmt, wantState, err)
	}
	if pgErr.Code != wantState {
		t.Errorf("%s: SQLSTATE = %s, want %s", stmt, pgErr.Code, wantState)
	}
}

// ---------------------------------------------------------------------------
// Requirement 1: external id uniqueness — the live identity.

// TestExternalReferenceIdentitySync proves the identity row exists for
// ANY write path: a version payload carrying the identity pair creates
// the identity row at COMMIT, keyed by the NORMALIZED pair, shared
// across projects, first writer winning the canonical_url.
func TestExternalReferenceIdentitySync(t *testing.T) {
	ctx := testCtx(t)
	f := newExternalRefFixture(t, ctx)

	// Project A references the DOI in its loudest spelling: scheme
	// prefix + uppercase. The stored identity is the normalized DOI.
	f.addExternalRef(t, ctx, f.projA, f.stateA, "publication", "doi:10.1000/XYZ", "https://doi.org/10.1000/xyz")

	var srcType, identifier, canonicalURL string
	if err := f.pool.QueryRow(ctx,
		`SELECT source_type, external_identifier, canonical_url FROM external_references`).Scan(&srcType, &identifier, &canonicalURL); err != nil {
		t.Fatalf("identity sync: probe row: %v", err)
	}
	if srcType != "publication" || identifier != "10.1000/xyz" {
		t.Fatalf("identity row = (%s, %s), want (publication, 10.1000/xyz) — the identifier must be normalized", srcType, identifier)
	}
	if canonicalURL != "https://doi.org/10.1000/xyz" {
		t.Fatalf("canonical_url = %q, want the first writer's URL", canonicalURL)
	}

	// Project B references the SAME DOI, bare and lowercase, and does
	// not name a URL: still ONE identity row (the pair is unique after
	// normalization), and the existing URL is not overwritten.
	f.addExternalRef(t, ctx, f.projB, f.stateB, "publication", "10.1000/xyz", "")

	var count int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM external_references`).Scan(&count); err != nil {
		t.Fatalf("identity sync: count: %v", err)
	}
	if count != 1 {
		t.Fatalf("identity rows = %d, want 1 — two projects naming one DOI share ONE identity", count)
	}
	if err := f.pool.QueryRow(ctx,
		`SELECT canonical_url FROM external_references WHERE source_type = 'publication' AND external_identifier = '10.1000/xyz'`).Scan(&canonicalURL); err != nil {
		t.Fatalf("identity sync: re-probe: %v", err)
	}
	if canonicalURL != "https://doi.org/10.1000/xyz" {
		t.Fatalf("canonical_url after second writer = %q, want the first writer's (first writer wins)", canonicalURL)
	}

	// A different pair is a different identity.
	f.addExternalRef(t, ctx, f.projA, f.stateA, "web", "https://example.com/paper", "https://example.com/paper")
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM external_references`).Scan(&count); err != nil {
		t.Fatalf("identity sync: count: %v", err)
	}
	if count != 2 {
		t.Fatalf("identity rows = %d, want 2 (a distinct pair is a distinct identity)", count)
	}
}

// TestExternalReferenceIdentityGuards proves the identity invariant on
// every write path: empty identity fields are refused (P0001), a half
// identity in a version payload is refused (P0001), and a draft without
// an identity is tolerated (the pr gate is the authority on whether the
// omission matters).
func TestExternalReferenceIdentityGuards(t *testing.T) {
	ctx := testCtx(t)
	f := newExternalRefFixture(t, ctx)

	// Direct identity writes: the pair is the identity, neither half may
	// be empty (P0001 — the guard raises before NOT NULL is consulted).
	_, err := f.pool.Exec(ctx, `INSERT INTO external_references (source_type, external_identifier) VALUES (NULL, 'x')`)
	wantPgErr(t, "INSERT external_references with NULL source_type", err, "P0001")
	_, err = f.pool.Exec(ctx, `INSERT INTO external_references (source_type, external_identifier) VALUES ('', 'x')`)
	wantPgErr(t, "INSERT external_references with blank source_type", err, "P0001")
	_, err = f.pool.Exec(ctx, `INSERT INTO external_references (source_type, external_identifier) VALUES ('web', '')`)
	wantPgErr(t, "INSERT external_references with blank external_identifier", err, "P0001")
	// The blank judgement trims with the explicit ASCII set: a tab-only
	// field is blank in both worlds (the Go semantics check and this
	// guard trim with the same set — see the drift test).
	_, err = f.pool.Exec(ctx, `INSERT INTO external_references (source_type, external_identifier) VALUES ('web', E'\t')`)
	wantPgErr(t, "INSERT external_references with tab-only external_identifier", err, "P0001")
	_, err = f.pool.Exec(ctx, `INSERT INTO external_references (source_type, external_identifier) VALUES (E'\x0b', 'x')`)
	wantPgErr(t, "INSERT external_references with VT-only source_type", err, "P0001")

	// Half a pair in a version payload: mechanically wrong, refused at
	// COMMIT by the deferred sync trigger (P0001).
	var objectID string
	if err := f.pool.QueryRow(ctx, `INSERT INTO scientific_objects (project_id, object_type, created_by)
		VALUES ($1, 'external_reference', $2) RETURNING id`, f.projA, f.userID).Scan(&objectID); err != nil {
		t.Fatalf("seed half-pair object: %v", err)
	}
	_, err = f.pool.Exec(ctx, `INSERT INTO scientific_object_versions
		(object_id, version_no, state_id, schema_id, schema_version, title,
		 lifecycle_state, payload, integrity_hash, created_by)
		VALUES ($1, 1, $2, 'core/external_reference', '1.0', 'Ref', 'active',
		        '{"source_type": "publication"}'::jsonb, 'ih-half', $3)`,
		objectID, f.stateA, f.userID)
	wantPgErr(t, "INSERT version with half an identity pair", err, "P0001")

	// A draft without any identity: tolerated, and no identity row is
	// fabricated for it.
	f.addObjectVersion(t, ctx, f.projA, f.stateA, "external_reference", map[string]any{"title": "draft"})
	var count int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM external_references`).Scan(&count); err != nil {
		t.Fatalf("identity guards: count: %v", err)
	}
	if count != 0 {
		t.Fatalf("identity rows = %d, want 0 — a draft without an identity must not fabricate one", count)
	}
}

// TestExternalReferenceIdentityGuardStoresTrimmedSourceType proves the
// identity guard stores the TRIMMED spelling of source_type, never the
// caller's: a direct INSERT of a whitespace-wrapped source_type (a write
// path the schema gate ladder never sees) must land on the SAME identity
// row as the clean spelling, with the stored value trimmed — otherwise
// one DOI would key two identities. Red without the guard's trim
// assignment: the wrapped insert stores its own spelling and the clean
// insert adds a SECOND row instead of colliding.
func TestExternalReferenceIdentityGuardStoresTrimmedSourceType(t *testing.T) {
	ctx := testCtx(t)
	f := newExternalRefFixture(t, ctx)

	f.addIdentity(t, ctx, " publication \t", "10.1000/trim")

	var stored string
	if err := f.pool.QueryRow(ctx,
		`SELECT source_type FROM external_references WHERE external_identifier = '10.1000/trim'`).Scan(&stored); err != nil {
		t.Fatalf("probe stored source_type: %v", err)
	}
	if stored != "publication" {
		t.Fatalf("stored source_type = %q, want %q — the row stores the trimmed spelling, never the caller's", stored, "publication")
	}

	// The clean spelling must COLLIDE with that row (the unique pair
	// matches after trimming): one identity, not two.
	_, err := f.pool.Exec(ctx,
		`INSERT INTO external_references (source_type, external_identifier) VALUES ('publication', '10.1000/trim')`)
	wantPgErr(t, "clean INSERT colliding with the wrapped spelling's identity row", err, "23505")

	var count int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM external_references`).Scan(&count); err != nil {
		t.Fatalf("count identities: %v", err)
	}
	if count != 1 {
		t.Fatalf("identity rows = %d, want 1 — the whitespace-wrapped spelling and the clean spelling are ONE identity", count)
	}
}

// ---------------------------------------------------------------------------
// Requirement 2 + acceptance criteria: snapshots.

// TestExternalReferenceSnapshotGuards proves the snapshot contract end
// to end: the derived hash always pins the stored bytes, a lying
// supplied hash is refused, non-object metadata and future accessed_at
// are refused, the log is append-only (UPDATE/DELETE refused by
// 00014/00015), and — the acceptance criterion 旧 snapshot 固定 — an
// existing snapshot row is byte-identical after a later snapshot lands.
func TestExternalReferenceSnapshotGuards(t *testing.T) {
	ctx := testCtx(t)
	f := newExternalRefFixture(t, ctx)
	refID := f.addIdentity(t, ctx, "publication", "10.1000/snap")

	// accessedAtExpr is a SQL timestamptz expression (fixed test
	// constants, never user input) so the cells stay correct relative to
	// the database's own clock.
	insertSnapshot := func(metadata, hash string, accessedAtExpr string) (string, error) {
		var id string
		err := f.pool.QueryRow(ctx, `INSERT INTO external_reference_snapshots
			(external_reference_id, accessed_at, upstream_version, metadata, snapshot_hash)
			VALUES ($1, `+accessedAtExpr+`, 'v1', $2::jsonb, NULLIF($3, '')::text) RETURNING id`,
			refID, metadata, hash).Scan(&id)
		return id, err
	}
	justNow := "now() - interval '1 hour'"

	// A NULL hash is derived server-side and pins the stored bytes.
	snapID, err := insertSnapshot(`{"title": "A", "n": 1}`, "", justNow)
	if err != nil {
		t.Fatalf("first snapshot insert: %v", err)
	}
	var storedHash, derived string
	if err := f.pool.QueryRow(ctx,
		`SELECT snapshot_hash, encode(digest(metadata::text, 'sha256'), 'hex')
		   FROM external_reference_snapshots WHERE id = $1`, snapID).Scan(&storedHash, &derived); err != nil {
		t.Fatalf("probe derived hash: %v", err)
	}
	if storedHash == "" || storedHash != derived {
		t.Fatalf("derived hash = %q, want sha256 of the stored metadata text (%q)", storedHash, derived)
	}

	// A supplied hash that disagrees with the stored bytes is refused —
	// a caller can never assert a hash the row does not have (P0001).
	_, err = insertSnapshot(`{"title": "B"}`, "deadbeef", justNow)
	wantPgErr(t, "INSERT snapshot with a hash that does not match", err, "P0001")

	// A supplied hash that DOES agree is accepted (the guard verifies,
	// it does not require NULL).
	agreeing := func(metadata string) string {
		var h string
		if err := f.pool.QueryRow(ctx,
			`SELECT encode(digest($1::jsonb::text, 'sha256'), 'hex')`, metadata).Scan(&h); err != nil {
			t.Fatalf("compute agreeing hash: %v", err)
		}
		return h
	}
	if _, err := insertSnapshot(`{"title": "C"}`, agreeing(`{"title": "C"}`), justNow); err != nil {
		t.Fatalf("snapshot with a matching supplied hash refused: %v", err)
	}

	// metadata must be a JSON object — a snapshot pins a document, never
	// a scalar, array or null (P0001).
	for _, scalar := range []string{`"x"`, `42`, `[1,2]`, `null`} {
		_, err = insertSnapshot(scalar, "", justNow)
		wantPgErr(t, "INSERT snapshot with non-object metadata "+scalar, err, "P0001")
	}

	// accessed_at records when upstream WAS observed — the future is not
	// observable (P0001). The guard's clock is clock_timestamp(), the
	// database's ACTUAL current time, and the tolerance is zero: a stamp
	// an hour ahead of it is still refused, full stop.
	_, err = insertSnapshot(`{}`, "", "clock_timestamp() + interval '1 hour'")
	wantPgErr(t, "INSERT snapshot with future accessed_at", err, "P0001")

	// A REAL observation inside a long transaction lands: accessed_at is
	// stamped by the database (clock_timestamp()) after an in-transaction
	// sleep, so it is necessarily ahead of now() — the transaction start.
	// The guard must NOT reject it: it records when upstream WAS observed
	// relative to the actual clock, not relative to when the transaction
	// began.
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin long-transaction observation: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_sleep(1.2)`); err != nil {
		t.Fatalf("in-transaction sleep: %v", err)
	}
	var sleptID string
	if err := tx.QueryRow(ctx, `INSERT INTO external_reference_snapshots
		(external_reference_id, accessed_at, upstream_version, metadata)
		VALUES ($1, clock_timestamp(), 'v1', '{}'::jsonb) RETURNING id`, refID).Scan(&sleptID); err != nil {
		t.Fatalf("in-transaction observation rejected: %v (the guard must compare against clock_timestamp(), not now())", err)
	}
	if sleptID == "" {
		t.Fatalf("in-transaction observation returned no row id")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit long-transaction observation: %v", err)
	}

	// An explicitly empty upstream_version is refused when present
	// (P0001); the NULL spelling is the way to say "upstream reports
	// none".
	_, err = f.pool.Exec(ctx, `INSERT INTO external_reference_snapshots
		(external_reference_id, accessed_at, upstream_version, metadata)
		VALUES ($1, now(), '', '{}'::jsonb)`, refID)
	wantPgErr(t, "INSERT snapshot with blank upstream_version", err, "P0001")
	if _, err := f.pool.Exec(ctx, `INSERT INTO external_reference_snapshots
		(external_reference_id, accessed_at, upstream_version, metadata)
		VALUES ($1, now(), NULL, '{}'::jsonb)`, refID); err != nil {
		t.Fatalf("snapshot without upstream_version refused: %v", err)
	}

	// The log is append-only: UPDATE and DELETE are refused by the
	// 00014/00015 guards (P0001) — history rows are pinned by the
	// database itself.
	_, err = f.pool.Exec(ctx, `UPDATE external_reference_snapshots SET metadata = '{"hacked": true}'::jsonb WHERE id = $1`, snapID)
	wantPgErr(t, "UPDATE external_reference_snapshots (append-only)", err, "P0001")
	_, err = f.pool.Exec(ctx, `DELETE FROM external_reference_snapshots WHERE id = $1`, snapID)
	wantPgErr(t, "DELETE FROM external_reference_snapshots (append-only)", err, "P0001")

	// 旧 snapshot 固定: the first snapshot row is byte-identical after a
	// later snapshot lands — a refresh appends, it never rewrites what
	// exists.
	var before string
	if err := f.pool.QueryRow(ctx,
		`SELECT (id || '|' || accessed_at || '|' || coalesce(upstream_version,'') || '|' || metadata::text || '|' || snapshot_hash || '|' || coalesce(blob_id::text,''))
		   FROM external_reference_snapshots WHERE id = $1`, snapID).Scan(&before); err != nil {
		t.Fatalf("probe first snapshot before append: %v", err)
	}
	if _, err := insertSnapshot(`{"title": "D"}`, "", "now()"); err != nil {
		t.Fatalf("later snapshot insert: %v", err)
	}
	var after string
	if err := f.pool.QueryRow(ctx,
		`SELECT (id || '|' || accessed_at || '|' || coalesce(upstream_version,'') || '|' || metadata::text || '|' || snapshot_hash || '|' || coalesce(blob_id::text,''))
		   FROM external_reference_snapshots WHERE id = $1`, snapID).Scan(&after); err != nil {
		t.Fatalf("probe first snapshot after append: %v", err)
	}
	if before != after {
		t.Fatalf("first snapshot changed after a later one landed:\nbefore: %s\nafter:  %s", before, after)
	}
}

// ---------------------------------------------------------------------------
// Requirement 4: citation/dependency relations.

// TestExternalReferenceRelationEndpoints proves docs/19 §3 in the
// database: only references (citation) and depends_on (dependency) may
// point AT an external reference, an external reference may never be a
// relation source, and the target object must live in the relation's
// project.
func TestExternalReferenceRelationEndpoints(t *testing.T) {
	ctx := testCtx(t)
	f := newExternalRefFixture(t, ctx)

	_, claimV := f.addObjectVersion(t, ctx, f.projA, f.stateA, "claim", map[string]any{"statement": "MOF-5 is water-stable."})
	_, extRefAV := f.addExternalRef(t, ctx, f.projA, f.stateA, "publication", "10.1000/rel", "")
	_, extRefBV := f.addExternalRef(t, ctx, f.projB, f.stateB, "publication", "10.1000/foreign", "")

	// The admitted pair: citation and dependency both land (addRelation
	// fails the test on any write error).
	for _, relType := range []string{"references", "depends_on"} {
		f.addRelation(t, ctx, f.projA, f.stateA, relType, claimV, extRefAV)
	}

	// Any other type targeting an external reference is refused (P0001).
	err := f.tryRelation(ctx, f.projA, f.stateA, "uses", claimV, extRefAV)
	wantPgErr(t, "INSERT uses → external_reference (not an admitted type)", err, "P0001")

	// An external reference may never be a relation SOURCE (P0001).
	err = f.tryRelation(ctx, f.projA, f.stateA, "references", extRefAV, claimV)
	wantPgErr(t, "INSERT relation with external_reference as source", err, "P0001")

	// A citation is a claim about the relation's OWN project: a target
	// external reference object from another project is refused (P0001).
	err = f.tryRelation(ctx, f.projA, f.stateA, "references", claimV, extRefBV)
	wantPgErr(t, "INSERT relation targeting another project's external_reference", err, "P0001")

	// The policy table itself is sealed: no ordinary write path may add
	// or change the admitted set — 'contains' can never be admitted
	// silently (P0001, the seal fires for INSERT, UPDATE and DELETE
	// alike). The set changes only through a migration that drops the
	// seal first.
	for _, stmt := range []string{
		`INSERT INTO external_reference_relation_types (relation_type) VALUES ('contains')`,
		`UPDATE external_reference_relation_types SET relation_type = 'contains' WHERE relation_type = 'references'`,
		`DELETE FROM external_reference_relation_types WHERE relation_type = 'references'`,
	} {
		_, err = f.pool.Exec(ctx, stmt)
		wantPgErr(t, stmt, err, "P0001")
	}
}

// ---------------------------------------------------------------------------
// Drift pins: the DB declarations and the Go catalogs are one truth in
// two copies; each pair is pinned so a change in one copy fails loudly
// here.

// TestExternalReferenceNormalizationDrift pins the SQL normalization
// function to the Go one, input by input — including every member of the
// explicit ASCII whitespace set on the edges and inside. The blank input
// is refused in both worlds (the Go semantic check and the DB guard raise
// on it BEFORE normalization runs), so it is covered by the identity
// guard test above, not here.
func TestExternalReferenceNormalizationDrift(t *testing.T) {
	ctx := testCtx(t)
	f := newExternalRefFixture(t, ctx)

	inputs := []string{
		"10.1000/xyz",
		"10.1000/XYZ",
		"doi:10.1000/xyz",
		"DOI:10.1000/xyz",
		"doi: 10.1000/xyz",
		"https://doi.org/10.1000/xyz",
		"https://dx.doi.org/10.1000/XYZ",
		"http://doi.org/10.123456789/suffix",
		" 10.1000/xyz ",
		"US1234567B2",
		"arXiv:2401.00001",
		"https://example.com/paper",
		// The whitespace edges the two copies must agree on, one input per
		// member of the explicit ASCII set (SP TAB LF CR FF VT), plus an
		// identifier whose VT sits INSIDE (DOI-shaped on neither side —
		// both must pass it through unchanged) and a doi: prefix followed
		// by a VT (consumed by the prefix class on both sides).
		"\t10.1000/ABC\t",
		"10.1000/xyz\n",
		"\r10.1000/xyz\r",
		"\f10.1000/xyz\f",
		"\v10.1000/xyz\v",
		"10.1000/ABC\vXYZ",
		"doi:\v10.1000/xyz",
	}
	for _, in := range inputs {
		goNormalized, err := domain.NormalizeExternalIdentifier(in)
		if err != nil {
			t.Fatalf("Go normalization of %q failed: %v", in, err)
		}
		var sqlNormalized string
		if err := f.pool.QueryRow(ctx,
			`SELECT external_reference_normalize_identifier($1)`, in).Scan(&sqlNormalized); err != nil {
			t.Fatalf("SQL normalization of %q: %v", in, err)
		}
		if goNormalized != sqlNormalized {
			t.Errorf("normalization drift for %q: Go = %q, SQL = %q", in, goNormalized, sqlNormalized)
		}
	}

	// The source_type trim contract: the Go emptiness/trim judgement
	// (domain.ASCIIWhitespace, the set nonEmptyString in the semantics
	// check trims with) and the SQL trim the guards judge with
	// (external_reference_trim) must agree value by value — a value that
	// is blank on one side may not be non-blank on the other.
	sourceTypes := []string{
		"publication",
		"\tdoi\t",
		"\vweb\v",
		"\fstandard\f",
		"\rpatent\r",
		"database\n",
		"  vendor  ",
	}
	for _, st := range sourceTypes {
		goTrimmed := strings.Trim(st, domain.ASCIIWhitespace)
		var sqlTrimmed string
		if err := f.pool.QueryRow(ctx,
			`SELECT external_reference_trim($1)`, st).Scan(&sqlTrimmed); err != nil {
			t.Fatalf("SQL trim of %q: %v", st, err)
		}
		if goTrimmed != sqlTrimmed {
			t.Errorf("source_type trim drift for %q: Go = %q, SQL = %q", st, goTrimmed, sqlTrimmed)
		}
	}
}

// TestExternalReferenceCollationPrecondition pins the database collation
// requirement that migration 00045's header states: SQL lower()
// case-folds non-ASCII characters ('Ä' -> 'ä') only under a
// Unicode-aware collation, while the Go copy (strings.ToLower) folds
// Unicode unconditionally. The identity sync keys DOI spellings on SQL
// lower(); on a C-collation database the two copies disagree on
// non-ASCII spellings and silently build two identity rows for one DOI.
// This test FAILS on such a database on purpose — the precondition is a
// red build, not a silent duplicate.
func TestExternalReferenceCollationPrecondition(t *testing.T) {
	ctx := testCtx(t)
	f := newExternalRefFixture(t, ctx)

	// The fold itself: under a C collation lower('Ä') IS 'Ä' and this
	// reads false.
	var folds bool
	if err := f.pool.QueryRow(ctx, `SELECT lower('Ä') = 'ä'`).Scan(&folds); err != nil {
		t.Fatalf("probe collation fold: %v", err)
	}
	if !folds {
		t.Fatal("database collation is not Unicode-aware: lower('Ä') <> 'ä' — migration 00045 requires a database created with a Unicode-aware collation (see its header); recreate the database with one")
	}

	// Through the identity surface: a non-ASCII DOI spelling syncs to the
	// SAME identity as its Go-normalized form. Go folds unconditionally;
	// SQL lower() must agree, or the unique pair would split into two
	// rows for one DOI.
	f.addExternalRef(t, ctx, f.projA, f.stateA, "publication", "10.1000/ÄBC", "")

	var stored string
	if err := f.pool.QueryRow(ctx,
		`SELECT external_identifier FROM external_references`).Scan(&stored); err != nil {
		t.Fatalf("probe synced identity: %v", err)
	}
	goForm, err := domain.NormalizeExternalIdentifier("10.1000/ÄBC")
	if err != nil {
		t.Fatalf("Go normalization of non-ASCII DOI: %v", err)
	}
	if stored != goForm {
		t.Fatalf("synced identity = %q, Go normalization = %q — SQL lower() and strings.ToLower disagree (the database is not on a Unicode-aware collation)", stored, goForm)
	}
}

// TestExternalReferenceRelationTypesDrift pins the database declaration
// of admitted relation types to the Go catalog.
func TestExternalReferenceRelationTypesDrift(t *testing.T) {
	ctx := testCtx(t)
	f := newExternalRefFixture(t, ctx)

	rows, err := f.pool.Query(ctx, `SELECT relation_type FROM external_reference_relation_types ORDER BY relation_type`)
	if err != nil {
		t.Fatalf("relation types drift: %v", err)
	}
	defer rows.Close()
	var dbTypes []string
	for rows.Next() {
		var typ string
		if err := rows.Scan(&typ); err != nil {
			t.Fatalf("relation types drift: %v", err)
		}
		dbTypes = append(dbTypes, typ)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("relation types drift: %v", err)
	}
	goTypes := relationcatalog.ExternalRefTargetTypes()
	if len(dbTypes) != len(goTypes) {
		t.Fatalf("relation types drift: DB %v vs catalog %v", dbTypes, goTypes)
	}
	for i := range dbTypes {
		if dbTypes[i] != goTypes[i] {
			t.Fatalf("relation types drift: DB %v vs catalog %v", dbTypes, goTypes)
		}
	}
}

// TestExternalReferenceSourceTypesDrift pins the domain's canonical
// source type list to the embedded schema enum: every canonical type
// validates, and anything else is refused by the schema itself.
func TestExternalReferenceSourceTypesDrift(t *testing.T) {
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	schema, err := reg.Lookup(schemareg.Ref{ID: schemareg.CanonicalNamespace + "external_reference.schema.json", Version: schemareg.CanonicalV1})
	if err != nil {
		t.Fatalf("lookup external_reference schema: %v", err)
	}
	if typ, ok := schema.TypeConst(); !ok || typ != "external_reference" {
		t.Fatalf("schema type const = %q (ok=%v), want external_reference", typ, ok)
	}

	buildDoc := func(sourceType string) []byte {
		doc := map[string]any{
			"id":                  "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
			"type":                "external_reference",
			"version":             1,
			"project_id":          "p",
			"title":               "t",
			"lifecycle_state":     "active",
			"schema_ref":          map[string]any{"id": "x", "version": "1"},
			"created_by":          "u",
			"created_at":          "2026-09-14T10:00:00Z",
			"source_type":         sourceType,
			"external_identifier": "10.1000/x",
		}
		raw, err := json.Marshal(doc)
		if err != nil {
			t.Fatalf("marshal doc: %v", err)
		}
		return raw
	}

	for _, sourceType := range domain.CanonicalExternalReferenceSourceTypes() {
		if err := schema.Validate(buildDoc(sourceType)); err != nil {
			t.Errorf("schema refused canonical source type %q: %v", sourceType, err)
		}
	}
	if err := schema.Validate(buildDoc("article")); err == nil {
		t.Error("schema accepted a non-canonical source type — the domain list and the schema enum have drifted apart")
	}
}
