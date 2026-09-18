// Task T0707 — asset references and dependencies, over real PostgreSQL.
//
// The table asset_dependencies has existed since migration 00010 and has
// never had a writer: the asset page could render "used/derived public
// links" (docs/42 §Asset Page, T0709) and nothing in the platform could
// ever put a row there. This file pins the recording path — the half that
// only a real database can settle — over the whole composed route: the real
// guard, the real project read gate, the real publish transaction writing
// the rows, and the real readers over the canonical queries
// (internal/persistence/queries/asset_dependencies.sql).
//
// What is asserted here, and why each one needs a database:
//
//  1. A publish RECORDS the usages its manifest declares, inside the same
//     transaction as the version row. The row names the RESOLVED version
//     row's id, is a depends_on (docs/11 §5: a pin is what the work was
//     built against), and declares the published version's own visibility.
//     A publish that is refused writes nothing at all — asserted on the
//     table, not on the response.
//
//  2. A pin is an EXACT version. The upstream publishes a newer version and
//     the pin — in the version document, in the recorded usage row, and on
//     the public page, where the newest version is now current — still names
//     the version it named before. The upgrade is real (the upstream asset
//     really gains a second version), so "nothing changed" cannot be the
//     answer because nothing happened.
//
//  3. The public page lists a usage only when the row declares it public AND
//     the using project is public. Both halves are exercised with a CONTROL
//     that flips exactly one column and shows the same row becoming
//     readable — so the absence is the rule firing, not an empty fixture.
//
//  4. No private entity is inferable (docs/23 §5, the issue #238 class): a
//     private project using a public asset leaves nothing on that asset's
//     public page — no name, no slug, no id, no count. The forbidden
//     strings are read out of the rows themselves, so a fixture that
//     stopped existing fails the test instead of vacuously passing it.
//
//  5. The project-side read answers the other direction ("what does this
//     project use"), over the real gate: a member sees the project's own
//     declarations, a non-member of a public project sees its public ones,
//     and a project the caller may not read answers the existence-hiding
//     404 the project surface answers.
//
//  6. dependency_type carries NO database CHECK: the vocabulary lives in Go
//     (internal/assets/dependency_type.go, the convention 00064:41-45,
//     00066, 00067 and 00045:74-79 declare). The probe that says so is a
//     write the database ACCEPTS — a value outside the vocabulary — with a
//     control proving the same probe finds the CHECK that IS there
//     (visibility_of_usage), and the readers rendering the stored value
//     verbatim.

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/assetshttp"
	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/policyhttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/internal/application/assetpublish"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rights"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// assetDependencyTaskID namespaces this task's test databases
// (test_T0707_<run_id>).
const assetDependencyTaskID = "T0707"

// --------------------------------------------------------------------------
// The composed surface

// dependencyWorld is the surface as production wires it.
//
// Two pools over ONE database, the same split the page suite makes: the
// writable one seeds the fixture and drives the publish command; the
// read-only one backs the dependency READ, so "the read cannot write" is a
// property of the connections it runs on rather than a promise about the
// code.
type dependencyWorld struct {
	ts       *httptest.Server
	pool     *pgxpool.Pool
	readOnly *pgxpool.Pool
}

// newDependencyWorld composes the production tree over one test database,
// exactly as cmd/api/main.go does.
func newDependencyWorld(t *testing.T, ctx context.Context) *dependencyWorld {
	t.Helper()
	pool, dbURL := testdb.Setup(t, ctx, adminURL(t), assetDependencyTaskID)
	readOnly := openReadOnlyPool(t, ctx, dbURL)

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
	orgStore := persistence.NewOrgStore(pool)
	projectStore := persistence.NewProjectStore(pool)
	projectAPI := projectshttp.New(projectshttp.Deps{
		Store: projectStore,
		Orgs:  orgStore,
		Authz: authz.NewMatrixEngine(),
	})
	policyAPI := policyhttp.New(policyhttp.Deps{
		Store:    persistence.NewPolicyStore(pool),
		Orgs:     orgStore,
		Projects: projectStore,
	})
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	publishCommand := assetpublish.NewCommand(assetpublish.Deps{
		Members:  projectStore,
		Policies: policyAPI.Service(),
		Rules:    policy.NewRuleEvaluator(),
		Store:    persistence.NewAssetPublishStore(pool, rsgvalidation.NewValidator(reg)),
		Authz:    authz.NewMatrixEngine(),
	})

	// The read routes' own view of projects, over READ-ONLY connections, for
	// the reason the page suite gives: the gate and the membership read are
	// reads, and here they run where the server refuses to write.
	readProjects := projectshttp.New(projectshttp.Deps{
		Store: persistence.NewProjectStore(readOnly),
		Orgs:  persistence.NewOrgStore(readOnly),
		Authz: authz.NewMatrixEngine(),
	})

	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", authAPI.Routes())
	mux.Handle("/api/v1/projects", projectAPI.Routes())
	mux.Handle("/api/v1/projects/", projectAPI.Routes())
	policyAPI.Register(mux)
	assetshttp.New(assetshttp.Deps{
		State:        assetshttp.NewPostgresStateStore(pool),
		Projects:     readProjects.Service(),
		Publish:      publishCommand,
		Pages:        persistence.NewAssetPageStore(readOnly),
		Members:      readProjects.Service(),
		Dependencies: persistence.NewProjectDependencyStore(readOnly),
	}).Register(mux)

	ts := httptest.NewServer(authAPI.Guard(mux))
	t.Cleanup(ts.Close)
	return &dependencyWorld{ts: ts, pool: pool, readOnly: readOnly}
}

// --------------------------------------------------------------------------
// The fixture

// dependencyProject is one seeded project: its identity, the release a
// publication of its work pins as provenance (docs/11 §3), and an openly
// attached blob its manifests may name.
type dependencyProject struct {
	ID       string
	Name     string
	Slug     string
	Release  string
	OpenBlob string
}

