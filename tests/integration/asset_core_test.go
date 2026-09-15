// Task T0701 required test "asset core" — the storage-layer half of the
// research asset identity machinery, against a REAL PostgreSQL
// (docs/66 §3: task/run-scoped namespaced database, no mocks):
//
//   - the pid column exists, is unique, is fixed-shape (CHECK), and its
//     DEFAULT backfill produces valid pids;
//   - the acceptance criterion: a pid survives a slug rename and an
//     organization transfer — the identity (and the persistent URL built
//     from it) does not change when mutable metadata does;
//   - the 4-type enum is a database guarantee (CHECK), not a Go
//     convention;
//   - origin_refs is mandatory on every published version (NOT NULL, at
//     least one element, no NULL element — the version schema's array of
//     strings with minItems 1);
//   - the upgrade path: rows that predate migration 00064 get backfilled
//     with a valid pid and their asset's origin project ref.
//
// The Go-side halves of the same contract (the pid alphabet, the closed
// type set, the URL builders) are pinned by the internal/assets unit
// suite; the migration catalog shape is pinned by TestFreshInstallCatalog.

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/testdb"
)

const assetCoreTaskID = "T0701"

// assetPIDLen mirrors the pid length that internal/assets (unexported
// pidLen) and the migration CHECK both fix. It is repeated here on
// purpose: the tests below assert their own cases sit at this length, so
// the pid shape has one numeric source of truth per layer and a change to
// either layer fails loudly here.
const assetPIDLen = 26

// assetPIDAlphabet is the Crockford base32 set the migration CHECK admits,
// repeated here for the same reason as assetPIDLen: it lets the off-length
// cases assert they are alphabet-legal, so only the length rule can refuse
// them.
const assetPIDAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"

func mustQueryUUID(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
		t.Fatalf("setup query failed: %s: %v", sql, err)
	}
	return id
}

// seedAssetFixture inserts one user, two organizations, one project (in
// the first org) and returns the ids — the minimal fixture the asset
// rows reference.
func seedAssetFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (userID, orgID, otherOrgID, projectID string) {
	t.Helper()
	userID = mustQueryUUID(t, ctx, pool, `INSERT INTO users (handle, display_name) VALUES ('alice', 'Alice') RETURNING id`)
	orgID = mustQueryUUID(t, ctx, pool, `INSERT INTO organizations (slug, name) VALUES ('acme', 'Acme') RETURNING id`)
	otherOrgID = mustQueryUUID(t, ctx, pool, `INSERT INTO organizations (slug, name) VALUES ('beta', 'Beta') RETURNING id`)
	projectID = mustQueryUUID(t, ctx, pool, `INSERT INTO projects (organization_id, slug, name, purpose, visibility, created_by)
		VALUES ($1, 'p1', 'P1', 'testing asset core', 'private', $2) RETURNING id`, orgID, userID)
	return userID, orgID, otherOrgID, projectID
}

// sqlState (defined in pullrequest_test.go) extracts the SQLSTATE of a
// rejected statement and fatals when the statement did not fail with a
// database error — exactly the assertion these tests want: the storage
// layer must refuse, loudly.

