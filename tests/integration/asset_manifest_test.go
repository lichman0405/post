// Task T0702 required test "asset validators" — the storage half of the
// four asset-type manifest validator and the publish gate, against a REAL
// PostgreSQL (docs/66 §3: task/run-scoped namespaced database, no mocks):
//
//   - research_asset_versions.manifest holds a manifest DOCUMENT (a JSON
//     object) and refuses every other JSON kind, each refusal attributed
//     to the constraint that made it (migration 00067, the same boundary
//     00066 drew for the sibling rights_json column);
//   - the bytes the publish gate hands back to store survive the column
//     byte-compatibly enough that the integrity hash the gate verified
//     still verifies against the STORED document, for all four asset
//     types — this is what "the asset carries its hash" (docs/11 §3) has
//     to mean in storage, and it is the only place the jsonb round trip
//     can be checked (jsonb reorders keys and normalises spacing, so a
//     verifier re-renders the document with internal/assets instead of
//     trusting the stored bytes);
//   - a document whose bytes were edited after publication no longer
//     hashes to the row's integrity_hash;
//   - the division of labour is pinned rather than assumed: storage
//     refuses the wrong JSON KIND, and accepts documents the Go validator
//     refuses (no required metadata, a rights document with no version) —
//     so the publish checklist of docs/11 §3 has exactly one enforcer,
//     internal/assets.Gate, and a reader may not conclude from a stored
//     row that the row was publishable.
//
// The Go halves — the four required-metadata tables, the manifest
// document rules, the pins and the gate — are pinned by the
// internal/assets unit suite, including the run of the ladder's own asset
// gate over the facts Gate produces. The migration catalog shape is
// pinned by TestFreshInstallCatalog.

package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rights"
)

// assetManifestTaskID namespaces this task's test databases
// (test_T0702_<run_id>).
const assetManifestTaskID = "T0702"

// assetManifestConstraint is the rule the wrong-kind cases must be refused
// BY — asserted by name, so a case that some neighbouring constraint
// happens to catch fails instead of passing by accident.
const assetManifestConstraint = "research_asset_versions_manifest_document"

type assetManifestFixture struct {
	pool  *pgxpool.Pool
	user  string
	asset string
	refs  []string
}

func newAssetManifestFixture(t *testing.T, ctx context.Context) *assetManifestFixture {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), assetManifestTaskID)
	f := &assetManifestFixture{pool: pool, refs: []string{"release:0f4d2c1b-9a87-4653-8b21-7e6f5d4c3b2a"}}
	f.user = mustQueryUUID(t, ctx, pool,
		`INSERT INTO users (handle, display_name) VALUES ('alice', 'Alice') RETURNING id`)
	org := mustQueryUUID(t, ctx, pool,
		`INSERT INTO organizations (slug, name) VALUES ('acme', 'Acme') RETURNING id`)
	project := mustQueryUUID(t, ctx, pool,
		`INSERT INTO projects (organization_id, slug, name, purpose, visibility, created_by)
		 VALUES ($1, 'p1', 'P1', 'asset manifest test', 'private', $2) RETURNING id`, org, f.user)
	f.asset = mustQueryUUID(t, ctx, pool,
		`INSERT INTO research_assets (asset_type, slug, title, origin_project_id)
		 VALUES ('dataset', 'ds-1', 'DS1', $1) RETURNING id`, project)
	return f
}

// writeVersion stores raw manifest bytes (and raw rights bytes) on a new
// version row, returning the row id and the error the database answered
// with. The integrity hash is the caller's: the round-trip test stores
// the hash the gate verified, and the other tests store a placeholder
// because the hash is not what they are asking about.
func (f *assetManifestFixture) writeVersion(t *testing.T, ctx context.Context, label, manifest, rightsJSON, integrityHash string) (string, error) {
	t.Helper()
	var id string
	err := f.pool.QueryRow(ctx,
		`INSERT INTO research_asset_versions
			(asset_id, version, manifest, rights_json, visibility, integrity_hash, published_by, origin_refs)
		 VALUES ($1, $2, $3::jsonb, $4::jsonb, 'public', $5, $6, $7)
		 RETURNING id`, f.asset, label, manifest, rightsJSON, integrityHash, f.user, f.refs).Scan(&id)
	return id, err
}

