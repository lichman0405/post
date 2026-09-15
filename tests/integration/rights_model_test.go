// Task T0703 required test "rights tests" — the storage half of the rights
// model, against a REAL PostgreSQL (docs/66 §3: task/run-scoped namespaced
// database, no mocks):
//
//   - both rights_json columns hold a JSON object and refuse every other
//     JSON kind, each refusal attributed to the constraint that made it;
//   - a document internal/rights produces round-trips through the column
//     byte for byte and is readable at its JSON paths;
//   - the vocabulary is NOT a storage rule: a document the Go model
//     refuses is storable, because the vocabulary has exactly one
//     definition (internal/rights) and a CHECK spelling it again would be
//     a second one to drift from. Pinning the absence is the point — it is
//     the same shape as T0701's
//     TestAssetCoreVersionLabelShapeIsNotAStorageRule;
//   - the empty object stays storable and stays NOT a document: rows that
//     predate the model (the fixtures in asset_core_test.go and
//     append_only_test.go) are not repaired by 00066, deliberately.
//
// The Go half — the document's shape, the vocabularies, the rules — is
// pinned by the internal/rights unit suite; the migration catalog shape is
// pinned by TestFreshInstallCatalog.

package integration

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rights"
)

// rightsTaskID namespaces this task's test databases (test_T0703_<run_id>).
const rightsTaskID = "T0703"

// The constraint each column's rule must be, by name: the tests below
// assert the refusal comes from THIS rule, so a case refused by some
// neighbouring constraint (the NOT NULL, in the scalar cases' reach) fails
// instead of passing by accident.
const (
	assetVersionRightsConstraint = "research_asset_versions_rights_json_document"
	publicationRightsConstraint  = "knowledge_publications_rights_json_document"
)

type rightsFixture struct {
	pool          *pgxpool.Pool
	user          string
	asset         string
	objectVersion string
}

func newRightsFixture(t *testing.T, ctx context.Context) *rightsFixture {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), rightsTaskID)
	f := &rightsFixture{pool: pool}
	f.user = mustQueryUUID(t, ctx, pool,
		`INSERT INTO users (handle, display_name) VALUES ('alice', 'Alice') RETURNING id`)
	org := mustQueryUUID(t, ctx, pool,
		`INSERT INTO organizations (slug, name) VALUES ('acme', 'Acme') RETURNING id`)
	project := mustQueryUUID(t, ctx, pool,
		`INSERT INTO projects (organization_id, slug, name, purpose, visibility, created_by)
		 VALUES ($1, 'p1', 'P1', 'rights model test', 'private', $2) RETURNING id`, org, f.user)
	f.asset = mustQueryUUID(t, ctx, pool,
		`INSERT INTO research_assets (asset_type, slug, title, origin_project_id)
		 VALUES ('dataset', 'ds-1', 'DS1', $1) RETURNING id`, project)

	// A published knowledge object version, for the second rights_json
	// column: branch → state → object → version, the shape the object
	// write path produces.
	branch := mustQueryUUID(t, ctx, pool,
		`INSERT INTO branches (project_id, name, visibility, git_ref, created_by)
		 VALUES ($1, 'main', 'private', 'refs/heads/main', $2) RETURNING id`, project, f.user)
	state := mustQueryUUID(t, ctx, pool,
		`INSERT INTO project_states (project_id, branch_id, state_hash, manifest_version)
		 VALUES ($1, $2, 'hash-1', 'v1') RETURNING id`, project, branch)
	object := mustQueryUUID(t, ctx, pool,
		`INSERT INTO scientific_objects (project_id, object_type, created_by)
		 VALUES ($1, 'dataset', $2) RETURNING id`, project, f.user)
	f.objectVersion = mustQueryUUID(t, ctx, pool,
		`INSERT INTO scientific_object_versions
			(object_id, version_no, state_id, schema_id, schema_version, title,
			 lifecycle_state, payload, integrity_hash, created_by)
		 VALUES ($1, 1, $2, 'https://open-rd.example/schemas/dataset.schema.json', '1',
		         'Dataset', 'active', '{}'::jsonb, 'ih-1', $3) RETURNING id`,
		object, state, f.user)
	return f
}

// writeAssetVersion stores raw rights bytes on a new research asset version
// row, returning the row id and the error the database answered with.
func (f *rightsFixture) writeAssetVersion(t *testing.T, ctx context.Context, label string, raw string) (string, error) {
	t.Helper()
	var id string
	err := f.pool.QueryRow(ctx,
		`INSERT INTO research_asset_versions
			(asset_id, version, manifest, rights_json, visibility, integrity_hash, published_by, origin_refs)
		 VALUES ($1, $2, '{}'::jsonb, $3::jsonb, 'public', 'h', $4, ARRAY['project:0'])
		 RETURNING id`, f.asset, label, raw, f.user).Scan(&id)
	return id, err
}

