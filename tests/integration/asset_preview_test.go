// Task T0704 required test "publish preview tests" — the end-to-end half.
//
// The unit suites pin the preview's RULES (internal/assets: the five
// lists, the blocking decisions, the gate reuse, the determinism of the
// pure function) and the transport's shape (cmd/api/assetshttp: the route,
// the guard, the refusals). This file pins the three things only a real
// PostgreSQL can settle, over the whole composed route:
//
//  1. A private→public preview NAMES the private dependencies. The three
//     leak shapes docs/23 §4 is about are seeded as real rows — a
//     dependency pin that resolves to a still-private version, an origin
//     ref into ANOTHER project's private state, and a blob whose bytes are
//     not openly attached while the version's rights declaration promises
//     open data access — and the preview names each one and blocks on it.
//     The same run shows the discrimination is real: a pin to a public
//     version and a blob that IS openly attached are not named, and the
//     publishing project's own private refs are named without blocking.
//
//  2. The preview is REPEATABLE: the same candidate against the same state
//     answers byte-identical JSON twice.
//
//  3. The preview CHANGES NOTHING. The store the route reads through could
//     not write even if it wanted to: its pool carries the server-side
//     default_transaction_read_only, proven by a write the server refuses
//     (SQLSTATE 25006) with a control proving the same statement succeeds
//     on the writable pool. On top of that instrument, every row of every
//     table in the database is fingerprinted before and after the run and
//     must be identical.
//
//  4. A manifest the platform CANNOT READ is a finding about the candidate,
//     not a failure of the reader: the route answers 200 with the gate's
//     refusal and the refs — which are the candidate's own field — still
//     resolved and still named.
//
//  5. (T0712, issue #238) The preview does NOT disclose another project's
//     private identity. The route's gate asks only whether the caller may
//     read the PUBLISHING project, so before this task an entry that
//     resolved into a third project rendered that project's id, its
//     visibility and the entity's title to anyone who could read the
//     publishing one. TestAssetPreviewDoesNotDiscloseAForeignPrivateEntity
//     asserts the absence on the raw response — the foreign project's id and
//     the object's title appear nowhere in it — while the private dependency
//     stays named and blocking, and TestAssetPreviewRendersPublicProjectIdentities
//     asserts the other direction, that a public project's ids and titles are
//     still rendered (the tightening must not swallow what the preview exists
//     to show).
//
// The composition is cmd/api/main.go's: the real auth guard, the real
// project read gate, the real state reader over the canonical queries.
// Only the session store is in-memory, exactly as the release e2e suite
// composes it.

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/assetshttp"
	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rights"
)

// assetPreviewTaskID namespaces this task's test databases
// (test_T0704_<run_id>).
const assetPreviewTaskID = "T0704"

// readOnlySQLState is the SQLSTATE the server answers a write with when the
// session forbids writes (read_only_sql_transaction).
const readOnlySQLState = "25006"

// --------------------------------------------------------------------------
// The wire shape this suite reads (declared here rather than reused from
// internal/assets, so a silent JSON tag change in the model fails here).

type previewPayload struct {
	ProjectID        string               `json:"project_id"`
	Version          string               `json:"version"`
	TargetVisibility string               `json:"target_visibility"`
	Asset            previewAssetWire     `json:"asset"`
	Facts            previewFactsWire     `json:"facts"`
	Publishable      bool                 `json:"publishable"`
	Objects          []previewObjectWire  `json:"objects"`
	Metadata         []previewMetaWire    `json:"metadata"`
	Blobs            []previewBlobWire    `json:"blobs"`
	Refs             []previewRefWire     `json:"refs"`
	Dependencies     []previewPinWire     `json:"dependencies"`
	PrivateDeps      []previewPrivateWire `json:"private_dependencies"`
	PublishBlockers  []previewBlockerWire `json:"publish_blockers"`
	RightsBlockers   []previewBlockerWire `json:"rights_blockers"`
}

type previewAssetWire struct {
	PID             string `json:"pid"`
	AssetType       string `json:"asset_type"`
	Resolved        bool   `json:"resolved"`
	Title           string `json:"title"`
	OriginProjectID string `json:"origin_project_id"`
}

type previewFactsWire struct {
	SourcePinned   bool `json:"source_pinned"`
	VersionPinned  bool `json:"version_pinned"`
	Contributors   bool `json:"contributors"`
	Rights         bool `json:"rights"`
	Visibility     bool `json:"visibility"`
	DependencyPins bool `json:"dependency_pins"`
	IntegrityHash  bool `json:"integrity_hash"`
	Metadata       bool `json:"metadata"`
}

type previewObjectWire struct {
	ObjectVersionID   string `json:"object_version_id"`
	ObjectID          string `json:"object_id"`
	ProjectID         string `json:"project_id"`
	Title             string `json:"title"`
	CurrentVisibility string `json:"current_visibility"`
}

type previewMetaWire struct {
	Key   string `json:"key"`
	Value any    `json:"value"`
}

type previewBlobWire struct {
	BlobID         string `json:"blob_id"`
	Resolved       bool   `json:"resolved"`
	CurrentAccess  string `json:"current_access"`
	DeclaredAccess string `json:"declared_access"`
}

type previewRefWire struct {
	Ref               string `json:"ref"`
	Kind              string `json:"kind"`
	Resolved          bool   `json:"resolved"`
	ProjectID         string `json:"project_id"`
	CurrentVisibility string `json:"current_visibility"`
}

type previewPinWire struct {
	Pin               string `json:"pin"`
	Resolved          bool   `json:"resolved"`
	CurrentVisibility string `json:"current_visibility"`
}

type previewPrivateWire struct {
	Kind     string `json:"kind"`
	Ref      string `json:"ref"`
	Blocking bool   `json:"blocking"`
	Detail   string `json:"detail"`
}

type previewBlockerWire struct {
	Code   string `json:"code"`
	Field  string `json:"field"`
	Detail string `json:"detail"`
}

// --------------------------------------------------------------------------
// The composed surface

// newAssetPreviewServer composes the production tree (like cmd/api/main.go):
// the real auth guard, the real project read gate, and the preview route
// with its state reader over a pool whose SESSION forbids writes. The
// writable pool is returned alongside it for fixture seeding and for the
// write control.
func newAssetPreviewServer(t *testing.T, ctx context.Context) (ts *httptest.Server, pool *pgxpool.Pool, readOnly *pgxpool.Pool) {
	t.Helper()
	var url string
	pool, url = testdb.Setup(t, ctx, adminURL(t), assetPreviewTaskID)
	readOnly = openReadOnlyPool(t, ctx, url)

	authAPI := authhttp.New(authhttp.Deps{
		Users:    persistence.NewCredentialStore(pool),
		Sessions: memstore.NewSessions(),
		Limiter:  memstore.NewLimiter(),
		Cfg: authn.Config{
			WebOrigin:          "http://web.test",
			SessionTTL:         time.Hour,
			LoginLimitPerEmail: 1000,
			LoginLimitPerIP:    10000,
			LoginWindow:        time.Minute,
			SignupLimitPerIP:   10000,
		},
	})
	projectAPI := projectshttp.New(projectshttp.Deps{
		Store: persistence.NewProjectStore(pool),
		Orgs:  persistence.NewOrgStore(pool),
		Authz: authz.NewMatrixEngine(),
	})

	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", authAPI.Routes())
	mux.Handle("/api/v1/projects", projectAPI.Routes())
	mux.Handle("/api/v1/projects/", projectAPI.Routes())
	// The preview is wired exactly as main.go wires it. Its state reader is
	// built on the READ-ONLY pool and nothing else: the pointer this
	// function returns is the pointer the store holds, and the write probe
	// below is what makes that claim checkable rather than intended.
	assetshttp.New(assetshttp.Deps{
		State:    assetshttp.NewPostgresStateStore(readOnly),
		Projects: projectAPI.Service(),
	}).Register(mux)

	ts = httptest.NewServer(authAPI.Guard(mux))
	t.Cleanup(ts.Close)
	return ts, pool, readOnly
}