// TestAssetManifestMustBeADocument stores every JSON kind a writer could
// put in the column. An object — the empty one and a real manifest — is
// accepted; the other kinds are refused BY the named constraint, one
// refusal per kind, because a rule that caught only some of them would
// leave the rest for every reader to special-case.
func TestAssetManifestMustBeADocument(t *testing.T) {
	ctx := testCtx(t)
	f := newAssetManifestFixture(t, ctx)

	accepted := []struct {
		name string
		raw  string
	}{
		{"empty object", `{}`},
		{"a manifest document", `{"version":1,"asset_type":"dataset","metadata":{"purpose":"x"}}`},
	}
	for _, tc := range accepted {
		if _, err := f.writeVersion(t, ctx, "ok-"+tc.name, tc.raw, `{}`, "h"); err != nil {
			t.Errorf("%s: storing %s was refused: %v", tc.name, tc.raw, err)
		}
	}

	rejected := []struct {
		name string
		raw  string
	}{
		{"array", `[{"version":1}]`},
		{"string", `"manifest"`},
		{"number", `7`},
		{"boolean", `true`},
		{"json null", `null`},
	}
	for _, tc := range rejected {
		_, err := f.writeVersion(t, ctx, "bad-"+tc.name, tc.raw, `{}`, "h")
		if err == nil {
			t.Errorf("%s: storing %s in the manifest column succeeded, want a refusal", tc.name, tc.raw)
			continue
		}
		if got := constraintOf(t, err); got != assetManifestConstraint {
			t.Errorf("%s: refused by %q, want %q", tc.name, got, assetManifestConstraint)
		}
	}
}

// TestAssetManifestStoredBytesStillVerifyAgainstTheRowsHash is the
// cross-layer half of the strict hash rule, run for all four asset types:
// the bytes the gate says to store go into the column, the column's own
// rendering of them comes back, and the row's integrity_hash is
// re-derived from what was READ — not from what was written.
//
// jsonb is why this needs a real database: it reorders object keys and
// normalises whitespace, so the stored rendering is NOT the canonical
// bytes. The verification works because internal/assets re-renders the
// document canonical before digesting it, which is exactly the property
// the gate's equality rule depends on.
func TestAssetManifestStoredBytesStillVerifyAgainstTheRowsHash(t *testing.T) {
	ctx := testCtx(t)
	f := newAssetManifestFixture(t, ctx)

	for _, assetType := range assets.AllTypes() {
		t.Run(string(assetType), func(t *testing.T) {
			candidate := manifestCandidate(t, assetType)
			res, err := assets.Gate(candidate)
			if err != nil {
				t.Fatalf("the gate refused a complete candidate: %v", err)
			}
			if _, err := f.writeVersion(t, ctx, string(assetType),
				string(res.ManifestJSON), string(candidate.RightsJSON), candidate.IntegrityHash); err != nil {
				t.Fatalf("storing the gate's canonical manifest: %v", err)
			}

			var stored, storedHash string
			if err := f.pool.QueryRow(ctx,
				`SELECT manifest::text, integrity_hash FROM research_asset_versions
				  WHERE asset_id = $1 AND version = $2`, f.asset, string(assetType)).
				Scan(&stored, &storedHash); err != nil {
				t.Fatalf("read the stored manifest: %v", err)
			}
			// The stored rendering is a manifest this platform reads...
			if _, err := assets.ParseManifest([]byte(stored)); err != nil {
				t.Fatalf("the stored manifest does not parse back: %v (stored: %s)", err, stored)
			}
			// ...and it hashes to the value the ROW carries — re-derived
			// from what was read, not from what was written.
			got, err := assets.ManifestHash([]byte(stored))
			if err != nil {
				t.Fatalf("ManifestHash(the stored bytes): %v", err)
			}
			if got != storedHash {
				t.Errorf("the stored document hashes to %s, the row claims %s", got, storedHash)
			}
			if storedHash != candidate.IntegrityHash {
				t.Errorf("the row stored %s while the candidate claimed %s", storedHash, candidate.IntegrityHash)
			}
			// The non-ASCII metadata survived the column.
			if !strings.Contains(stored, "这是版本清单的说明") {
				t.Errorf("the non-ASCII metadata member did not survive the jsonb round trip: %s", stored)
			}

			// An edit after the fact no longer verifies. The row is
			// immutable (00014), so the edit is made where an attacker
			// would have to make it: in a copy of the stored document.
			edited := strings.Replace(stored, `"a value"`, `"another value"`, 1)
			if edited == stored {
				t.Fatalf("the fixture did not contain the text to edit: %s", stored)
			}
			editedHash, err := assets.ManifestHash([]byte(edited))
			if err != nil {
				t.Fatalf("ManifestHash(the edited bytes): %v", err)
			}
			if editedHash == candidate.IntegrityHash {
				t.Error("an edited manifest still hashes to the row's integrity_hash: the hash does not cover the content")
			}
		})
	}
}

