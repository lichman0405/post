// Task T0802 — the Explore index over real PostgreSQL.
//
// The unit suite pins the index's RULES (internal/application/explore:
// which rows render, which project may be named, the freshness ranking, the
// section cap) and the e2e suite pins the composed surface over HTTP against
// in-memory readers (tests/e2e/explore_e2e_test.go). Neither can settle what
// only a real database settles, and this file is that half:
//
//  1. The three reads T0802 added really run against the canonical schema.
//     They are the ONLY new SQL this task introduces — publications joined
//     to their object versions and objects, accounts joined to their
//     profiles, organizations — and a column that has drifted is a 503 in
//     production and nothing at all in an in-memory reader. Every one of
//     them is exercised here over the real tables, through the REAL adapter
//     (cmd/api/explorehttp.NewStore over a real pool).
//
//  2. The composed surface answers over real rows through the production
//     wiring — cmd/api/explorehttp.Sources with the real project service,
//     the real asset page store, the real contribution service over the real
//     opportunity store, and the real explorehttp.Store. This is the
//     composition cmd/api/main.go mounts, minus only the router above it.
//
//  3. The disclosure rules hold over those rows. The fixture is RAW: the
//     database holds a private project that PUBLISHED knowledge and an asset
//     version, a private project with a publicized open opportunity, a
//     disabled account, a deactivated organization, an internal opportunity
//     and a closed one. The answer must name none of the private things, and
//     the negative assertions run against the RAW RESPONSE BODY — so a field
//     some layer added on its own fails here, which is what makes this a
//     check on the whole surface rather than on one function. Each negative
//     block carries the positive half (the public identities the same bytes
//     MUST hold), because a leak check that passed on an empty answer
//     measured nothing.
//
//  4. The order is freshness, never popularity — and the fixture is written
//     in the OPPOSITE order of every section's freshness key, so a surface
//     that echoed the read order (or the tables' physical order) fails
//     instead of passing by coincidence. This is acceptance "无 like-based
//     core ranking": no popularity field travels in the payload (asserted on
//     the bytes) and the ranking that does exist is asserted section by
//     section.
//
//  5. The index writes NOTHING. Every read runs on connections whose
//     default_transaction_read_only is on, so the server itself refuses a
//     write — with a control proving the probe is a real write — and any
//     read that tried to write would turn this suite's 200s into 503s.
//
// What is deliberately NOT here: the browser rendering of the page
// (tests/e2e-explore), the asset hub's own rules (T0709's suites — this file
// reuses BuildBrowse through the production adapter rather than re-testing
// it), and the rationale of "a private project may publish" (docs/12 §2,
// stated where it is decided: internal/application/explore).

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/explorehttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	appcontribution "github.com/lichman0405/post/internal/application/contribution"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/contribution"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/testdb"
)

// exploreTaskID namespaces this task's test databases
// (test_T0802_<run_id>).
const exploreTaskID = "T0802"

// The fixture's identities. The private ones are the strings the leak
// assertions search the response bytes for, so each is declared once and is
// deliberately distinctive: a short needle would match other fixture text,
// and a leak check that can match by accident is a check nobody can trust
// either way.
const (
	exploreOpenProjectSlug   = "explore-int-open-lab"
	exploreOpenProjectName   = "Explore Integration Open Lab"
	exploreSecondProjectSlug = "explore-int-second-lab"
	exploreSecondProjectName = "Explore Integration Second Lab"
	exploreHiddenProjectSlug = "explore-int-hidden-lab"
	exploreHiddenProjectName = "Explore Integration Hidden Lab"

	exploreActiveOrgSlug  = "explore-int-institute"
	exploreActiveOrgName  = "Explore Integration Institute"
	exploreActiveOrgDesc  = "zeolite chemistry"
	exploreSecondOrgSlug  = "explore-int-labs"
	exploreSecondOrgName  = "Explore Integration Labs"
	exploreSecondOrgDesc  = "instrumentation and metrology"
	exploreRetiredOrgSlug = "explore-int-retired-institute"
	exploreRetiredOrgName = "Explore Integration Retired Institute"
	exploreRetiredOrgDesc = "wound up before this index existed"

	exploreAliceHandle = "explore-int-alice"
	exploreAliceName   = "Alice Integration"
	exploreAliceBio    = "catalysis and MOF screening"
	exploreBobHandle   = "explore-int-bob"
	exploreBobName     = "Bob Integration"
	// exploreRetiredHandle/Name appear in no answer: the account is disabled
	// (users.disabled_at), so the directory does not list it.
	exploreRetiredHandle = "explore-int-retired"
	exploreRetiredName   = "Retired Integration"

	exploreOpenKnowledgeTitle   = "Integration Open Question"
	exploreHiddenKnowledgeTitle = "Integration Hidden Question"

	exploreOpenAssetTitle   = "Integration Public Asset"
	exploreSecondAssetTitle = "Integration Second Asset"
	// exploreHiddenAssetTitle is an asset of the PRIVATE project whose
	// version is public: T0709's rule lists such an asset and withholds its
	// project, so this title must appear while the project's identity must
	// not. It is also the NEWEST asset in the fixture, so a leak would sort
	// to the top of the section.
	exploreHiddenAssetTitle = "Hidden Integration Asset"
	// exploreDraftAssetTitle has no public version at all: it is in no
	// answer, and no count mentions it.
	exploreDraftAssetTitle = "Draft Integration Asset"

	exploreBeginnerTask     = "Integration beginner task"
	exploreAdvancedTask     = "Integration advanced task"
	exploreIntermediateTask = "Integration intermediate task"
	// exploreHiddenTask is publicized against the PRIVATE project's target
	// and is the NEWEST publicized row in the fixture: it is on the open
	// network (someone publicized it explicitly) and it sorts first, so a
	// leak of its project would be the first thing in the section.
	exploreHiddenTask = "Integration hidden opportunity"
	// exploreClosedTask was publicized and then closed: history the index
	// does not offer, and nothing a contributor can take on.
	exploreClosedTask = "Integration closed opportunity"
	// exploreInternalTask was never publicized at all.
	exploreInternalTask = "Integration internal opportunity"
)