func TestAssetCorePID(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), assetCoreTaskID)
	_, _, _, p1 := seedAssetFixture(t, ctx, pool)

	pid, err := assets.NewPID()
	if err != nil {
		t.Fatal(err)
	}
	ra := mustQueryUUID(t, ctx, pool, `INSERT INTO research_assets (asset_type, slug, title, origin_project_id, pid)
		VALUES ('dataset', 'ds-1', 'DS1', $1, $2) RETURNING id`, p1, string(pid))

	readPID := func() (string, string) {
		var gotPID, slug string
		if err := pool.QueryRow(ctx, `SELECT pid, slug FROM research_assets WHERE id = $1`, ra).Scan(&gotPID, &slug); err != nil {
			t.Fatalf("read asset: %v", err)
		}
		return gotPID, slug
	}

	gotPID, slug := readPID()
	if gotPID != string(pid) {
		t.Fatalf("pid = %q, want %q", gotPID, pid)
	}
	if slug != "ds-1" {
		t.Fatalf("slug = %q, want ds-1", slug)
	}

	// The pid is not the row id: the row id is a uuid, the pid is the
	// public identity.
	if ra == gotPID {
		t.Fatalf("pid %q must not equal the row id %q", gotPID, ra)
	}
	if !assets.ValidPID(gotPID) {
		t.Fatalf("stored pid %q does not satisfy the pid shape", gotPID)
	}

	// The persistent URL is a pure function of the pid — pin it before
	// any metadata moves.
	urlBefore := assets.AssetURL(assets.PID(gotPID))

	// Acceptance: a slug rename changes nothing about the identity.
	if _, err := pool.Exec(ctx, `UPDATE research_assets SET slug = 'renamed-ds' WHERE id = $1`, ra); err != nil {
		t.Fatalf("slug rename: %v", err)
	}
	afterRename, renamedSlug := readPID()
	if afterRename != string(pid) {
		t.Errorf("acceptance: pid changed across a slug rename: %q -> %q", pid, afterRename)
	}
	if renamedSlug != "renamed-ds" {
		t.Errorf("slug rename did not land: %q", renamedSlug)
	}

	// Acceptance: an organization transfer (the origin project moves to
	// another org) changes nothing about the identity.
	if _, err := pool.Exec(ctx, `UPDATE projects SET organization_id = (SELECT id FROM organizations WHERE slug = 'beta') WHERE id = $1`, p1); err != nil {
		t.Fatalf("org transfer: %v", err)
	}
	afterTransfer, _ := readPID()
	if afterTransfer != string(pid) {
		t.Errorf("acceptance: pid changed across an org transfer: %q -> %q", pid, afterTransfer)
	}

	// The URL is byte-identical after both mutations — "persistent URL"
	// is the same guarantee as the acceptance criterion.
	if urlAfter := assets.AssetURL(assets.PID(afterTransfer)); urlAfter != urlBefore {
		t.Errorf("persistent URL changed: %q -> %q", urlBefore, urlAfter)
	}

	// Uniqueness: a second asset with the same pid is refused by the
	// database, not by application luck.
	if _, err := pool.Exec(ctx, `INSERT INTO research_assets (asset_type, slug, title, origin_project_id, pid)
		VALUES ('dataset', 'ds-2', 'DS2', $1, $2)`, p1, string(pid)); sqlState(t, err) != "23505" {
		t.Errorf("duplicate pid: state = %q, want 23505", sqlState(t, err))
	}

	// Shape: a pid outside the Crockford shape is refused by the CHECK
	// (the storage layer is the backstop behind the Go validation). The
	// CHECK has the same two rules as assets.ValidPID — the fixed length
	// and the alphabet — and each group below can only be refused by the
	// rule it names, with that membership asserted: an off-length case
	// must be entirely alphabet-legal, and a bad-character case must be
	// exactly assetPIDLen long. Without those assertions a case tests the
	// wrong rule silently, which is how the first T0701 delivery left
	// both rules unguarded (its reject values were all off-length, so the
	// character class could be deleted with the suite still green; and
	// off-length values that contain illegal characters likewise never
	// exercise the length check).
	inAlphabet := func(s string) bool {
		for _, r := range s {
			if !strings.ContainsRune(assetPIDAlphabet, r) {
				return false
			}
		}
		return true
	}
	offLength := []string{"", "abc", "0123456789abcdefghjkmnpqr", "0123456789abcdefghjkmnpqrsv"}
	for _, bad := range offLength {
		if len(bad) == assetPIDLen {
			t.Errorf("off-length pid case %q is %d chars: it must be refused for its length", bad, assetPIDLen)
		}
		if !inAlphabet(bad) {
			t.Errorf("off-length pid case %q contains a character outside the alphabet: only the length rule may refuse it", bad)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO research_assets (asset_type, slug, title, origin_project_id, pid)
			VALUES ('dataset', 'ds-x', 'DSX', $1, $2)`, p1, bad); sqlState(t, err) != "23514" {
			t.Errorf("bad pid %q: state = %q, want 23514", bad, sqlState(t, err))
		}
	}
	badChar := []string{
		"0123456789ABCDEFGHJKMNPQRS", // uppercase, not emitted
		"0123456789abcdefghjkmnpqri", // 'i' is not in the alphabet
		"my-dataset-version-1-2026-", // a slug is not a pid
	}
	for _, bad := range badChar {
		if len(bad) != assetPIDLen {
			t.Errorf("character pid case %q is %d chars, want %d: only the alphabet may refuse it", bad, len(bad), assetPIDLen)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO research_assets (asset_type, slug, title, origin_project_id, pid)
			VALUES ('dataset', 'ds-x', 'DSX', $1, $2)`, p1, bad); sqlState(t, err) != "23514" {
			t.Errorf("bad pid %q: state = %q, want 23514", bad, sqlState(t, err))
		}
	}

	// The DEFAULT is the backfill path, not the product path — but it
	// must produce the same shape the Go side generates, so a row that
	// lands without an explicit pid still carries a valid one.
	var defaultedPID string
	if err := pool.QueryRow(ctx, `INSERT INTO research_assets (asset_type, slug, title, origin_project_id)
		VALUES ('protocol', 'pr-1', 'PR1', $1) RETURNING pid`, p1).Scan(&defaultedPID); err != nil {
		t.Fatalf("insert with default pid: %v", err)
	}
	if !assets.ValidPID(defaultedPID) {
		t.Errorf("DEFAULT pid %q does not satisfy the pid shape", defaultedPID)
	}
}