// TestTheManifestColumnEnforcesTheKindNotTheChecklist pins the division
// of labour between storage and the gate, so a later reader cannot
// conclude from a stored row that the row was publishable:
//
//   - the column accepts documents the Go validator REFUSES — an empty
//     object, and a manifest missing every required metadata field. Rows
//     that predate the model are not repaired and their shape is not
//     re-litigated by 00067;
//   - the publish checklist of docs/11 §3 therefore has exactly one
//     enforcer, internal/assets.Gate, and this test fails if a
//     future migration starts enforcing the vocabulary in SQL — which
//     would be the second definition of it that 00064, 00066 and 00067
//     all decline to create.
func TestTheManifestColumnEnforcesTheKindNotTheChecklist(t *testing.T) {
	ctx := testCtx(t)
	f := newAssetManifestFixture(t, ctx)

	// storable, refused by the Go validator
	notAManifest := []struct {
		name string
		raw  string
	}{
		{"the empty object", `{}`},
		{"no required metadata", `{"version":1,"asset_type":"dataset","metadata":{}}`},
		{"an unknown asset type", `{"version":1,"asset_type":"model","metadata":{"purpose":"x"}}`},
		{"an unsupported format version", `{"version":99,"asset_type":"dataset","metadata":{}}`},
		{"a floating dependency pin", `{"version":1,"asset_type":"protocol","metadata":{},"dependency_pins":["latest"]}`},
	}
	for _, tc := range notAManifest {
		if _, err := f.writeVersion(t, ctx, "stored-"+tc.name, tc.raw, `{}`, "h"); err != nil {
			t.Errorf("%s: the column refused %s, but the manifest KIND is an object and storage enforces no more: %v", tc.name, tc.raw, err)
		}
		if _, err := assets.ParseManifest([]byte(tc.raw)); err == nil {
			t.Errorf("%s: %s must be refused by the validator, or this test proves nothing", tc.name, tc.raw)
		}
	}

	// The same for the rights document beside it: '{}' is storable (the
	// pre-model fixtures store it) and is not a declaration.
	if _, err := f.writeVersion(t, ctx, "empty-rights", `{}`, `{}`, "h"); err != nil {
		t.Errorf("the empty rights object must stay storable: %v", err)
	}
	if _, err := rights.Parse([]byte(`{}`)); err == nil {
		t.Error("rights.Parse({}) must refuse the empty object, or this test proves nothing")
	}
}