// dependencyFixture is the state the cases run against: three projects with
// their owners, and three seeded (version-less) assets, one per project.
//
// The projects and assets are rows rather than requests: what this file is
// about is the dependency recording, and a fixture that also had to prove
// the create-project flow would be testing T0103/T0705 again. The versions
// this suite reads ARE published through the real route, so the manifests,
// the integrity hashes and the usage rows are the ones production writes.
type dependencyFixture struct {
	world                   *dependencyWorld
	alice, bob, carol       *testUserClient
	anon                    *testUserClient
	aliceID, bobID, carolID string
	// up publishes the upstream asset, down depends on it, hidden is a
	// PRIVATE project that also depends on it (the disclosure case).
	up, down, hidden dependencyProject
	// x/y/z are the seeded assets' pids with the titles and slugs the
	// assertions search for on the wire.
	xPID, xTitle, xSlug string
	yPID, yTitle, ySlug string
	zPID, zTitle, zSlug string
}

// newDependencyFixture builds the fixture over a fresh database.
func newDependencyFixture(t *testing.T, ctx context.Context) *dependencyFixture {
	t.Helper()
	w := newDependencyWorld(t, ctx)
	fx := &dependencyFixture{world: w, anon: newTestUserClient(w.ts.URL)}
	fx.alice, fx.aliceID = signup(t, w.ts.URL, "dep-alice@example.com", "dep-alice")
	fx.bob, fx.bobID = signup(t, w.ts.URL, "dep-bob@example.com", "dep-bob")
	fx.carol, fx.carolID = signup(t, w.ts.URL, "dep-carol@example.com", "dep-carol")

	fx.up = seedDependencyProject(t, ctx, w.pool, fx.aliceID, "upstream-lab", "Upstream Lab", "public")
	fx.down = seedDependencyProject(t, ctx, w.pool, fx.bobID, "downstream-lab", "Downstream Lab", "public")
	fx.hidden = seedDependencyProject(t, ctx, w.pool, fx.carolID, "hidden-lab", "Hidden Lab", "private")

	fx.xPID, fx.xTitle, fx.xSlug = seedDependencyAsset(t, ctx, w.pool, fx.up.ID, fx.aliceID,
		"01j9z6k3m4n5p6q7r8s9t0v2a1", "zeolite-stability", "Zeolite Stability Screening")
	fx.yPID, fx.yTitle, fx.ySlug = seedDependencyAsset(t, ctx, w.pool, fx.down.ID, fx.bobID,
		"01j9z6k3m4n5p6q7r8s9t0v2a2", "mof-screening", "MOF Screening Set")
	fx.zPID, fx.zTitle, fx.zSlug = seedDependencyAsset(t, ctx, w.pool, fx.hidden.ID, fx.carolID,
		"01j9z6k3m4n5p6q7r8s9t0v2a3", "hidden-study", "Hidden Study")
	return fx
}

// seedDependencyProject inserts one project with everything a publication of
// its work needs: the owner's membership (the gate reads the caller's role),
// an accepted state and a release to pin as provenance, and an openly
// attached blob.
func seedDependencyProject(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ownerID, slug, name, visibility string) dependencyProject {
	t.Helper()
	p := dependencyProject{Name: name, Slug: slug}
	p.ID = mustQueryUUID(t, ctx, pool,
		`INSERT INTO projects (slug, name, purpose, visibility, created_by)
		 VALUES ($1, $2, 'T0707 fixture', $3, $4) RETURNING id`, slug, name, visibility, ownerID)
	if _, err := pool.Exec(ctx,
		`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, 'owner')`,
		p.ID, ownerID); err != nil {
		t.Fatalf("seed %s membership: %v", slug, err)
	}
	stateID := mustQueryUUID(t, ctx, pool,
		`INSERT INTO project_states (project_id, state_hash, manifest_version)
		 VALUES ($1, $2, '1') RETURNING id`, p.ID, "genesis-"+slug)
	p.Release = mustQueryUUID(t, ctx, pool,
		`INSERT INTO releases (project_id, version, title, state_id, manifest, manifest_hash, created_by)
		 VALUES ($1, 'v1.0.0', $2, $3, '{}'::jsonb, 'x', $4) RETURNING id`,
		p.ID, name+" release", stateID, ownerID)

	objectID := mustQueryUUID(t, ctx, pool,
		`INSERT INTO scientific_objects (project_id, object_type, created_by)
		 VALUES ($1, 'dataset_record', $2) RETURNING id`, p.ID, ownerID)
	objectVersionID := mustQueryUUID(t, ctx, pool,
		`INSERT INTO scientific_object_versions
			(object_id, version_no, state_id, schema_id, schema_version, title, lifecycle_state, payload, integrity_hash, created_by)
		 VALUES ($1, 1, $2, 'https://open-rd.example/schemas/dataset.schema.json', '1', $3, 'active',
		         '{}'::jsonb, 'x', $4) RETURNING id`,
		objectID, stateID, name+" record", ownerID)
	p.OpenBlob = mustQueryUUID(t, ctx, pool,
		`INSERT INTO blobs (content_hash, size_bytes, storage_key, created_by)
		 VALUES ($1, 1024, $2, $3) RETURNING id`, "sha256:"+slug+"-open", "blobs/"+slug+"/open", ownerID)
	if _, err := pool.Exec(ctx,
		`INSERT INTO blob_attachments (blob_id, scientific_object_version_id, attachment_role, access_level, state_id)
		 VALUES ($1, $2, 'data', 'open', $3)`, p.OpenBlob, objectVersionID, stateID); err != nil {
		t.Fatalf("attach %s blob: %v", slug, err)
	}
	return p
}

// seedDependencyAsset inserts one research asset with NO version: the
// versions this suite reads are the ones the publish route writes.
func seedDependencyAsset(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID, ownerID, pid, slug, title string) (string, string, string) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`INSERT INTO research_assets (asset_type, slug, title, origin_project_id, pid)
		 VALUES ('dataset', $1, $2, $3, $4)`, slug, title, projectID, pid); err != nil {
		t.Fatalf("seed asset %s: %v", slug, err)
	}
	return pid, title, slug
}

