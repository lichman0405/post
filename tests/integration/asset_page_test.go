// Task T0709 — the asset hub's read surface over real PostgreSQL.
//
// The unit suites pin the asset page's RULES (internal/assets: BuildPage,
// BuildBrowse, the entry rule, the two visibility axes) and the
// transport's shape (cmd/api/assetshttp: the routes, the guard, the one
// 404). This file pins what only a real database can settle, over the
// whole composed route — the real guard, the real project read gate, the
// real publish command writing the rows, and the real reader over the
// canonical queries (internal/persistence/queries/asset_page.sql):
//
//  1. The eleven items of docs/42 §Asset Page are rendered from REAL rows.
//     The versions this suite reads are published through the real publish
//     command, so the manifest, the rights document, the integrity hash and
//     the research events are the ones production writes — not a fixture's
//     idea of them. The two kinds of row no product writer can produce are
//     seeded by SQL, and said so where they are seeded: asset_lineage and
//     asset_dependencies have no writer yet (T0709 ships a READER), and the
//     projects, releases and memberships a fixture needs are rows rather
//     than requests.
//
//  2. The disclosure rules hold over those rows. The fixture is RAW: the
//     database holds a private project's public usage of the rendered
//     version, a lineage edge into that project's asset, a pin to that
//     asset's public version, and a private version of the rendered asset
//     itself. The anonymous answer must name none of them, and the negative
//     assertions are made on the RAW RESPONSE BODY — so a field the
//     transport added on its own fails here, which is what makes this a
//     check on the whole surface rather than on one function.
//
//  3. The browse list is a function of real rows, and its type filter is
//     the closed four-type set the CHECK on research_assets.asset_type
//     enforces.
//
//  4. The reads write NOTHING. The page's own readers (the page store and
//     the membership read) sit on a pool whose connections carry
//     default_transaction_read_only=on, so the server refuses a write with
//     SQLSTATE 25006 — with a control proving the probe is a real write —
//     and the whole database is fingerprinted before and after every read
//     route.
//
//  5. There is no blob download route to find, over this composition as
//     over the e2e one: a fetch path answers the mux's own 404 rather than
//     an envelope.
//
// What is deliberately NOT here: /explore (ceded to T0802 — the browse list
// this file exercises is /assets), the browser rendering of the page
// (tests/e2e-assets, the required "asset ui e2e"), and any test of a blob
// route's own policy, because no such route exists.

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// assetPageTaskID namespaces this task's test databases
// (test_T0709_<run_id>).
const assetPageTaskID = "T0709"

// --------------------------------------------------------------------------
// The composed surface

// assetPageWorld is the asset hub's read surface as production wires it.
//
// Two pools over ONE database, and the split is the point: the writable one
// seeds the fixture and drives the publish command; the read-only one backs
// the two ports the READ ROUTES use (the page store and the project service
// the gate and the membership read go through), so "the page cannot write"
// is a property of the connections the reads run on rather than a promise
// about the code. The auth surface stays on the writable pool: signup is a
// write, and it is not part of the page route.
type assetPageWorld struct {
	ts       *httptest.Server
	pool     *pgxpool.Pool
	readOnly *pgxpool.Pool
}

// newAssetPageWorld composes the production tree over one test database,
// exactly as cmd/api/main.go does.
func newAssetPageWorld(t *testing.T, ctx context.Context) *assetPageWorld {
	t.Helper()
	pool, dbURL := testdb.Setup(t, ctx, adminURL(t), assetPageTaskID)
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

	// The read routes' own view of projects: a second project service over
	// READ-ONLY connections. The page route asks it two questions — the
	// project read gate and the membership bit — and both are reads, so
	// both can (and here must) run where the server refuses to write.
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
		State:    assetshttp.NewPostgresStateStore(pool),
		Projects: readProjects.Service(),
		Publish:  publishCommand,
		Pages:    persistence.NewAssetPageStore(readOnly),
		Members:  readProjects.Service(),
	}).Register(mux)

	ts := httptest.NewServer(authAPI.Guard(mux))
	t.Cleanup(ts.Close)
	return &assetPageWorld{ts: ts, pool: pool, readOnly: readOnly}
}

// --------------------------------------------------------------------------
// The wire shape this suite reads (declared here rather than reused from
// internal/assets, so a silent JSON tag change in the model fails here)

type assetPagePayload struct {
	Asset        assetPageAssetWire     `json:"asset"`
	Version      assetPageVersionWire   `json:"version"`
	Origin       []assetPageOriginWire  `json:"origin"`
	Rights       json.RawMessage        `json:"rights"`
	Creators     []assetPageCreatorWire `json:"creators"`
	Metadata     []assetPageMetaWire    `json:"metadata"`
	Dependencies []assetPageDepWire     `json:"dependencies"`
	Lineage      []assetPageLineWire    `json:"lineage"`
	UsedBy       []assetPageUseWire     `json:"used_by"`
	Versions     []assetPageSummaryWire `json:"versions"`
	Events       []assetPageEventWire   `json:"events"`
}

type assetPageAssetWire struct {
	PID           string                `json:"pid"`
	Type          string                `json:"type"`
	Title         string                `json:"title"`
	Slug          string                `json:"slug"`
	OriginProject *assetPageProjectWire `json:"origin_project"`
	CreatedAt     time.Time             `json:"created_at"`
}

type assetPageProjectWire struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Slug       string `json:"slug"`
	Visibility string `json:"visibility"`
}

type assetPageVersionWire struct {
	Version       string             `json:"version"`
	URL           string             `json:"url"`
	Visibility    string             `json:"visibility"`
	IntegrityHash string             `json:"integrity_hash"`
	PublishedAt   time.Time          `json:"published_at"`
	PublishedBy   *assetPageUserWire `json:"published_by"`
}

type assetPageUserWire struct {
	UserID      string `json:"user_id"`
	Handle      string `json:"handle"`
	DisplayName string `json:"display_name"`
}