func TestAssetCoreTypeEnum(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), assetCoreTaskID)
	_, _, _, p1 := seedAssetFixture(t, ctx, pool)

	// The four V1 types are the closed set (docs/11 §2) — each is
	// storable.
	for _, ty := range assets.AllTypes() {
		if _, err := pool.Exec(ctx, `INSERT INTO research_assets (asset_type, slug, title, origin_project_id)
			VALUES ($1, $2, 'T', $3)`, string(ty), string(ty)+"-slug", p1); err != nil {
			t.Errorf("type %q refused by the database: %v", ty, err)
		}
	}
	// Anything else is refused by the CHECK — the enum is a database
	// guarantee, not a Go convention.
	for _, bad := range []string{"model", "paper", "code", "software", "Dataset", " dataset"} {
		if _, err := pool.Exec(ctx, `INSERT INTO research_assets (asset_type, slug, title, origin_project_id)
			VALUES ($1, 'x-slug', 'X', $2)`, bad, p1); sqlState(t, err) != "23514" {
			t.Errorf("type %q: state = %q, want 23514", bad, sqlState(t, err))
		}
	}
}

// TestAssetCoreTypeSetMatchesSchema pins the third copy of the type set.
// The four V1 types live in three places — internal/assets.AllTypes(), the
// research_assets.asset_type CHECK (pinned against Go by
// TestAssetCoreTypeEnum above) and the enum of
// specs/schemas/research-asset-version.schema.json. Nothing else compares
// the Go list with the JSON schema: check-schema-drift only checks that the
// three copies of specs/schemas agree with each other. This test is what
// makes that third pair actually pinned instead of coincidentally equal.
func TestAssetCoreTypeSetMatchesSchema(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "specs", "schemas", "research-asset-version.schema.json"))
	if err != nil {
		t.Fatalf("read research-asset-version.schema.json: %v", err)
	}
	var doc struct {
		Properties struct {
			AssetType struct {
				Enum []string `json:"enum"`
			} `json:"asset_type"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse research-asset-version.schema.json: %v", err)
	}
	schemaTypes := slices.Clone(doc.Properties.AssetType.Enum)
	if len(schemaTypes) == 0 {
		t.Fatal("research-asset-version.schema.json carries no asset_type enum — this parity check would be vacuous")
	}
	goTypes := make([]string, 0, len(assets.AllTypes()))
	for _, ty := range assets.AllTypes() {
		goTypes = append(goTypes, string(ty))
	}
	sort.Strings(goTypes)
	sort.Strings(schemaTypes)
	if !slices.Equal(goTypes, schemaTypes) {
		t.Errorf("asset type sets diverge: internal/assets %v vs schema enum %v", goTypes, schemaTypes)
	}
}

// TestAssetCoreVisibilityEnum is the same pin as TestAssetCoreTypeEnum
// for the other closed value domain of an identity row:
// internal/assets.Visibility is documented as "exactly the two values
// research_asset_versions.visibility admits (migration 00010)", and
// TestVisibilityValid only measures the Go half of that sentence. This is
// the half that notices the database CHECK being widened on its own —
// without it, a migration admitting "internal" would leave every test in
// the tree green while the comment kept claiming two values.
func TestAssetCoreVisibilityEnum(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), assetCoreTaskID)
	u1, _, _, p1 := seedAssetFixture(t, ctx, pool)
	ra := mustQueryUUID(t, ctx, pool, `INSERT INTO research_assets (asset_type, slug, title, origin_project_id)
		VALUES ('dataset', 'ds-1', 'DS1', $1) RETURNING id`, p1)

	insert := func(label, visibility string) error {
		_, err := pool.Exec(ctx, `INSERT INTO research_asset_versions
			(asset_id, version, manifest, rights_json, visibility, integrity_hash, published_by, origin_refs)
			VALUES ($1, $2, '{}'::jsonb, '{}'::jsonb, $3, 'h', $4, ARRAY['project:' || $5::text])`,
			ra, label, visibility, u1, p1)
		return err
	}
	for _, v := range []assets.Visibility{assets.VisibilityPublic, assets.VisibilityPrivate} {
		if err := insert("v-"+string(v), string(v)); err != nil {
			t.Errorf("visibility %q refused by the database: %v", v, err)
		}
	}
	for i, bad := range []string{"internal", "unlisted", "Public", "public ", ""} {
		if err := insert(fmt.Sprintf("bad-%d", i), bad); sqlState(t, err) != "23514" {
			t.Errorf("visibility %q: state = %q, want 23514", bad, sqlState(t, err))
		}
	}
}

// TestAssetCoreVersionLabelShapeIsNotAStorageRule pins an absence, so
// that the comment claiming it is measured rather than asserted:
// research_asset_versions.version has no shape CHECK (00010 constrains it
// only with NOT NULL and UNIQUE(asset_id, version); TestFreshInstallCatalog
// reads that catalog independently). A dot-segment label is therefore
// storable if a caller skips validation — ValidVersionLabel and whatever
// T0705's publish command enforces are the whole gate, by design: the
// label vocabulary is the domain's, and the database is not where a
// version scheme is defined. The test fails if a label CHECK is ever
// added, which is the point at which that comment (and the split of
// responsibility it describes) needs a deliberate decision.
func TestAssetCoreVersionLabelShapeIsNotAStorageRule(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), assetCoreTaskID)
	u1, _, _, p1 := seedAssetFixture(t, ctx, pool)
	ra := mustQueryUUID(t, ctx, pool, `INSERT INTO research_assets (asset_type, slug, title, origin_project_id)
		VALUES ('dataset', 'ds-1', 'DS1', $1) RETURNING id`, p1)

	for _, label := range []string{"..", "1/2", "not a label"} {
		if assets.ValidVersionLabel(label) {
			t.Errorf("label %q must be refused by ValidVersionLabel for this test to mean anything", label)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO research_asset_versions
			(asset_id, version, manifest, rights_json, visibility, integrity_hash, published_by, origin_refs)
			VALUES ($1, $2, '{}'::jsonb, '{}'::jsonb, 'public', 'h', $3, ARRAY['project:' || $4::text])`,
			ra, label, u1, p1); err != nil {
			t.Errorf("label %q refused by the database: %v — the storage layer constrains presence and uniqueness, not the label's shape", label, err)
		}
	}
}