// writePublication stores raw rights bytes on a new knowledge publication
// row.
func (f *rightsFixture) writePublication(t *testing.T, ctx context.Context, label string, raw string) error {
	t.Helper()
	_, err := f.pool.Exec(ctx,
		`INSERT INTO knowledge_publications (object_version_id, public_version, rights_json, published_by)
		 VALUES ($1, $2, $3::jsonb, $4)`, f.objectVersion, label, raw, f.user)
	return err
}

// constraintOf returns the name of the constraint a PostgreSQL error came
// from, failing the test when the error is not a constraint violation —
// a case refused by a trigger or a type error is not this rule's work.
func constraintOf(t *testing.T, err error) string {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected a PostgreSQL error, got %v", err)
	}
	if pgErr.Code != "23514" {
		t.Fatalf("SQLSTATE = %s (%s), want 23514 (check_violation)", pgErr.Code, pgErr.Message)
	}
	return pgErr.ConstraintName
}

// TestRightsJSONMustBeADocument stores every JSON kind a writer could put
// in the column. The object is accepted; the scalar kinds are refused by
// the named constraint, one refusal per kind — an array and a number are
// as unusable as a string, and a rule that only caught one of them would
// leave the others to be special-cased by every reader.
func TestRightsJSONMustBeADocument(t *testing.T) {
	ctx := testCtx(t)
	f := newRightsFixture(t, ctx)

	if _, err := f.writeAssetVersion(t, ctx, "1.0", `{"version":1}`); err != nil {
		t.Errorf("storing a rights object: %v", err)
	}
	if err := f.writePublication(t, ctx, "v1", `{"version":1}`); err != nil {
		t.Errorf("storing a rights object on a publication: %v", err)
	}

	rejected := []struct {
		name string
		raw  string
	}{
		{"array", `[]`},
		{"string", `"MIT"`},
		{"number", `7`},
		{"boolean", `true`},
		{"json null", `null`},
	}
	for _, tc := range rejected {
		_, err := f.writeAssetVersion(t, ctx, "asset-"+tc.name, tc.raw)
		if err == nil {
			t.Errorf("%s: storing %s on a research asset version succeeded, want a refusal", tc.name, tc.raw)
		} else if got := constraintOf(t, err); got != assetVersionRightsConstraint {
			t.Errorf("%s: refused by %q, want %q", tc.name, got, assetVersionRightsConstraint)
		}

		if err := f.writePublication(t, ctx, "pub-"+tc.name, tc.raw); err == nil {
			t.Errorf("%s: storing %s on a knowledge publication succeeded, want a refusal", tc.name, tc.raw)
		} else if got := constraintOf(t, err); got != publicationRightsConstraint {
			t.Errorf("%s (publication): refused by %q, want %q", tc.name, got, publicationRightsConstraint)
		}
	}
}

// TestRightsDocumentRoundTripsThroughTheColumn is the cross-layer half: a
// document the Go model produces is storable, comes back byte-identical,
// parses back into the same value, and its fields are addressable as JSON
// paths (which is what a query would filter on — a license or a data
// access filter reads the column, not the Go struct).
func TestRightsDocumentRoundTripsThroughTheColumn(t *testing.T) {
	ctx := testCtx(t)
	f := newRightsFixture(t, ctx)

	license := "CC-BY-4.0"
	ref := "agreements/mof-2026.pdf"
	doc := rights.New()
	doc.StandardLicenseID = &license
	doc.CustomAgreementRef = &ref
	doc.Usage.CommercialUse = rights.PermissionRestricted
	doc.Usage.Derivatives = rights.PermissionAllowed
	doc.Usage.Redistribution = rights.PermissionRestricted
	doc.Usage.ModelTraining = rights.PermissionRestricted
	doc.Usage.PatentGrant = rights.PatentGrantSeeAgreement
	doc.Visibility.Metadata = "PUBLIC" // metadata may be public while the bytes stay restricted
	raw, err := doc.Marshal()
	if err != nil {
		t.Fatalf("Marshal() = %v, want nil", err)
	}

	if _, err := f.writeAssetVersion(t, ctx, "1.0", string(raw)); err != nil {
		t.Fatalf("storing the model's document: %v", err)
	}

	var stored []byte
	if err := f.pool.QueryRow(ctx,
		`SELECT rights_json FROM research_asset_versions WHERE asset_id = $1 AND version = '1.0'`,
		f.asset).Scan(&stored); err != nil {
		t.Fatalf("reading rights_json back: %v", err)
	}
	back, err := rights.Parse(stored)
	if err != nil {
		t.Fatalf("Parse(stored) = %v, want nil", err)
	}
	if !reflect.DeepEqual(back, doc) {
		t.Errorf("round trip = %+v\nwant %+v", back, doc)
	}

	// The JSON is queryable at its paths: the two axes the model keeps
	// apart are separately addressable, which is what makes
	// "metadata public, bytes restricted" a filterable state rather than
	// a comment in a Go struct.
	var metadata, dataAccess, licenseAtPath string
	if err := f.pool.QueryRow(ctx,
		`SELECT rights_json->'visibility'->>'metadata',
		        rights_json->'visibility'->>'data_access',
		        rights_json->>'standard_license_id'
		 FROM research_asset_versions WHERE asset_id = $1 AND version = '1.0'`,
		f.asset).Scan(&metadata, &dataAccess, &licenseAtPath); err != nil {
		t.Fatalf("reading rights_json paths: %v", err)
	}
	if metadata != "PUBLIC" || dataAccess != "restricted" || licenseAtPath != "CC-BY-4.0" {
		t.Errorf("paths = (%q, %q, %q), want (PUBLIC, restricted, CC-BY-4.0)", metadata, dataAccess, licenseAtPath)
	}
}