type assetPageCreatorWire struct {
	UserID      string `json:"user_id"`
	Handle      string `json:"handle"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

type assetPageOriginWire struct {
	Ref       string `json:"ref"`
	Kind      string `json:"kind"`
	Resolved  bool   `json:"resolved"`
	Title     string `json:"title"`
	ProjectID string `json:"project_id"`
	Link      string `json:"link"`
}

type assetPageMetaWire struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

type assetPageDepWire struct {
	Pin      string `json:"pin"`
	Resolved bool   `json:"resolved"`
	Public   bool   `json:"public"`
	Title    string `json:"title"`
	Type     string `json:"type"`
	URL      string `json:"url"`
}

type assetPageLineWire struct {
	Relation  string `json:"relation"`
	Direction string `json:"direction"`
	PID       string `json:"pid"`
	Version   string `json:"version"`
	URL       string `json:"url"`
	Title     string `json:"title"`
}

type assetPageUseWire struct {
	ProjectID      string    `json:"project_id"`
	ProjectName    string    `json:"project_name"`
	ProjectSlug    string    `json:"project_slug"`
	DependencyType string    `json:"dependency_type"`
	CreatedAt      time.Time `json:"created_at"`
}

type assetPageSummaryWire struct {
	Version       string    `json:"version"`
	URL           string    `json:"url"`
	Visibility    string    `json:"visibility"`
	IntegrityHash string    `json:"integrity_hash"`
	PublishedAt   time.Time `json:"published_at"`
	Current       bool      `json:"current"`
}

type assetPageEventWire struct {
	Type       string             `json:"type"`
	OccurredAt time.Time          `json:"occurred_at"`
	Version    string             `json:"version"`
	Actor      *assetPageUserWire `json:"actor"`
}

// assetPageRightsDoc is the stored rights document as this suite reads it:
// the two axes the page renders through the shared panel (T0703).
type assetPageRightsDoc struct {
	Version    int `json:"version"`
	Visibility struct {
		Metadata   string `json:"metadata"`
		DataAccess string `json:"data_access"`
	} `json:"visibility"`
}

type assetBrowsePayload struct {
	Type   *string          `json:"type"`
	Types  []string         `json:"types"`
	Assets []assetBrowseRow `json:"assets"`
}

type assetBrowseRow struct {
	PID               string                `json:"pid"`
	Type              string                `json:"type"`
	Title             string                `json:"title"`
	Slug              string                `json:"slug"`
	URL               string                `json:"url"`
	OriginProject     *assetPageProjectWire `json:"origin_project"`
	PublicVersions    int                   `json:"public_versions"`
	LatestVersion     string                `json:"latest_version"`
	LatestURL         string                `json:"latest_url"`
	LatestPublishedAt time.Time             `json:"latest_published_at"`
}

// --------------------------------------------------------------------------
// Requests

func assetPageURL(pid, version string) string {
	path := "/api/v1/assets/" + pid
	if version != "" {
		path += "?version=" + url.QueryEscape(version)
	}
	return path
}

// fetchAssetPage requires the 200 and returns the raw body with the decoded
// payload. The raw body is what the leak assertions read: a field the model
// withheld but the transport rendered would be invisible to a check on the
// decoded struct alone.
func fetchAssetPage(t *testing.T, uc *testUserClient, pid, version string) (string, assetPagePayload) {
	t.Helper()
	resp := uc.do(t, http.MethodGet, assetPageURL(pid, version), "")
	mustStatus(t, resp, http.StatusOK)
	raw := readAll(t, resp)
	var got assetPagePayload
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("asset page payload: %v: %s", err, raw)
	}
	return raw, got
}

// fetchAssetRefusal requires one status and returns the raw body with the
// decoded error envelope.
func fetchAssetRefusal(t *testing.T, uc *testUserClient, method, path string, want int) (string, errorEnvelope) {
	t.Helper()
	resp := uc.do(t, method, path, "")
	if resp.StatusCode != want {
		t.Fatalf("%s %s = %d, want %d: %s", method, path, resp.StatusCode, want, readAll(t, resp))
	}
	raw := readAll(t, resp)
	var env errorEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("%s %s: error envelope: %v: %s", method, path, err, raw)
	}
	return raw, env
}

// fetchAssetBrowse requires the 200 and returns the raw body with the
// decoded list.
func fetchAssetBrowse(t *testing.T, uc *testUserClient, typeFilter string) (string, assetBrowsePayload) {
	t.Helper()
	path := "/api/v1/assets"
	if typeFilter != "" {
		path += "?type=" + url.QueryEscape(typeFilter)
	}
	resp := uc.do(t, http.MethodGet, path, "")
	mustStatus(t, resp, http.StatusOK)
	raw := readAll(t, resp)
	var got assetBrowsePayload
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("asset browse payload: %v: %s", err, raw)
	}
	return raw, got
}

// --------------------------------------------------------------------------
// The fixture

// assetPageFixture is the state the page is read against. Every field a
// negative assertion names is a REAL row: the private project exists, its
// usage of the rendered version exists, the pin to its public version
// exists, the lineage edge into its asset exists, and the rendered asset
// really has a third, private version.
type assetPageFixture struct {
	aliceID string
	bobID   string
	// openProject is the rendered asset's own project: PUBLIC at read time,
	// which is what lets an anonymous caller read the page at all (the
	// route puts the asset's project through the project read gate).
	openProject string
	// otherPublic is a second PUBLIC project, so the origin block has a
	// foreign project whose identity may be rendered — the direction that
	// must not be swallowed by the withholding rules.
	otherPublic string
	// privateProject is another project's PRIVATE state: the leak side.
	privateProject string

	// releaseOpen and releaseOther are releases of the two public projects
	// (origin refs that resolve), releasePrivate a release of the private
	// project (an origin ref that must not be rendered for a non-member).
	releaseOpen    string
	releaseOther   string
	releasePrivate string
	// objectVersionOpen is an object version in the open project: the ref
	// kind whose page entry carries a title.
	objectVersionOpen string

	// subject is the asset the page is about, in the open project; its pid
	// is minted by the publish command, so it is only known after the
	// fixture has published.
	subject string
	// subjectVersions maps a version label to its research_asset_versions
	// row id (the identity lineage and usages are keyed by).
	subjectVersions map[string]string
	// subjectHashes maps a version label to the integrity hash the PUBLISH
	// returned for it — the content hash the page renders must be that one,
	// and two versions' hashes must differ.
	subjectHashes map[string]string
	// depPublicPID and depPrivatePID are the two pinned assets: one in the
	// open project (public project, public version — rendered), one in the
	// private project (public version, private project — dropped).
	depPublicPID  string
	depPrivatePID string
	// depPrivateVersionID is depPrivate's public version row.
	depPrivateVersionID string

	// privatePublisherHandle is the handle of the user who published the
	// rendered asset's private 2.1 — and nothing else in this fixture. The
	// handle is a disclosure of its own: it is reachable through the events
	// block alone (creator lists and version facts name the other versions'
	// publishers), so a page that renders the 2.1 event renders this
	// identity with it.
	privatePublisherHandle string

	// openBlob is the blob the rendered version's manifest names, openly
	// attached (the manifest's blob_ids render inside metadata).
	openBlob string

	// Type coverage for the browse list: one asset per remaining V1 type in
	// the open project, and one asset whose only version is private.
	protocolPID    string
	materialPID    string
	benchmarkPID   string
	privateOnlyPID string
}

// seedAssetPageFixture writes the fixture. Projects, releases, memberships,
// lineage and usages are SQL (rows, not requests — no product writer exists
// for the last two); the versions a page reads are PUBLISHED through the
// real command, so every fact the page renders about them is a fact
// production recorded.
func seedAssetPageFixture(t *testing.T, ctx context.Context, w *assetPageWorld, alice *testUserClient, aliceID string, bob *testUserClient, bobID string) *assetPageFixture {
	t.Helper()
	f := &assetPageFixture{
		aliceID: aliceID, bobID: bobID,
		subjectVersions: map[string]string{},
		subjectHashes:   map[string]string{},
	}
	pool := w.pool

	f.openProject = mustQueryUUID(t, ctx, pool,
		`INSERT INTO projects (slug, name, purpose, visibility, created_by)
		 VALUES ('open-materials-lab', 'Open Materials Lab', 'T0709 fixture', 'public', $1) RETURNING id`, aliceID)
	f.otherPublic = mustQueryUUID(t, ctx, pool,
		`INSERT INTO projects (slug, name, purpose, visibility, created_by)
		 VALUES ('shared-instruments', 'Shared Instruments', 'T0709 fixture', 'public', $1) RETURNING id`, aliceID)
	f.privateProject = mustQueryUUID(t, ctx, pool,
		`INSERT INTO projects (slug, name, purpose, visibility, created_by)
		 VALUES ('hidden-usage-lab', 'Hidden Usage Lab', 'T0709 fixture', 'private', $1) RETURNING id`, bobID)
	for _, m := range []struct {
		project, user string
	}{
		{f.openProject, aliceID}, {f.otherPublic, aliceID}, {f.privateProject, bobID},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, 'owner')`,
			m.project, m.user); err != nil {
			t.Fatalf("seed membership: %v", err)
		}
	}

	stateOpen := mustQueryUUID(t, ctx, pool,
		`INSERT INTO project_states (project_id, state_hash, manifest_version)
		 VALUES ($1, 'genesis-open', '1') RETURNING id`, f.openProject)
	stateOther := mustQueryUUID(t, ctx, pool,
		`INSERT INTO project_states (project_id, state_hash, manifest_version)
		 VALUES ($1, 'genesis-other', '1') RETURNING id`, f.otherPublic)
	statePrivate := mustQueryUUID(t, ctx, pool,
		`INSERT INTO project_states (project_id, state_hash, manifest_version)
		 VALUES ($1, 'genesis-private', '1') RETURNING id`, f.privateProject)

	f.releaseOpen = mustQueryUUID(t, ctx, pool,
		`INSERT INTO releases (project_id, version, title, state_id, manifest, manifest_hash, created_by)
		 VALUES ($1, 'v1.0.0', 'Materials Baseline', $2, '{}'::jsonb, 'x', $3) RETURNING id`,
		f.openProject, stateOpen, aliceID)
	f.releaseOther = mustQueryUUID(t, ctx, pool,
		`INSERT INTO releases (project_id, version, title, state_id, manifest, manifest_hash, created_by)
		 VALUES ($1, 'v1.0.0', 'Instrument Calibration', $2, '{}'::jsonb, 'x', $3) RETURNING id`,
		f.otherPublic, stateOther, aliceID)
	f.releasePrivate = mustQueryUUID(t, ctx, pool,
		`INSERT INTO releases (project_id, version, title, state_id, manifest, manifest_hash, created_by)
		 VALUES ($1, 'v1.0.0', 'Private Reference Set', $2, '{}'::jsonb, 'x', $3) RETURNING id`,
		f.privateProject, statePrivate, bobID)

	// The object version the matched-fabrication version was published from,
	// and the blob its manifest names. Both are the open project's own, and
	// the blob is attached OPEN: the publish gate refuses a manifest that
	// promises open data access over a restricted attachment, so an openly
	// published version carries an openly attached blob.
	objectOpen := mustQueryUUID(t, ctx, pool,
		`INSERT INTO scientific_objects (project_id, object_type, created_by)
		 VALUES ($1, 'dataset_record', $2) RETURNING id`, f.openProject, aliceID)
	f.objectVersionOpen = mustQueryUUID(t, ctx, pool,
		`INSERT INTO scientific_object_versions
			(object_id, version_no, state_id, schema_id, schema_version, title, lifecycle_state, payload, integrity_hash, created_by)
		 VALUES ($1, 1, $2, 'https://open-rd.example/schemas/dataset.schema.json', '1',
		         'Matched Alloy Screen', 'active', '{}'::jsonb, 'x', $3) RETURNING id`,
		objectOpen, stateOpen, aliceID)
	f.openBlob = mustQueryUUID(t, ctx, pool,
		`INSERT INTO blobs (content_hash, size_bytes, storage_key, created_by)
		 VALUES ('sha256:page-open', 4096, 'blobs/page-open', $1) RETURNING id`, aliceID)
	if _, err := pool.Exec(ctx,
		`INSERT INTO blob_attachments (blob_id, scientific_object_version_id, attachment_role, access_level, state_id)
		 VALUES ($1, $2, 'data', 'open', $3)`, f.openBlob, f.objectVersionOpen, stateOpen); err != nil {
		t.Fatalf("attach the open blob: %v", err)
	}

	// The two pinned assets are published first: a pin names a published
	// version, and the publish gate resolves every pin against current
	// state.
	published := mustPublish(t, alice, f.openProject, buildCandidate(t, candidateOptions{
		version: "1.0", visibility: "public",
		refs: []string{"release:" + f.releaseOpen}, creators: []string{aliceID},
		blobIDs: []string{f.openBlob}, accessLevel: "open", dataAccess: rights.DataAccessOpen,
		title: "Baseline Alloy Set", slug: "baseline-alloy-set",
	}).body(t), "page-dep-public-1")
	f.depPublicPID = published.AssetPID

	published = mustPublish(t, bob, f.privateProject, buildCandidate(t, candidateOptions{
		version: "1.0", visibility: "public",
		refs: []string{"release:" + f.releasePrivate}, creators: []string{bobID},
		blobIDs: []string{f.openBlob}, accessLevel: "open", dataAccess: rights.DataAccessOpen,
		title: "Private Reference Screen", slug: "private-reference-screen",
	}).body(t), "page-dep-private-1")
	f.depPrivatePID = published.AssetPID
	f.depPrivateVersionID = versionRowID(t, ctx, pool, assetRowID(t, ctx, pool, f.depPrivatePID), "1.0")

	// The asset the page is about. Its first version creates it (display
	// fields ride along only on a create); its second carries the pins and
	// the refs the page's dependency and origin blocks are read from.
	subject := buildCandidate(t, candidateOptions{
		version: "1.0", visibility: "public",
		refs: []string{"release:" + f.releaseOpen}, creators: []string{aliceID},
		blobIDs: []string{f.openBlob}, accessLevel: "open", dataAccess: rights.DataAccessOpen,
		title: "Open Alloy Screen", slug: "open-alloy-screen",
	})
	published = mustPublish(t, alice, f.openProject, subject.body(t), "page-subject-1")
	f.subject = published.AssetPID
	subjectID := assetRowID(t, ctx, pool, f.subject)
	f.subjectVersions["1.0"] = versionRowID(t, ctx, pool, subjectID, "1.0")
	f.subjectHashes["1.0"] = published.IntegrityHash

	published = mustPublish(t, alice, f.openProject, buildCandidate(t, candidateOptions{
		pid: f.subject, version: "2.0", visibility: "public",
		refs: []string{
			"release:" + f.releaseOpen,
			"release:" + f.releaseOther,
			"object_version:" + f.objectVersionOpen,
		},
		creators:    []string{aliceID},
		pins:        []assets.DependencyPin{mustPin(t, f.depPublicPID, "1.0"), mustPin(t, f.depPrivatePID, "1.0")},
		blobIDs:     []string{f.openBlob},
		accessLevel: "open", dataAccess: rights.DataAccessOpen,
	}).body(t), "page-subject-2")
	f.subjectVersions["2.0"] = versionRowID(t, ctx, pool, subjectID, "2.0")
	f.subjectHashes["2.0"] = published.IntegrityHash

	// The arrangement the events block's SECOND guard exists for, which no
	// flip can produce: a PUBLIC project publishing a PRIVATE version. The
	// publish command permits it — a private publication widens nothing, so
	// no policy is read (internal/application/assetpublish) — and the event it
	// records is PUBLIC, because the recording project is public. The event's
	// own visibility therefore passes on its own, and what the event would
	// render is a private version's label, publish instant and publisher:
	// exactly what the versions block withholds from the same reader, and what
	// the page's one 404 for that version's address is meant to hide. The
	// publisher is a THIRD user who publishes nothing else, so the actor is a
	// disclosure reachable through this block alone.
	carol, carolID := signup(t, w.ts.URL, "page-carol@example.com", "page-carol")
	if _, err := pool.Exec(ctx,
		`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, 'owner')`,
		f.openProject, carolID); err != nil {
		t.Fatalf("seed the private version's publisher's membership: %v", err)
	}
	f.privatePublisherHandle = "page-carol"
	published = mustPublish(t, carol, f.openProject, buildCandidate(t, candidateOptions{
		pid: f.subject, version: "2.1", visibility: "private",
		refs: []string{"release:" + f.releaseOpen}, creators: []string{carolID},
		blobIDs: []string{f.openBlob}, accessLevel: "open", dataAccess: rights.DataAccessOpen,
	}).body(t), "page-subject-2-1")
	f.subjectVersions["2.1"] = versionRowID(t, ctx, pool, subjectID, "2.1")
	f.subjectHashes["2.1"] = published.IntegrityHash

	// The private version. It is published while the project is PRIVATE,
	// and then the project is public again — the only way a public project
	// can hold a version whose publish event is private (the event's
	// visibility is the project's AT PUBLISH TIME). That covers the events
	// block's FIRST guard: an event whose own stored visibility is not
	// public is not rendered to a non-member.
	if _, err := pool.Exec(ctx, `UPDATE projects SET visibility = 'private' WHERE id = $1`, f.openProject); err != nil {
		t.Fatalf("make the rendered asset's project private: %v", err)
	}
	published = mustPublish(t, alice, f.openProject, buildCandidate(t, candidateOptions{
		pid: f.subject, version: "3.0", visibility: "private",
		refs: []string{"release:" + f.releaseOpen}, creators: []string{aliceID},
		blobIDs: []string{f.openBlob}, accessLevel: "open", dataAccess: rights.DataAccessOpen,
	}).body(t), "page-subject-3")
	f.subjectVersions["3.0"] = versionRowID(t, ctx, pool, subjectID, "3.0")
	f.subjectHashes["3.0"] = published.IntegrityHash
	if _, err := pool.Exec(ctx, `UPDATE projects SET visibility = 'public' WHERE id = $1`, f.openProject); err != nil {
		t.Fatalf("make the rendered asset's project public again: %v", err)
	}

	// The eleven items are read off the version the page renders, so the
	// newness the assertions below rely on is measured rather than assumed
	// (two versions written in one microsecond would order by label).
	assertVersionOrder(t, ctx, pool, subjectID, "1.0", "2.0")
	assertVersionOrder(t, ctx, pool, subjectID, "2.0", "2.1")
	assertVersionOrder(t, ctx, pool, subjectID, "2.1", "3.0")

	// asset_lineage and asset_dependencies have no product writer in V1
	// (T0709 ships the reader), so the rows the page's lineage and used_by
	// blocks are read from are inserted here, as rows:
	//
	//   - 1.0 superseded by 2.0: both ends public, in a public project — the
	//     edge a reader may follow.
	//   - 2.0 derived from the private project's asset: a public VERSION
	//     whose project is private — the edge that must be dropped whole,
	//     because the page it would link to answers 404 to this caller.
	//   - a public usage by the open project (rendered), a public usage by
	//     the private project (dropped), and a private usage by the open
	//     project (dropped): the three rows issue #238's rule is about.
	if _, err := pool.Exec(ctx,
		`INSERT INTO asset_lineage (parent_asset_version_id, child_asset_version_id, relation_type)
		 VALUES ($1, $2, 'supersedes'), ($3, $2, 'derived_from')`,
		f.subjectVersions["1.0"], f.subjectVersions["2.0"], f.depPrivateVersionID); err != nil {
		t.Fatalf("seed lineage: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO asset_dependencies (project_id, asset_version_id, dependency_type, visibility_of_usage)
		 VALUES ($1, $2, 'reuses', 'public'),
		        ($3, $2, 'cites', 'public'),
		        ($1, $2, 'documents', 'private')`,
		f.openProject, f.subjectVersions["2.0"], f.privateProject); err != nil {
		t.Fatalf("seed usages: %v", err)
	}

	// The browse list's type coverage: one asset of each remaining V1 type
	// in the open project, and one whose only version is private.
	f.protocolPID = seedBrowseAsset(t, ctx, pool, f.openProject, aliceID,
		"wash-protocol", "Wash Protocol", "01j9z6k3m4n5p6q7r8s9t0v1w7", string(assets.TypeProtocol), "1.0", "public")
	f.materialPID = seedBrowseAsset(t, ctx, pool, f.openProject, aliceID,
		"alloy-selection", "Alloy Selection", "01j9z6k3m4n5p6q7r8s9t0v1w8", string(assets.TypeMaterialCollection), "1.0", "public")
	f.benchmarkPID = seedBrowseAsset(t, ctx, pool, f.openProject, aliceID,
		"screen-benchmark", "Screen Benchmark", "01j9z6k3m4n5p6q7r8s9t0v1w9", string(assets.TypeBenchmark), "1.0", "public")
	f.privateOnlyPID = seedBrowseAsset(t, ctx, pool, f.openProject, aliceID,
		"unpublished-draft", "Unpublished Draft", "01j9z6k3m4n5p6q7r8s9t0v1x1", string(assets.TypeDataset), "0.1", "private")
	return f
}

// assertVersionOrder fails when one version row is not strictly newer than
// another: the page renders the newest version a caller may see, and a
// fixture that wrote two of them in the same microsecond would be measuring
// the label tie-break instead of the recency the test names.
func assertVersionOrder(t *testing.T, ctx context.Context, pool *pgxpool.Pool, assetID, older, newer string) {
	t.Helper()
	var olderAt, newerAt time.Time
	if err := pool.QueryRow(ctx,
		`SELECT (SELECT published_at FROM research_asset_versions WHERE asset_id = $1 AND version = $2),
		        (SELECT published_at FROM research_asset_versions WHERE asset_id = $1 AND version = $3)`,
		assetID, older, newer).Scan(&olderAt, &newerAt); err != nil {
		t.Fatalf("read version timestamps: %v", err)
	}
	if !newerAt.After(olderAt) {
		t.Fatalf("the fixture published %s at %s and %s at %s: the recency this test asserts is not "+
			"measurable from these rows", older, olderAt, newer, newerAt)
	}
}

// seedBrowseAsset inserts one asset with one version at the given
// visibility and returns its pid. It is the browse list's type coverage:
// those rows are not published through the command (a protocol manifest has
// its own required metadata, and the command's create path is T0705's
// subject), so they are rows — which is what the browse query reads.
func seedBrowseAsset(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID, userID, slug, title, pid, assetType, version, visibility string) string {
	t.Helper()
	assetID := mustQueryUUID(t, ctx, pool,
		`INSERT INTO research_assets (asset_type, slug, title, origin_project_id, pid)
		 VALUES ($1, $2, $3, $4, $5) RETURNING id`, assetType, slug, title, projectID, pid)
	if _, err := pool.Exec(ctx,
		`INSERT INTO research_asset_versions
			(asset_id, version, manifest, rights_json, visibility, integrity_hash, published_by, origin_refs)
		 VALUES ($1, $2, '{}'::jsonb, '{}'::jsonb, $3, 'x', $4, ARRAY['project:' || $5::text])`,
		assetID, version, visibility, userID, projectID); err != nil {
		t.Fatalf("seed browse asset %s: %v", slug, err)
	}
	return pid
}

// --------------------------------------------------------------------------
// Raw-body assertions

// forbidInBody fails when any of the strings appears in the raw response.
// An empty string is a failure rather than a pass: a check that forbade
// nothing is the shape that reads green while measuring nothing.
func forbidInBody(t *testing.T, what, raw string, forbidden ...string) {
	t.Helper()
	for _, s := range forbidden {
		if s == "" {
			t.Fatalf("%s: a forbidden string is empty, so this check would pass on any answer", what)
		}
		if i := indexToken(raw, s); i >= 0 {
			t.Errorf("%s leaks %q: ...%s...", what, s, leakWindow(raw, i))
		}
	}
}

// indexToken finds s in raw, except that a needle made only of digits and
// dots must not match INSIDE a longer numeric run.
//
// A version label is a token, and several needles here are labels like "2.1".
// Searched as a bare substring, "2.1" also matches the microseconds of an ISO
// timestamp — "2026-09-16T04:03:42.127948Z" contains "42.127", and "2.1"
// starts at its second character — so on roughly one run in ten this check
// fired on a body that had leaked nothing at all. That is the shape the
// doctrine above rejects from the other direction: a check whose failure does
// not mean the thing it names. A page that really names version 2.1 writes it
// after a quote, a slash or a space, never in the middle of a longer number,
// so requiring a non-numeric neighbour keeps every real leak and drops the
// timestamp.
func indexToken(raw, s string) int {
	if !numericLabel(s) {
		return strings.Index(raw, s)
	}
	for from := 0; from < len(raw); {
		i := strings.Index(raw[from:], s)
		if i < 0 {
			return -1
		}
		i += from
		if !insideNumber(raw, i, len(s)) {
			return i
		}
		from = i + 1
	}
	return -1
}

// numericLabel reports whether s is made only of digits and dots, i.e. the
// shape of a version label rather than of an identifier or a phrase.
func numericLabel(s string) bool {
	for i := 0; i < len(s); i++ {
		if (s[i] < '0' || s[i] > '9') && s[i] != '.' {
			return false
		}
	}
	return true
}

// insideNumber reports whether raw[i:i+n] has a digit or a dot on either side,
// which makes it part of a longer number rather than a token of its own.
func insideNumber(raw string, i, n int) bool {
	digitOrDot := func(b byte) bool { return (b >= '0' && b <= '9') || b == '.' }
	if i > 0 && digitOrDot(raw[i-1]) {
		return true
	}
	if j := i + n; j < len(raw) && digitOrDot(raw[j]) {
		return true
	}
	return false
}

// The check above is only worth having if it still says no to a real leak AND
// stops saying yes to a clock. Both directions are asserted here: the failure
// it guards against was silent one way and wrong the other, and a fix that
// traded one for the other would be worse than the bug.
func TestForbidInBodyMatchesLabelsNotTimestamps(t *testing.T) {
	// The body that produced the false positive: it names version 2.0 and a
	// timestamp whose microseconds happen to contain "2.1".
	const clean = `{"version":{"version":"2.0","url":"/assets/v3dczzg54m6qxzzscd2gmm0xm0",` +
		`"created_at":"2026-09-16T04:03:42.127948Z"}}`
	if i := indexToken(clean, "2.1"); i >= 0 {
		t.Errorf("indexToken matched %q inside a timestamp at %d: %q", "2.1", i, leakWindow(clean, i))
	}
	// Every position a real leak can take: a JSON value, a path segment, and
	// running prose on a server-rendered page.
	for _, leaked := range []string{
		`{"version":{"version":"2.1","url":"/assets/x"}}`,
		`{"url":"/assets/x/2.1"}`,
		`<span>Version 2.1</span>`,
		`{"label":"2.1"}`,
	} {
		if indexToken(leaked, "2.1") < 0 {
			t.Errorf("indexToken missed a real leak of %q in %q", "2.1", leaked)
		}
	}
	// A needle that is not a version label keeps the plain substring rule.
	if indexToken(`{"handle":"bob"}`, "bob") < 0 {
		t.Error("indexToken missed a non-numeric needle")
	}
}

// leakWindow renders the neighbourhood of the leak, so a failure shows what
// the answer actually said instead of a whole 4KB body.
func leakWindow(raw string, at int) string {
	const half = 80
	start := at - half
	if start < 0 {
		start = 0
	}
	end := at + half
	if end > len(raw) {
		end = len(raw)
	}
	return strings.ReplaceAll(raw[start:end], "\n", " ")
}

// assertEventsNameRenderedVersions states the events block's rule as the
// invariant it is, rather than as the labels one fixture happens to hold:
// every version an event names is a version the versions block of THE SAME
// ANSWER renders.
//
// The two blocks are two answers to one question — may this caller see this
// version — and a page that gave them differently would be a page that
// answers "no such version" for /assets/{pid}/2.1 in one block while printing
// that label, its publish instant and its publisher in another. The version
// axis is the one the event's own stored visibility cannot stand in for: a
// public project may publish a private version, and the event it records is
// public (internal/application/assetpublish), so the label rides on an event
// whose own axis passes.
//
// It returns false when the invariant is broken so a caller that wants to
// keep checking can do so; here every caller fails on it.
func assertEventsNameRenderedVersions(t *testing.T, what string, page assetPagePayload) bool {
	t.Helper()
	rendered := make(map[string]bool, len(page.Versions))
	for _, v := range page.Versions {
		rendered[v.Version] = true
	}
	ok := true
	for _, e := range page.Events {
		if e.Version == "" || rendered[e.Version] {
			continue
		}
		ok = false
		actor := "no actor"
		if e.Actor != nil {
			actor = "actor " + e.Actor.Handle
		}
		t.Errorf("%s: the events block names version %q (%s, %s, %s), which its versions block withholds",
			what, e.Version, e.Type, e.OccurredAt.Format(time.RFC3339), actor)
	}
	return ok
}

// --------------------------------------------------------------------------
// 1. The eleven items, over real rows, for a caller who is nobody

func TestAssetPageRendersTheElevenItemsOverRealRows(t *testing.T) {
	ctx := testCtx(t)
	w := newAssetPageWorld(t, ctx)
	alice, aliceID := signup(t, w.ts.URL, "page-alice@example.com", "page-alice")
	bob, bobID := signup(t, w.ts.URL, "page-bob@example.com", "page-bob")
	f := seedAssetPageFixture(t, ctx, w, alice, aliceID, bob, bobID)

	// The same URL a signed-out visitor reaches: a fresh client with no
	// cookie jar entries and no CSRF token.
	visitor := newTestUserClient(w.ts.URL)
	raw, page := fetchAssetPage(t, visitor, f.subject, "")

	// --- 1. PID / version ---
	if page.Asset.PID != f.subject {
		t.Errorf("asset.pid = %q, want the published pid %q", page.Asset.PID, f.subject)
	}
	if page.Version.Version != "2.0" {
		t.Errorf("version = %q, want 2.0 — the newest version an anonymous caller may see", page.Version.Version)
	}
	if page.Version.URL != "/assets/"+f.subject+"/2.0" {
		t.Errorf("version.url = %q, want the persistent version address", page.Version.URL)
	}
	if page.Version.Visibility != "public" {
		t.Errorf("version.visibility = %q, want public", page.Version.Visibility)
	}
	// The hash is the one the publish returned for its own manifest bytes:
	// the page does not compute a hash, it renders the stored one.
	if page.Version.IntegrityHash != f.subjectHashes["2.0"] {
		t.Errorf("version.integrity_hash = %q, want the published %q",
			page.Version.IntegrityHash, f.subjectHashes["2.0"])
	}

	// --- 2. type ---
	if page.Asset.Type != string(assets.TypeDataset) {
		t.Errorf("asset.type = %q, want the closed set's dataset (the CHECK the row carries)", page.Asset.Type)
	}
	if page.Asset.Title != "Open Alloy Screen" || page.Asset.Slug != "open-alloy-screen" {
		t.Errorf("asset = %q / %q, want the title and slug the creating publish declared",
			page.Asset.Title, page.Asset.Slug)
	}

	// --- 3. origin ---
	// The version's refs are the three the publish declared, each resolved
	// against real rows: the project's own release, another PUBLIC project's
	// release, and an object version (the kind whose entry carries a title).
	if len(page.Origin) != 3 {
		t.Fatalf("origin has %d entries, want the version's three resolved refs: %+v", len(page.Origin), page.Origin)
	}
	seenKinds := map[string]bool{}
	for _, o := range page.Origin {
		seenKinds[o.Kind] = true
		if !o.Resolved {
			t.Errorf("origin entry = %+v, want every rendered ref resolved (an unresolved one is dropped)", o)
		}
		if !strings.HasPrefix(o.Ref, o.Kind+":") {
			t.Errorf("origin entry %+v: the ref does not spell its own kind", o)
		}
	}
	if !seenKinds["release"] || !seenKinds["object_version"] {
		t.Errorf("origin kinds = %v, want a release ref and an object_version ref", seenKinds)
	}
	var foreign bool
	for _, o := range page.Origin {
		switch o.Ref {
		case "release:" + f.releaseOpen:
			if o.ProjectID != f.openProject || o.Link != "/projects/"+f.openProject+"/releases/"+f.releaseOpen {
				t.Errorf("the own-project release ref = %+v, want the open project's release and its page", o)
			}
		case "release:" + f.releaseOther:
			// A PUBLIC project that is not the asset's: its identity is
			// rendered, because a reader may follow the link.
			foreign = true
			if o.ProjectID != f.otherPublic {
				t.Errorf("the foreign release ref = %+v, want the other public project %q", o, f.otherPublic)
			}
		case "object_version:" + f.objectVersionOpen:
			if o.Title != "Matched Alloy Screen" {
				t.Errorf("the object version ref = %+v, want the object version's own title", o)
			}
		default:
			t.Errorf("origin renders an unexpected ref %q", o.Ref)
		}
	}
	if !foreign {
		t.Error("the origin block dropped the ref into another PUBLIC project: a link a reader can follow must be rendered")
	}

	// --- 4. rights ---
	// The block is the stored document, not a re-rendering of it: the bytes
	// the publish wrote come back.
	var rightsDoc assetPageRightsDoc
	if err := json.Unmarshal(page.Rights, &rightsDoc); err != nil {
		t.Fatalf("rights block: %v: %s", err, page.Rights)
	}
	if rightsDoc.Version != 1 || rightsDoc.Visibility.DataAccess != string(rights.DataAccessOpen) {
		t.Errorf("rights = %s, want version 1 with data_access open (the document the publish stored)", page.Rights)
	}

	// --- 5. creators ---
	if len(page.Creators) != 1 {
		t.Fatalf("creators = %+v, want the publishing actor as the one credited party", page.Creators)
	}
	if got := page.Creators[0]; got.UserID != aliceID || got.Handle != "page-alice" || got.Role != assets.CreatorRolePublisher {
		t.Errorf("the creator entry = %+v, want the publishing user under the role %q", got, assets.CreatorRolePublisher)
	}

	// --- 6. metadata ---
	wantMeta := map[string]string{
		"access_level":  `"open"`,
		"blob_ids":      `["` + f.openBlob + `"]`,
		"data_type":     `"table"`,
		"purpose":       `"asset publish test"`,
		"quality_notes": `"reviewed"`,
	}
	if len(page.Metadata) != len(wantMeta) {
		t.Errorf("metadata has %d keys, want the %d the manifest declares: %+v",
			len(page.Metadata), len(wantMeta), page.Metadata)
	}
	for _, m := range page.Metadata {
		want, ok := wantMeta[m.Key]
		if !ok {
			t.Errorf("metadata renders an undeclared key %q", m.Key)
			continue
		}
		if string(m.Value) != want {
			t.Errorf("metadata[%s] = %s, want %s", m.Key, m.Value, want)
		}
		delete(wantMeta, m.Key)
	}
	for key := range wantMeta {
		t.Errorf("metadata omits the manifest's %q", key)
	}

	// --- 7. dependencies ---
	// One pin is rendered and one is dropped, and the drops are the point:
	// a pin to a public version of a PRIVATE project names a page that
	// answers 404 for this caller, so the whole entry — pin bytes included —
	// must be gone.
	if len(page.Dependencies) != 1 {
		t.Fatalf("dependencies = %+v, want exactly the one pinned version a reader can open", page.Dependencies)
	}
	dep := page.Dependencies[0]
	wantPin := f.depPublicPID + "@1.0"
	if dep.Pin != wantPin {
		t.Errorf("dependency = %q, want %q", dep.Pin, wantPin)
	}
	if !dep.Resolved || !dep.Public {
		t.Errorf("dependency = %+v, want it resolved and public", dep)
	}
	if dep.Title != "Baseline Alloy Set" || dep.Type != string(assets.TypeDataset) {
		t.Errorf("dependency = %+v, want the pinned asset's title and type", dep)
	}
	if dep.URL != "/assets/"+f.depPublicPID+"/1.0" {
		t.Errorf("dependency.url = %q, want the pinned version's page", dep.URL)
	}

	// --- 8. lineage ---
	if len(page.Lineage) != 1 {
		t.Fatalf("lineage = %+v, want exactly the supersede edge (the derive edge points into a private project)",
			page.Lineage)
	}
	edge := page.Lineage[0]
	if edge.Relation != "supersedes" || edge.Direction != assets.LineageParent {
		t.Errorf("lineage edge = %+v, want a supersedes edge read as the parent of 2.0", edge)
	}
	if edge.PID != f.subject || edge.Version != "1.0" || edge.Title != "Open Alloy Screen" {
		t.Errorf("lineage edge = %+v, want version 1.0 of this asset", edge)
	}
	if edge.URL != "/assets/"+f.subject+"/1.0" {
		t.Errorf("lineage.url = %q, want the superseded version's page", edge.URL)
	}

	// --- 9. used/derived public links ---
	if len(page.UsedBy) != 1 {
		t.Fatalf("used_by = %+v, want exactly the one PUBLIC usage by a PUBLIC project", page.UsedBy)
	}
	use := page.UsedBy[0]
	if use.ProjectID != f.openProject || use.ProjectSlug != "open-materials-lab" {
		t.Errorf("usage = %+v, want the open project", use)
	}
	if use.DependencyType != "reuses" {
		t.Errorf("usage.dependency_type = %q, want the stored row's own kind", use.DependencyType)
	}

	// --- 10. versions ---
	if len(page.Versions) != 2 {
		t.Fatalf("versions = %+v, want the two public versions (the private one is not a caller's business here)",
			page.Versions)
	}
	if page.Versions[0].Version != "2.0" || !page.Versions[0].Current {
		t.Errorf("versions[0] = %+v, want 2.0 marked current", page.Versions[0])
	}
	if page.Versions[1].Version != "1.0" || page.Versions[1].Current {
		t.Errorf("versions[1] = %+v, want 1.0", page.Versions[1])
	}
	// Two versions of one asset are told apart by label AND content: the
	// hashes are the ones the publishes returned, and they differ.
	if h1, h2 := page.Versions[0].IntegrityHash, page.Versions[1].IntegrityHash; h1 == h2 {
		t.Errorf("the two versions' hashes = %q / %q, want two different content hashes", h1, h2)
	}
	if page.Versions[0].IntegrityHash != f.subjectHashes["2.0"] || page.Versions[1].IntegrityHash != f.subjectHashes["1.0"] {
		t.Errorf("the versions block's hashes = %q / %q, want the published %q / %q",
			page.Versions[0].IntegrityHash, page.Versions[1].IntegrityHash,
			f.subjectHashes["2.0"], f.subjectHashes["1.0"])
	}
	if page.Version.IntegrityHash != page.Versions[0].IntegrityHash {
		t.Errorf("the rendered version's hash %q is not the current version's %q",
			page.Version.IntegrityHash, page.Versions[0].IntegrityHash)
	}
	if page.Versions[0].URL != "/assets/"+f.subject+"/2.0" || page.Versions[1].URL != "/assets/"+f.subject+"/1.0" {
		t.Errorf("the version addresses = %q / %q, want each label with the pid",
			page.Versions[0].URL, page.Versions[1].URL)
	}

	// --- 11. network events ---
	// The publish command wrote one event per version (inside the publish
	// transaction) — four of them. Two are rendered: the public versions',
	// which are the two the versions block above renders. The private 3.0's
	// event was written while the project was private (its own visibility
	// withholds it), and the private 2.1's event is PUBLIC — it was recorded
	// while the project was public — so the only reason it is absent is that
	// the version it names is one this caller may not see.
	if len(page.Events) != 2 {
		t.Fatalf("events = %+v, want the two events about the versions an anonymous caller may see", page.Events)
	}
	for _, e := range page.Events {
		if e.Type != assetVersionEventType {
			t.Errorf("event = %+v, want the publish event the command records", e)
		}
		if e.Version != "2.0" && e.Version != "1.0" {
			t.Errorf("event = %+v, want it to name one of the visible versions", e)
		}
		if e.Actor == nil || e.Actor.UserID != aliceID {
			t.Errorf("event actor = %+v, want the publishing user", e.Actor)
		}
	}
	// …stated as the invariant rather than as the two labels above: no event
	// may name a version the versions block does not render. A page whose
	// events block named the private 2.1 would answer "no such version" for
	// /assets/{pid}/2.1 in one block and print the label in another.
	assertEventsNameRenderedVersions(t, "the anonymous asset page", page)

	// --- the other half of every rule above: what must NOT be on the wire ---
	forbidInBody(t, "the anonymous asset page", raw,
		// the originating project could be named (it is public) — but the
		// other private project could not, in any of its three identities.
		f.privateProject, "hidden-usage-lab", "Hidden Usage Lab",
		// the pinned asset of the private project: its public version's pin
		// is dropped whole, so not even the pin bytes may ride along.
		f.depPrivatePID,
		// the private versions of the rendered asset: their labels, their row
		// ids, and their publish events. 3.0's event is private; 2.1's event
		// is public and names the version, so the label is withheld by the
		// events block's version guard or by nothing at all.
		"3.0", f.subjectVersions["3.0"], "2.1", f.subjectVersions["2.1"],
		// who published the private 2.1: reachable through the events block
		// alone, and withheld with the event.
		f.privatePublisherHandle,
		// no internal row id of any version of this asset: the identifiers
		// the page renders are the pid and the version label.
		f.subjectVersions["1.0"], f.subjectVersions["2.0"],
		// the private project's release, named by no ref of this version.
		f.releasePrivate,
		// no blob fetch URL of any kind, for the open blob or for anything
		// else: the platform has no such route and the page must not offer
		// one.
		"/blob", "files/raw", "/download",
	)
	// The rendered project IS named: the withholding above must not be a
	// page that says nothing.
	if !strings.Contains(raw, "open-materials-lab") {
		t.Errorf("the page does not name the asset's own public project: %s", leakWindow(raw, len(raw)/2))
	}
}

// --------------------------------------------------------------------------
// 2. The same URL, for the project's own member and for a signed-in
//    outsider

func TestAssetPageMembershipComesFromTheProjectService(t *testing.T) {
	ctx := testCtx(t)
	w := newAssetPageWorld(t, ctx)
	alice, aliceID := signup(t, w.ts.URL, "page-member@example.com", "page-member")
	bob, bobID := signup(t, w.ts.URL, "page-outsider@example.com", "page-outsider")
	f := seedAssetPageFixture(t, ctx, w, alice, aliceID, bob, bobID)

	// A visitor with no session. One URL, two answers: what differs is the
	// membership read, not the request.
	visitor := newTestUserClient(w.ts.URL)
	_, anon := fetchAssetPage(t, visitor, f.subject, "")
	if len(anon.Versions) != 2 {
		t.Errorf("the anonymous versions block = %+v, want the public versions only", anon.Versions)
	}
	if anon.Version.Version != "2.0" {
		t.Errorf("the anonymous page renders %q, want 2.0", anon.Version.Version)
	}

	// The project's owner: the same page, and now the private version is
	// part of it — it is the newest one, so it is the one rendered, and the
	// versions block says which of them is private rather than hiding it.
	raw, member := fetchAssetPage(t, alice, f.subject, "")
	if member.Version.Version != "3.0" || member.Version.Visibility != "private" {
		t.Errorf("the member's rendered version = %q/%q, want the private 3.0",
			member.Version.Version, member.Version.Visibility)
	}
	if len(member.Versions) != 4 {
		t.Fatalf("the member's versions block = %+v, want all four versions", member.Versions)
	}
	if member.Versions[0].Version != "3.0" || !member.Versions[0].Current ||
		member.Versions[0].Visibility != "private" {
		t.Errorf("the member's versions[0] = %+v, want the private 3.0 marked current", member.Versions[0])
	}
	// The private version the events block below is about: the member sees it
	// as a version, which is exactly what makes its event the member's to read
	// and nobody else's. Both facts come from one rule, asked twice.
	if member.Versions[1].Version != "2.1" || member.Versions[1].Visibility != "private" ||
		member.Versions[1].Current {
		t.Errorf("the member's versions[1] = %+v, want the private 2.1, not current", member.Versions[1])
	}
	if len(member.Events) != 4 {
		t.Errorf("the member sees %d events, want all four (both private versions' events included)",
			len(member.Events))
	}
	// The member does see the 2.1 publish event, and it is the PUBLIC one: it
	// was recorded while the project was public (the recording project's
	// visibility at publish time), so its own axis lets it through — what
	// makes it invisible to a non-member is the version it names.
	memberNamed21 := false
	for _, e := range member.Events {
		if e.Version == "2.1" {
			memberNamed21 = true
			if e.Actor == nil || e.Actor.Handle != f.privatePublisherHandle {
				t.Errorf("the 2.1 event's actor = %+v, want its publisher %q", e.Actor, f.privatePublisherHandle)
			}
		}
	}
	if !memberNamed21 {
		t.Errorf("the member's events = %+v, want the 2.1 publish event", member.Events)
	}
	assertEventsNameRenderedVersions(t, "the member's asset page", member)
	if page := member.Asset.OriginProject; page == nil || page.Slug != "open-materials-lab" {
		t.Errorf("the member's origin_project = %+v, want the project they belong to", page)
	}
	if !strings.Contains(raw, "3.0") {
		t.Error("the member's page does not render the private version: the membership bit was not read")
	}

	// A signed-in caller who is NOT a member reads exactly what a visitor
	// reads. Signing in is not membership, and the difference cannot come
	// from the client.
	outsiderRaw, outsider := fetchAssetPage(t, bob, f.subject, "")
	if outsider.Version.Version != "2.0" {
		t.Errorf("the signed-in outsider renders %q, want the newest PUBLIC version 2.0", outsider.Version.Version)
	}
	forbidInBody(t, "the signed-in outsider's asset page", outsiderRaw,
		f.subjectVersions["3.0"], f.privateProject, "hidden-usage-lab", f.depPrivatePID,
		// and the private versions' events: 3.0's own event is private, and
		// 2.1's is public — the outsider must be told about neither, and must
		// not learn who published 2.1 from the event that names it.
		"3.0", "2.1", f.subjectVersions["2.1"], f.privatePublisherHandle)
	assertEventsNameRenderedVersions(t, "the signed-in outsider's asset page", outsider)

	// The address of a version the caller may not see is the same 404 an
	// unknown version gets: a page that answered differently would be an
	// oracle for which labels exist.
	refusalRaw, env := fetchAssetRefusal(t, visitor, http.MethodGet, assetPageURL(f.subject, "3.0"), http.StatusNotFound)
	if env.Code != assetshttp.CodeAssetNotFound {
		t.Errorf("requesting the private version = code %q, want %q", env.Code, assetshttp.CodeAssetNotFound)
	}
	forbidInBody(t, "the refusal for the private version", refusalRaw, "3.0", "private", f.privateProject)

	// A version label that cannot be stored is refused before anything is
	// read, and the refusal says nothing about the asset. The code is the
	// PAGE route's own (ASSET_PAGE_VALIDATION_FAILED): a bad version address
	// and a bad type filter are two different surfaces and are answered — and
	// logged — as what they are.
	badRaw, env := fetchAssetRefusal(t, visitor, http.MethodGet, assetPageURL(f.subject, "not a label"), http.StatusBadRequest)
	if env.Code != assetshttp.CodeAssetPageValidationFailed {
		t.Errorf("a malformed version label = code %q, want %q", env.Code, assetshttp.CodeAssetPageValidationFailed)
	}
	if env.Code == assetshttp.CodeAssetListValidationFailed {
		t.Error("a malformed version label answers the browse list's validation code")
	}
	forbidInBody(t, "the malformed-label refusal", badRaw, f.subject, f.openProject)
}

// --------------------------------------------------------------------------
// 3. The other project: unreachable for a stranger, readable by its member,
//    and named nowhere in the public answer

func TestAssetPageOfAPrivateProjectIsTheSameNotFound(t *testing.T) {
	ctx := testCtx(t)
	w := newAssetPageWorld(t, ctx)
	alice, aliceID := signup(t, w.ts.URL, "page-gate@example.com", "page-gate")
	bob, bobID := signup(t, w.ts.URL, "page-owner@example.com", "page-owner")
	f := seedAssetPageFixture(t, ctx, w, alice, aliceID, bob, bobID)

	// The asset is real, it has a PUBLIC version, and it is in a private
	// project: the project read gate — not the version's visibility — is
	// what decides who may read the page.
	visitor := newTestUserClient(w.ts.URL)
	raw, env := fetchAssetRefusal(t, visitor, http.MethodGet, assetPageURL(f.depPrivatePID, ""), http.StatusNotFound)
	if env.Code != assetshttp.CodeAssetNotFound {
		t.Errorf("an asset in a private project = code %q, want the one not-found code", env.Code)
	}
	forbidInBody(t, "the 404 for an asset in a private project", raw,
		f.depPrivatePID, f.privateProject, "hidden-usage-lab", "Hidden Usage Lab", "Private Reference Screen")

	// The same 404 for a pid that names nothing at all — the two answers
	// must be indistinguishable, or the page says which pids exist.
	unknownRaw, _ := fetchAssetRefusal(t, visitor, http.MethodGet, assetPageURL("01j9z6k3m4n5p6q7r8s9t0v1zz", ""), http.StatusNotFound)
	if unknownRaw == "" {
		t.Fatal("the unknown-pid refusal is empty")
	}

	// Its own project's member reads it, and there the project is named.
	_, member := fetchAssetPage(t, bob, f.depPrivatePID, "")
	if member.Asset.OriginProject == nil || member.Asset.OriginProject.Slug != "hidden-usage-lab" {
		t.Errorf("the member's origin_project = %+v, want their own project named", member.Asset.OriginProject)
	}
	if len(member.Versions) != 1 || member.Version.Version != "1.0" {
		t.Errorf("the member reads %+v / %q, want the one public version", member.Versions, member.Version.Version)
	}
}

// --------------------------------------------------------------------------
// 4. The browse list

func TestAssetBrowseListsRealRowsAndFiltersByTheClosedTypeSet(t *testing.T) {
	ctx := testCtx(t)
	w := newAssetPageWorld(t, ctx)
	alice, aliceID := signup(t, w.ts.URL, "page-browse@example.com", "page-browse")
	bob, bobID := signup(t, w.ts.URL, "page-browse-other@example.com", "page-browse-other")
	f := seedAssetPageFixture(t, ctx, w, alice, aliceID, bob, bobID)

	visitor := newTestUserClient(w.ts.URL)
	raw, list := fetchAssetBrowse(t, visitor, "")

	// The set the filter accepts is the platform's own closed four, echoed
	// back so a client's control is built from it rather than from a copy.
	wantTypes := []string{"dataset", "protocol", "material_collection", "benchmark"}
	if len(list.Types) != len(wantTypes) {
		t.Fatalf("types = %v, want the closed V1 set %v", list.Types, wantTypes)
	}
	for i, want := range wantTypes {
		if list.Types[i] != want {
			t.Errorf("types[%d] = %q, want %q (declaration order)", i, list.Types[i], want)
		}
	}
	if list.Type != nil {
		t.Errorf("an unfiltered list echoes type = %v, want null (an absent filter is not a filter)", *list.Type)
	}

	byPIN := map[string]assetBrowseRow{}
	for _, row := range list.Assets {
		byPIN[row.PID] = row
		if !assets.Type(row.Type).Valid() {
			t.Errorf("a listed row carries the type %q, outside the closed set", row.Type)
		}
		if row.PublicVersions < 1 || row.LatestVersion == "" {
			t.Errorf("row %s = %+v, want a public version count and label", row.PID, row)
		}
		if row.URL != "/assets/"+row.PID || row.LatestURL != "/assets/"+row.PID+"/"+row.LatestVersion {
			t.Errorf("row %s carries the URLs %q / %q", row.PID, row.URL, row.LatestURL)
		}
	}

	// Every type is represented by a real row (the four-type coverage the
	// fixture seeded), and the rendered asset is in the list with its own
	// public project named.
	for name, pid := range map[string]string{
		"dataset": f.subject, "protocol": f.protocolPID,
		"material_collection": f.materialPID, "benchmark": f.benchmarkPID,
	} {
		row, ok := byPIN[pid]
		if !ok {
			t.Errorf("the %s asset %s is missing from the list", name, pid)
			continue
		}
		if row.Type != name {
			t.Errorf("asset %s is listed as %q, want %q", pid, row.Type, name)
		}
	}
	if row := byPIN[f.subject]; row.OriginProject == nil || row.OriginProject.Slug != "open-materials-lab" {
		t.Errorf("the rendered asset's row = %+v, want its public project named", row)
	}
	// The private project's asset IS listed — it has a public version, and an
	// asset with a public version is a network object (the Supervisor's
	// ruling on this task) — and whose it is, is not: null, with no id, no
	// slug and no visibility. The row DOES carry the pid and the
	// /assets/{pid}/{version} address, and that address is NOT openable by the
	// anonymous reader this list is served to: the page route puts the asset's
	// project through the project read gate first and answers the one 404
	// (TestAssetPageOfAPrivateProjectIsTheSameNotFound, below). The listing
	// and the page therefore disagree about whether such an asset is nameable
	// — reported as an L3 product question in this task's result, and not
	// decided here by either side.
	if row, ok := byPIN[f.depPrivatePID]; !ok {
		t.Error("the private project's public asset is missing from the list: its version is public")
	} else if row.OriginProject != nil {
		t.Errorf("the row of the private project's asset = %+v, want origin_project null", row.OriginProject)
	}

	// An asset whose every version is private is not listed and not
	// counted: not a row, not a number, nothing to compare.
	if _, ok := byPIN[f.privateOnlyPID]; ok {
		t.Error("the list contains an asset with no public version")
	}
	forbidInBody(t, "the browse list", raw,
		f.privateProject, "hidden-usage-lab", "Hidden Usage Lab", f.privateOnlyPID, "unpublished-draft",
	)

	// The filter is the request's, echoed, and every row it selects agrees
	// with it — over real rows, whose types the CHECK constrains.
	for _, want := range wantTypes {
		filteredRaw, filtered := fetchAssetBrowse(t, visitor, want)
		if filtered.Type == nil || *filtered.Type != want {
			t.Fatalf("the %s list echoes type = %v, want %q", want, filtered.Type, want)
		}
		if len(filtered.Assets) == 0 {
			t.Errorf("the %s list is empty: the fixture seeded one", want)
		}
		for _, row := range filtered.Assets {
			if row.Type != want {
				t.Errorf("the %s list contains a %s row (%s)", want, row.Type, row.PID)
			}
		}
		forbidInBody(t, "the "+want+" list", filteredRaw, f.privateProject, "hidden-usage-lab")
	}

	// A type outside the set is REFUSED, not answered with an empty list:
	// "there are no such assets" is a claim this platform cannot make about
	// a type it does not have.
	badRaw, env := fetchAssetRefusal(t, visitor, http.MethodGet, "/api/v1/assets?type=melting_point", http.StatusBadRequest)
	if env.Code != assetshttp.CodeAssetListValidationFailed {
		t.Errorf("an unknown type = code %q, want %q", env.Code, assetshttp.CodeAssetListValidationFailed)
	}
	for _, want := range wantTypes {
		if !strings.Contains(env.Message, want) {
			t.Errorf("the refusal does not name %q: %s", want, env.Message)
		}
	}
	forbidInBody(t, "the unknown-type refusal", badRaw, f.privateProject, f.privateOnlyPID)
}

// --------------------------------------------------------------------------
// 5. Nothing a read route touches can write, and nothing it does writes

func TestAssetPageReadsThroughAReadOnlySession(t *testing.T) {
	ctx := testCtx(t)
	w := newAssetPageWorld(t, ctx)
	alice, aliceID := signup(t, w.ts.URL, "page-readonly@example.com", "page-readonly")
	bob, bobID := signup(t, w.ts.URL, "page-readonly-other@example.com", "page-readonly-other")
	f := seedAssetPageFixture(t, ctx, w, alice, aliceID, bob, bobID)

	// The setting is on the CONNECTIONS the page's readers use, and it is
	// the server's: every connection this pool will ever open carries it.
	var setting string
	if err := w.readOnly.QueryRow(ctx, `SHOW default_transaction_read_only`).Scan(&setting); err != nil {
		t.Fatalf("read the session setting: %v", err)
	}
	if setting != "on" {
		t.Fatalf("the page's pool carries default_transaction_read_only=%q, want on", setting)
	}

	// The probe IS a write, and it succeeds where writes are allowed — the
	// control that keeps the refusal below from being a typo's symptom.
	if err := probeWrite(ctx, w.pool, "page-write-control"); err != nil {
		t.Fatalf("the control write was refused on the writable pool (%v): the probe is not a valid write", err)
	}
	err := probeWrite(ctx, w.readOnly, "page-write-probe")
	if err == nil {
		t.Fatal("a write through the page's pool succeeded: the read-only session is not in force")
	}
	if got := sqlState(t, err); got != readOnlySQLState {
		t.Fatalf("the write was refused with SQLSTATE %s, want %s (read_only_sql_transaction): %v",
			got, readOnlySQLState, err)
	}

	// Every row of every table, before and after the whole read surface —
	// the page for a visitor, the page for a member, the version parameter,
	// the browse list, the refusals and the blob probes. A page that wrote
	// an audit row, an event, an outbox entry or a counter would move this.
	before := fingerprintDatabase(t, ctx, w.pool)
	if before == "" {
		t.Fatal("the fingerprint is empty: the instrument would pass on an empty database")
	}

	visitor := newTestUserClient(w.ts.URL)
	fetchAssetPage(t, visitor, f.subject, "")
	fetchAssetPage(t, visitor, f.subject, "1.0")
	fetchAssetPage(t, alice, f.subject, "")
	fetchAssetBrowse(t, visitor, "")
	fetchAssetBrowse(t, visitor, "dataset")
	fetchAssetRefusal(t, visitor, http.MethodGet, assetPageURL(f.subject, "9.9"), http.StatusNotFound)
	fetchAssetRefusal(t, visitor, http.MethodGet, assetPageURL("01j9z6k3m4n5p6q7r8s9t0v1zz", ""), http.StatusNotFound)
	for _, path := range []string{
		"/api/v1/assets/" + f.subject + "/blob",
		"/api/v1/assets/" + f.subject + "/versions/2.0/blob",
		"/api/v1/blobs/" + f.openBlob,
	} {
		probeBlobRouteMissing(t, visitor, path)
	}

	after := fingerprintDatabase(t, ctx, w.pool)
	if before != after {
		t.Errorf("the database changed across the read surface:\nbefore:\n%s\nafter:\n%s", before, after)
	}

	// The instrument's own control, after every comparison it is used for:
	// a fingerprint that could not notice a change would have passed the
	// check above for the wrong reason.
	if err := probeWrite(ctx, w.pool, "page-fingerprint-control"); err != nil {
		t.Fatalf("the control write failed: %v", err)
	}
	if moved := fingerprintDatabase(t, ctx, w.pool); moved == after {
		t.Error("the fingerprint did not change after a row was written: the check above measured nothing")
	}
}

// --------------------------------------------------------------------------
// 6. There is no blob download route

func TestAssetPageHasNoBlobDownloadRoute(t *testing.T) {
	ctx := testCtx(t)
	w := newAssetPageWorld(t, ctx)
	alice, aliceID := signup(t, w.ts.URL, "page-blob@example.com", "page-blob")
	bob, bobID := signup(t, w.ts.URL, "page-blob-other@example.com", "page-blob-other")
	f := seedAssetPageFixture(t, ctx, w, alice, aliceID, bob, bobID)

	visitor := newTestUserClient(w.ts.URL)

	// The control: a path the API DOES route answers 404 with this API's
	// own envelope, code field and all. Without it, "the body below has no
	// code field" would be a property of the client rather than a
	// difference between the two answers.
	unknownRaw, env := fetchAssetRefusal(t, visitor, http.MethodGet,
		assetPageURL("01j9z6k3m4n5p6q7r8s9t0v1zz", ""), http.StatusNotFound)
	if env.Code != assetshttp.CodeAssetNotFound {
		t.Fatalf("the control 404 = code %q, want %q", env.Code, assetshttp.CodeAssetNotFound)
	}
	if !strings.Contains(unknownRaw, `"code"`) {
		t.Fatalf("the control 404 does not carry a code field: the check below would measure nothing: %s", unknownRaw)
	}

	// docs/17 §5 wants every fetch to pass the project/object/blob policy at
	// fetch time. The page therefore links to no blob, and the platform
	// serves no fetch route for one: the proof is that the route does not
	// exist, not that a button is hidden. assetshttp registers two GETs, and
	// neither of them takes a second path segment.
	for _, path := range []string{
		"/api/v1/assets/" + f.subject + "/blob",
		"/api/v1/assets/" + f.subject + "/versions/2.0/blob",
		"/api/v1/assets/" + f.subject + "/download",
		"/api/v1/blobs/" + f.openBlob,
		"/api/v1/files/raw?blob=" + f.openBlob,
	} {
		probeBlobRouteMissing(t, visitor, path)
	}
}

// probeBlobRouteMissing requires that a fetch path answers the mux's own
// 404 — the stdlib's plain-text "404 page not found", which is what an
// unmatched ServeMux pattern produces — rather than this API's JSON error
// envelope. The two are distinguishable on the body: every envelope this
// surface writes carries a "code" field, and the stdlib's 404 carries none.
//
// The body is read raw rather than decoded: this helper exists to tell the
// two 404 SHAPES apart, so it cannot go through the envelope decoder.
func probeBlobRouteMissing(t *testing.T, uc *testUserClient, path string) {
	t.Helper()
	resp := uc.do(t, http.MethodGet, path, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s = %d, want 404 — there is no such route: %s", path, resp.StatusCode, readAll(t, resp))
	}
	raw := readAll(t, resp)
	if strings.Contains(raw, `"code"`) {
		t.Errorf("GET %s answered this API's error envelope (%s), want no such route", path, raw)
	}
	if !strings.Contains(raw, "404 page not found") {
		t.Errorf("GET %s answered %q with %s, want the router's own 404",
			path, resp.Header.Get("Content-Type"), raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("GET %s answered Content-Type %q, want text/plain from the router", path, ct)
	}
}