// --------------------------------------------------------------------------
// Publishing, through the real route

// publishVersion publishes one version of the fixture's asset, through the
// real publish route and the real store, with the given pins.
func (fx *dependencyFixture) publishVersion(t *testing.T, uc *testUserClient, p dependencyProject, pid, version, visibility, creatorID string, pins ...assets.DependencyPin) publishedAssetWire {
	t.Helper()
	cand := buildCandidate(t, candidateOptions{
		pid:         pid,
		version:     version,
		visibility:  visibility,
		refs:        []string{"release:" + p.Release},
		pins:        pins,
		creators:    []string{creatorID},
		blobIDs:     []string{p.OpenBlob},
		accessLevel: "open",
		dataAccess:  rights.DataAccessOpen,
	})
	return mustPublish(t, uc, p.ID, cand.body(t), "k-"+pid+"-"+version+"-"+visibility)
}

// differentVersionCandidate is a publishable version of the fixture's asset
// that is NOT published: the cases that need a refusal send it.
func (fx *dependencyFixture) differentVersionCandidate(t *testing.T, p dependencyProject, pid, version, visibility, creatorID string, pins ...assets.DependencyPin) publishCandidate {
	t.Helper()
	return buildCandidate(t, candidateOptions{
		pid:         pid,
		version:     version,
		visibility:  visibility,
		refs:        []string{"release:" + p.Release},
		pins:        pins,
		creators:    []string{creatorID},
		blobIDs:     []string{p.OpenBlob},
		accessLevel: "open",
		dataAccess:  rights.DataAccessOpen,
	})
}

// --------------------------------------------------------------------------
// Reading, through the real routes

// projectDependenciesURL is the route T0707 mounts (cmd/api/assetshttp/
// dependencies.go): not a contract path, mounted on the v1 mux the way
// provenancehttp mounts its projection walk.
func projectDependenciesURL(projectID string) string {
	return "/api/v1/projects/" + projectID + "/dependencies"
}

// dependencyPayload is the wire shape this suite reads (declared here rather
// than reused from the handler, so a silent JSON tag change fails here).
type dependencyPayload struct {
	Dependencies []dependencyEntryWire `json:"dependencies"`
}

type dependencyEntryWire struct {
	Pin               string    `json:"pin"`
	Title             string    `json:"title"`
	Type              string    `json:"type"`
	URL               string    `json:"url"`
	DependencyType    string    `json:"dependency_type"`
	ImpactAnalysis    bool      `json:"impact_analysis"`
	VisibilityOfUsage string    `json:"visibility_of_usage"`
	CreatedAt         time.Time `json:"created_at"`
}

// fetchProjectDependencies requires the 200 and returns the raw body with
// the decoded payload — the negative assertions read the raw bytes, which is
// what makes them a check on the whole surface rather than on one struct.
func fetchProjectDependencies(t *testing.T, uc *testUserClient, projectID string) (string, dependencyPayload) {
	t.Helper()
	resp := uc.do(t, http.MethodGet, projectDependenciesURL(projectID), "")
	mustStatus(t, resp, http.StatusOK)
	raw := readAll(t, resp)
	var got dependencyPayload
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("project dependencies payload: %v: %s", err, raw)
	}
	return raw, got
}

// --------------------------------------------------------------------------
// The rows, as SQL reports them

// dependencyRow is one asset_dependencies row as this suite reads it back.
type dependencyRow struct {
	AssetVersionID    string
	DependencyType    string
	VisibilityOfUsage string
	CreatedAt         time.Time
}