// exploreDay is a fixture instant: day d of September 2026, 10:00 UTC.
//
// Every freshness key in the fixture is an explicit instant rather than
// now(), because the ORDER is half of what this suite asserts and rows
// written microseconds apart cannot assert it.
func exploreDay(d int) time.Time {
	return time.Date(2026, time.September, d, 10, 0, 0, 0, time.UTC)
}

// exploreWorld is the composed Explore surface over one test database.
//
// The pools are split the way T0709's asset page splits them: the writable
// one SEEDS (a fixture is repository state, written with raw SQL), and every
// READ the surface performs runs on a pool whose connections are read-only
// from the moment they connect. So "the index cannot write" is a property of
// the server rather than a promise about the code.
type exploreWorld struct {
	ts       *httptest.Server
	pool     *pgxpool.Pool
	readOnly *pgxpool.Pool

	// Ids the fixture resolved for its own assertions. The private one is
	// here to be forbidden from the response, not to be looked for.
	aliceID         string
	openProjectID   string
	hiddenProjectID string
}

// newExploreWorld composes the production tree over one freshly migrated
// test database, exactly as cmd/api/main.go does.
func newExploreWorld(t *testing.T, ctx context.Context) *exploreWorld {
	t.Helper()
	pool, dbURL := testdb.Setup(t, ctx, adminURL(t), exploreTaskID)
	readOnly := openReadOnlyPool(t, ctx, dbURL)

	// The Projects section's own read runs where the other reads run: the
	// project service answers the PUBLIC half when it is given no actor
	// (explorehttp.ProjectSource passes the zero projects.Reader), and that
	// half is a SELECT.
	projectAPI := projectshttp.New(projectshttp.Deps{
		Store: persistence.NewProjectStore(readOnly),
		Orgs:  persistence.NewOrgStore(readOnly),
		Authz: authz.NewMatrixEngine(),
	})

	api := explorehttp.New(explorehttp.Deps{
		Reader: explorehttp.Sources{
			ProjectSource: explorehttp.ProjectSource{Service: projectAPI.Service()},
			AssetSource:   explorehttp.AssetSource{Pages: persistence.NewAssetPageStore(readOnly)},
			ContributionSource: explorehttp.ContributionSource{
				Service: appcontribution.NewService(contribution.NewOpportunityStore(readOnly)),
			},
			Store: explorehttp.NewStore(readOnly),
		},
	})

	mux := http.NewServeMux()
	api.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	return &exploreWorld{ts: ts, pool: pool, readOnly: readOnly}
}

// mustIndex reads the surface the way a client does and returns the RAW body
// with the decoded index. Every assertion in this file is made on those
// bytes.
func (w *exploreWorld) mustIndex(t *testing.T) (string, exploreWireIndex) {
	t.Helper()
	resp, err := http.Get(w.ts.URL + "/api/v1/explore")
	if err != nil {
		t.Fatalf("GET /api/v1/explore: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/explore = %d: %s", resp.StatusCode, raw)
	}
	var index exploreWireIndex
	if err := json.Unmarshal([]byte(raw), &index); err != nil {
		t.Fatalf("the index is not the JSON document a client reads: %v\n%s", err, raw)
	}
	return raw, index
}

// --------------------------------------------------------------------------
// The fixture

// What seedExploreOpportunity does with the row after writing it.
const (
	exploreKeepInternal = "internal"
	explorePublicize    = "publicize"
	exploreMayClose     = "closed"
)