// TestRightsVocabularyIsNotAStorageRule pins the absence this task
// decided on. The column accepts a document whose usage value is a word no
// vocabulary contains, and whose shape carries a field the model does not
// know — the database does not hold a second copy of the vocabulary.
//
// The refusal that matters happens in Go, and it is asserted here too, so
// the pair of assertions says exactly one thing: the rule exists, once.
func TestRightsVocabularyIsNotAStorageRule(t *testing.T) {
	ctx := testCtx(t)
	f := newRightsFixture(t, ctx)

	const offVocabulary = `{"version":1,"usage":{"commercial_use":"banana"},"embargo":"2027-01-01"}`
	if _, err := f.writeAssetVersion(t, ctx, "1.0", offVocabulary); err != nil {
		t.Fatalf("storing a document outside the vocabulary: %v\n"+
			"the column is meant to hold any rights object; the vocabulary lives in internal/rights", err)
	}
	if err := f.writePublication(t, ctx, "v1", offVocabulary); err != nil {
		t.Fatalf("storing a document outside the vocabulary on a publication: %v", err)
	}

	var stored []byte
	if err := f.pool.QueryRow(ctx,
		`SELECT rights_json FROM research_asset_versions WHERE asset_id = $1 AND version = '1.0'`,
		f.asset).Scan(&stored); err != nil {
		t.Fatalf("reading rights_json back: %v", err)
	}
	if _, err := rights.Parse(stored); err == nil {
		t.Fatal("internal/rights accepted a document outside its vocabulary: " +
			"then nothing refuses it, and the storage layer's silence is a hole rather than a division of labour")
	}
}

// TestRightsEmptyObjectIsStorableButNotADocument pins the one gap between
// the column and the model, deliberately left open by 00066: the rows that
// predate the model hold {}, they stay storable untouched, and the model
// refuses to read them as declarations rather than inventing one.
func TestRightsEmptyObjectIsStorableButNotADocument(t *testing.T) {
	ctx := testCtx(t)
	f := newRightsFixture(t, ctx)

	if _, err := f.writeAssetVersion(t, ctx, "1.0", `{}`); err != nil {
		t.Fatalf("storing the empty object: %v\n"+
			"existing fixtures store it (asset_core_test.go, append_only_test.go) and 00066 does not repair rows", err)
	}

	var stored []byte
	if err := f.pool.QueryRow(ctx,
		`SELECT rights_json FROM research_asset_versions WHERE asset_id = $1 AND version = '1.0'`,
		f.asset).Scan(&stored); err != nil {
		t.Fatalf("reading rights_json back: %v", err)
	}
	if string(stored) != "{}" {
		t.Errorf("stored = %s, want {} (the migration must not rewrite it)", stored)
	}
	var verr *rights.ValidationError
	_, err := rights.Parse(stored)
	if !errors.As(err, &verr) || verr.Code != rights.CodeUnsupportedVersion {
		t.Errorf("Parse({}) = %v, want %s: an empty object states no version and is not a document",
			err, rights.CodeUnsupportedVersion)
	}
}

// TestRightsDocumentIsImmutable pins that the new rule did not disturb the
// append-only guard 00014 puts on published versions: rights_json is part
// of the stored document and a document that could be edited after
// publication would make "what the publisher declared" a moving target.
func TestRightsDocumentIsImmutable(t *testing.T) {
	ctx := testCtx(t)
	f := newRightsFixture(t, ctx)

	id, err := f.writeAssetVersion(t, ctx, "1.0", `{"version":1}`)
	if err != nil {
		t.Fatalf("storing a rights object: %v", err)
	}
	_, err = f.pool.Exec(ctx,
		`UPDATE research_asset_versions SET rights_json = '{"version":2}'::jsonb WHERE id = $1`, id)
	if err == nil {
		t.Fatal("updating rights_json on a published version succeeded, want the append-only guard to refuse it")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "P0001" {
		t.Errorf("update refused with %v, want SQLSTATE P0001 from the append-only trigger", err)
	}
}