// dependencyRows reads the project's rows, oldest first — the table's own
// answer, which is what the wire is compared against.
func dependencyRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID string) []dependencyRow {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT asset_version_id::text, dependency_type, visibility_of_usage, created_at
		   FROM asset_dependencies WHERE project_id = $1
		  ORDER BY created_at, asset_version_id, dependency_type`, projectID)
	if err != nil {
		t.Fatalf("read asset_dependencies: %v", err)
	}
	defer rows.Close()
	var out []dependencyRow
	for rows.Next() {
		var r dependencyRow
		if err := rows.Scan(&r.AssetVersionID, &r.DependencyType, &r.VisibilityOfUsage, &r.CreatedAt); err != nil {
			t.Fatalf("scan asset_dependencies: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read asset_dependencies: %v", err)
	}
	return out
}

// mustVersionID resolves one published version's row id: the identity the
// recorded usage names, which the wire deliberately does not carry.
func mustVersionID(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid, version string) string {
	t.Helper()
	return versionRowID(t, ctx, pool, assetRowID(t, ctx, pool, pid), version)
}

// --------------------------------------------------------------------------
// 1. Recording

// TestAssetDependencyPublishRecordsTheDeclaredUsages: the publish is the
// governed write that carries a project's declaration of what it was built
// against, so the usage rows are written in its transaction — one row per
// declared pin, naming the RESOLVED version, typed depends_on, declaring the
// published version's own visibility. A replay under the same idempotency
// key writes no second row, and a REFUSED publish writes nothing at all.
func TestAssetDependencyPublishRecordsTheDeclaredUsages(t *testing.T) {
	ctx := context.Background()
	fx := newDependencyFixture(t, ctx)
	x1 := mustPin(t, fx.xPID, "1.0")

	fx.publishVersion(t, fx.alice, fx.up, fx.xPID, "1.0", "public", fx.aliceID)
	fx.publishVersion(t, fx.bob, fx.down, fx.yPID, "1.0", "public", fx.bobID, x1)

	rows := dependencyRows(t, ctx, fx.world.pool, fx.down.ID)
	if len(rows) != 1 {
		t.Fatalf("the publish wrote %d dependency rows, want exactly 1: %+v", len(rows), rows)
	}
	got := rows[0]
	if want := mustVersionID(t, ctx, fx.world.pool, fx.xPID, "1.0"); got.AssetVersionID != want {
		t.Errorf("the row names version row %s, want the RESOLVED version's id %s (the pin %s)", got.AssetVersionID, want, x1)
	}
	if got.DependencyType != string(assets.DependencyTypeDependsOn) {
		t.Errorf("dependency_type = %q, want %q (docs/11 §5: a pin is what the work was built against)", got.DependencyType, assets.DependencyTypeDependsOn)
	}
	if got.VisibilityOfUsage != string(assets.VisibilityPublic) {
		t.Errorf("visibility_of_usage = %q, want the published version's own %q", got.VisibilityOfUsage, assets.VisibilityPublic)
	}
	if got.CreatedAt.IsZero() {
		t.Error("the row's created_at is the zero instant")
	}

	// The same publish again, same key: the row is not written twice.
	cand := buildCandidate(t, candidateOptions{
		pid: fx.yPID, version: "1.0", visibility: "public",
		refs: []string{"release:" + fx.down.Release}, pins: []assets.DependencyPin{x1},
		creators: []string{fx.bobID}, blobIDs: []string{fx.down.OpenBlob},
		accessLevel: "open", dataAccess: rights.DataAccessOpen,
	})
	mustPublish(t, fx.bob, fx.down.ID, cand.body(t), "k-"+fx.yPID+"-1.0-public")
	if again := dependencyRows(t, ctx, fx.world.pool, fx.down.ID); len(again) != 1 {
		t.Errorf("a replayed publish left %d rows, want the one it already wrote: %+v", len(again), again)
	}

	// A publish the gate REFUSES — here, a pin that resolves to no stored
	// version (PREVIEW_DEPENDENCY_UNRESOLVED) — writes no usage and no
	// version: both halves of the write live in one transaction.
	unresolved := mustPin(t, fx.xPID, "9.9")
	refused := fx.differentVersionCandidate(t, fx.down, fx.yPID, "2.0", "public", fx.bobID, unresolved)
	report := mustRefuse(t, fx.bob, fx.down.ID, refused.body(t), "k-"+fx.yPID+"-2.0-public")
	if !hasBlocker(report.Preview.PublishBlockers, assets.CodePreviewPinUnresolved) {
		t.Errorf("the refusal does not name %s among its blockers: %+v", assets.CodePreviewPinUnresolved, report.Preview.PublishBlockers)
	}
	if after := dependencyRows(t, ctx, fx.world.pool, fx.down.ID); len(after) != 1 {
		t.Errorf("a refused publish left %d dependency rows, want the 1 it already had: %+v", len(after), after)
	}
	if n := countRows(t, ctx, fx.world.pool,
		`SELECT count(*) FROM research_asset_versions v JOIN research_assets a ON a.id = v.asset_id
		  WHERE a.pid = $1 AND v.version = '2.0'`, fx.yPID); n != 0 {
		t.Errorf("a refused publish stored %d version rows", n)
	}
}

// hasBlocker reports whether one code is among the preview's blockers.
func hasBlocker(blockers []previewBlockerWire, code string) bool {
	for _, b := range blockers {
		if b.Code == code {
			return true
		}
	}
	return false
}

// --------------------------------------------------------------------------
// 2. An exact pin against an upstream upgrade

// TestAssetDependencyPinSurvivesAnUpstreamUpgrade is the acceptance
// criterion that a pin means an EXACT version: the upstream publishes a
// newer version, and the pin — in the version document, in the recorded
// usage, and on both read surfaces — still names the version it named
// before. The upgrade is real: the upstream asset really gains a second
// version and really moves its current version, so "the answer did not
// change" cannot be the answer to "nothing happened".
func TestAssetDependencyPinSurvivesAnUpstreamUpgrade(t *testing.T) {
	ctx := context.Background()
	fx := newDependencyFixture(t, ctx)
	x1 := mustPin(t, fx.xPID, "1.0")

	fx.publishVersion(t, fx.alice, fx.up, fx.xPID, "1.0", "public", fx.aliceID)
	fx.publishVersion(t, fx.bob, fx.down, fx.yPID, "1.0", "public", fx.bobID, x1)

	before := dependencyRows(t, ctx, fx.world.pool, fx.down.ID)
	if len(before) != 1 {
		t.Fatalf("the fixture recorded %d dependency rows, want 1: %+v", len(before), before)
	}

	// The upstream moves: a second version of the SAME asset, public.
	fx.publishVersion(t, fx.alice, fx.up, fx.xPID, "2.0", "public", fx.aliceID)
	if n := countRows(t, ctx, fx.world.pool,
		`SELECT count(*) FROM research_asset_versions v JOIN research_assets a ON a.id = v.asset_id
		  WHERE a.pid = $1`, fx.xPID); n != 2 {
		t.Fatalf("the upgrade did not happen: the upstream asset has %d versions, want 2", n)
	}
	// And the upgrade really is the new current version, read the way a
	// client reads it: the page for the asset, with no version asked for.
	rawPage, page := fetchAssetPage(t, fx.anon, fx.xPID, "")
	if page.Version.Version != "2.0" {
		t.Fatalf("the upstream asset's current version is %q, want the upgraded %q: %s", page.Version.Version, "2.0", rawPage)
	}

	// The pin did not move: the stored row still names the 1.0 version row,
	// and nothing names 2.0.
	after := dependencyRows(t, ctx, fx.world.pool, fx.down.ID)
	if len(after) != 1 || after[0].AssetVersionID != before[0].AssetVersionID {
		t.Fatalf("the upstream upgrade changed the recorded usage: before %+v, after %+v", before, after)
	}
	if want := mustVersionID(t, ctx, fx.world.pool, fx.xPID, "1.0"); after[0].AssetVersionID != want {
		t.Errorf("the usage names version row %s, want the pinned 1.0 row %s", after[0].AssetVersionID, want)
	}
	if n := countRows(t, ctx, fx.world.pool,
		`SELECT count(*) FROM asset_dependencies WHERE asset_version_id = $1`,
		mustVersionID(t, ctx, fx.world.pool, fx.xPID, "2.0")); n != 0 {
		t.Errorf("%d usage rows name the upstream's NEW version; a pin is an exact version", n)
	}
	// The version document itself still pins 1.0 — the manifest is inside
	// an append-only row, so this is the stored bytes, not a rendering.
	if pins := storedManifestPins(t, ctx, fx.world.pool, fx.yPID, "1.0"); len(pins) != 1 || pins[0] != string(x1) {
		t.Errorf("the stored manifest pins %v, want the pinned %q", pins, x1)
	}

	// The project-side read still answers 1.0 for the downstream project,
	// for its member and for the network alike.
	_, memberView := fetchProjectDependencies(t, fx.bob, fx.down.ID)
	if len(memberView.Dependencies) != 1 {
		t.Fatalf("the member's dependency list = %+v, want exactly the pinned version", memberView.Dependencies)
	}
	if want := string(x1); memberView.Dependencies[0].Pin != want {
		t.Errorf("the member's entry names %q, want the pinned %q", memberView.Dependencies[0].Pin, want)
	}
	if !memberView.Dependencies[0].ImpactAnalysis {
		t.Error("a depends_on entry does not carry impact_analysis (docs/19 §3)")
	}
	_, anonView := fetchProjectDependencies(t, fx.anon, fx.down.ID)
	if len(anonView.Dependencies) != 1 || anonView.Dependencies[0].Pin != string(x1) {
		t.Errorf("the network's dependency list = %+v, want the same single pinned entry", anonView.Dependencies)
	}

	// The page's other direction agrees, and it agrees about the VERSION:
	// the usage is a statement about the pinned 1.0, so it is rendered on
	// 1.0's page and not on the page of the new current version.
	_, pinnedPage := fetchAssetPage(t, fx.anon, fx.xPID, "1.0")
	if len(pinnedPage.UsedBy) != 1 || pinnedPage.UsedBy[0].ProjectName != fx.down.Name {
		t.Fatalf("the pinned version's page lists used_by %+v, want the downstream project %q", pinnedPage.UsedBy, fx.down.Name)
	}
	if len(page.UsedBy) != 0 {
		t.Errorf("the CURRENT (upgraded) version's page lists used_by %+v; nobody uses 2.0", page.UsedBy)
	}
}

// storedManifestPins reads one stored version document's dependency pins.
func storedManifestPins(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid, version string) []string {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(ctx,
		`SELECT v.manifest FROM research_asset_versions v JOIN research_assets a ON a.id = v.asset_id
		  WHERE a.pid = $1 AND v.version = $2`, pid, version).Scan(&raw); err != nil {
		t.Fatalf("read the stored manifest of %s@%s: %v", pid, version, err)
	}
	manifest, err := assets.ParseManifest(raw)
	if err != nil {
		t.Fatalf("the stored manifest of %s@%s is not readable: %v", pid, version, err)
	}
	out := make([]string, 0, len(manifest.DependencyPins))
	for _, pin := range manifest.DependencyPins {
		out = append(out, string(pin))
	}
	return out
}

// --------------------------------------------------------------------------
// 3. What the public page may say about a usage

// TestAssetDependencyPageListsOnlyPublicDeclarationsOfPublicProjects is
// criteria 3 and 4 in one place, because the page's rule has two halves and
// the interesting cases are the ones where exactly one half fails:
//
//	carol  — a PRIVATE project declaring its usage PUBLIC  (the project fails)
//	bob    — a PUBLIC project that published its version privately, so the
//	         row declares the usage private                     (the row fails)
//
// Neither may appear on the asset's public page, and neither may be
// inferable — no name, no slug, no id, no count. The strings the assertions
// forbid are read OUT OF THE ROWS, so a fixture that stopped existing fails
// here instead of passing vacuously. Then each case is CONTROLLED: flipping
// exactly one column makes the same row readable, which is what proves the
// absence was the rule firing.
func TestAssetDependencyPageListsOnlyPublicDeclarationsOfPublicProjects(t *testing.T) {
	ctx := context.Background()
	fx := newDependencyFixture(t, ctx)
	x1 := mustPin(t, fx.xPID, "1.0")

	fx.publishVersion(t, fx.alice, fx.up, fx.xPID, "1.0", "public", fx.aliceID)
	// carol's private project publishes a PUBLIC version that uses the
	// upstream asset: the row says public, the project says private.
	fx.publishVersion(t, fx.carol, fx.hidden, fx.zPID, "1.0", "public", fx.carolID, x1)
	// bob's public project publishes a PRIVATE version that uses it: the
	// project says public, the row says private.
	fx.publishVersion(t, fx.bob, fx.down, fx.yPID, "1.0", "private", fx.bobID, x1)

	// Both rows really exist, with the values each case needs — read back
	// from the table rather than assumed.
	hiddenName := projectColumn(t, ctx, fx.world.pool, fx.hidden.ID, "name")
	hiddenSlug := projectColumn(t, ctx, fx.world.pool, fx.hidden.ID, "slug")
	downName := projectColumn(t, ctx, fx.world.pool, fx.down.ID, "name")
	downSlug := projectColumn(t, ctx, fx.world.pool, fx.down.ID, "slug")
	for what, s := range map[string]string{
		"the hidden project's name": hiddenName, "the hidden project's slug": hiddenSlug,
		"the downstream project's name": downName, "the downstream project's slug": downSlug,
	} {
		if s == "" {
			t.Fatalf("%s is empty, so a negative assertion about it would pass on any answer", what)
		}
	}
	hiddenRow := rowFor(t, ctx, fx.world.pool, fx.hidden.ID)
	downRow := rowFor(t, ctx, fx.world.pool, fx.down.ID)
	if hiddenRow.VisibilityOfUsage != string(assets.VisibilityPublic) || hiddenRow.AssetVersionID != mustVersionID(t, ctx, fx.world.pool, fx.xPID, "1.0") {
		t.Fatalf("the private project's row is %+v, want a PUBLIC declaration of the pinned version", hiddenRow)
	}
	if downRow.VisibilityOfUsage != string(assets.VisibilityPrivate) {
		t.Fatalf("the downstream row is %+v, want a private declaration", downRow)
	}

	// The public page: no usage, and no trace of either case.
	raw, page := fetchAssetPage(t, fx.anon, fx.xPID, "")
	if len(page.UsedBy) != 0 {
		t.Errorf("the public page lists %+v; neither declaration qualifies", page.UsedBy)
	}
	if !strings.Contains(raw, `"used_by":[]`) {
		t.Errorf("used_by is not an answered empty list: %s", raw)
	}
	forbidInBody(t, "the public page", raw,
		hiddenName, hiddenSlug, fx.hidden.ID, downName, downSlug, fx.down.ID)
	assertNoCountKeys(t, "the public page", raw)

	// Control (a): the row's own declaration. Flip ONLY
	// visibility_of_usage on bob's row — the same row, the same project,
	// the same version — and the page now names his project. This is what
	// makes the absence above a statement about the rule.
	if _, err := fx.world.pool.Exec(ctx,
		`UPDATE asset_dependencies SET visibility_of_usage = 'public' WHERE project_id = $1`, fx.down.ID); err != nil {
		t.Fatalf("flip the declaration: %v", err)
	}
	controlRaw, control := fetchAssetPage(t, fx.anon, fx.xPID, "")
	if len(control.UsedBy) != 1 || control.UsedBy[0].ProjectID != fx.down.ID {
		t.Fatalf("after the declaration was made public the page lists %+v, want %q — the check cannot see a qualifying usage at all", control.UsedBy, downName)
	}
	if !strings.Contains(controlRaw, downName) {
		t.Errorf("the qualifying usage's project name is not on the page: %s", controlRaw)
	}

	// Control (b): the using project's visibility. Flip ONLY the hidden
	// project — carol's row is still the public declaration it was — and
	// her project appears, still with nothing else about her state.
	if _, err := fx.world.pool.Exec(ctx,
		`UPDATE projects SET visibility = 'public' WHERE id = $1`, fx.hidden.ID); err != nil {
		t.Fatalf("flip the project visibility: %v", err)
	}
	bothRaw, both := fetchAssetPage(t, fx.anon, fx.xPID, "")
	if len(both.UsedBy) != 2 {
		t.Fatalf("with both halves satisfied the page lists %+v, want the two public declarations", both.UsedBy)
	}
	names := map[string]bool{}
	for _, use := range both.UsedBy {
		names[use.ProjectName] = true
	}
	if !names[hiddenName] || !names[downName] {
		t.Errorf("the page lists %v, want both %q and %q", names, hiddenName, downName)
	}
	if !strings.Contains(bothRaw, hiddenName) {
		t.Errorf("the private project's name is missing once its usage qualifies: %s", bothRaw)
	}
}

// projectColumn reads one text column of one project row.
func projectColumn(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID, column string) string {
	t.Helper()
	var out string
	if err := pool.QueryRow(ctx, `SELECT `+column+` FROM projects WHERE id = $1`, projectID).Scan(&out); err != nil {
		t.Fatalf("read projects.%s of %s: %v", column, projectID, err)
	}
	return out
}

// rowFor reads one project's single dependency row, failing when the fixture
// does not hold exactly one.
func rowFor(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID string) dependencyRow {
	t.Helper()
	rows := dependencyRows(t, ctx, pool, projectID)
	if len(rows) != 1 {
		t.Fatalf("the project %s has %d dependency rows, want exactly 1: %+v", projectID, len(rows), rows)
	}
	return rows[0]
}

// assertNoCountKeys fails when any object in the JSON body carries a key
// named for a count (docs/23 §5: V1 renders no private count). It walks the
// decoded document rather than searching for a substring, so a field named
// "account" cannot satisfy it and a nested count cannot hide from it.
func assertNoCountKeys(t *testing.T, what, raw string) {
	t.Helper()
	var doc any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("%s: decode: %v", what, err)
	}
	var walk func(node any)
	walk = func(node any) {
		switch v := node.(type) {
		case map[string]any:
			for k, child := range v {
				if strings.Contains(strings.ToLower(k), "count") {
					t.Errorf("%s carries the key %q: %s", what, k, raw)
				}
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(doc)
}

// --------------------------------------------------------------------------
// 4. The project-side read

// TestAssetDependencyProjectRouteAnswersBothDirections: the same row read
// from the other end. The project's dependency list names the versions it
// declared it uses (with the recorded kind and its impact-analysis
// consequence), the asset's page names the projects that declared it — and
// the read is gated like every other project read, so a project this caller
// may not read answers the project surface's existence-hiding refusal
// without any row being read.
func TestAssetDependencyProjectRouteAnswersBothDirections(t *testing.T) {
	ctx := context.Background()
	fx := newDependencyFixture(t, ctx)
	x1 := mustPin(t, fx.xPID, "1.0")
	x2 := mustPin(t, fx.xPID, "2.0")

	fx.publishVersion(t, fx.alice, fx.up, fx.xPID, "1.0", "public", fx.aliceID)
	fx.publishVersion(t, fx.alice, fx.up, fx.xPID, "2.0", "public", fx.aliceID)
	// bob declares both versions publicly, then republishes one of them
	// privately: the private version's declaration is the project's own
	// business, and a non-member must not see it.
	fx.publishVersion(t, fx.bob, fx.down, fx.yPID, "1.0", "public", fx.bobID, x1)
	fx.publishVersion(t, fx.bob, fx.down, fx.yPID, "2.0", "private", fx.bobID, x2)

	// The member's view: both declarations, each with the kind it was
	// recorded as and the visibility it declares.
	rawMember, member := fetchProjectDependencies(t, fx.bob, fx.down.ID)
	if len(member.Dependencies) != 2 {
		t.Fatalf("the member's list = %+v, want both declarations: %s", member.Dependencies, rawMember)
	}
	byPin := map[string]dependencyEntryWire{}
	for _, e := range member.Dependencies {
		byPin[e.Pin] = e
	}
	for _, want := range []struct {
		pin        string
		version    string
		visibility string
	}{
		{string(x1), "1.0", string(assets.VisibilityPublic)},
		{string(x2), "2.0", string(assets.VisibilityPrivate)},
	} {
		entry, ok := byPin[want.pin]
		if !ok {
			t.Fatalf("the member's list has no entry for %s: %s", want.pin, rawMember)
		}
		if entry.DependencyType != string(assets.DependencyTypeDependsOn) {
			t.Errorf("%s is recorded as %q, want %q", want.pin, entry.DependencyType, assets.DependencyTypeDependsOn)
		}
		if !entry.ImpactAnalysis {
			t.Errorf("%s does not carry impact_analysis (docs/19 §3)", want.pin)
		}
		if entry.VisibilityOfUsage != want.visibility {
			t.Errorf("%s declares %q, want %q", want.pin, entry.VisibilityOfUsage, want.visibility)
		}
		if entry.Title != fx.xTitle || entry.Type != string(assets.TypeDataset) {
			t.Errorf("%s renders title/type %q/%q, want the stored asset's %q/%q", want.pin, entry.Title, entry.Type, fx.xTitle, assets.TypeDataset)
		}
		if wantURL := assets.AssetVersionURL(assets.PID(fx.xPID), want.version); entry.URL != wantURL {
			t.Errorf("%s renders url %q, want %q", want.pin, entry.URL, wantURL)
		}
		if entry.CreatedAt.IsZero() {
			t.Errorf("%s renders the zero instant", want.pin)
		}
	}

	// The non-member's and the anonymous caller's view: the public
	// declaration only — and the private one leaves no trace.
	for _, viewer := range []struct {
		name string
		uc   *testUserClient
	}{{"a non-member", fx.alice}, {"an anonymous caller", fx.anon}} {
		raw, got := fetchProjectDependencies(t, viewer.uc, fx.down.ID)
		if len(got.Dependencies) != 1 || got.Dependencies[0].Pin != string(x1) {
			t.Errorf("%s saw %+v, want exactly the public declaration %s: %s", viewer.name, got.Dependencies, x1, raw)
		}
		forbidInBody(t, viewer.name+"'s dependency list", raw, string(assets.VisibilityPrivate), string(x2))
		assertNoCountKeys(t, viewer.name+"'s dependency list", raw)
	}

	// The other direction, on the asset's page: the downstream project is
	// named against the version it declared, and the page's own
	// "dependencies" block (what THIS version pins) stays empty — the two
	// blocks are two directions of one table, not one list twice.
	_, page := fetchAssetPage(t, fx.anon, fx.xPID, "1.0")
	if len(page.UsedBy) != 1 || page.UsedBy[0].ProjectSlug != fx.down.Slug {
		t.Fatalf("the version's page lists used_by %+v, want the downstream project's declaration", page.UsedBy)
	}
	if page.UsedBy[0].DependencyType != string(assets.DependencyTypeDependsOn) {
		t.Errorf("the page renders the usage as %q, want the recorded %q", page.UsedBy[0].DependencyType, assets.DependencyTypeDependsOn)
	}
	if len(page.Dependencies) != 0 {
		t.Errorf("the upstream asset pins %+v; it declares no dependency of its own", page.Dependencies)
	}

	// The gate: a project this caller may not read is the project surface's
	// own refusal — the private project answers alice exactly what it
	// answers an anonymous caller, and no row is read either way.
	for _, viewer := range []struct {
		name string
		uc   *testUserClient
	}{{"a non-member", fx.alice}, {"an anonymous caller", fx.anon}} {
		resp := viewer.uc.do(t, http.MethodGet, projectDependenciesURL(fx.hidden.ID), "")
		mustStatus(t, resp, http.StatusNotFound)
		body := readAll(t, resp)
		if !strings.Contains(body, "PROJECT_NOT_FOUND") {
			t.Errorf("%s was answered %s, want the project surface's own not-found code", viewer.name, body)
		}
		forbidInBody(t, viewer.name+"'s refusal", body, fx.hidden.Name, fx.hidden.Slug, fx.hidden.ID)
	}
	// The member of that project still reads their own list: the gate is
	// about the caller, not about a project being private.
	resp := fx.carol.do(t, http.MethodGet, projectDependenciesURL(fx.hidden.ID), "")
	mustStatus(t, resp, http.StatusOK)
}

// --------------------------------------------------------------------------
// 5. The vocabulary is not in the database

// TestAssetDependencyTypeColumnHasNoCheckConstraint: dependency_type carries
// no CHECK, so the vocabulary lives in Go (internal/assets/dependency_type.go;
// the convention migrations 00064:41-45, 00066:21-30, 00067:22-34 and
// 00045:74-79 declare). The probe is the strongest form the claim has: a
// WRITE the database accepts — a value outside the vocabulary — followed by
// the readers rendering it verbatim, and then its removal.
//
// It carries its own control: the same query shape must find the CHECK that
// IS on this table (visibility_of_usage), so a query that found nothing
// because it was asking the wrong question cannot pass this test.
func TestAssetDependencyTypeColumnHasNoCheckConstraint(t *testing.T) {
	ctx := context.Background()
	fx := newDependencyFixture(t, ctx)
	x1 := mustPin(t, fx.xPID, "1.0")
	fx.publishVersion(t, fx.alice, fx.up, fx.xPID, "1.0", "public", fx.aliceID)
	fx.publishVersion(t, fx.bob, fx.down, fx.yPID, "1.0", "public", fx.bobID, x1)

	// The control first: the probe finds a constraint where one exists.
	if n := checksOn(t, ctx, fx.world.pool, "visibility_of_usage"); n == 0 {
		t.Fatal("the probe found no CHECK on visibility_of_usage, which migration 00010 declares; " +
			"it cannot be trusted to report the absence of one on dependency_type")
	}
	if n := checksOn(t, ctx, fx.world.pool, "dependency_type"); n != 0 {
		t.Errorf("dependency_type carries %d CHECK constraint(s); the vocabulary is supposed to live in Go", n)
	}
	// And it is a text column, not an enum: the value is a word the
	// application knows rather than a type the database enforces.
	var dataType, udtName string
	if err := fx.world.pool.QueryRow(ctx,
		`SELECT data_type, udt_name FROM information_schema.columns
		  WHERE table_name = 'asset_dependencies' AND column_name = 'dependency_type'`).Scan(&dataType, &udtName); err != nil {
		t.Fatalf("read the column's type: %v", err)
	}
	if dataType != "text" {
		t.Errorf("dependency_type is %s (%s), want text", dataType, udtName)
	}

	// The write the database accepts: a value no product path can produce.
	thirdParty := "reuses"
	if _, err := fx.world.pool.Exec(ctx,
		`INSERT INTO asset_dependencies (project_id, asset_version_id, dependency_type, visibility_of_usage)
		 VALUES ($1, $2, $3, 'public')`, fx.down.ID, mustVersionID(t, ctx, fx.world.pool, fx.xPID, "1.0"), thirdParty); err != nil {
		t.Fatalf("the database refused %q, so dependency_type does carry a vocabulary of its own: %v", thirdParty, err)
	}
	// The readers render what is stored, verbatim, with the fail-closed
	// false for "does a change here trigger re-analysis": the reader is not
	// the gate, and a stored value outside the vocabulary is not a reason
	// to hide a row (project_dependency.go).
	_, got := fetchProjectDependencies(t, fx.bob, fx.down.ID)
	found := false
	for _, e := range got.Dependencies {
		if e.DependencyType != thirdParty {
			continue
		}
		found = true
		if e.ImpactAnalysis {
			t.Errorf("a stored %q claims impact analysis; the catalog does not know it (dependency_type.go)", thirdParty)
		}
	}
	if !found {
		t.Errorf("the stored %q row was not rendered: %+v", thirdParty, got.Dependencies)
	}
	// Both rows are the member's: the depends_on the publish wrote, and the
	// value the platform cannot produce.
	if len(got.Dependencies) != 2 {
		t.Errorf("the member sees %d entries, want the publish's and the stored third value: %+v", len(got.Dependencies), got.Dependencies)
	}

	// Leave the table as the platform's own writers leave it. (The row is
	// the test's, not history: asset_dependencies is mutable by design, and
	// a database this file did not seed a value into is what the next
	// assertion of "no CHECK" is read against.)
	if _, err := fx.world.pool.Exec(ctx,
		`DELETE FROM asset_dependencies WHERE project_id = $1 AND dependency_type = $2`, fx.down.ID, thirdParty); err != nil {
		t.Fatalf("remove the probe row: %v", err)
	}
}

// checksOn counts the CHECK constraints on asset_dependencies whose
// definition mentions one column.
func checksOn(t *testing.T, ctx context.Context, pool *pgxpool.Pool, column string) int {
	t.Helper()
	return countRows(t, ctx, pool,
		`SELECT count(*) FROM pg_constraint c
		   JOIN pg_class r ON r.oid = c.conrelid
		  WHERE r.relname = 'asset_dependencies' AND c.contype = 'c'
		    AND pg_get_constraintdef(c.oid) LIKE '%' || $1 || '%'`, column)
}

// --------------------------------------------------------------------------
// 6. The row is current state, not history

// TestAssetDependencyRowIsTheProjectsCurrentDeclaration: asset_dependencies
// is exempt from the append-only triggers ("mutable by design", 00014), so
// one project re-declaring a version it already declared updates the row it
// has rather than accumulating rows — and the update takes the LATEST
// declaration. The fail-open direction is DO NOTHING (a usage declared
// private once would stay public forever after the project withdrew the
// declaration), so last-write-wins is what the writer does, and this test
// pins it in all three orders.
func TestAssetDependencyRowIsTheProjectsCurrentDeclaration(t *testing.T) {
	ctx := context.Background()
	fx := newDependencyFixture(t, ctx)
	x1 := mustPin(t, fx.xPID, "1.0")
	fx.publishVersion(t, fx.alice, fx.up, fx.xPID, "1.0", "public", fx.aliceID)

	fx.publishVersion(t, fx.bob, fx.down, fx.yPID, "1.0", "public", fx.bobID, x1)
	first := rowFor(t, ctx, fx.world.pool, fx.down.ID)
	if first.VisibilityOfUsage != string(assets.VisibilityPublic) {
		t.Fatalf("the first declaration recorded %q, want public", first.VisibilityOfUsage)
	}

	// The same project declares the same version again, privately.
	fx.publishVersion(t, fx.bob, fx.down, fx.yPID, "2.0", "private", fx.bobID, x1)
	rows := dependencyRows(t, ctx, fx.world.pool, fx.down.ID)
	if len(rows) != 1 {
		t.Fatalf("the second declaration left %d rows, want the one row the project has: %+v", len(rows), rows)
	}
	if rows[0].VisibilityOfUsage != string(assets.VisibilityPrivate) {
		t.Errorf("the row declares %q after a private declaration, want private — the current declaration is what is recorded", rows[0].VisibilityOfUsage)
	}
	if !rows[0].CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("the update moved created_at from %v to %v; the instant is when the project first declared the version", first.CreatedAt, rows[0].CreatedAt)
	}
	// And it is off the public page again, for the same reason.
	if _, page := fetchAssetPage(t, fx.anon, fx.xPID, ""); len(page.UsedBy) != 0 {
		t.Errorf("the page lists %+v after the declaration was withdrawn, want none", page.UsedBy)
	}

	// Declaring it publicly again puts it back: the row is current state.
	fx.publishVersion(t, fx.bob, fx.down, fx.yPID, "3.0", "public", fx.bobID, x1)
	rows = dependencyRows(t, ctx, fx.world.pool, fx.down.ID)
	if len(rows) != 1 || rows[0].VisibilityOfUsage != string(assets.VisibilityPublic) {
		t.Fatalf("after the public re-declaration the row is %+v, want one public row", rows)
	}
	if _, page := fetchAssetPage(t, fx.anon, fx.xPID, ""); len(page.UsedBy) != 1 {
		t.Errorf("the page lists %+v after the public re-declaration, want the project", page.UsedBy)
	}
}