// openReadOnlyPool opens a second pool over the same database whose
// connections are read-only from the moment they connect.
//
// default_transaction_read_only is a startup parameter, so it applies to
// EVERY connection the pool will ever open rather than to the ones a test
// remembered to configure — which is what makes the route's inability to
// write a fact about the server rather than a promise about the code.
func openReadOnlyPool(t *testing.T, ctx context.Context, url string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse %s: %v", url, err)
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open read-only pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// --------------------------------------------------------------------------
// The fixture

// assetPreviewFixture is the state a private→public preview is computed
// against: one private publishing project publishing an asset version
// public, and one FOREIGN private project holding the things that must not
// ride along.
type assetPreviewFixture struct {
	aliceID string
	// projectA is the publishing project: private, and the asset's own
	// project (docs/11 §2).
	projectA string
	// projectB is another project's private state — the leak side.
	projectB string
	// releaseA is a release of the publishing project itself: private
	// today, and legitimately carried by a publication of its own work
	// (docs/12 §2: a private project may explicitly publish an asset).
	releaseA string
	// releaseB and objectVersionB belong to projectB.
	releaseB       string
	objectB        string
	objectVersionB string
	// pubAssetPID is the pid of the asset a NEW version would be published
	// under; the other two pids name assets that already have a version.
	pubAssetPID     string
	publicAssetPID  string
	privateAssetPID string
	// privateAssetID is the row id of the private dependency's asset (the
	// visibility of its version is a column a test flips).
	privateAssetID string
	// publicPin and privatePin are pid@version pins to a public and to a
	// still-private stored version.
	publicPin  string
	privatePin string
	// openBlob is openly attached; restrictedBlob is attached restricted,
	// so its bytes stay behind the download gate.
	openBlob       string
	restrictedBlob string
}

// seedAssetPreviewFixture writes the rows. Everything is inserted with raw
// SQL on the WRITABLE pool: the fixture is the repository's state, and the
// route under test is only ever allowed to read it.
func seedAssetPreviewFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool, aliceID, bobID string) *assetPreviewFixture {
	t.Helper()
	f := &assetPreviewFixture{aliceID: aliceID}

	f.projectA = mustQueryUUID(t, ctx, pool,
		`INSERT INTO projects (slug, name, purpose, visibility, created_by)
		 VALUES ('preview-publishing', 'Publishing Project', 'T0704 fixture', 'private', $1) RETURNING id`, aliceID)
	f.projectB = mustQueryUUID(t, ctx, pool,
		`INSERT INTO projects (slug, name, purpose, visibility, created_by)
		 VALUES ('preview-foreign', 'Foreign Private Project', 'T0704 fixture', 'private', $1) RETURNING id`, bobID)
	// The project read gate is membership-driven for a private project, so
	// the publishing project needs its owner.
	if _, err := pool.Exec(ctx,
		`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, 'owner')`,
		f.projectA, aliceID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	stateA := mustQueryUUID(t, ctx, pool,
		`INSERT INTO project_states (project_id, state_hash, manifest_version)
		 VALUES ($1, 'genesis-a', '1') RETURNING id`, f.projectA)
	stateB := mustQueryUUID(t, ctx, pool,
		`INSERT INTO project_states (project_id, state_hash, manifest_version)
		 VALUES ($1, 'genesis-b', '1') RETURNING id`, f.projectB)

	f.releaseA = mustQueryUUID(t, ctx, pool,
		`INSERT INTO releases (project_id, version, title, state_id, manifest, manifest_hash, created_by)
		 VALUES ($1, 'v1.0.0', 'Release A', $2, '{}'::jsonb, 'x', $3) RETURNING id`, f.projectA, stateA, aliceID)
	f.releaseB = mustQueryUUID(t, ctx, pool,
		`INSERT INTO releases (project_id, version, title, state_id, manifest, manifest_hash, created_by)
		 VALUES ($1, 'v1.0.0', 'Release B', $2, '{}'::jsonb, 'x', $3) RETURNING id`, f.projectB, stateB, bobID)

	// One object version in the publishing project (to attach the open blob
	// to) and one in the foreign project (the object_version ref, and the
	// restricted blob).
	objectA := mustQueryUUID(t, ctx, pool,
		`INSERT INTO scientific_objects (project_id, object_type, created_by)
		 VALUES ($1, 'dataset_record', $2) RETURNING id`, f.projectA, aliceID)
	versionA := mustQueryUUID(t, ctx, pool,
		`INSERT INTO scientific_object_versions
			(object_id, version_no, state_id, schema_id, schema_version, title, lifecycle_state, payload, integrity_hash, created_by)
		 VALUES ($1, 1, $2, 'https://open-rd.example/schemas/dataset.schema.json', '1', 'Open record', 'active',
		         '{}'::jsonb, 'x', $3) RETURNING id`, objectA, stateA, aliceID)
	f.objectB = mustQueryUUID(t, ctx, pool,
		`INSERT INTO scientific_objects (project_id, object_type, created_by)
		 VALUES ($1, 'dataset_record', $2) RETURNING id`, f.projectB, bobID)
	f.objectVersionB = mustQueryUUID(t, ctx, pool,
		`INSERT INTO scientific_object_versions
			(object_id, version_no, state_id, schema_id, schema_version, title, lifecycle_state, payload, integrity_hash, created_by)
		 VALUES ($1, 1, $2, 'https://open-rd.example/schemas/dataset.schema.json', '1', 'Private record', 'active',
		         '{}'::jsonb, 'x', $3) RETURNING id`, f.objectB, stateB, bobID)

	f.openBlob = mustQueryUUID(t, ctx, pool,
		`INSERT INTO blobs (content_hash, size_bytes, storage_key, created_by)
		 VALUES ('sha256:open', 1024, 'blobs/open', $1) RETURNING id`, aliceID)
	f.restrictedBlob = mustQueryUUID(t, ctx, pool,
		`INSERT INTO blobs (content_hash, size_bytes, storage_key, created_by)
		 VALUES ('sha256:restricted', 2048, 'blobs/restricted', $1) RETURNING id`, bobID)
	if _, err := pool.Exec(ctx,
		`INSERT INTO blob_attachments (blob_id, scientific_object_version_id, attachment_role, access_level, state_id)
		 VALUES ($1, $2, 'data', 'open', $3)`, f.openBlob, versionA, stateA); err != nil {
		t.Fatalf("attach open blob: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO blob_attachments (blob_id, scientific_object_version_id, attachment_role, access_level, state_id)
		 VALUES ($1, $2, 'data', 'restricted', $3)`, f.restrictedBlob, f.objectVersionB, stateB); err != nil {
		t.Fatalf("attach restricted blob: %v", err)
	}

	// Three assets: the one a new version is published under, a dependency
	// that resolves to a public version, and a dependency that resolves to
	// a still-private one.
	f.pubAssetPID, _ = seedAssetWithVersion(t, ctx, pool, f.projectA, aliceID,
		"pub-asset", "Publishing asset", "01j9z6k3m4n5p6q7r8s9t0v1w2", "", "")
	f.publicAssetPID, _ = seedAssetWithVersion(t, ctx, pool, f.projectA, aliceID,
		"dep-public", "Public dependency", "01j9z6k3m4n5p6q7r8s9t0v1w3", "1.0", "public")
	f.privateAssetPID, f.privateAssetID = seedAssetWithVersion(t, ctx, pool, f.projectB, bobID,
		"dep-private", "Private dependency", "01j9z6k3m4n5p6q7r8s9t0v1w4", "0.1", "private")

	f.publicPin = f.publicAssetPID + "@1.0"
	f.privatePin = f.privateAssetPID + "@0.1"
	return f
}

// seedAssetWithVersion inserts one research asset (and, when a version label
// is given, one version of it at the visibility the caller asks for) and
// returns the asset's pid and row id.
func seedAssetWithVersion(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID, userID, slug, title, pid, version, visibility string) (string, string) {
	t.Helper()
	assetID := mustQueryUUID(t, ctx, pool,
		`INSERT INTO research_assets (asset_type, slug, title, origin_project_id, pid)
		 VALUES ('dataset', $1, $2, $3, $4) RETURNING id`, slug, title, projectID, pid)
	if version == "" {
		return pid, assetID
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO research_asset_versions
			(asset_id, version, manifest, rights_json, visibility, integrity_hash, published_by, origin_refs)
		 VALUES ($1, $2, '{}'::jsonb, '{}'::jsonb, $3, 'x', $4, ARRAY['project:' || $5::text])`,
		assetID, version, visibility, userID, projectID); err != nil {
		t.Fatalf("seed asset version %s@%s: %v", slug, version, err)
	}
	return pid, assetID
}

// makeForeignProjectPublic flips the one visibility a "nothing private
// here" case needs. Only projects.visibility is mutable: an asset version
// is append-only (research_asset_versions_append_only), so a version's
// visibility is decided when it is written and a test that wants a public
// dependency pins the asset the fixture seeded public — it does not edit
// history.
func (f *assetPreviewFixture) makeForeignProjectPublic(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE projects SET visibility = 'public' WHERE id = $1`, f.projectB); err != nil {
		t.Fatalf("make the foreign project public: %v", err)
	}
}

// --------------------------------------------------------------------------
// The candidate

// previewCandidate is the proposed publish: the knobs are the fields a case
// varies.
type previewCandidate struct {
	assetPID    string
	assetType   string
	version     string
	visibility  string
	blobs       []string
	accessLevel string
	pins        []string
	refs        []string
	creators    []string
	dataAccess  rights.DataAccess
}

// defaultCandidate is a publishable version of the fixture's asset: public
// visibility, both pins, all three refs, both blobs, and a rights
// declaration that says its data is open.
func defaultCandidate(f *assetPreviewFixture) previewCandidate {
	return previewCandidate{
		assetPID:    f.pubAssetPID,
		assetType:   "dataset",
		version:     "1.0",
		visibility:  "public",
		blobs:       []string{f.openBlob, f.restrictedBlob},
		accessLevel: "restricted",
		pins:        []string{f.publicPin, f.privatePin},
		refs: []string{
			"release:" + f.releaseA, "release:" + f.releaseB, "object_version:" + f.objectVersionB,
		},
		creators:   []string{f.aliceID},
		dataAccess: rights.DataAccessOpen,
	}
}

// previewCandidateJSON renders one candidate as the route's request body.
//
// The integrity hash is computed from the manifest this function renders,
// through the same parser and the same canonical form the publish gate
// verifies against — a fixture carrying a stale hash would be testing the
// gate's hash refusal instead of the preview.
func previewCandidateJSON(t *testing.T, c previewCandidate) string {
	t.Helper()
	manifest := previewManifest(c)
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	parsed, err := assets.ParseManifest(raw)
	if err != nil {
		t.Fatalf("the fixture manifest must pass the gate's own parser: %v", err)
	}
	hash, err := parsed.Hash()
	if err != nil {
		t.Fatalf("manifest hash: %v", err)
	}
	return previewBodyJSON(t, c, manifest, hash)
}

// previewManifest is the manifest document a candidate carries: a dataset
// declaring every key its type requires.
func previewManifest(c previewCandidate) map[string]any {
	return map[string]any{
		"version":    1,
		"asset_type": "dataset",
		"metadata": map[string]any{
			"purpose":       "preview fixture",
			"data_type":     "table",
			"blob_ids":      c.blobs,
			"access_level":  c.accessLevel,
			"quality_notes": "checked",
		},
		"dependency_pins": c.pins,
	}
}

// previewBodyJSON renders the request body around one manifest document and
// the integrity hash the candidate claims for it.
//
// It is split out of previewCandidateJSON because the case where the
// platform CANNOT read the manifest — the one this suite has to be able to
// send — is exactly the case where no canonical hash can be derived from
// it: there is nothing to parse, so the candidate claims a syntactically
// valid digest that covers nothing. The gate does not compare a hash it has
// no canonical bytes for (it reports the manifest refusal instead), so the
// hash is deliberately not the subject of that case.
func previewBodyJSON(t *testing.T, c previewCandidate, manifest any, hash string) string {
	t.Helper()
	doc := rights.New()
	doc.Visibility.DataAccess = c.dataAccess
	rightsJSON, err := doc.Marshal()
	if err != nil {
		t.Fatalf("marshal rights: %v", err)
	}
	body := map[string]any{
		"asset_pid":      c.assetPID,
		"asset_type":     c.assetType,
		"version":        c.version,
		"manifest":       manifest,
		"rights":         json.RawMessage(rightsJSON),
		"origin_refs":    c.refs,
		"visibility":     c.visibility,
		"integrity_hash": hash,
		"creator_ids":    c.creators,
	}
	out, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal candidate: %v", err)
	}
	return string(out)
}

// previewURL is the contract's path (specs/api/openapi.yaml).
func previewURL(projectID string) string {
	return "/api/v1/projects/" + projectID + "/assets:publish-preview"
}

// runPreview sends one preview and returns the RAW body (the determinism
// assertions compare bytes) plus the decoded payload.
func runPreview(t *testing.T, uc *testUserClient, projectID, body string) (string, previewPayload) {
	t.Helper()
	resp := uc.do(t, http.MethodPost, previewURL(projectID), body)
	mustStatus(t, resp, http.StatusOK)
	raw := readAll(t, resp)
	var payload previewPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("preview payload: %v: %s", err, raw)
	}
	return raw, payload
}

// privateDep finds the named entry for one ref, and fails when the preview
// did not name it at all.
func privateDep(t *testing.T, deps []previewPrivateWire, ref string) previewPrivateWire {
	t.Helper()
	for _, dep := range deps {
		if dep.Ref == ref {
			return dep
		}
	}
	t.Fatalf("the preview did not name %q; it named %v", ref, deps)
	return previewPrivateWire{}
}

// namedRefs lists the entries of a private-dependency list as
// "kind ref", for diagnostics and for set comparisons.
func namedRefs(deps []previewPrivateWire) []string {
	out := make([]string, 0, len(deps))
	for _, dep := range deps {
		out = append(out, dep.Kind+" "+dep.Ref)
	}
	return out
}

// --------------------------------------------------------------------------
// 1. The private dependencies are named

// TestAssetPreviewNamesPrivateDependencies is the acceptance criterion the
// whole task turns on: the private→public preview names every private
// dependency — and only the private ones.
func TestAssetPreviewNamesPrivateDependencies(t *testing.T) {
	ctx := testCtx(t)
	ts, pool, _ := newAssetPreviewServer(t, ctx)
	alice, aliceID := signup(t, ts.URL, "preview-alice@example.com", "preview-alice")
	_, bobID := signup(t, ts.URL, "preview-bob@example.com", "preview-bob")
	f := seedAssetPreviewFixture(t, ctx, pool, aliceID, bobID)

	raw, got := runPreview(t, alice, f.projectA, previewCandidateJSON(t, defaultCandidate(f)))

	// --- what the preview is about ---
	if got.ProjectID != f.projectA {
		t.Errorf("project_id = %q, want the publishing project", got.ProjectID)
	}
	if got.Version != "1.0" || got.TargetVisibility != "public" {
		t.Errorf("version/visibility = %q/%q, want 1.0/public (the private→public case)", got.Version, got.TargetVisibility)
	}
	if !got.Asset.Resolved || got.Asset.PID != f.pubAssetPID || got.Asset.OriginProjectID != f.projectA {
		t.Errorf("asset = %+v, want the fixture's asset resolved in the publishing project", got.Asset)
	}

	// The gate itself is happy: every checklist fact holds, and neither
	// blocker list has anything in it. That is what makes the verdict below
	// attributable to the DEPENDENCIES rather than to a refused candidate.
	if !got.Facts.SourcePinned || !got.Facts.VersionPinned || !got.Facts.Contributors || !got.Facts.Rights ||
		!got.Facts.Visibility || !got.Facts.DependencyPins || !got.Facts.IntegrityHash || !got.Facts.Metadata {
		t.Errorf("facts = %+v, want every publish checklist fact true", got.Facts)
	}
	if len(got.PublishBlockers) != 0 || len(got.RightsBlockers) != 0 {
		t.Errorf("publish_blockers = %v, rights_blockers = %v, want none: the candidate itself is publishable",
			got.PublishBlockers, got.RightsBlockers)
	}
	if got.Publishable {
		t.Errorf("publishable = true over a private dependency: %s", raw)
	}

	// --- the three leak shapes, each named ---
	// (a) a pin that resolves to a version that is still private.
	pin := privateDep(t, got.PrivateDeps, f.privatePin)
	if pin.Kind != "asset_version" || !pin.Blocking {
		t.Errorf("the private pin entry = %+v, want kind asset_version and blocking", pin)
	}
	// (b) origin refs into ANOTHER project's private state.
	for _, ref := range []string{"release:" + f.releaseB, "object_version:" + f.objectVersionB} {
		dep := privateDep(t, got.PrivateDeps, ref)
		if !dep.Blocking {
			t.Errorf("the foreign private ref %s = %+v, want blocking", ref, dep)
		}
	}
	// (c) a blob that is not openly attached while the version's documents
	// promise open data access.
	blob := privateDep(t, got.PrivateDeps, f.restrictedBlob)
	if blob.Kind != "blob" || !blob.Blocking {
		t.Errorf("the restricted blob entry = %+v, want kind blob and blocking", blob)
	}

	// --- the same run, and the things that must NOT be named, or must not
	// block ---
	for _, named := range got.PrivateDeps {
		// A pin to a version that is already public is a dependency like
		// any other, not a private one.
		if named.Ref == f.publicPin {
			t.Errorf("the preview named the public pin as a private dependency: %+v", named)
		}
		// An openly attached blob is not a private dependency either.
		if named.Ref == f.openBlob {
			t.Errorf("the preview named the openly attached blob as a private dependency: %+v", named)
		}
	}
	// The publishing project's own private release is carried by its own
	// publication (docs/12 §2: a private project may explicitly publish an
	// asset) — named, so the reader sees it, but not blocking.
	own := privateDep(t, got.PrivateDeps, "release:"+f.releaseA)
	if own.Blocking {
		t.Errorf("the publishing project's own private ref = %+v, want named but not blocking", own)
	}

	// --- every pin was resolved and reported, not only the private ones ---
	pins := map[string]previewPinWire{}
	for _, p := range got.Dependencies {
		pins[p.Pin] = p
	}
	if len(got.Dependencies) != 2 {
		t.Fatalf("dependencies = %v, want one entry per pin", got.Dependencies)
	}
	if p := pins[f.privatePin]; !p.Resolved || p.CurrentVisibility != "private" {
		t.Errorf("the private pin resolved to %+v, want resolved and private", p)
	}
	if p := pins[f.publicPin]; !p.Resolved || p.CurrentVisibility != "public" {
		t.Errorf("the public pin resolved to %+v, want resolved and public", p)
	}

	// --- the refs, with the project each one resolved into ---
	refs := map[string]previewRefWire{}
	for _, r := range got.Refs {
		refs[r.Ref] = r
	}
	if len(got.Refs) != 3 {
		t.Fatalf("refs = %v, want one entry per origin ref", got.Refs)
	}
	// The publishing project's OWN private ref is rendered as it is: the
	// publication carries this project's state, and the caller passed the
	// project read gate for it (docs/12 §2).
	ownRef := refs["release:"+f.releaseA]
	if !ownRef.Resolved || ownRef.ProjectID != f.projectA || ownRef.CurrentVisibility != "private" {
		t.Errorf("ref release:%s = %+v, want the publishing project's own ref rendered", f.releaseA, ownRef)
	}
	// The two refs into the FOREIGN private project resolve — the caller
	// declared them, so their existence is not news to it — and name neither
	// the project they point into nor that project's visibility (T0712,
	// issue #238). Asserted on both sides below and again on the raw body
	// (TestAssetPreviewDoesNotDiscloseAForeignPrivateEntity).
	for _, ref := range []string{"release:" + f.releaseB, "object_version:" + f.objectVersionB} {
		r := refs[ref]
		if !r.Resolved {
			t.Errorf("ref %s = %+v, want it reported as resolved", ref, r)
		}
		if r.ProjectID != "" || r.CurrentVisibility != "" {
			t.Errorf("ref %s = %+v, want the foreign private project's id and visibility withheld", ref, r)
		}
	}

	// --- the object the publication would carry ---
	if len(got.Objects) != 1 {
		t.Fatalf("objects = %v, want the object version the ref carries", got.Objects)
	}
	obj := got.Objects[0]
	if obj.ObjectVersionID != f.objectVersionB {
		t.Errorf("objects[0] = %+v, want the version id the caller's own ref named", obj)
	}
	// Carried, and not attributed: the version is what the ref resolved to,
	// while the object behind it, the project it lives in and its title are
	// that project's information, not this caller's.
	if obj.ObjectID != "" || obj.ProjectID != "" || obj.Title != "" || obj.CurrentVisibility != "" {
		t.Errorf("objects[0] = %+v, want the foreign object, project, title and visibility withheld", obj)
	}

	// --- the blobs, both of them, with today's access and the declared one ---
	blobs := map[string]previewBlobWire{}
	for _, b := range got.Blobs {
		blobs[b.BlobID] = b
	}
	if len(got.Blobs) != 2 {
		t.Fatalf("blobs = %v, want one entry per declared blob id", got.Blobs)
	}
	if b := blobs[f.openBlob]; !b.Resolved || b.CurrentAccess != "open" || b.DeclaredAccess != "open" {
		t.Errorf("the open blob = %+v, want resolved, open today, declared open", b)
	}
	if b := blobs[f.restrictedBlob]; !b.Resolved || b.CurrentAccess != "restricted" || b.DeclaredAccess != "open" {
		t.Errorf("the restricted blob = %+v, want resolved, restricted today, declared open (the contradiction)", b)
	}

	// --- the candidate's own metadata, ordered ---
	if len(got.Metadata) != 5 {
		t.Fatalf("metadata = %v, want the manifest's five keys", got.Metadata)
	}
	wantKeys := []string{"access_level", "blob_ids", "data_type", "purpose", "quality_notes"}
	for i, want := range wantKeys {
		if got.Metadata[i].Key != want {
			t.Errorf("metadata[%d] = %q, want %q (ascending by key)", i, got.Metadata[i].Key, want)
		}
	}

	// The control for the verdict: the SAME candidate in the SAME state,
	// published PRIVATE, is publishable — a private publication widens
	// nothing, and docs/23 §4 is about private→public. This is what makes
	// "publishable: false" above a statement about the target visibility
	// rather than a blanket refusal.
	c := defaultCandidate(f)
	c.visibility = "private"
	_, privateTarget := runPreview(t, alice, f.projectA, previewCandidateJSON(t, c))
	if !privateTarget.Publishable {
		t.Errorf("a private publication of the same candidate = %v, want publishable", privateTarget.PrivateDeps)
	}
	if len(privateTarget.PrivateDeps) == 0 {
		t.Errorf("the private publication named no dependency at all; they are named, they just do not block")
	}
}

// TestAssetPreviewDoesNotNameAPublicDependency is the other half of the
// naming rule, over the same rows with the foreign project made public: a
// preview whose dependencies are all public is publishable and names
// nothing private except what the publishing project itself owns. A
// preview that flagged everything would be as useless as one that flagged
// nothing.
func TestAssetPreviewDoesNotNameAPublicDependency(t *testing.T) {
	ctx := testCtx(t)
	ts, pool, _ := newAssetPreviewServer(t, ctx)
	alice, aliceID := signup(t, ts.URL, "preview-alice2@example.com", "preview-alice2")
	_, bobID := signup(t, ts.URL, "preview-bob2@example.com", "preview-bob2")
	f := seedAssetPreviewFixture(t, ctx, pool, aliceID, bobID)
	f.makeForeignProjectPublic(t, ctx, pool)

	// The pin names the version the fixture seeded public (a version's
	// visibility is written once and never edited), and the rights
	// declaration stops promising open data, so the restricted blob is a
	// statement about access rather than a contradiction.
	c := defaultCandidate(f)
	c.pins = []string{f.publicPin}
	c.dataAccess = rights.DataAccessRestricted
	_, got := runPreview(t, alice, f.projectA, previewCandidateJSON(t, c))

	if !got.Publishable {
		t.Errorf("publishable = false over public dependencies: private_dependencies = %v, blockers = %v/%v",
			namedRefs(got.PrivateDeps), got.PublishBlockers, got.RightsBlockers)
	}
	for _, dep := range got.PrivateDeps {
		if dep.Blocking {
			t.Errorf("the preview blocks on %+v, want nothing blocking", dep)
		}
	}
	// What is named is what is genuinely not public: the publishing
	// project's own private release (named, not blocking — docs/12 §2), and
	// the restricted blob (named, not blocking: the declaration no longer
	// claims its bytes are open).
	named := namedRefs(got.PrivateDeps)
	want := map[string]bool{
		"release release:" + f.releaseA: true,
		"blob " + f.restrictedBlob:      true,
	}
	if len(named) != len(want) {
		t.Fatalf("private_dependencies = %v, want exactly %v", named, want)
	}
	for _, entry := range named {
		if !want[entry] {
			t.Errorf("private_dependencies named %s, which is not private in this state", entry)
		}
	}
	// The public dependency is resolved and reported as public — the pin
	// list carries every pin, so a reader can see the whole set.
	if len(got.Dependencies) != 1 || !got.Dependencies[0].Resolved ||
		got.Dependencies[0].CurrentVisibility != "public" {
		t.Errorf("dependencies = %+v, want the public pin resolved and public", got.Dependencies)
	}
}

// --------------------------------------------------------------------------
// 1c. Another project's private identity is not disclosed (T0712, issue #238)

// TestAssetPreviewDoesNotDiscloseAForeignPrivateEntity is T0712's acceptance
// criterion, over the composed route and a real PostgreSQL.
//
// The caller is alice: owner and member of the PUBLISHING project A, and a
// member of nothing else — the fixture seeds no membership for her in B, and
// she cannot read B at all (docs/12 §2, specs/policies/permissions-matrix.csv:
// read_private_project is a deny for a non-member). The project gate on this
// route asks only about A, so everything below is what the route used to say
// to her about B, given identities she holds from somewhere else:
//
//	before T0712                          after T0712
//	B's project id, in refs and objects   withheld
//	B's visibility, in refs and objects   withheld
//	"Private record", B's version title   withheld
//	B's project id, in the blocker detail withheld
//	B's ASSET TITLE, for a pid of B's     withheld
//	the pin/ref named, blocking            unchanged — the finding survives
//
// The last line is the one that must not move: the route exists to refuse on
// this dependency and to tell the caller which entry of its own list it is
// (docs/23 §4), and a "fix" that hid the entry would trade the protection for
// the silence.
func TestAssetPreviewDoesNotDiscloseAForeignPrivateEntity(t *testing.T) {
	ctx := testCtx(t)
	ts, pool, _ := newAssetPreviewServer(t, ctx)
	alice, aliceID := signup(t, ts.URL, "preview-disclose@example.com", "preview-disclose")
	_, bobID := signup(t, ts.URL, "preview-disclose-bob@example.com", "preview-disclose-bob")
	f := seedAssetPreviewFixture(t, ctx, pool, aliceID, bobID)

	// --- (1) the refs, the object and the pin ---
	raw, got := runPreview(t, alice, f.projectA, previewCandidateJSON(t, defaultCandidate(f)))

	// The whole answer at once: neither B's identity, nor the title of its
	// object version, nor the object row behind the version the caller named
	// appears anywhere in it. Asserting on the raw bytes is
	// deliberate — it is the check that catches a leak in a field no
	// field-by-field case thought to look at, and it is what a reviewer of the
	// wire response can reproduce.
	for _, secret := range []string{f.projectB, "Private record", f.objectB} {
		if strings.Contains(raw, secret) {
			t.Errorf("the preview answer contains %q, which belongs to another private project:\n%s", secret, raw)
		}
	}
	// The caller's own project is rendered as usual, so the absences above are
	// about the other project and not about a preview that stopped saying
	// anything.
	if got.Asset.OriginProjectID != f.projectA {
		t.Errorf("asset = %+v, want the caller's own project rendered", got.Asset)
	}
	// The object version is carried — it is what the ref resolved to — and is
	// not attributed to a project, a title or a visibility.
	if len(got.Objects) != 1 {
		t.Fatalf("objects = %v, want the object version the ref carries", got.Objects)
	}
	obj := got.Objects[0]
	if obj.ObjectVersionID != f.objectVersionB {
		t.Errorf("objects[0] = %+v, want the version id the caller's own ref named", obj)
	}
	if obj.ObjectID != "" || obj.ProjectID != "" || obj.Title != "" || obj.CurrentVisibility != "" {
		t.Errorf("objects[0] = %+v, want the foreign object, project, title and visibility withheld", obj)
	}
	// The refs into B resolved (the caller declared them) and withheld its
	// identity.
	refs := map[string]previewRefWire{}
	for _, r := range got.Refs {
		refs[r.Ref] = r
	}
	for _, ref := range []string{"release:" + f.releaseB, "object_version:" + f.objectVersionB} {
		r := refs[ref]
		if !r.Resolved {
			t.Errorf("ref %s = %+v, want it reported as resolved", ref, r)
		}
		if r.ProjectID != "" || r.CurrentVisibility != "" {
			t.Errorf("ref %s = %+v, want the foreign private project's id and visibility withheld", ref, r)
		}
	}
	// AND THE FINDING SURVIVES, entry by entry: every dependency into B is
	// still named and still blocking, so the publication is still refused for
	// the reason docs/23 §4 gives.
	for _, ref := range []string{
		f.privatePin, // a pin resolving to B's still-private version
		"release:" + f.releaseB,
		"object_version:" + f.objectVersionB,
	} {
		dep := privateDep(t, got.PrivateDeps, ref)
		if !dep.Blocking {
			t.Errorf("the foreign private dependency %s = %+v, want blocking: withholding the project's name "+
				"must not stop the preview from refusing on it", ref, dep)
		}
	}
	if got.Publishable {
		t.Error("publishable = true over a foreign private dependency")
	}

	// --- (2) the asset half: a pid of B's private asset ---
	// A pid is an identity the caller can hold without being able to read the
	// project it names, and the asset half of the defect answered with that
	// project's id and the asset's title.
	c := defaultCandidate(f)
	c.assetPID = f.privateAssetPID
	rawAsset, gotAsset := runPreview(t, alice, f.projectA, previewCandidateJSON(t, c))

	for _, secret := range []string{f.projectB, "Private dependency"} {
		if strings.Contains(rawAsset, secret) {
			t.Errorf("the preview of a foreign asset's pid contains %q, which belongs to another private "+
				"project:\n%s", secret, rawAsset)
		}
	}
	if !gotAsset.Asset.Resolved {
		t.Error("asset.resolved = false for a pid that names a stored asset: the caller sent the pid, so its " +
			"existence is not news, and reporting an existing asset as nonexistent would be a false finding")
	}
	if gotAsset.Asset.Title != "" || gotAsset.Asset.OriginProjectID != "" {
		t.Errorf("asset = %+v, want the foreign private asset's title and project withheld", gotAsset.Asset)
	}
	// The refusal the asset half produces is still produced, and it no longer
	// names the project it refuses about.
	mismatch := blockerWithCode(t, gotAsset.PublishBlockers, "PREVIEW_ASSET_PROJECT_MISMATCH")
	if strings.Contains(mismatch.Detail, f.projectB) {
		t.Errorf("the asset-mismatch blocker names the foreign project: %q", mismatch.Detail)
	}
	if !strings.Contains(mismatch.Detail, "another private project") {
		t.Errorf("the asset-mismatch blocker neither names nor describes the asset's project: %q", mismatch.Detail)
	}
}

// TestAssetPreviewRendersPublicProjectIdentities is the other half of the rule
// T0712 adds, over the same rows with the foreign project made public: the
// tightening must not withhold what is not private. A public project's id, its
// visibility and its entities' titles are what any authenticated caller can
// read from the project's own routes (docs/12 §2), so they are rendered here
// as they were before — for the refs, for the carried object version, and for
// the asset half, whose refusal names the project it refuses about.
//
// The title assertions on the RAW response in both directions are the point:
// "Private record" is proven absent in one state and present in the other, so
// the absence in TestAssetPreviewDoesNotDiscloseAForeignPrivateEntity is a
// fact about the visibility rule rather than about a string that never appears
// in this response at all.
func TestAssetPreviewRendersPublicProjectIdentities(t *testing.T) {
	ctx := testCtx(t)
	ts, pool, _ := newAssetPreviewServer(t, ctx)
	alice, aliceID := signup(t, ts.URL, "preview-public-render@example.com", "preview-public-render")
	_, bobID := signup(t, ts.URL, "preview-public-render-bob@example.com", "preview-public-render-bob")
	f := seedAssetPreviewFixture(t, ctx, pool, aliceID, bobID)
	f.makeForeignProjectPublic(t, ctx, pool)

	raw, got := runPreview(t, alice, f.projectA, previewCandidateJSON(t, defaultCandidate(f)))

	if !strings.Contains(raw, "Private record") {
		t.Errorf("the preview of a PUBLIC project's object version does not render its title:\n%s", raw)
	}
	if len(got.Objects) != 1 {
		t.Fatalf("objects = %v, want the object version the ref carries", got.Objects)
	}
	if obj := got.Objects[0]; obj.ProjectID != f.projectB || obj.Title != "Private record" ||
		obj.CurrentVisibility != "public" || obj.ObjectID != f.objectB {
		t.Errorf("objects[0] = %+v, want the public project's object version rendered with its project, title "+
			"and visibility", obj)
	}
	refs := map[string]previewRefWire{}
	for _, r := range got.Refs {
		refs[r.Ref] = r
	}
	for _, ref := range []string{"release:" + f.releaseB, "object_version:" + f.objectVersionB} {
		r := refs[ref]
		if !r.Resolved || r.ProjectID != f.projectB || r.CurrentVisibility != "public" {
			t.Errorf("ref %s = %+v, want a public project's ref rendered with its project and visibility", ref, r)
		}
	}

	// The asset half, same state: a pid of the now-public project's asset
	// renders its title and its project, and the mismatch refusal names the
	// project it refuses about.
	c := defaultCandidate(f)
	c.assetPID = f.privateAssetPID
	rawAsset, gotAsset := runPreview(t, alice, f.projectA, previewCandidateJSON(t, c))

	if !strings.Contains(rawAsset, "Private dependency") {
		t.Errorf("the preview of a PUBLIC project's asset does not render its title:\n%s", rawAsset)
	}
	if gotAsset.Asset.Title != "Private dependency" || gotAsset.Asset.OriginProjectID != f.projectB {
		t.Errorf("asset = %+v, want the public project's asset rendered with its title and project", gotAsset.Asset)
	}
	mismatch := blockerWithCode(t, gotAsset.PublishBlockers, "PREVIEW_ASSET_PROJECT_MISMATCH")
	if !strings.Contains(mismatch.Detail, f.projectB) {
		t.Errorf("the asset-mismatch blocker does not name the public project it refuses about: %q", mismatch.Detail)
	}
}

// --------------------------------------------------------------------------
// 1b. A manifest the platform cannot read is a finding, not an error

// unreadableManifestHash is the digest a candidate whose manifest cannot be
// read carries: 64 lowercase hex characters, i.e. a syntactically valid
// sha256 that covers no canonical bytes at all — because there are none to
// cover (see previewBodyJSON). It is deliberately distinctive so a failure
// message naming it is obviously about this case.
var unreadableManifestHash = strings.Repeat("ab12cd34", 8)

// TestAssetPreviewAnswersAnUnreadableManifest pins the case the route used
// to answer with a 500 (INTERNAL_ERROR, "state reader dropped a ref"): a
// candidate whose manifest cannot be read. It is not a hypothetical input —
// a version whose document is missing a required key is exactly what a
// preview exists to catch — and a client-input finding must not be served
// as a service defect.
//
// Two halves are pinned, and both are needed:
//
//   - the READER, called directly: the refs are the candidate's own field,
//     so they are resolved whether or not the manifest parses. The pins and
//     the blobs really are declared in the manifest, so an unreadable one
//     costs exactly those two lists.
//   - the ROUTE, over real PostgreSQL: 200 with the gate's manifest refusal
//     listed, publishable false, the refs reported as usual, and — the part
//     that must not be lost with the manifest — the foreign private refs
//     still NAMED as blocking.
//
// The third half, that Preview itself treats an unreadable manifest as a
// refused candidate rather than an unanswerable one, is pinned in
// internal/assets (TestPreviewAnswersForAnUnreadableManifest): it takes the
// state directly, so it cannot see the reader's early return, which is why
// the first half above exists.
func TestAssetPreviewAnswersAnUnreadableManifest(t *testing.T) {
	ctx := testCtx(t)
	ts, pool, _ := newAssetPreviewServer(t, ctx)
	alice, aliceID := signup(t, ts.URL, "preview-unreadable@example.com", "preview-unreadable")
	_, bobID := signup(t, ts.URL, "preview-unreadable-bob@example.com", "preview-unreadable-bob")
	f := seedAssetPreviewFixture(t, ctx, pool, aliceID, bobID)

	// A dataset manifest missing quality_notes: the asset type requires it
	// (internal/assets, requiredMetadata), so the platform refuses to read
	// the document at all.
	c := defaultCandidate(f)
	manifest := previewManifest(c)
	delete(manifest["metadata"].(map[string]any), "quality_notes")
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal the unreadable manifest: %v", err)
	}
	// The fixture must really be unreadable, by the platform's own parser.
	// Without this the test would assert on a readable document and pass
	// while measuring nothing.
	if _, err := assets.ParseManifest(raw); err == nil {
		t.Fatal("the fixture manifest parses; this case is about one the platform refuses to read")
	}

	// --- the reader's half, called directly ---
	store := assetshttp.NewPostgresStateStore(pool)
	state, err := store.ResolvePreviewState(ctx, assets.PublishCandidate{
		AssetPID:   assets.PID(c.assetPID),
		Manifest:   raw,
		OriginRefs: c.refs,
	})
	if err != nil {
		t.Fatalf("the state reader failed on a candidate whose manifest it cannot read: %v", err)
	}
	if state.Asset == nil || !state.Asset.Type.Valid() {
		t.Errorf("asset = %+v, want the published-under asset resolved: the manifest axis must not cost "+
			"the asset axis", state.Asset)
	}
	if len(state.Refs) != len(c.refs) {
		t.Fatalf("the reader answered for %d of the candidate's %d refs; an unreadable manifest must not "+
			"cost the refs, which the candidate declares itself", len(state.Refs), len(c.refs))
	}
	for i, ref := range state.Refs {
		if !ref.Resolved || ref.ProjectID == "" || ref.ProjectVisibility == "" {
			t.Errorf("refs[%d] = %+v, want a resolved ref with its project's visibility", i, ref)
		}
	}
	if len(state.Pins) != 0 || len(state.Blobs) != 0 {
		t.Errorf("pins = %+v, blobs = %+v, want both empty: they are declared in the manifest that could "+
			"not be read, so there is nothing to resolve them from", state.Pins, state.Blobs)
	}

	// --- the route's half, over real PostgreSQL ---
	_, got := runPreview(t, alice, f.projectA, previewBodyJSON(t, c, manifest, unreadableManifestHash))

	// The manifest refusal is the gate's own code, in the candidate's own
	// blocker list — this is what stands in for the pins and the blobs the
	// unreadable document could not declare, and what makes the empty lists
	// below read as "could not be read" rather than "nothing private here".
	blocker := blockerWithCode(t, got.PublishBlockers, "ASSET_METADATA_MISSING")
	if blocker.Field != "metadata.quality_notes" {
		t.Errorf("the manifest refusal names field %q, want the missing key", blocker.Field)
	}
	if got.Publishable {
		t.Error("publishable = true for a candidate whose manifest cannot be read")
	}
	if len(got.Metadata) != 0 || len(got.Blobs) != 0 || len(got.Dependencies) != 0 {
		t.Errorf("metadata = %v, blobs = %v, dependencies = %v, want all three empty: the manifest that "+
			"declares them could not be read", got.Metadata, got.Blobs, got.Dependencies)
	}

	// The refs are the candidate's own field, and they survive: all three
	// are resolved. The one into the publishing project renders where it
	// points, and the two into the foreign private project do not (T0712,
	// issue #238) — the same rule as everywhere else on this route, which an
	// unreadable manifest does not suspend: a document the platform cannot
	// read is not a reason to disclose another project.
	byRef := map[string]previewRefWire{}
	for _, ref := range got.Refs {
		byRef[ref.Ref] = ref
	}
	for _, ref := range []string{"release:" + f.releaseA, "release:" + f.releaseB, "object_version:" + f.objectVersionB} {
		if _, ok := byRef[ref]; !ok {
			t.Fatalf("refs = %+v, want %s among them", got.Refs, ref)
		}
	}
	if own := byRef["release:"+f.releaseA]; !own.Resolved || own.ProjectID != f.projectA ||
		own.CurrentVisibility != "private" {
		t.Errorf("release:%s = %+v, want the publishing project's own ref rendered", f.releaseA, own)
	}
	for _, ref := range []string{"release:" + f.releaseB, "object_version:" + f.objectVersionB} {
		r := byRef[ref]
		if !r.Resolved {
			t.Errorf("%s = %+v, want it reported as resolved", ref, r)
		}
		if r.ProjectID != "" || r.CurrentVisibility != "" {
			t.Errorf("%s = %+v, want the foreign private project's id and visibility withheld", ref, r)
		}
	}

	// And the leak naming does not depend on the manifest: the two refs into
	// the foreign private project are still named and still blocking.
	for _, ref := range []string{"release:" + f.releaseB, "object_version:" + f.objectVersionB} {
		dep := privateDep(t, got.PrivateDeps, ref)
		if !dep.Blocking {
			t.Errorf("%s = %+v, want blocking: an unreadable manifest does not make another project's "+
				"private state safe to disclose", ref, dep)
		}
	}
	if dep := privateDep(t, got.PrivateDeps, "release:"+f.releaseA); dep.Blocking {
		t.Errorf("the publishing project's own private release = %+v, want named without blocking", dep)
	}
}

// blockerWithCode returns the blocker of one list carrying the given code,
// and fails when the list does not carry it.
func blockerWithCode(t *testing.T, blockers []previewBlockerWire, code string) previewBlockerWire {
	t.Helper()
	for _, b := range blockers {
		if b.Code == code {
			return b
		}
	}
	t.Fatalf("no %s among %+v", code, blockers)
	return previewBlockerWire{}
}

// --------------------------------------------------------------------------
// 2. Repeatable, and 3. it changes nothing

// TestAssetPreviewIsRepeatableAndReadOnly is acceptance criteria 1 and 2
// together: the same candidate against the same state answers
// byte-identical JSON twice, and neither run changes a single row.
func TestAssetPreviewIsRepeatableAndReadOnly(t *testing.T) {
	ctx := testCtx(t)
	ts, pool, _ := newAssetPreviewServer(t, ctx)
	alice, aliceID := signup(t, ts.URL, "preview-repeat@example.com", "preview-repeat")
	_, bobID := signup(t, ts.URL, "preview-repeat-bob@example.com", "preview-repeat-bob")
	f := seedAssetPreviewFixture(t, ctx, pool, aliceID, bobID)

	before := fingerprintDatabase(t, ctx, pool)
	if before == "" {
		t.Fatal("the fingerprint is empty: the instrument would pass on an empty database")
	}

	body := previewCandidateJSON(t, defaultCandidate(f))
	first, payload := runPreview(t, alice, f.projectA, body)
	second, _ := runPreview(t, alice, f.projectA, body)
	if first != second {
		t.Errorf("two previews of the same candidate differ:\n%s\n%s", first, second)
	}

	after := fingerprintDatabase(t, ctx, pool)
	if before != after {
		t.Errorf("the database changed across a preview:\nbefore:\n%s\nafter:\n%s", before, after)
	}

	// The preview still says what it said: a state that had drifted would
	// show up here as well as in the fingerprint above.
	if payload.Publishable {
		t.Errorf("publishable = true, want the private dependencies to block")
	}
	if len(payload.PrivateDeps) == 0 {
		t.Errorf("private_dependencies = %v, want the private dependencies named", namedRefs(payload.PrivateDeps))
	}

	// The instrument's own control, after every comparison it is used for:
	// a fingerprint that could not notice a change would have passed the
	// check above for the wrong reason. One row is written here and the
	// fingerprint must move — this is the same write the read-only suite
	// proves the route's own pool cannot perform.
	if err := probeWrite(ctx, pool, "preview-fingerprint-control"); err != nil {
		t.Fatalf("the control write failed: %v", err)
	}
	if moved := fingerprintDatabase(t, ctx, pool); moved == after {
		t.Error("the fingerprint did not change after a row was written: the check above measured nothing")
	}
}

// TestAssetPreviewSessionRefusesWrites proves the store's pool is read-only
// BY THE SERVER, and that the proof means something.
//
// The claim needs both halves: a write refused because the session forbids
// it, and the same write accepted on a pool that does not carry the setting
// — otherwise a typo in the probe would look exactly like a protection.
func TestAssetPreviewSessionRefusesWrites(t *testing.T) {
	ctx := testCtx(t)
	_, pool, readOnly := newAssetPreviewServer(t, ctx)

	var setting string
	if err := readOnly.QueryRow(ctx, `SHOW default_transaction_read_only`).Scan(&setting); err != nil {
		t.Fatalf("read the session setting: %v", err)
	}
	if setting != "on" {
		t.Fatalf("the store's pool carries default_transaction_read_only=%q, want on", setting)
	}

	// The control first: the statement IS a write, and it succeeds where
	// writes are allowed.
	if err := probeWrite(ctx, pool, "preview-write-control"); err != nil {
		t.Fatalf("the control write was refused on the writable pool (%v): the probe is not a valid write", err)
	}
	// ... and it is refused here, by the server.
	err := probeWrite(ctx, readOnly, "preview-write-probe")
	if err == nil {
		t.Fatal("a write through the store's pool succeeded: the read-only session is not in force")
	}
	if got := sqlState(t, err); got != readOnlySQLState {
		t.Fatalf("the write was refused with SQLSTATE %s, want %s (read_only_sql_transaction): %v",
			got, readOnlySQLState, err)
	}
}

// probeWrite attempts one insertion.
func probeWrite(ctx context.Context, pool *pgxpool.Pool, handle string) error {
	_, err := pool.Exec(ctx, `INSERT INTO users (handle, display_name) VALUES ($1, $2)`, handle, handle)
	return err
}

// fingerprintDatabase renders every row of every table, plus the row count
// per table, as one text: the instrument the "no state written" criterion
// is checked with.
//
// Every table, not the interesting ones: a preview that wrote an audit row,
// a domain event or an outbox entry would be as much a state change as one
// that wrote the asset version, and a check that only looked at
// research_asset_versions would not see it. The row text is ordered inside
// each table, so the fingerprint is a function of the CONTENTS rather than
// of the physical row order.
func fingerprintDatabase(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT table_name FROM information_schema.tables
		 WHERE table_schema = 'public' AND table_type = 'BASE TABLE' ORDER BY table_name`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("list tables: %v", err)
	}
	if len(tables) == 0 {
		t.Fatal("no tables in the test database")
	}

	var out []byte
	for _, table := range tables {
		var count int64
		var digest string
		q := fmt.Sprintf(
			`SELECT count(*), coalesce(md5(string_agg(t::text, E'\n' ORDER BY t::text)), '<empty>') FROM %s t`,
			pgx.Identifier{table}.Sanitize())
		if err := pool.QueryRow(ctx, q).Scan(&count, &digest); err != nil {
			t.Fatalf("fingerprint %s: %v", table, err)
		}
		out = append(out, fmt.Sprintf("%s rows=%d md5=%s\n", table, count, digest)...)
	}
	return string(out)
}