// TestAStoredVersionMissingProvenanceCannotExist is the acceptance
// criterion's storage half: 缺 provenance 阻止发布 is enforced one layer
// BELOW the gate as well — origin_refs is NOT NULL, non-empty and
// NULL-element-free (00064), so a publish that skipped the gate still
// cannot store a version with no provenance.
//
// It is asserted again here, next to the gate's own refusal, because the
// two are the same criterion at two layers and a reader of this file
// should not have to find T0701's test to know it holds.
func TestAStoredVersionMissingProvenanceCannotExist(t *testing.T) {
	ctx := testCtx(t)
	f := newAssetManifestFixture(t, ctx)

	cases := []struct {
		name string
		stmt string
		args []any
		want string
	}{
		{
			"absent",
			`INSERT INTO research_asset_versions
				(asset_id, version, manifest, rights_json, visibility, integrity_hash, published_by)
			 VALUES ($1, $2, '{}'::jsonb, '{}'::jsonb, 'public', 'h', $3)`,
			[]any{f.asset, "noprovenance-absent", f.user},
			"23502",
		},
		{
			"empty",
			`INSERT INTO research_asset_versions
				(asset_id, version, manifest, rights_json, visibility, integrity_hash, published_by, origin_refs)
			 VALUES ($1, $2, '{}'::jsonb, '{}'::jsonb, 'public', 'h', $3, ARRAY[]::text[])`,
			[]any{f.asset, "noprovenance-empty", f.user},
			"23514",
		},
		{
			// A real SQL NULL element, spelled in SQL because a Go
			// []string cannot carry one: the case cardinally-1-but-empty
			// that 00064's second CHECK exists for.
			"one NULL element",
			`INSERT INTO research_asset_versions
				(asset_id, version, manifest, rights_json, visibility, integrity_hash, published_by, origin_refs)
			 VALUES ($1, $2, '{}'::jsonb, '{}'::jsonb, 'public', 'h', $3, ARRAY[$4::text, NULL]::text[])`,
			[]any{f.asset, "noprovenance-null-element", f.user, f.refs[0]},
			"23514",
		},
		{
			// A blank element is NOT a NULL element and stays storable,
			// deliberately: storage types the elements as text and does not
			// read their shape (00064's comment). The refs' canonical
			// kind:value form is internal/assets.OriginRef's rule, checked
			// by the gate — which is exactly why the gate exists beside
			// this layer.
			"a blank element (storable, refused by the gate)",
			`INSERT INTO research_asset_versions
				(asset_id, version, manifest, rights_json, visibility, integrity_hash, published_by, origin_refs)
			 VALUES ($1, $2, '{}'::jsonb, '{}'::jsonb, 'public', 'h', $3, ARRAY['']::text[])`,
			[]any{f.asset, "noprovenance-blank-element", f.user},
			"",
		},
	}
	for _, tc := range cases {
		var id string
		err := f.pool.QueryRow(ctx, tc.stmt+" RETURNING id", tc.args...).Scan(&id)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v, want the row stored", tc.name, err)
		case tc.want == "":
			// the gate refuses the same row's provenance
			blank := manifestCandidate(t, assets.TypeDataset)
			blank.OriginRefs = []string{""}
			if _, gerr := assets.Gate(blank); gerr == nil {
				t.Errorf("%s: the gate accepted a blank origin ref", tc.name)
			}
		case err == nil:
			t.Errorf("%s: a version row with no real provenance was stored (id %s)", tc.name, id)
		case sqlState(t, err) != tc.want:
			t.Errorf("%s: SQLSTATE = %s, want %s", tc.name, sqlState(t, err), tc.want)
		}
	}

	// And the gate refuses the same candidate, so the layer above agrees
	// with the layer below rather than leaning on it.
	candidate := manifestCandidate(t, assets.TypeDataset)
	candidate.OriginRefs = nil
	res, err := assets.Gate(candidate)
	if err == nil {
		t.Fatal("the gate accepted a candidate with no provenance")
	}
	if res.Facts.SourcePinned {
		t.Error("SourcePinned = true for a candidate with no provenance")
	}
}