func TestAssetCoreOriginRefs(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), assetCoreTaskID)
	u1, _, _, p1 := seedAssetFixture(t, ctx, pool)
	ra := mustQueryUUID(t, ctx, pool, `INSERT INTO research_assets (asset_type, slug, title, origin_project_id)
		VALUES ('dataset', 'ds-1', 'DS1', $1) RETURNING id`, p1)

	ref, ok := assets.NewOriginRef(assets.KindProject, p1)
	if !ok {
		t.Fatalf("NewOriginRef(project, %s) not ok", p1)
	}

	// A version with its origin pinned is storable, and the stored refs
	// round-trip.
	av := mustQueryUUID(t, ctx, pool, `INSERT INTO research_asset_versions
		(asset_id, version, manifest, rights_json, visibility, integrity_hash, published_by, origin_refs)
		VALUES ($1, '1.0', '{}'::jsonb, '{}'::jsonb, 'public', 'h', $2, $3) RETURNING id`, ra, u1, []string{string(ref)})
	var stored []string
	if err := pool.QueryRow(ctx, `SELECT origin_refs FROM research_asset_versions WHERE id = $1`, av).Scan(&stored); err != nil {
		t.Fatalf("read origin_refs: %v", err)
	}
	if len(stored) != 1 || stored[0] != string(ref) {
		t.Fatalf("origin_refs = %v, want [%s]", stored, ref)
	}

	// Origin refs are mandatory, and the mandate is exactly the version
	// schema's own type: an array of strings with at least one element.
	// There are three ways to store "no origin" and each is refused by
	// the rule named here — the column absent (NOT NULL), the array
	// empty (cardinality), and an array whose entries are NULL, which
	// has length but names nothing (array_position). The NULL cases are
	// the ones cardinality alone lets through.
	noOrigin := []struct {
		name  string
		label string
		stmt  string
		args  []any
		want  string
	}{
		{"column absent", "2.0",
			`INSERT INTO research_asset_versions
			 (asset_id, version, manifest, rights_json, visibility, integrity_hash, published_by)
			 VALUES ($1, $2, '{}'::jsonb, '{}'::jsonb, 'public', 'h', $3)`, []any{ra, "2.0", u1}, "23502"},
		{"empty array", "2.1",
			`INSERT INTO research_asset_versions
			 (asset_id, version, manifest, rights_json, visibility, integrity_hash, published_by, origin_refs)
			 VALUES ($1, $2, '{}'::jsonb, '{}'::jsonb, 'public', 'h', $3, ARRAY[]::text[])`, []any{ra, "2.1", u1}, "23514"},
		{"one NULL element", "2.2",
			`INSERT INTO research_asset_versions
			 (asset_id, version, manifest, rights_json, visibility, integrity_hash, published_by, origin_refs)
			 VALUES ($1, $2, '{}'::jsonb, '{}'::jsonb, 'public', 'h', $3, ARRAY[NULL]::text[])`, []any{ra, "2.2", u1}, "23514"},
		{"NULL next to a real ref", "2.3",
			`INSERT INTO research_asset_versions
			 (asset_id, version, manifest, rights_json, visibility, integrity_hash, published_by, origin_refs)
			 VALUES ($1, $2, '{}'::jsonb, '{}'::jsonb, 'public', 'h', $3, ARRAY[$4::text, NULL]::text[])`, []any{ra, "2.3", u1, string(ref)}, "23514"},
	}
	for _, c := range noOrigin {
		if _, err := pool.Exec(ctx, c.stmt, c.args...); sqlState(t, err) != c.want {
			t.Errorf("%s: state = %q, want %q", c.name, sqlState(t, err), c.want)
		}
	}

	// What the database guarantees stops at "at least one non-NULL
	// string". It is not a validation of the ref text: the version
	// schema types the elements as plain strings, so a ref with an
	// unknown kind, a non-uuid value or no separator at all is
	// schema-legal and the storage layer accepts it — the kind:value
	// vocabulary is internal/assets.OriginRef's (NewOriginRef, Valid),
	// enforced on the application path. Pinned here so that adding a
	// CHECK on the ref text later is a deliberate change to the storage
	// contract, not an accident, and so the guarantee the comment in
	// migration 00064 states is the one the tests measure.
	for i, refs := range [][]string{{"bogus"}, {"nonsense:" + p1}, {""}} {
		if _, err := pool.Exec(ctx, `INSERT INTO research_asset_versions
			(asset_id, version, manifest, rights_json, visibility, integrity_hash, published_by, origin_refs)
			VALUES ($1, $2, '{}'::jsonb, '{}'::jsonb, 'public', 'h', $3, $4)`,
			ra, fmt.Sprintf("3.%d", i), u1, refs); err != nil {
			t.Errorf("origin_refs %v refused by the database: %v — the storage layer types the elements as text and the schema does not constrain their shape", refs, err)
		}
	}

	// Immutable versions: the version label is per-asset unique (the
	// identity pair (pid, version) has one row, forever), and the row
	// itself is append-only.
	if _, err := pool.Exec(ctx, `INSERT INTO research_asset_versions
		(asset_id, version, manifest, rights_json, visibility, integrity_hash, published_by, origin_refs)
		VALUES ($1, '1.0', '{}'::jsonb, '{}'::jsonb, 'public', 'h', $2, $3)`, ra, u1, []string{string(ref)}); sqlState(t, err) != "23505" {
		t.Errorf("duplicate version label: state = %q, want 23505", sqlState(t, err))
	}
	if _, err := pool.Exec(ctx, `UPDATE research_asset_versions SET version = 'REWRITTEN' WHERE id = $1`, av); sqlState(t, err) != "P0001" {
		t.Errorf("version UPDATE: state = %q, want P0001 (append-only guard)", sqlState(t, err))
	}
}