// seedExploreFixture writes the whole fixture with raw SQL.
//
// The ORDER of the writes is load-bearing and is the reverse of every
// section's freshness order: the oldest rows are written first and the newest
// last, so a section that echoed the order the rows came back in — rather
// than sorting by its own freshness key — fails the ordering assertions
// instead of passing by luck.
func seedExploreFixture(t *testing.T, ctx context.Context, w *exploreWorld) {
	t.Helper()
	pool := w.pool

	// --- accounts: oldest first, the disabled one LAST --------------------
	// Bob is the OLDER account and is written first, so the directory's
	// freshness order (alice first) is the reverse of the write order.
	//
	// A user inserted after migration 00017's backfill has no profiles row at
	// all (the backfill ran once, over the rows that existed then), and bob
	// is that user: the directory must still list him — the people read LEFT
	// JOINs profiles — which is why his bio renders empty rather than his row
	// vanishing.
	mustQueryUUID(t, ctx, pool,
		`INSERT INTO users (handle, display_name, created_at) VALUES ($1, $2, $3) RETURNING id`,
		exploreBobHandle, exploreBobName, exploreDay(2))
	w.aliceID = mustQueryUUID(t, ctx, pool,
		`INSERT INTO users (handle, email, display_name, created_at)
		 VALUES ($1, $2, $3, $4) RETURNING id`,
		exploreAliceHandle, "explore-int-alice@example.com", exploreAliceName, exploreDay(5))
	if _, err := pool.Exec(ctx,
		`INSERT INTO profiles (user_id, bio) VALUES ($1, $2)`, w.aliceID, exploreAliceBio); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	mustQueryUUID(t, ctx, pool,
		`INSERT INTO users (handle, display_name, created_at, disabled_at)
		 VALUES ($1, $2, $3, now()) RETURNING id`,
		exploreRetiredHandle, exploreRetiredName, exploreDay(9))

	// --- organizations: the deactivated one FIRST -------------------------
	if _, err := pool.Exec(ctx,
		`INSERT INTO organizations (slug, name, description, created_at, deactivated_at)
		 VALUES ($1, $2, $3, $4, now())`,
		exploreRetiredOrgSlug, exploreRetiredOrgName, exploreRetiredOrgDesc, exploreDay(1)); err != nil {
		t.Fatalf("seed deactivated organization: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO organizations (slug, name, description, created_at) VALUES ($1, $2, $3, $4)`,
		exploreActiveOrgSlug, exploreActiveOrgName, exploreActiveOrgDesc, exploreDay(3)); err != nil {
		t.Fatalf("seed organization: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO organizations (slug, name, description, created_at) VALUES ($1, $2, $3, $4)`,
		exploreSecondOrgSlug, exploreSecondOrgName, exploreSecondOrgDesc, exploreDay(7)); err != nil {
		t.Fatalf("seed second organization: %v", err)
	}

	// --- projects: the older public one first, the private one in the
	// middle, the newer public one last, so no write order is right --------
	w.openProjectID = mustQueryUUID(t, ctx, pool,
		`INSERT INTO projects (slug, name, purpose, activity_status, visibility, created_by, created_at)
		 VALUES ($1, $2, $3, 'active', 'public', $4, $5) RETURNING id`,
		exploreOpenProjectSlug, exploreOpenProjectName, "open integration research", w.aliceID, exploreDay(2))
	w.hiddenProjectID = mustQueryUUID(t, ctx, pool,
		`INSERT INTO projects (slug, name, purpose, activity_status, visibility, created_by, created_at)
		 VALUES ($1, $2, $3, 'active', 'private', $4, $5) RETURNING id`,
		exploreHiddenProjectSlug, exploreHiddenProjectName, "unpublished alloy screening", w.aliceID, exploreDay(4))
	secondProjectID := mustQueryUUID(t, ctx, pool,
		`INSERT INTO projects (slug, name, purpose, activity_status, visibility, created_by, created_at)
		 VALUES ($1, $2, $3, 'planning', 'public', $4, $5) RETURNING id`,
		exploreSecondProjectSlug, exploreSecondProjectName, "second open integration research", w.aliceID, exploreDay(8))

	// --- one public version of an asset of each project, one asset whose
	// every version is private, and the private project's public asset
	// written LAST (it is the newest, so a leak sorts to the top) ---------
	seedExploreAsset(t, ctx, pool, w.openProjectID, w.aliceID,
		exploreOpenAssetTitle, "1.0.0", "public", exploreDay(3))
	seedExploreAsset(t, ctx, pool, secondProjectID, w.aliceID,
		exploreSecondAssetTitle, "2.0.0", "public", exploreDay(6))
	seedExploreAsset(t, ctx, pool, w.openProjectID, w.aliceID,
		exploreDraftAssetTitle, "0.0.1", "private", exploreDay(11))
	seedExploreAsset(t, ctx, pool, w.hiddenProjectID, w.aliceID,
		exploreHiddenAssetTitle, "0.9.0", "public", exploreDay(15))

	// --- knowledge: the private project's publication FIRST, so the
	// section's freshness order (the public project's row, newest) is the
	// reverse of the write order ------------------------------------------
	seedExploreKnowledge(t, ctx, pool, w.hiddenProjectID, w.aliceID, 2,
		exploreHiddenKnowledgeTitle, "superseded", exploreDay(11))
	seedExploreKnowledge(t, ctx, pool, w.openProjectID, w.aliceID, 1,
		exploreOpenKnowledgeTitle, "active", exploreDay(14))

	// --- opportunities: newest publicized FIRST, so the rendered order is
	// again the reverse of the write order --------------------------------
	// The private project's opportunity is the newest of all: it IS on the
	// open network (someone publicized it explicitly — that is what
	// publicizing means, docs/12 §3), so the row is listed and its project
	// is not.
	seedExploreOpportunity(t, ctx, pool, w, w.hiddenProjectID,
		exploreHiddenTask, "advanced", exploreDay(17), explorePublicize, "")
	seedExploreOpportunity(t, ctx, pool, w, w.openProjectID,
		exploreIntermediateTask, "intermediate", exploreDay(16), explorePublicize, "")
	seedExploreOpportunity(t, ctx, pool, w, w.openProjectID,
		exploreAdvancedTask, "advanced", exploreDay(13), explorePublicize, "")
	seedExploreOpportunity(t, ctx, pool, w, w.openProjectID,
		exploreClosedTask, "beginner", exploreDay(12), explorePublicize, exploreMayClose)
	seedExploreOpportunity(t, ctx, pool, w, w.openProjectID,
		exploreBeginnerTask, "beginner", exploreDay(10), explorePublicize, "")
	seedExploreOpportunity(t, ctx, pool, w, w.openProjectID,
		exploreInternalTask, "advanced", time.Time{}, exploreKeepInternal, "")
}

// seedExploreKnowledge writes one research question, its first version and
// the publication that puts that version on the network.
//
// The object type is research_question because that is the type 00062's
// target guard resolves a research_question opportunity target against — the
// same objects serve both fixture needs.
func seedExploreKnowledge(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	projectID, authorID string, versionNo int, title, lifecycle string, publishedAt time.Time,
) {
	t.Helper()
	objectID := mustQueryUUID(t, ctx, pool,
		`INSERT INTO scientific_objects (project_id, object_type, created_by)
		 VALUES ($1, 'research_question', $2) RETURNING id`, projectID, authorID)
	stateID := mustQueryUUID(t, ctx, pool,
		`INSERT INTO project_states (project_id, state_hash, manifest_version)
		 VALUES ($1, 'genesis-'||$2, '1') RETURNING id`, projectID, title)
	versionID := mustQueryUUID(t, ctx, pool,
		`INSERT INTO scientific_object_versions
		   (object_id, version_no, state_id, schema_id, schema_version, title,
		    lifecycle_state, payload, integrity_hash, created_by)
		 VALUES ($1, $2, $3, 'https://open-rd.example/schemas/research_question.schema.json', '1',
		         $4, $5, '{}'::jsonb, 'sha256:'||$4, $6) RETURNING id`,
		objectID, versionNo, stateID, title, lifecycle, authorID)
	if _, err := pool.Exec(ctx,
		`INSERT INTO knowledge_publications
		   (object_version_id, public_version, rights_json, published_by, published_at)
		 VALUES ($1, $2, '{"version":1}'::jsonb, $3, $4)`,
		versionID, "v"+strconv.Itoa(versionNo), authorID, publishedAt); err != nil {
		t.Fatalf("publish %q: %v", title, err)
	}
}

// seedExploreOpportunity writes one contribution opportunity.
//
// The row is born internal — the only way the database admits one: 00062's
// guard refuses a non-internal INSERT and refuses the publicize stamps at
// creation — and publicizing is the separate, explicit action this helper
// performs in its own transaction with the setting the guard requires. A
// fixture that could insert a public row directly would be a fixture testing
// a state production cannot reach.
//
// Each opportunity gets its OWN target object: the partial unique index
// contribution_opportunities_target_active_idx allows one active row per
// (target_type, target_id), so two open opportunities cannot share one.
func seedExploreOpportunity(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, w *exploreWorld,
	projectID, title, difficulty string, publicizedAt time.Time, action, closeState string,
) {
	t.Helper()
	targetID := mustQueryUUID(t, ctx, pool,
		`INSERT INTO scientific_objects (project_id, object_type, created_by)
		 VALUES ($1, 'research_question', $2) RETURNING id`, projectID, w.aliceID)

	opID := mustQueryUUID(t, ctx, pool,
		`INSERT INTO contribution_opportunities
		   (project_id, target_type, target_id, title, description, difficulty,
		    required_capabilities, state, visibility, created_by)
		 VALUES ($1, 'research_question', $2, $3, 'T0802 fixture', $4,
		         ARRAY['python','dft'], 'open', 'internal', $5) RETURNING id`,
		projectID, targetID, title, difficulty, w.aliceID)

	if action != explorePublicize {
		return
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin publicize %q: %v", title, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('post.co_publicize', 'on', true)`); err != nil {
		t.Fatalf("set publicize flag for %q: %v", title, err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE contribution_opportunities
		    SET visibility = 'public', publicized_by = $2, publicized_at = $3
		  WHERE id = $1`, opID, w.aliceID, publicizedAt); err != nil {
		t.Fatalf("publicize %q: %v", title, err)
	}
	if closeState != "" {
		if _, err := tx.Exec(ctx,
			`UPDATE contribution_opportunities SET state = $2 WHERE id = $1`, opID, closeState); err != nil {
			t.Fatalf("close %q: %v", title, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit publicize %q: %v", title, err)
	}
}

// seedExploreAsset writes one research asset and one version of it.
//
// The pid comes from the platform's own generator (internal/assets.NewPID):
// the column has a format CHECK and a unique index, and a hand-written string
// here would be a second definition of the pid shape.
func seedExploreAsset(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	projectID, authorID, title, version, visibility string, publishedAt time.Time,
) {
	t.Helper()
	pid, err := assets.NewPID()
	if err != nil {
		t.Fatalf("generate pid: %v", err)
	}
	assetID := mustQueryUUID(t, ctx, pool,
		`INSERT INTO research_assets (asset_type, pid, slug, title, origin_project_id)
		 VALUES ('dataset', $1, $2, $3, $4) RETURNING id`,
		string(pid), strings.ToLower(strings.ReplaceAll(title, " ", "-")), title, projectID)
	if _, err := pool.Exec(ctx,
		`INSERT INTO research_asset_versions
		   (asset_id, version, manifest, rights_json, visibility, integrity_hash,
		    published_by, published_at, origin_refs)
		 VALUES ($1, $2, '{}'::jsonb, '{"version":1}'::jsonb, $3, 'sha256:'||$2, $4, $5,
		         ARRAY['project:'||$6])`,
		assetID, version, visibility, authorID, publishedAt, projectID); err != nil {
		t.Fatalf("publish %s@%s: %v", title, version, err)
	}
}

// --------------------------------------------------------------------------
// The wire shape this suite reads (declared here rather than reused from
// internal/application/explore, so a silent JSON tag change fails HERE)

type exploreWireIndex struct {
	Projects      exploreWireSection[exploreWireProject]      `json:"projects"`
	Assets        exploreWireSection[exploreWireAsset]        `json:"assets"`
	Knowledge     exploreWireSection[exploreWireKnowledge]    `json:"knowledge"`
	People        exploreWireSection[exploreWirePerson]       `json:"people"`
	Organizations exploreWireSection[exploreWireOrganization] `json:"organizations"`
	Contributions exploreWireSection[exploreWireContribution] `json:"contributions"`
}

type exploreWireSection[T any] struct {
	Tab   string `json:"tab"`
	Items []T    `json:"items"`
}

type exploreWireProject struct {
	ID             string `json:"id"`
	Slug           string `json:"slug"`
	Name           string `json:"name"`
	Purpose        string `json:"purpose"`
	ActivityStatus string `json:"activity_status"`
	URL            string `json:"url"`
	CreatedAt      string `json:"created_at"`
}

type exploreWireAsset struct {
	PID               string              `json:"pid"`
	Type              string              `json:"type"`
	Title             string              `json:"title"`
	Slug              string              `json:"slug"`
	URL               string              `json:"url"`
	OriginProject     *exploreWireProjRef `json:"origin_project"`
	LatestVersion     string              `json:"latest_version"`
	LatestURL         string              `json:"latest_url"`
	LatestPublishedAt string              `json:"latest_published_at"`
}

type exploreWireKnowledge struct {
	ID             string              `json:"id"`
	ObjectID       string              `json:"object_id"`
	ObjectType     string              `json:"object_type"`
	PublicVersion  string              `json:"public_version"`
	Title          string              `json:"title"`
	Project        *exploreWireProjRef `json:"project"`
	PublishedAt    string              `json:"published_at"`
	LifecycleState string              `json:"lifecycle_state"`
}

type exploreWirePerson struct {
	ID          string `json:"id"`
	Handle      string `json:"handle"`
	DisplayName string `json:"display_name"`
	Bio         string `json:"bio"`
	URL         string `json:"url"`
}

type exploreWireOrganization struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

type exploreWireContribution struct {
	ID                   string              `json:"id"`
	Title                string              `json:"title"`
	Description          string              `json:"description"`
	Difficulty           string              `json:"difficulty"`
	TargetType           string              `json:"target_type"`
	RequiredCapabilities []string            `json:"required_capabilities"`
	Project              *exploreWireProjRef `json:"project"`
	PublicizedAt         string              `json:"publicized_at"`
}

// exploreWireProjRef is the project identity a row may print.
type exploreWireProjRef struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

// --------------------------------------------------------------------------
// Assertions

// exploreTitles lists a section's rendered titles, so an order assertion
// states the order it expects in one line.
func exploreTitles[T any](items []T, title func(T) string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, title(item))
	}
	return out
}

// assertExploreOrder fails unless a section rendered exactly want, in order.
// It is the positive half of every negative assertion in this file: a check
// that a private row is absent says nothing unless the public rows it lives
// among are known to be there.
func assertExploreOrder(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s renders %d rows, want %d: %v", what, len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s renders %v, want %v", what, got, want)
		}
	}
}

// assertExploreLacks fails when unwanted is one of a section's rows.
func assertExploreLacks(t *testing.T, what, unwanted string, got []string) {
	t.Helper()
	for _, g := range got {
		if g == unwanted {
			t.Errorf("%s renders %q although it must not: %v", what, unwanted, got)
			return
		}
	}
}

// exploreInstant parses a rendered instant, so an order assertion compares
// instants rather than strings.
func exploreInstant(t *testing.T, what, raw string) time.Time {
	t.Helper()
	if raw == "" {
		t.Fatalf("%s: no instant rendered", what)
	}
	ts, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("%s: %q is not an instant: %v", what, raw, err)
	}
	return ts
}

// assertExploreNewestFirst fails when a section's rows are not in
// descending freshness order.
func assertExploreNewestFirst[T any](t *testing.T, what string, items []T, key func(T) (string, string)) {
	t.Helper()
	if len(items) < 2 {
		t.Fatalf("%s holds %d rows, so an order check would measure nothing", what, len(items))
	}
	for i := 1; i < len(items); i++ {
		prevLabel, prevRaw := key(items[i-1])
		currLabel, currRaw := key(items[i])
		prev := exploreInstant(t, what+" "+prevLabel, prevRaw)
		curr := exploreInstant(t, what+" "+currLabel, currRaw)
		if curr.After(prev) {
			t.Errorf("%s is not newest-first: %s (%s) precedes %s (%s)",
				what, prevLabel, prevRaw, currLabel, currRaw)
		}
	}
}

// --------------------------------------------------------------------------
// Tests

// TestExploreIndexOverRealPostgres is the whole surface over real rows.
//
// It asserts, section by section, which rows the production composition
// resolved out of the canonical schema and where each one landed — including
// the rows that must land nowhere — and then asserts the same thing a second
// time on the RAW BYTES, where a field no section type declares would show
// up.
func TestExploreIndexOverRealPostgres(t *testing.T) {
	ctx := testCtx(t)
	w := newExploreWorld(t, ctx)
	seedExploreFixture(t, ctx, w)

	raw, index := w.mustIndex(t)

	// --- each section carries its own tab ---------------------------------
	for _, section := range []struct{ want, got string }{
		{"projects", index.Projects.Tab},
		{"assets", index.Assets.Tab},
		{"knowledge", index.Knowledge.Tab},
		{"people", index.People.Tab},
		{"organizations", index.Organizations.Tab},
		{"contributions", index.Contributions.Tab},
	} {
		if section.got != section.want {
			t.Errorf("the %s section carries tab %q", section.want, section.got)
		}
	}

	// --- Projects: both public projects, in freshness order, and only
	// those ---------------------------------------------------------------
	projectNames := exploreTitles(index.Projects.Items, func(p exploreWireProject) string { return p.Name })
	assertExploreOrder(t, "Projects", projectNames,
		[]string{exploreSecondProjectName, exploreOpenProjectName})
	assertExploreLacks(t, "Projects", exploreHiddenProjectName, projectNames)
	open := index.Projects.Items[1]
	if open.Slug != exploreOpenProjectSlug || open.Purpose != "open integration research" ||
		open.ActivityStatus != "active" || open.URL != "/projects/"+w.openProjectID {
		t.Errorf("the public project's rendered row lost a field: %+v", open)
	}

	// --- Assets: T0709's rule through this surface's adapter --------------
	// The newest asset is the PRIVATE project's, and it IS listed: its
	// version is public (docs/12 §2 lets a private project publish an asset)
	// and whose it is, is not — so its project is null. The asset with no
	// public version is in no answer at all.
	assetTitles := exploreTitles(index.Assets.Items, func(a exploreWireAsset) string { return a.Title })
	assertExploreOrder(t, "Assets", assetTitles,
		[]string{exploreHiddenAssetTitle, exploreSecondAssetTitle, exploreOpenAssetTitle})
	assertExploreLacks(t, "Assets", exploreDraftAssetTitle, assetTitles)
	for _, a := range index.Assets.Items {
		if a.PID == "" || a.Type != "dataset" || a.LatestVersion == "" || a.LatestURL == "" {
			t.Errorf("the asset row is not fully rendered: %+v", a)
		}
		switch a.Title {
		case exploreHiddenAssetTitle:
			if a.OriginProject != nil {
				t.Errorf("a private project's asset named its project: %+v", a.OriginProject)
			}
		case exploreOpenAssetTitle:
			if a.OriginProject == nil || a.OriginProject.Slug != exploreOpenProjectSlug {
				t.Errorf("a public project's asset lost its project: %+v", a.OriginProject)
			}
		}
	}

	// --- Knowledge: BOTH publications, only the public project named ------
	knowledgeTitles := exploreTitles(index.Knowledge.Items, func(k exploreWireKnowledge) string { return k.Title })
	assertExploreOrder(t, "Knowledge", knowledgeTitles,
		[]string{exploreOpenKnowledgeTitle, exploreHiddenKnowledgeTitle})
	for _, k := range index.Knowledge.Items {
		if k.ID == "" || k.ObjectID == "" || k.ObjectType != "research_question" {
			t.Errorf("the publication row is not fully rendered: %+v", k)
		}
		switch k.Title {
		case exploreOpenKnowledgeTitle:
			if k.Project == nil || k.Project.ID != w.openProjectID {
				t.Errorf("the public project's publication lost its project: %+v", k.Project)
			}
			if k.LifecycleState != "active" || k.PublicVersion != "v1" {
				t.Errorf("the published version's own state did not reach the index: %+v", k)
			}
		case exploreHiddenKnowledgeTitle:
			// The publication is public (docs/12 §2: a private project may
			// publish knowledge), so it is listed — and it travels WITHOUT
			// its project.
			if k.Project != nil {
				t.Errorf("the private project's publication named its project: %+v", k.Project)
			}
			// The published VERSION's own state (docs/43): a superseded
			// version renders as superseded, so a reader never takes stale
			// research for current.
			if k.LifecycleState != "superseded" || k.PublicVersion != "v2" {
				t.Errorf("the published version's own state did not reach the index: %+v", k)
			}
		}
	}

	// --- People: the two active accounts, including the one with no profile
	// row, and not the disabled one ---------------------------------------
	personHandles := exploreTitles(index.People.Items, func(p exploreWirePerson) string { return p.Handle })
	assertExploreOrder(t, "People", personHandles, []string{exploreAliceHandle, exploreBobHandle})
	assertExploreLacks(t, "People", exploreRetiredHandle, personHandles)
	for _, p := range index.People.Items {
		if p.URL != "/users/"+p.ID {
			t.Errorf("a person row does not link to its own profile: %+v", p)
		}
		switch p.Handle {
		case exploreAliceHandle:
			if p.DisplayName != exploreAliceName || p.Bio != exploreAliceBio {
				t.Errorf("the profile bio did not reach the index: %+v", p)
			}
		case exploreBobHandle:
			// A user with no profiles row is still a person of this network
			// (the read LEFT JOINs the profile), rendered with an empty bio
			// rather than dropped.
			if p.DisplayName != exploreBobName || p.Bio != "" {
				t.Errorf("the account without a profile row is not rendered as one: %+v", p)
			}
		}
	}

	// --- Organizations: the two active ones, not the deactivated one ------
	orgNames := exploreTitles(index.Organizations.Items, func(o exploreWireOrganization) string { return o.Name })
	assertExploreOrder(t, "Organizations", orgNames, []string{exploreSecondOrgName, exploreActiveOrgName})
	assertExploreLacks(t, "Organizations", exploreRetiredOrgName, orgNames)
	if index.Organizations.Items[1].Description != exploreActiveOrgDesc {
		t.Errorf("the organization's description did not reach the index: %+v", index.Organizations.Items[1])
	}

	// --- Contributions: publicized AND open, nothing else -----------------
	// The publicized row of the PRIVATE project is in this list and sorts
	// first (it is the newest): what publicizing does is put the opportunity
	// on the open network, and what a private project may not do is be named
	// on it — so the row renders with a nil project, exactly as the asset
	// and knowledge rows do.
	taskTitles := exploreTitles(index.Contributions.Items, func(c exploreWireContribution) string { return c.Title })
	assertExploreOrder(t, "Contributions", taskTitles,
		[]string{exploreHiddenTask, exploreIntermediateTask, exploreAdvancedTask, exploreBeginnerTask})
	for _, unwanted := range []string{exploreClosedTask, exploreInternalTask} {
		assertExploreLacks(t, "Contributions", unwanted, taskTitles)
	}
	for _, c := range index.Contributions.Items {
		switch c.Title {
		case exploreHiddenTask:
			if c.Project != nil {
				t.Errorf("the private project's opportunity named its project: %+v", c.Project)
			}
		default:
			if c.Project == nil || c.Project.ID != w.openProjectID {
				t.Errorf("an open opportunity lost its (public) project: %+v", c)
			}
		}
		if c.ID == "" || c.TargetType != "research_question" || c.Description == "" || c.PublicizedAt == "" {
			t.Errorf("the opportunity row is not fully rendered: %+v", c)
		}
		// Capability tags render in canonical (sorted) order; the fixture
		// stores them unsorted (['python','dft']).
		if len(c.RequiredCapabilities) != 2 ||
			c.RequiredCapabilities[0] != "dft" || c.RequiredCapabilities[1] != "python" {
			t.Errorf("capabilities are not rendered in canonical order: %+v", c.RequiredCapabilities)
		}
	}

	// --- the leak assertions, on the raw bytes ----------------------------
	// Every form of every private identity: the private project (name, slug
	// AND id — all three, because a field that carried one of them would not
	// have to carry the others), the disabled account, the deactivated
	// organization, the two opportunities that are not on the open network,
	// and the asset with no public version.
	//
	// Three rows are deliberately NOT forbidden: the private project's
	// PUBLICATION, its public ASSET VERSION and its publicized OPPORTUNITY.
	// All three are public objects — an explicit publication, a public
	// version and an explicit publicize are what make them so (docs/12 §2/§3)
	// — and forbidding them would be a stricter rule than the platform has.
	// What may not appear is whose they are, which is what this list checks,
	// and it is why the project's name, slug AND id are all three forbidden
	// rather than one: a field carrying any one of them need not carry the
	// others.
	forbidInBody(t, "explore index", raw,
		exploreHiddenProjectSlug, exploreHiddenProjectName, w.hiddenProjectID,
		exploreRetiredHandle, exploreRetiredName,
		exploreRetiredOrgSlug, exploreRetiredOrgName, exploreRetiredOrgDesc,
		exploreInternalTask, exploreClosedTask,
		exploreDraftAssetTitle,
	)
	// ...and the same bytes must carry the public identities, so a leak check
	// that passed because the index was empty fails here instead.
	for _, want := range []string{
		exploreOpenProjectName, exploreOpenProjectSlug,
		exploreSecondProjectName, exploreSecondProjectSlug,
		exploreActiveOrgName, exploreSecondOrgName, exploreActiveOrgDesc,
		exploreAliceName, exploreBobName, exploreAliceBio,
		exploreOpenKnowledgeTitle, exploreHiddenKnowledgeTitle,
		exploreOpenAssetTitle, exploreSecondAssetTitle, exploreHiddenAssetTitle,
		exploreBeginnerTask, exploreAdvancedTask, exploreIntermediateTask,
		// The rows of a private project that were explicitly made public are
		// on the network (that is what an explicit publication, a public
		// version and a publicize DO); only their project is withheld.
		exploreHiddenTask,
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("the index lost %q; whole body = %s", want, leakWindow(raw, 0))
		}
	}
}

// TestExploreIndexOrdersEachSectionByFreshness asserts the ORDER rules on
// the same fixture: each section is newest-first by its own freshness key,
// no section renders a ranking key it should not, and the payload carries no
// popularity input at all.
//
// The acceptance this pins is "无 like-based core ranking", and it has two
// halves. Nothing here is ranked by likes because no like (or view, or vote,
// or star) travels in the answer — asserted on the bytes. And the order that
// IS rendered is freshness: the fixture writes each section's rows in the
// reverse of their freshness order, so a section that echoed the read order
// fails rather than passing by coincidence.
func TestExploreIndexOrdersEachSectionByFreshness(t *testing.T) {
	ctx := testCtx(t)
	w := newExploreWorld(t, ctx)
	seedExploreFixture(t, ctx, w)

	raw, index := w.mustIndex(t)

	// The orders the fixture was written to disprove. Each helper fails when
	// a section holds fewer than two rows, so an order assertion can never
	// pass by having nothing to order.
	assertExploreNewestFirst(t, "Projects", index.Projects.Items,
		func(p exploreWireProject) (string, string) { return p.Name, p.CreatedAt })
	assertExploreNewestFirst(t, "Assets", index.Assets.Items,
		func(a exploreWireAsset) (string, string) { return a.Title, a.LatestPublishedAt })
	assertExploreNewestFirst(t, "Knowledge", index.Knowledge.Items,
		func(k exploreWireKnowledge) (string, string) { return k.Title, k.PublishedAt })
	assertExploreNewestFirst(t, "Contributions", index.Contributions.Items,
		func(c exploreWireContribution) (string, string) { return c.Title, c.PublicizedAt })

	// People and organizations rank on an instant this surface does NOT
	// render (when a person registered, when an organization was created —
	// neither is a published fact about those entities), so their order is
	// asserted by identity instead, on the same reverse-written fixture.
	assertExploreOrder(t, "People",
		exploreTitles(index.People.Items, func(p exploreWirePerson) string { return p.Handle }),
		[]string{exploreAliceHandle, exploreBobHandle})
	assertExploreOrder(t, "Organizations",
		exploreTitles(index.Organizations.Items, func(o exploreWireOrganization) string { return o.Name }),
		[]string{exploreSecondOrgName, exploreActiveOrgName})

	// A section that ranks on a key it does not render must not leak the key
	// into the payload: a reader would then be able to read a registration
	// date the surface decided not to publish. Asserted on the DECODED
	// sections, where an undeclared field would show up as an extra key.
	var sections map[string]struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(raw), &sections); err != nil {
		t.Fatalf("decode sections: %v", err)
	}
	for _, name := range []string{"people", "organizations"} {
		if len(sections[name].Items) == 0 {
			t.Fatalf("the %s section is empty, so this check would pass on anything", name)
		}
		for _, item := range sections[name].Items {
			for _, key := range []string{"created_at", "created", "registered_at", "joined_at"} {
				if _, ok := item[key]; ok {
					t.Errorf("%s renders its hidden ranking key %q: %v", name, key, item)
				}
			}
		}
	}

	// No popularity input exists anywhere in the answer: not as a field, not
	// as a number beside a row. A like-based core ranking would have to carry
	// its input in one of these words, and the payload holds none of them.
	forbidInBody(t, "explore index", raw,
		"like", "likes", "upvote", "downvote", "votes", "star", "stars",
		"popularity", "trending", "view_count", "views", "score",
		`"total"`, `"count"`, "hot_rank",
	)
}

// TestExploreIndexOnAnEmptyDatabaseIsSixEmptySections reads the surface over
// a freshly migrated database that no fixture has touched.
//
// An empty network is not a failure and not six errors: the answer is 200
// with six sections, each present, each carrying its own tab and an EMPTY
// items array. This is the other half of the failure rule — a surface that
// answered 503 for "nothing published yet" would make a young deployment look
// broken, and one that omitted an empty section would leave a client unable
// to tell "no projects" from "no projects key".
func TestExploreIndexOnAnEmptyDatabaseIsSixEmptySections(t *testing.T) {
	ctx := testCtx(t)
	w := newExploreWorld(t, ctx)

	raw, index := w.mustIndex(t)

	for _, section := range []struct {
		want  string
		tab   string
		count int
	}{
		{"projects", index.Projects.Tab, len(index.Projects.Items)},
		{"assets", index.Assets.Tab, len(index.Assets.Items)},
		{"knowledge", index.Knowledge.Tab, len(index.Knowledge.Items)},
		{"people", index.People.Tab, len(index.People.Items)},
		{"organizations", index.Organizations.Tab, len(index.Organizations.Items)},
		{"contributions", index.Contributions.Tab, len(index.Contributions.Items)},
	} {
		if section.tab != section.want {
			t.Errorf("an empty index's %s section carries tab %q", section.want, section.tab)
		}
		if section.count != 0 {
			t.Errorf("an empty database rendered %d %s rows", section.count, section.want)
		}
	}
	// Empty means an empty ARRAY, not null: a client that has to tell [] from
	// null before rendering a list is a client with a branch nothing tests.
	// (Decoded, both are a zero-length slice, so this half has to read the
	// bytes.)
	for _, key := range []string{"projects", "assets", "knowledge", "people", "organizations", "contributions"} {
		if !strings.Contains(raw, `"`+key+`":{"tab":"`+key+`","items":[]}`) {
			t.Errorf("the %s section of an empty index is not an empty array: %s",
				key, leakWindow(raw, strings.Index(raw, `"`+key+`"`)))
		}
	}
}

// TestExploreIndexReadsCannotWrite drives the same route over connections the
// server refuses to write on.
//
// The whole composed surface — the project read, the asset page store, the
// contribution service and this task's own three reads — runs on a pool whose
// connections carry default_transaction_read_only=on, so a SELECT succeeds
// and anything else fails with SQLSTATE 25006. A 200 here is therefore
// evidence that the index issues no write of any kind, rather than a promise
// that it does not; and the control below proves the probe is a real write
// rather than one the server would have refused anyway.
func TestExploreIndexReadsCannotWrite(t *testing.T) {
	ctx := testCtx(t)
	w := newExploreWorld(t, ctx)
	seedExploreFixture(t, ctx, w)

	// The control: the read-only pool really does refuse writes, so the green
	// assertions below are not checks that measured nothing. The UPDATE is
	// the stronger of the two — it names a row this suite owns.
	if _, err := w.readOnly.Exec(ctx,
		`INSERT INTO organizations (slug, name) VALUES ('explore-int-must-fail', 'nope')`); err == nil {
		t.Fatalf("the read-only pool accepted an INSERT, so this test can prove nothing")
	}
	if _, err := w.readOnly.Exec(ctx,
		`UPDATE projects SET name = 'rewritten' WHERE id = $1`, w.openProjectID); err == nil {
		t.Fatalf("the read-only pool accepted an UPDATE, so this test can prove nothing")
	}

	raw, index := w.mustIndex(t)

	// The answer is the whole answer, read on connections that cannot write.
	if len(index.Projects.Items) != 2 || len(index.Contributions.Items) != 4 || len(index.Assets.Items) != 3 {
		t.Fatalf("the read-only pass did not read the same state: %d projects, %d assets, %d contributions",
			len(index.Projects.Items), len(index.Assets.Items), len(index.Contributions.Items))
	}
	forbidInBody(t, "explore index over read-only connections", raw,
		exploreHiddenProjectName, exploreHiddenProjectSlug, w.hiddenProjectID)
}