// manifestCandidate builds a publish candidate that breaks no rule, of
// the given asset type. Its metadata is built from the type's own
// required table, so a type added to the V1 set extends this test rather
// than breaking it.
func manifestCandidate(t *testing.T, assetType assets.Type) assets.PublishCandidate {
	t.Helper()
	metadata := assets.Metadata{}
	for _, field := range assets.RequiredMetadata(assetType) {
		switch field.Kind {
		case assets.KindText:
			metadata[field.Key] = "a value"
		case assets.KindTextList:
			metadata[field.Key] = []any{"a value"}
		case assets.KindTextObject:
			metadata[field.Key] = map[string]any{"a key": "a value"}
		case assets.KindEnum:
			if len(field.Values) == 0 {
				t.Fatalf("%s.%s: an enum field with no values", assetType, field.Key)
			}
			metadata[field.Key] = field.Values[0]
		}
	}
	// Non-ASCII text, deliberately: the column is jsonb and the
	// repository's working language is Chinese, so the round trip has to
	// hold for text that is not ASCII.
	metadata["notes_zh"] = "这是版本清单的说明"

	m := assets.Manifest{
		Version:   assets.ManifestFormatVersion,
		AssetType: assetType,
		Metadata:  metadata,
		DependencyPins: []assets.DependencyPin{
			assets.DependencyPin("01j9z6k3m4n5p6q7r8s9t0v1w3@2.1"),
		},
	}
	raw, err := m.CanonicalJSON()
	if err != nil {
		t.Fatalf("canonical manifest: %v", err)
	}
	hash, err := m.Hash()
	if err != nil {
		t.Fatalf("manifest hash: %v", err)
	}
	rightsRaw, err := rights.New().Marshal()
	if err != nil {
		t.Fatalf("rights.New().Marshal: %v", err)
	}
	return assets.PublishCandidate{
		AssetPID:      assets.PID("01j9z6k3m4n5p6q7r8s9t0v1w2"),
		AssetType:     assetType,
		Version:       string(assetType) + "-1.0", // one label per type on the one asset
		Manifest:      raw,
		RightsJSON:    rightsRaw,
		OriginRefs:    []string{"release:0f4d2c1b-9a87-4653-8b21-7e6f5d4c3b2a"},
		Visibility:    assets.VisibilityPublic,
		IntegrityHash: hash,
		CreatorIDs:    []string{"0f4d2c1b-9a87-4653-8b21-7e6f5d4c3b2a"},
	}
}

// TestAssetManifestIsNotAReleaseManifest pins the boundary 00067 draws:
// the constraint is on research_asset_versions, not on the releases table
// whose column is also called manifest. A release manifest is a different
// document (RSG snapshot hash, git commit, blob hashes, pinned schema and
// policy versions) and nothing has decided its shape yet, so a scalar
// there is still storable.
func TestAssetManifestIsNotAReleaseManifest(t *testing.T) {
	ctx := testCtx(t)
	f := newAssetManifestFixture(t, ctx)

	project := mustQueryUUID(t, ctx, f.pool,
		`SELECT origin_project_id::text FROM research_assets WHERE id = $1`, f.asset)
	branch := mustQueryUUID(t, ctx, f.pool,
		`INSERT INTO branches (project_id, name, visibility, git_ref, created_by)
		 VALUES ($1, 'main', 'private', 'refs/heads/main', $2) RETURNING id`, project, f.user)
	state := mustQueryUUID(t, ctx, f.pool,
		`INSERT INTO project_states (project_id, branch_id, state_hash, manifest_version)
		 VALUES ($1, $2, 'hash-1', 'v1') RETURNING id`, project, branch)

	// releases.manifest is untouched by 00067: the constraint is named
	// after the table it belongs to, and this insert proves it does not
	// reach across.
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO releases (project_id, version, title, state_id, manifest, manifest_hash, created_by)
		 VALUES ($1, 'r1', 'R1', $2, '[]'::jsonb, 'h', $3)`, project, state, f.user); err != nil {
		t.Errorf("releases.manifest must stay unconstrained by 00067: %v", err)
	}

	// The asset version's manifest, by contrast, is constrained by name.
	_, err := f.writeVersion(t, ctx, "2.0", `[]`, `{}`, "h")
	if err == nil {
		t.Fatal("an array manifest on a version was stored")
	}
	if got := constraintOf(t, err); got != assetManifestConstraint {
		t.Errorf("refused by %q, want %q", got, assetManifestConstraint)
	}
}

// TestAssetManifestColumnsAreNotNull pins the two columns' presence rules
// together (00010): a version always carries both documents, whatever
// their contents.
func TestAssetManifestColumnsAreNotNull(t *testing.T) {
	ctx := testCtx(t)
	f := newAssetManifestFixture(t, ctx)

	var id string
	err := f.pool.QueryRow(ctx,
		`INSERT INTO research_asset_versions
			(asset_id, version, manifest, rights_json, visibility, integrity_hash, published_by, origin_refs)
		 VALUES ($1, '1.0', NULL, '{}'::jsonb, 'public', 'h', $2, $3) RETURNING id`,
		f.asset, f.user, f.refs).Scan(&id)
	if err == nil {
		t.Fatal("a version row with a NULL manifest was stored")
	}
	if got := sqlState(t, err); got != "23502" {
		t.Errorf("SQLSTATE = %s, want 23502 (not_null_violation)", got)
	}
}