// TestAssetCoreUpgradeBackfill proves the migration's own upgrade path:
// rows written before 00064 existed (without pid / origin_refs) survive
// the migration with a valid, unique pid and their origin project ref —
// the identity is assigned once and never re-derived.
func TestAssetCoreUpgradeBackfill(t *testing.T) {
	ctx := testCtx(t)
	pool, url := testdb.SetupEmpty(t, ctx, adminURL(t), assetCoreTaskID)

	if _, err := persistence.MigrateTo(ctx, url, 53); err != nil {
		t.Fatalf("migrate to 53: %v", err)
	}
	if v := appliedVersion(t, ctx, pool); v != 53 {
		t.Fatalf("migrate to 53: applied version = %d, want 53", v)
	}
	u1, _, _, p1 := seedAssetFixture(t, ctx, pool)
	// Pre-00064 rows: no pid, no origin_refs — the shapes the old
	// schema knew.
	ra1 := mustQueryUUID(t, ctx, pool, `INSERT INTO research_assets (asset_type, slug, title, origin_project_id)
		VALUES ('dataset', 'ds-1', 'DS1', $1) RETURNING id`, p1)
	ra2 := mustQueryUUID(t, ctx, pool, `INSERT INTO research_assets (asset_type, slug, title, origin_project_id)
		VALUES ('benchmark', 'bm-1', 'BM1', $1) RETURNING id`, p1)
	mustQueryUUID(t, ctx, pool, `INSERT INTO research_asset_versions
		(asset_id, version, manifest, rights_json, visibility, integrity_hash, published_by)
		VALUES ($1, '1.0', '{}'::jsonb, '{}'::jsonb, 'private', 'h', $2) RETURNING id`, ra1, u1)
	mustQueryUUID(t, ctx, pool, `INSERT INTO research_asset_versions
		(asset_id, version, manifest, rights_json, visibility, integrity_hash, published_by)
		VALUES ($1, '1.0', '{}'::jsonb, '{}'::jsonb, 'private', 'h', $2) RETURNING id`, ra2, u1)

	if _, err := persistence.Migrate(ctx, url); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	if v := appliedVersion(t, ctx, pool); v != maxVersionNo {
		t.Fatalf("migrate to head: applied version = %d, want %d", v, maxVersionNo)
	}

	read := func(id string) (string, []string) {
		var pid string
		var refs []string
		if err := pool.QueryRow(ctx, `SELECT pid FROM research_assets WHERE id = $1`, id).Scan(&pid); err != nil {
			t.Fatalf("read pid: %v", err)
		}
		if err := pool.QueryRow(ctx, `SELECT origin_refs FROM research_asset_versions WHERE asset_id = $1`, id).Scan(&refs); err != nil {
			t.Fatalf("read origin_refs: %v", err)
		}
		return pid, refs
	}

	pid1, refs1 := read(ra1)
	pid2, _ := read(ra2)
	if !assets.ValidPID(pid1) || !assets.ValidPID(pid2) {
		t.Errorf("backfilled pids %q / %q do not satisfy the pid shape", pid1, pid2)
	}
	if pid1 == pid2 {
		t.Errorf("backfilled pids must be distinct: both are %q", pid1)
	}
	wantRef := "project:" + p1
	if len(refs1) != 1 || refs1[0] != wantRef {
		t.Errorf("backfilled origin_refs = %v, want [%s]", refs1, wantRef)
	}

	// The backfilled identity is as stable as an application-generated
	// one: a rename after the upgrade leaves it untouched.
	if _, err := pool.Exec(ctx, `UPDATE research_assets SET slug = 'renamed' WHERE id = $1`, ra1); err != nil {
		t.Fatalf("rename after upgrade: %v", err)
	}
	after, _ := read(ra1)
	if after != pid1 {
		t.Errorf("backfilled pid changed across a rename: %q -> %q", pid1, after)
	}

	// The backfill lifted the append-only guard for its own repair — and
	// the guard must be armed again when the migration is done. The
	// failure mode of the disable/enable pattern is forgetting the
	// enable: version rows would become silently rewritable history.
	var avID string
	if err := pool.QueryRow(ctx, `SELECT id FROM research_asset_versions WHERE asset_id = $1`, ra1).Scan(&avID); err != nil {
		t.Fatalf("read version id: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE research_asset_versions SET version = 'REWRITTEN' WHERE id = $1`, avID); sqlState(t, err) != "P0001" {
		t.Errorf("append-only guard after backfill migration: state = %q, want P0001", sqlState(t, err))
	}
}
