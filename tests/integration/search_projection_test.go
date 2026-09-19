// Task T0901 — required test "search projection".
//
// The unit suite (internal/search/projection_test.go) pins the decision
// surface without a database: the rule table, the identity shapes, and the
// fail-closed default of the visibility rule. This file pins what only a
// real PostgreSQL — and, in two cases, a real worker PROCESS — can settle:
//
//  1. THE PROJECTION REALLY LANDS ROWS. Each of the four mapped event types
//     is recorded into the outbox, published by the real dispatcher and
//     consumed by the real projector, and the assertions read
//     search_documents BY entity_ref — the table, never the write function's
//     return value — comparing title, content and structured against the
//     SOURCE rows the fixture wrote.
//
//  2. A PRIVATE ENTITY PROJECTS TO private, from either axis: a private
//     version in a public project, and a public version in a private
//     project. Both are then asserted against the canonical read query with
//     an empty project scope, together with the discriminating half — a
//     scope that NAMES the private project does return its row, so "no leak"
//     is not satisfied by returning nothing.
//
//  3. PROJECTION IS IDEMPOTENT through the canonical write path. The same
//     event is projected twice (the consumer's cursor is cleared in
//     between, which is exactly what a crash between the upsert and the
//     cursor write replays), and the second pass UPDATES the existing row
//     rather than adding one — proved by changing the SOURCE between the two
//     passes and reading the new value back out of the table.
//
//  4. REBUILD IS IDEMPOTENT, and the test drives the COMMAND, not the
//     function: ./cmd/worker is built and run with -search-rebuild twice,
//     and the derived columns of every row are compared across the two runs.
//     The same pair of runs proves the rebuild removes a row whose source
//     does not exist — the repair the incremental path cannot make.
//
//  5. THE CONSUMER IS REALLY MOUNTED.
//     TestSearchProjectionWorkerConsumesEvents starts the real worker
//     binary, against the real database and a real Redis-protocol server,
//     records an event from OUTSIDE the process and waits for the worker's
//     own goroutines to publish and project it. Nothing there calls RunOnce:
//     if the projector were not mounted in cmd/worker, no row would appear.
//     The same test drives the search.rebuild job type through the queue.
//
//  7. THE KNOWLEDGE SUBSCRIPTION LINE IS CONNECTED, end to end and over the
//     real producer and the real worker: publications are published through
//     the real HTTP command (so the events and their payloads are the
//     producer's, not the test's), alice follows one BY PID, and the three
//     axes knowledgepublish.AudienceFor decides with are each exercised —
//     the version's own visibility policy, the owning project's visibility,
//     and the rights document's metadata token — together with the
//     counter-proof that an event naming an unresolvable PID delivers to
//     nobody. 光有正面例不算过.

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/lichman0405/post/internal/application/subscriptions"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/persistence/sqlc"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rights"
	"github.com/lichman0405/post/internal/search"
)

// searchProjectionTaskID namespaces this task's test databases
// (test_T0901_<run_id>).
const searchProjectionTaskID = "T0901"

// --------------------------------------------------------------------------
// Fixture

// searchProjectionFixture is one real database with the production
// projection pipeline wired over it: the outbox dispatcher (driven the way
// cmd/worker drives it) and the projector. It holds a public project and a
// private one, and the two accounts the leak cases need.
type searchProjectionFixture struct {
	ctx        context.Context
	pool       *pgxpool.Pool
	url        string
	q          *sqlc.Queries
	dispatcher *events.Dispatcher
	projector  *search.Projector

	aliceID, bobID string
	orgID          string

	publicProject  string
	privateProject string
}

func newSearchProjectionFixture(t *testing.T, ctx context.Context) *searchProjectionFixture {
	t.Helper()
	pool, dbURL := testdb.Setup(t, ctx, adminURL(t), searchProjectionTaskID)
	f := &searchProjectionFixture{
		ctx:        ctx,
		pool:       pool,
		url:        dbURL,
		q:          sqlc.New(pool),
		dispatcher: events.NewDispatcher(pool, events.WithLogger(silentLogger())),
		projector:  search.NewProjector(pool, search.WithLogger(silentLogger())),
	}
	f.aliceID = mustQueryUUID(t, ctx, pool,
		`INSERT INTO users (handle, display_name) VALUES ('sp-alice','Alice') RETURNING id`)
	f.bobID = mustQueryUUID(t, ctx, pool,
		`INSERT INTO users (handle, display_name) VALUES ('sp-bob','Bob') RETURNING id`)
	f.orgID = mustQueryUUID(t, ctx, pool,
		`INSERT INTO organizations (slug, name) VALUES ('sp-lab','Search Projection Lab') RETURNING id`)
	f.publicProject = f.seedProject(t, "sp-public", "public", f.aliceID)
	f.privateProject = f.seedProject(t, "sp-private", "private", f.aliceID)
	return f
}

func (f *searchProjectionFixture) seedProject(t *testing.T, slug, visibility string, members ...string) string {
	t.Helper()
	id := mustQueryUUID(t, f.ctx, f.pool,
		`INSERT INTO projects (organization_id, slug, name, purpose, visibility, created_by)
		 VALUES ($1, $2, $2, 'T0901 search projection fixture', $3, $4) RETURNING id`,
		f.orgID, slug, visibility, f.aliceID)
	for _, m := range members {
		if _, err := f.pool.Exec(f.ctx,
			`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, 'owner')`,
			id, m); err != nil {
			t.Fatalf("seed membership on %s: %v", slug, err)
		}
	}
	return id
}

// seedState inserts one committed state: the branch, the state itself and
// the state_commits row that produced it — the three rows the state source
// joins, and the three a state.committed event names.
func (f *searchProjectionFixture) seedState(t *testing.T, projectID, branchName, branchVisibility, message string) string {
	t.Helper()
	branchID := mustQueryUUID(t, f.ctx, f.pool,
		`INSERT INTO branches (project_id, name, visibility, git_ref, created_by)
		 VALUES ($1, $2, $3, 'refs/heads/' || $2, $4) RETURNING id`,
		projectID, branchName, branchVisibility, f.aliceID)
	stateID := mustQueryUUID(t, f.ctx, f.pool,
		`INSERT INTO project_states (project_id, branch_id, state_hash, git_commit_sha, manifest_version)
		 VALUES ($1, $2, 'h-' || gen_random_uuid()::text, 'sha-' || gen_random_uuid()::text, '1')
		 RETURNING id`, projectID, branchID)
	mustQueryUUID(t, f.ctx, f.pool,
		`INSERT INTO state_commits (project_id, branch_id, result_state_id, actor_id, via, message, operation_summary)
		 VALUES ($1, $2, $3, $4, 'api', $5, '{}'::jsonb) RETURNING id`,
		projectID, branchID, stateID, f.aliceID, message)
	return stateID
}

// seedRelease inserts one release on stateID and returns its row id.
func (f *searchProjectionFixture) seedRelease(t *testing.T, projectID, stateID, version, title, manifestHash string) string {
	t.Helper()
	return mustQueryUUID(t, f.ctx, f.pool,
		`INSERT INTO releases (project_id, version, title, state_id, manifest, manifest_hash, created_by)
		 VALUES ($1, $2, $3, $4, '{}'::jsonb, $5, $6) RETURNING id`,
		projectID, version, title, stateID, manifestHash, f.aliceID)
}

// seedAsset inserts one research asset with one published version and
// returns the asset's pid, which is the identity the event carries.
func (f *searchProjectionFixture) seedAsset(t *testing.T, projectID, pid, slug, title, version, versionVisibility string) string {
	t.Helper()
	assetID := mustQueryUUID(t, f.ctx, f.pool,
		`INSERT INTO research_assets (asset_type, slug, title, origin_project_id, pid)
		 VALUES ('dataset', $1, $2, $3, $4) RETURNING id`, slug, title, projectID, pid)
	mustQueryUUID(t, f.ctx, f.pool,
		`INSERT INTO research_asset_versions
		   (asset_id, version, manifest, rights_json, visibility, integrity_hash, published_by, origin_refs)
		 VALUES ($1, $2, '{}'::jsonb, $3::jsonb, $4, 'sha256:seed', $5, ARRAY['project:seed']) RETURNING id`,
		assetID, version, seedRightsJSON(t), versionVisibility, f.aliceID)
	return pid
}

// seedPublication inserts one published knowledge object — the object, the
// version, the state it sits on and the publication row — and returns the
// publication's pid. A non-nil policyID pins the version's own visibility
// axis (the first of AudienceFor's three axes).
func (f *searchProjectionFixture) seedPublication(t *testing.T, projectID, pid, title string, policyID *string) string {
	t.Helper()
	objectID := mustQueryUUID(t, f.ctx, f.pool,
		`INSERT INTO scientific_objects (project_id, object_type, created_by)
		 VALUES ($1, 'research_question', $2) RETURNING id`, projectID, f.aliceID)
	// Its own branch: branches is UNIQUE(project_id, name), and this fixture
	// shares a database with the explicitly seeded states.
	stateID := f.seedState(t, projectID, "publish-"+pid, "public", "publish "+title)
	versionID := mustQueryUUID(t, f.ctx, f.pool,
		`INSERT INTO scientific_object_versions
		   (object_id, version_no, state_id, schema_id, schema_version, title, lifecycle_state,
		    payload, visibility_policy_id, integrity_hash, created_by)
		 VALUES ($1, 1, $2, 'research_question', '1', $3, 'active', '{}'::jsonb, $4, 'sha256:seed', $5)
		 RETURNING id`, objectID, stateID, title, policyID, f.aliceID)
	return mustQueryUUID(t, f.ctx, f.pool,
		`INSERT INTO knowledge_publications (object_version_id, public_version, rights_json, published_by, pid)
		 VALUES ($1, 'v1', $2::jsonb, $3, $4) RETURNING id`,
		versionID, seedRightsJSON(t), f.aliceID, pid)
}

// seedRightsJSON is a rights declaration carrying the rights model's own
// fail-closed default (internal/rights: MetadataProjectPolicy), which is the
// value AudienceFor resolves as "whatever the owning project's policy says".
func seedRightsJSON(t *testing.T) string {
	t.Helper()
	raw, err := rights.New().Marshal()
	if err != nil {
		t.Fatalf("rights.New().Marshal: %v", err)
	}
	return string(raw)
}

// record writes one event into the outbox. It does NOT publish it: the
// caller decides whether the test's own dispatcher or a real worker process
// does that.
func (f *searchProjectionFixture) record(t *testing.T, eventType, visibility, projectID string, payload map[string]any) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("render payload: %v", err)
	}
	if err := events.Record(f.ctx, f.pool, events.Event{
		EventType:     eventType,
		ActorID:       f.aliceID,
		ProjectID:     projectID,
		Visibility:    visibility,
		CorrelationID: "T0901-" + eventType,
		Payload:       body,
	}); err != nil {
		t.Fatalf("record %s: %v", eventType, err)
	}
}

// announce records one event and publishes it with the test's dispatcher —
// the producer half of the pipeline, run exactly as cmd/api and cmd/worker
// run it.
func (f *searchProjectionFixture) announce(t *testing.T, eventType, visibility, projectID string, payload map[string]any) {
	t.Helper()
	f.record(t, eventType, visibility, projectID, payload)
	f.dispatch(t)
}

func (f *searchProjectionFixture) dispatch(t *testing.T) {
	t.Helper()
	if _, err := f.dispatcher.RunOnce(f.ctx); err != nil {
		t.Fatalf("dispatcher RunOnce: %v", err)
	}
}

// projectOnce runs one projection pass through the production consumer.
func (f *searchProjectionFixture) projectOnce(t *testing.T) search.PassReport {
	t.Helper()
	report, err := f.projector.RunOnce(f.ctx)
	if err != nil {
		t.Fatalf("projector RunOnce: %v", err)
	}
	return report
}

// --------------------------------------------------------------------------
// Reading the projection

// projectedRow is one search_documents row, read with raw SQL rather than
// through any code under test.
type projectedRow struct {
	EntityRef  string
	EntityType string
	Visibility string
	ProjectID  *string
	Title      string
	Content    string
	Structured map[string]string
	Embedding  *string
	UpdatedAt  time.Time
}

const projectedColumns = `entity_ref, entity_type, visibility, project_id::text,
	title, content, structured, embedding::text, updated_at`

func scanProjectedRows(t *testing.T, rows pgx.Rows) []projectedRow {
	t.Helper()
	defer rows.Close()
	var out []projectedRow
	for rows.Next() {
		var (
			r          projectedRow
			structured []byte
		)
		if err := rows.Scan(&r.EntityRef, &r.EntityType, &r.Visibility, &r.ProjectID, &r.Title,
			&r.Content, &structured, &r.Embedding, &r.UpdatedAt); err != nil {
			t.Fatalf("scan search_documents: %v", err)
		}
		if err := json.Unmarshal(structured, &r.Structured); err != nil {
			t.Fatalf("decode structured for %s: %v (%s)", r.EntityRef, err, structured)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate search_documents: %v", err)
	}
	return out
}

// docs reads every projected row matching where (with args), ordered by
// entity_ref so a comparison across runs is stable.
func (f *searchProjectionFixture) docs(t *testing.T, where string, args ...any) []projectedRow {
	t.Helper()
	rows, err := f.pool.Query(f.ctx,
		`SELECT `+projectedColumns+` FROM search_documents `+where+` ORDER BY entity_ref`, args...)
	if err != nil {
		t.Fatalf("read search_documents: %v", err)
	}
	return scanProjectedRows(t, rows)
}

// doc reads one row by entity_ref, or nil when the projection holds none.
func (f *searchProjectionFixture) doc(t *testing.T, entityRef string) *projectedRow {
	t.Helper()
	rows := f.docs(t, `WHERE entity_ref = $1`, entityRef)
	if len(rows) == 0 {
		return nil
	}
	return &rows[0]
}

// documentProbe reads one projected row for a caller that has a pool but no
// fixture (the knowledge case composes its own world).
type documentProbe struct {
	ctx  context.Context
	pool *pgxpool.Pool
}

func probeDocuments(ctx context.Context, pool *pgxpool.Pool) documentProbe {
	return documentProbe{ctx: ctx, pool: pool}
}

func (p documentProbe) doc(t *testing.T, entityRef string) *projectedRow {
	t.Helper()
	rows, err := p.pool.Query(p.ctx,
		`SELECT `+projectedColumns+` FROM search_documents WHERE entity_ref = $1`, entityRef)
	if err != nil {
		t.Fatalf("read search_documents: %v", err)
	}
	out := scanProjectedRows(t, rows)
	if len(out) == 0 {
		return nil
	}
	return &out[0]
}

// --------------------------------------------------------------------------
// 1. The projection lands rows, and they say what their source says

func TestSearchProjectionLandsDocumentsForEveryMappedEvent(t *testing.T) {
	ctx := testCtx(t)
	f := newSearchProjectionFixture(t, ctx)

	stateID := f.seedState(t, f.publicProject, "main", "public", "Accept the adsorption run\n\nwith details")
	releaseID := f.seedRelease(t, f.publicProject, stateID, "v1.0.0", "First public release", "sha256:release")
	assetPID := f.seedAsset(t, f.publicProject, "01j9z6k3m4n5p6q7r8s9t0v1a1",
		"sp-public-asset", "Adsorption dataset", "v3", "public")
	knowledgePID := "01j9z6k3m4n5p6q7r8s9t0v1b1"
	f.seedPublication(t, f.publicProject, knowledgePID, "Published knowledge object", nil)

	f.announce(t, search.EventTypeStateCommitted, "public", f.publicProject, map[string]any{
		"project_id": f.publicProject, "state_id": stateID, "message": "Accept the adsorption run", "via": "api",
	})
	f.announce(t, search.EventTypeReleasePublished, "public", f.publicProject, map[string]any{
		"release_id": releaseID, "version": "v1.0.0", "state_id": stateID, "manifest_hash": "sha256:release",
	})
	f.announce(t, search.EventTypeAssetVersionPublished, "public", f.publicProject, map[string]any{
		"asset_id": assetPID, "asset_version_id": "", "version": "v3",
	})
	f.announce(t, search.EventTypeKnowledgeVersionPublished, "public", f.publicProject, map[string]any{
		"publication_id": knowledgePID, "object_version_id": "", "public_version": "v1", "project_id": f.publicProject,
	})

	report := f.projectOnce(t)
	if report.Candidates != 4 || report.Projected != 4 {
		t.Fatalf("pass report = %+v, want 4 candidates and 4 projected documents", report)
	}
	if len(report.Unmapped) != 0 || len(report.Unaddressed) != 0 || len(report.Void) != 0 || report.Failed != 0 {
		t.Fatalf("the pass carries uncovered facts: %+v", report)
	}

	// Every assertion below reads the TABLE by entity_ref. Nothing here
	// consults the write function's return value.
	cases := []struct {
		entityRef      string
		entityType     string
		visibility     string
		title          string
		contentHas     []string
		structuredWant map[string]string
	}{
		{
			entityRef: "state:" + stateID, entityType: search.EntityState, visibility: "public",
			title:      "[main] Accept the adsorption run",
			contentHas: []string{"Accept the adsorption run", "with details", "main"},
			structuredWant: map[string]string{
				"state_id": stateID, "branch": "main",
			},
		},
		{
			entityRef: "release:" + releaseID, entityType: search.EntityRelease, visibility: "public",
			title:      "First public release",
			contentHas: []string{"First public release", "v1.0.0"},
			structuredWant: map[string]string{
				"release_id": releaseID, "version": "v1.0.0", "manifest_hash": "sha256:release",
			},
		},
		{
			entityRef: "asset:" + assetPID, entityType: search.EntityAsset, visibility: "public",
			title:      "Adsorption dataset",
			contentHas: []string{"Adsorption dataset", "sp-public-asset", "dataset"},
			structuredWant: map[string]string{
				"pid": assetPID, "slug": "sp-public-asset", "asset_type": "dataset", "version": "v3",
			},
		},
		{
			entityRef: "knowledge:" + knowledgePID, entityType: search.EntityKnowledge, visibility: "public",
			title:      "Published knowledge object",
			contentHas: []string{"Published knowledge object", "research_question", "v1"},
			structuredWant: map[string]string{
				"pid": knowledgePID, "object_type": "research_question",
				"public_version": "v1", "lifecycle_state": "active",
			},
		},
	}
	for _, tc := range cases {
		row := f.doc(t, tc.entityRef)
		if row == nil {
			t.Errorf("%s: no projected row in search_documents (the table is the acceptance, not the write call)", tc.entityRef)
			continue
		}
		if row.EntityType != tc.entityType {
			t.Errorf("%s: entity_type = %q, want %q", tc.entityRef, row.EntityType, tc.entityType)
		}
		if row.Visibility != tc.visibility {
			t.Errorf("%s: visibility = %q, want %q", tc.entityRef, row.Visibility, tc.visibility)
		}
		if row.ProjectID == nil || *row.ProjectID != f.publicProject {
			t.Errorf("%s: project_id = %v, want %s", tc.entityRef, row.ProjectID, f.publicProject)
		}
		if row.Title != tc.title {
			t.Errorf("%s: title = %q, want %q", tc.entityRef, row.Title, tc.title)
		}
		for _, want := range tc.contentHas {
			if !strings.Contains(row.Content, want) {
				t.Errorf("%s: content %q does not carry %q", tc.entityRef, row.Content, want)
			}
		}
		for key, want := range tc.structuredWant {
			if got := row.Structured[key]; got != want {
				t.Errorf("%s: structured[%q] = %q, want %q (full: %v)",
					tc.entityRef, key, got, want, row.Structured)
			}
		}
		if row.Structured["entity_type"] != tc.entityType || row.Structured["project_id"] != f.publicProject {
			t.Errorf("%s: structured identity facets = %v", tc.entityRef, row.Structured)
		}
		// T0902 owns the embedding column; this round must leave it empty
		// rather than fill it with a vector no provider produced.
		if row.Embedding != nil {
			t.Errorf("%s: embedding is not NULL (%s); vector retrieval is T0902's surface",
				tc.entityRef, *row.Embedding)
		}
	}

	// The read half: the rows the projection wrote are rows the canonical
	// query returns. That is what makes them SEARCHABLE rather than merely
	// present.
	rows, err := f.q.SearchDocuments(ctx, sqlc.SearchDocumentsParams{
		Query: "adsorption", AllowedProjectIds: nil, PageSize: 50, PageOffset: 0,
	})
	if err != nil {
		t.Fatalf("SearchDocuments: %v", err)
	}
	found := map[string]bool{}
	for _, r := range rows {
		found[r.EntityRef] = true
	}
	if !found["state:"+stateID] || !found["asset:"+assetPID] {
		t.Errorf("the canonical query did not return the projected public rows: %v", found)
	}
}

// --------------------------------------------------------------------------
// 2. The leak case

func TestSearchProjectionKeepsPrivateContentPrivate(t *testing.T) {
	ctx := testCtx(t)
	f := newSearchProjectionFixture(t, ctx)

	// The two ways a row could wrongly say public: a PRIVATE version in a
	// PUBLIC project, and a PUBLIC version in a PRIVATE project.
	privateVersionPID := f.seedAsset(t, f.publicProject, "01j9z6k3m4n5p6q7r8s9t0v1c1",
		"sp-private-version", "Members-only dataset", "v1", "private")
	privateProjectPID := f.seedAsset(t, f.privateProject, "01j9z6k3m4n5p6q7r8s9t0v1c2",
		"sp-private-project", "Private project dataset", "v1", "public")

	f.announce(t, search.EventTypeAssetVersionPublished, "private", f.publicProject,
		map[string]any{"asset_id": privateVersionPID, "version": "v1"})
	f.announce(t, search.EventTypeAssetVersionPublished, "private", f.privateProject,
		map[string]any{"asset_id": privateProjectPID, "version": "v1"})
	f.projectOnce(t)

	for _, tc := range []struct{ ref, why string }{
		{"asset:" + privateVersionPID, "a private version in a public project"},
		{"asset:" + privateProjectPID, "a public version in a private project"},
	} {
		row := f.doc(t, tc.ref)
		if row == nil {
			t.Fatalf("%s (%s) was not projected at all", tc.ref, tc.why)
		}
		if row.Visibility != "private" {
			t.Errorf("%s (%s): visibility = %q, want private", tc.ref, tc.why, row.Visibility)
		}
		// The row exists and is scoped to its project: the protection is the
		// visibility value plus the read filter, not deleting the row.
		if row.ProjectID == nil {
			t.Errorf("%s: project_id is NULL, so the read query can never scope it", tc.ref)
		}
	}

	// The canonical read refuses both to a caller with no project scope.
	// search_access_test.go owns this property; what this asserts is that it
	// holds for rows the PROJECTION wrote rather than rows the test inserted.
	rows, err := f.q.SearchDocuments(ctx, sqlc.SearchDocumentsParams{
		Query: "dataset", AllowedProjectIds: nil, PageSize: 50, PageOffset: 0,
	})
	if err != nil {
		t.Fatalf("SearchDocuments: %v", err)
	}
	for _, r := range rows {
		if r.EntityRef == "asset:"+privateVersionPID || r.EntityRef == "asset:"+privateProjectPID {
			t.Errorf("empty-scope search returned the private row %s (visibility %s)", r.EntityRef, r.Visibility)
		}
	}

	// The discriminating half: a scope that NAMES the private project does
	// return its row, so "no leak" cannot be satisfied by returning nothing.
	scoped, err := f.q.SearchDocuments(ctx, sqlc.SearchDocumentsParams{
		Query:             "dataset",
		AllowedProjectIds: []pgtype.UUID{parseUUIDOrDie(f.privateProject)},
		PageSize:          50,
		PageOffset:        0,
	})
	if err != nil {
		t.Fatalf("SearchDocuments (scoped): %v", err)
	}
	var scopedHit bool
	for _, r := range scoped {
		if r.EntityRef == "asset:"+privateProjectPID {
			scopedHit = true
		}
	}
	if !scopedHit {
		t.Errorf("a scope naming the private project did not return its projected row: %+v", scoped)
	}
}

// --------------------------------------------------------------------------
// 3. Idempotence

func TestSearchProjectionIsIdempotent(t *testing.T) {
	ctx := testCtx(t)
	f := newSearchProjectionFixture(t, ctx)

	assetPID := f.seedAsset(t, f.publicProject, "01j9z6k3m4n5p6q7r8s9t0v1d1",
		"sp-idempotent", "Idempotent dataset", "v1", "public")
	f.announce(t, search.EventTypeAssetVersionPublished, "public", f.publicProject,
		map[string]any{"asset_id": assetPID, "version": "v1"})

	first := f.projectOnce(t)
	if first.Projected != 1 {
		t.Fatalf("first pass = %+v, want one projected document", first)
	}
	before := f.doc(t, "asset:"+assetPID)
	if before == nil {
		t.Fatal("the first pass wrote no row")
	}

	// The replay a crash between the upsert and the cursor write produces:
	// the cursor row is gone, the document is not, and the SAME event is
	// claimed and projected a second time.
	f.replay(t)
	second := f.projectOnce(t)
	if second.Candidates != 1 {
		t.Fatalf("the replay claimed %d rows, want the same one event", second.Candidates)
	}
	after := f.doc(t, "asset:"+assetPID)
	if after == nil {
		t.Fatal("the replay removed the row")
	}
	if n := len(f.docs(t, `WHERE entity_ref = $1`, "asset:"+assetPID)); n != 1 {
		t.Fatalf("the replay wrote %d rows for one entity, want 1", n)
	}
	if after.Title != before.Title || after.Content != before.Content ||
		after.Visibility != before.Visibility || !sameFacets(after.Structured, before.Structured) {
		t.Fatalf("the replay changed the document:\nbefore %+v\nafter  %+v", before, after)
	}
	if after.UpdatedAt.Before(before.UpdatedAt) {
		t.Errorf("updated_at went backwards: %s -> %s", before.UpdatedAt, after.UpdatedAt)
	}

	// The discriminating half: the canonical write really UPDATES. An
	// insert-once guard would keep the stale content and a second write path
	// would add a row; ON CONFLICT DO UPDATE must do neither, so a SOURCE
	// change between two projections of the same event has to land.
	newTitle := "Idempotent dataset (corrected)"
	if _, err := f.pool.Exec(ctx,
		`UPDATE research_assets SET title = $1 WHERE pid = $2`, newTitle, assetPID); err != nil {
		t.Fatalf("change the source: %v", err)
	}
	f.replay(t)
	f.projectOnce(t)
	updated := f.doc(t, "asset:"+assetPID)
	if updated == nil {
		t.Fatal("the row vanished after the source changed")
	}
	if updated.Title != newTitle {
		t.Errorf("title = %q, want %q — the second projection of one event must update the row",
			updated.Title, newTitle)
	}
	if n := len(f.docs(t, `WHERE entity_ref = $1`, "asset:"+assetPID)); n != 1 {
		t.Errorf("the update path left %d rows for one entity, want 1", n)
	}
}

// replay clears the consumer's cursor for every event of the fixture's asset
// publishes, which is the state a crash between the upsert and the cursor
// write leaves behind.
func (f *searchProjectionFixture) replay(t *testing.T) {
	t.Helper()
	if _, err := f.pool.Exec(f.ctx,
		`DELETE FROM search_projected_events WHERE outbox_event_id IN
		 (SELECT id FROM outbox_events WHERE event_type = $1)`,
		search.EventTypeAssetVersionPublished); err != nil {
		t.Fatalf("clear the consumer cursor: %v", err)
	}
}

// --------------------------------------------------------------------------
// 4. The rebuild, driven through the command

func TestSearchProjectionRebuildIsIdempotentThroughTheCommand(t *testing.T) {
	ctx := testCtx(t)
	f := newSearchProjectionFixture(t, ctx)

	stateID := f.seedState(t, f.publicProject, "main", "public", "Rebuildable state")
	assetPID := f.seedAsset(t, f.publicProject, "01j9z6k3m4n5p6q7r8s9t0v1e1",
		"sp-rebuild", "Rebuildable dataset", "v1", "public")
	releaseID := f.seedRelease(t, f.publicProject, stateID, "v9.9.9", "Rebuildable release", "sha256:r")
	f.announce(t, search.EventTypeAssetVersionPublished, "public", f.publicProject,
		map[string]any{"asset_id": assetPID, "version": "v1"})
	f.projectOnce(t)

	// A row whose source does not exist: an index entry nothing derives any
	// more. Only a rebuild removes it — the incremental path has no event to
	// consume for a deletion. It is written with raw SQL because the code
	// under test must never be the thing that plants it.
	if _, err := f.pool.Exec(ctx, `INSERT INTO search_documents
		(entity_ref, entity_type, visibility, project_id, title, content, structured)
		VALUES ('asset:01j9z6k3m4n5p6q7r8s9t0v1f9','asset','public',$1,'Ghost','Ghost','{}'::jsonb)`,
		f.publicProject); err != nil {
		t.Fatalf("plant a stale row: %v", err)
	}

	binary := buildPostWorker(t)

	first := runSearchRebuild(t, binary, f.url)
	rowsAfterFirst := f.docs(t, `WHERE true`)

	if f.doc(t, "asset:01j9z6k3m4n5p6q7r8s9t0v1f9") != nil {
		t.Error("the rebuild kept a row whose source does not exist")
	}
	// The rebuild re-derives every projection from its sources, so the
	// entities that do exist are all present — including the release and the
	// state the incremental path never indexed.
	for _, ref := range []string{"asset:" + assetPID, "state:" + stateID, "release:" + releaseID} {
		if f.doc(t, ref) == nil {
			t.Errorf("the rebuild did not derive %s", ref)
		}
	}
	if first.Projected < 3 {
		t.Errorf("the first rebuild projected %d rows, want at least the three seeded entities", first.Projected)
	}
	if len(rowsAfterFirst) != first.Projected {
		t.Errorf("the report says %d rows but the table holds %d", first.Projected, len(rowsAfterFirst))
	}
	if first.Removed < 2 {
		t.Errorf("the rebuild removed %d rows, want the projected asset plus the planted ghost", first.Removed)
	}

	second := runSearchRebuild(t, binary, f.url)
	rowsAfterSecond := f.docs(t, `WHERE true`)

	// Idempotence is a property of the DERIVED state: the second run must
	// re-derive exactly the same rows. `removed` differs by construction —
	// the first run started from one row, the second from a full index — so
	// what is compared is what each run projected and what the table then
	// holds.
	if first.Projected != second.Projected {
		t.Errorf("the two rebuilds projected different counts: %d then %d", first.Projected, second.Projected)
	}
	if first.Types != second.Types {
		t.Errorf("the two rebuilds projected different entity types:\n%s\n%s", first.Types, second.Types)
	}
	if len(rowsAfterFirst) != len(rowsAfterSecond) {
		t.Fatalf("row count changed across two rebuilds: %d -> %d", len(rowsAfterFirst), len(rowsAfterSecond))
	}
	if len(rowsAfterSecond) == 0 {
		t.Fatal("the rebuild produced no rows at all")
	}
	for i := range rowsAfterFirst {
		a, b := rowsAfterFirst[i], rowsAfterSecond[i]
		if a.EntityRef != b.EntityRef || a.EntityType != b.EntityType || a.Visibility != b.Visibility ||
			a.Title != b.Title || a.Content != b.Content || !sameFacets(a.Structured, b.Structured) {
			t.Errorf("row %s changed across two rebuilds:\n%+v\n%+v", a.EntityRef, a, b)
		}
		// T0902 owns the embedding; a rebuild must not invent one, and must
		// not silently carry a stale one either (Rebuild's doc comment).
		if b.Embedding != nil {
			t.Errorf("row %s carries an embedding (%s) after a rebuild", b.EntityRef, *b.Embedding)
		}
	}
}

// --------------------------------------------------------------------------
// 5. The consumer is really mounted in the worker

// TestSearchProjectionWorkerConsumesEvents starts the REAL worker binary —
// the one cmd/worker builds, with every consumer main.go mounts — against
// the test database and a real Redis-protocol server, then records an event
// from OUTSIDE the process and waits for the worker's own dispatcher and
// projector to consume it. Nothing here calls RunOnce: if the projector were
// not mounted in cmd/worker, no row would ever appear.
func TestSearchProjectionWorkerConsumesEvents(t *testing.T) {
	ctx := testCtx(t)
	f := newSearchProjectionFixture(t, ctx)

	assetPID := f.seedAsset(t, f.publicProject, "01j9z6k3m4n5p6q7r8s9t0v1g1",
		"sp-worker", "Projected by the worker", "v2", "public")

	redisServer := startMiniRedis(t)
	worker := startPostWorker(t, buildPostWorker(t), f.url, redisServer.Addr())

	// Recorded, deliberately NOT dispatched: publishing it is the worker's
	// job, so both halves of the pipeline run inside the worker process.
	f.record(t, search.EventTypeAssetVersionPublished, "public", f.publicProject,
		map[string]any{"asset_id": assetPID, "version": "v2"})

	waitFor(t, 45*time.Second, func() string {
		row := f.doc(t, "asset:"+assetPID)
		if row == nil {
			return "no search_documents row for asset:" + assetPID
		}
		if row.Title != "Projected by the worker" {
			return fmt.Sprintf("the worker projected title %q, want the source title", row.Title)
		}
		if row.Visibility != "public" {
			return fmt.Sprintf("the worker projected visibility %q, want public", row.Visibility)
		}
		return ""
	}, func() string { return worker.output() })

	// The second mount point, through the real queue: enqueue a
	// search.rebuild job and wait for the loop to complete it. Deleting the
	// derived row first makes the completion observable in the DATABASE
	// rather than only in the loop's bookkeeping.
	if _, err := f.pool.Exec(ctx, `DELETE FROM search_documents WHERE entity_ref = $1`, "asset:"+assetPID); err != nil {
		t.Fatalf("delete the projected row: %v", err)
	}
	client := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	jobID := "T0901-search-rebuild-1"
	job, err := json.Marshal(map[string]any{
		"id": jobID, "type": "search.rebuild", "correlation_id": "T0901-rebuild",
	})
	if err != nil {
		t.Fatalf("render the job: %v", err)
	}
	if err := client.RPush(ctx, "post:queue:jobs", job).Err(); err != nil {
		t.Fatalf("enqueue the rebuild job: %v", err)
	}
	waitFor(t, 45*time.Second, func() string {
		seen, err := client.Exists(ctx, "post:worker:seen:"+jobID).Result()
		if err != nil {
			t.Fatalf("read the completion mark: %v", err)
		}
		if seen == 0 {
			return "the search.rebuild job has not completed"
		}
		return ""
	}, func() string { return worker.output() })
	if row := f.doc(t, "asset:"+assetPID); row == nil {
		t.Error("the rebuild job completed but the row it should have re-derived is missing")
	}
}

// --------------------------------------------------------------------------
// 7. The knowledge subscription line, end to end

// TestSearchProjectionKnowledgeSubscriptionEndToEnd publishes knowledge
// objects through the real HTTP command, follows one BY PID, and lets the
// REAL worker turn the producer's events into subscriber deliveries.
//
// Each of the three axes knowledgepublish.AudienceFor decides with is
// exercised, because each of them alone is enough to make a publication
// members-only, and a rule that read only one of them would leak:
//
//   - the version pins a visibility_policy_id (in a public project),
//   - the owning project's visibility (a public project turned private),
//   - the rights declaration's metadata token (a non-default one).
//
// The counter-proof closes it: an event naming a well-formed PID that
// resolves to no publication must deliver to NOBODY. Without that, "a
// delivery appeared" would be evidence that the fan-out delivers, not that
// it resolves.
func TestSearchProjectionKnowledgeSubscriptionEndToEnd(t *testing.T) {
	ctx := testCtx(t)
	w := newKnowledgeWorld(t, ctx)
	w.signups(t)
	// alice created the project through the API, so she is its owner (the
	// project service writes that membership itself); bob is a signed-in
	// stranger to it.
	project := w.newProject(t, ctx, "kp-t0901", "public")

	store := events.NewSubscriptionStore(w.pool)
	svc := subscriptions.NewService(store)
	probe := probeDocuments(ctx, w.pool)

	worker := startPostWorker(t, buildPostWorker(t), databaseURLOf(t, ctx, w.pool), startMiniRedis(t).Addr())

	statuses := func(t *testing.T, userID string) map[string]int {
		t.Helper()
		rows, err := store.Deliveries(ctx, w.pool, userID, "", 200)
		if err != nil {
			t.Fatalf("read deliveries: %v", err)
		}
		out := map[string]int{}
		for _, d := range rows {
			out[d.Channel+"/"+d.Status]++
		}
		return out
	}
	// A notification is a fact about one EVENT, not about a subscription:
	// a subscriber added after an event was consumed is owed nothing for
	// it. The counts below are therefore made deterministic rather than
	// hoped for — each step first lets the pipeline drain the events that
	// already exist (waitDrained), then changes the subscriptions, and only
	// then announces a FRESH event, which is the one the subscription is
	// expected to receive.
	waitDeliveries := func(t *testing.T, userID string, wantWeb int) {
		t.Helper()
		waitFor(t, 45*time.Second, func() string {
			if got := statuses(t, userID)["web/delivered"]; got != wantWeb {
				return fmt.Sprintf("%s has %d delivered web notifications, want %d (all channels: %v)",
					userID, got, wantWeb, statuses(t, userID))
			}
			return ""
		}, func() string { return worker.output() })
	}

	// --- Axis 1: the publication's own visibility policy -------------------
	_, versionID, _ := w.seedReviewedVersion(t, ctx, project, "member only object", 1, true)
	pinnedVersion := w.pinVisibilityPolicy(t, ctx, versionID)
	memberOnly := w.mustKnowledgePublish(t, w.alice, project.id,
		knowledgePublishBody(t, pinnedVersion, "v1", ""), "kp-t0901-mem-only-1")
	waitDrained(t, ctx, w.pool, worker)

	// Alice, a member, follows it BY PID.
	aliceSub, err := svc.Subscribe(ctx, domain.User{ID: w.aliceID},
		events.Target{Type: events.TargetTypeKnowledge, ID: memberOnly.PID}, nil, []string{"web"})
	if err != nil {
		t.Fatalf("a project member could not follow the publication by pid: %v", err)
	}
	if aliceSub.TargetID != memberOnly.PID {
		t.Fatalf("the subscription stores target_id %q; a knowledge target is the publication's pid (%q)",
			aliceSub.TargetID, memberOnly.PID)
	}
	// Bob, a non-member, cannot even create the follow: the subscribe path
	// asks the same rule the fan-out will.
	if _, err := svc.Subscribe(ctx, domain.User{ID: w.bobID},
		events.Target{Type: events.TargetTypeKnowledge, ID: memberOnly.PID}, nil, []string{"web"}); err == nil {
		t.Error("a non-member followed a members-only publication; the subscribe path leaked")
	} else if !strings.Contains(err.Error(), "not found") {
		t.Errorf("the refusal is %v, want the existence-hiding not-found answer", err)
	}

	// The database carries the same rule as a second line of defence
	// (00090's subscriptions_target_id_shape), for a session that writes SQL
	// directly. The value below is the publication's ROW UUID — the
	// identifier a knowledge target used to demand and that no user could
	// ever hold, because the public read resolves a publication by pid — and
	// it must be refused by the CHECK rather than by the application path the
	// assertions above already exercised.
	var memberOnlyRowID string
	if err := w.pool.QueryRow(ctx,
		`SELECT id::text FROM knowledge_publications WHERE pid = $1`, memberOnly.PID).Scan(&memberOnlyRowID); err != nil {
		t.Fatalf("read the publication's row uuid: %v", err)
	}
	if _, err := w.pool.Exec(ctx, `
		INSERT INTO subscriptions (user_id, target_type, target_id, channels)
		VALUES ($1, 'knowledge', $2, ARRAY['web'])`, w.aliceID, memberOnlyRowID); err == nil {
		t.Error("the database accepted a knowledge target addressed by the publication's row uuid")
	} else if !strings.Contains(err.Error(), "subscriptions_target_id_shape") {
		t.Errorf("the uuid-shaped knowledge target was refused by %v, want the target-id CHECK", err)
	}

	announceKnowledgeEvent(t, ctx, w, project.id, memberOnly.PID)
	waitDeliveries(t, w.aliceID, 1)
	waitDrained(t, ctx, w.pool, worker)
	if got := statuses(t, w.bobID); len(got) != 0 {
		t.Errorf("the non-member has delivery rows on a members-only publication: %v", got)
	}
	// The projection agrees with the read path about the same publication: a
	// pinned visibility policy in a public project is not the network's.
	memberOnlyRow := waitForDocument(t, probe, "knowledge:"+memberOnly.PID, worker)
	if memberOnlyRow.Visibility != "private" {
		t.Errorf("the members-only publication projected as %q, want private", memberOnlyRow.Visibility)
	}

	// --- Axis 2: the owning project's visibility ---------------------------
	_, openVersionID, _ := w.seedReviewedVersion(t, ctx, project, "open object", 2, true)
	openPublication := w.mustKnowledgePublish(t, w.alice, project.id,
		knowledgePublishBody(t, openVersionID, "v1", ""), "kp-t0901-open-1")
	waitDrained(t, ctx, w.pool, worker)

	// The SAME subscription, pointed at the network-audience publication.
	if _, err := w.pool.Exec(ctx, `UPDATE subscriptions SET target_id = $2 WHERE id = $1`,
		aliceSub.ID, openPublication.PID); err != nil {
		t.Fatalf("re-point the subscription: %v", err)
	}
	// The non-member follows this one: it is the network's, so the same rule
	// that refused him above admits him here.
	if _, err := svc.Subscribe(ctx, domain.User{ID: w.bobID},
		events.Target{Type: events.TargetTypeKnowledge, ID: openPublication.PID}, nil,
		[]string{"web", "email"}); err != nil {
		t.Fatalf("a non-member could not follow a network-audience publication: %v", err)
	}
	announceKnowledgeEvent(t, ctx, w, project.id, openPublication.PID)
	waitDeliveries(t, w.aliceID, 2)
	waitDeliveries(t, w.bobID, 1)
	waitDrained(t, ctx, w.pool, worker)
	if got := statuses(t, w.bobID); got["email/pending"] != 1 {
		t.Errorf("the non-member's email channel = %v, want one pending row", got)
	}
	if row := waitForDocument(t, probe, "knowledge:"+openPublication.PID, worker); row.Visibility != "public" {
		t.Errorf("the network-audience publication projected as %q, want public", row.Visibility)
	}

	// The project turns private. The audience is re-resolved per event
	// against LIVE state, so the same publication announced again decides
	// differently: the member keeps receiving, and the non-member's queued
	// notification is WITHDRAWN rather than delivered later.
	if _, err := w.pool.Exec(ctx, `UPDATE projects SET visibility = 'private' WHERE id = $1`, project.id); err != nil {
		t.Fatalf("make the project private: %v", err)
	}
	announceKnowledgeEvent(t, ctx, w, project.id, openPublication.PID)
	waitDeliveries(t, w.aliceID, 3)
	waitDrained(t, ctx, w.pool, worker)
	if got := statuses(t, w.bobID); got["web/delivered"] != 1 || got["email/cancelled"] != 1 || len(got) != 2 {
		t.Errorf("the non-member after the project turned private = %v, want exactly the delivered web row "+
			"and a withdrawn email row", got)
	}
	// The rebuild repairs what the incremental path cannot: the visibility a
	// row was projected with is a copy of its source's axes, so the rows the
	// flip invalidated are re-derived from current state.
	rebuild := buildPostWorker(t)
	dbURL := databaseURLOf(t, ctx, w.pool)
	runSearchRebuild(t, rebuild, dbURL)
	for _, ref := range []string{"knowledge:" + openPublication.PID, "knowledge:" + memberOnly.PID} {
		if row := waitForDocument(t, probe, ref, worker); row.Visibility != "private" {
			t.Errorf("%s: after the project turned private and the index was rebuilt, visibility = %q, want private",
				ref, row.Visibility)
		}
	}
	if _, err := w.pool.Exec(ctx, `UPDATE projects SET visibility = 'public' WHERE id = $1`, project.id); err != nil {
		t.Fatalf("restore the project: %v", err)
	}
	runSearchRebuild(t, rebuild, dbURL)
	if row := waitForDocument(t, probe, "knowledge:"+openPublication.PID, worker); row.Visibility != "public" {
		t.Errorf("knowledge:%s: after the project was restored and the index rebuilt, visibility = %q, want public",
			openPublication.PID, row.Visibility)
	}

	// --- Axis 3: the rights document's metadata token ----------------------
	_, rightsVersion, _ := w.seedReviewedVersion(t, ctx, project, "rights pinned object", 3, true)
	rightsPinned := w.mustKnowledgePublish(t, w.alice, project.id,
		knowledgePublishBody(t, rightsVersion, "v1", "members_only_policy_v1"), "kp-t0901-rights-1")
	waitDrained(t, ctx, w.pool, worker)
	if _, err := svc.Subscribe(ctx, domain.User{ID: w.bobID},
		events.Target{Type: events.TargetTypeKnowledge, ID: rightsPinned.PID}, nil, []string{"web"}); err == nil {
		t.Error("a non-member followed a publication whose rights declaration pins a non-default metadata token")
	}
	// The discriminating half: the member CAN follow the same publication, so
	// the refusal above is a fact about the subscriber rather than about a
	// publication nobody may follow.
	if _, err := svc.Subscribe(ctx, domain.User{ID: w.aliceID},
		events.Target{Type: events.TargetTypeKnowledge, ID: rightsPinned.PID}, nil, []string{"web"}); err != nil {
		t.Errorf("a project member could not follow the rights-pinned publication: %v", err)
	}
	if row := waitForDocument(t, probe, "knowledge:"+rightsPinned.PID, worker); row.Visibility != "private" {
		t.Errorf("the rights-pinned publication projected as %q, want private", row.Visibility)
	}

	// --- The counter-proof -------------------------------------------------
	unknownPID := "01j9z6k3m4n5p6q7r8s9t0zzz9"
	if publicationExists(t, ctx, w.pool, unknownPID) {
		t.Fatalf("the counter-proof pid %s resolves to a publication", unknownPID)
	}
	// Nothing is in flight when the counts are taken, so "unchanged" below
	// means the unresolvable event delivered nothing rather than that its
	// turn had not come.
	waitDrained(t, ctx, w.pool, worker)
	beforeAlice := len(deliveriesOf(t, ctx, store, w.pool, w.aliceID))
	beforeBob := len(deliveriesOf(t, ctx, store, w.pool, w.bobID))
	unknownEvent := announceKnowledgeEvent(t, ctx, w, project.id, unknownPID)
	// The positive control for a NEGATIVE: a later, resolvable event on the
	// same subscriptions. It is announced AFTER the unresolvable one and the
	// fan-out claims rows in created_at order, so once ITS deliveries exist
	// the unresolvable event has been decided — and its own deliveries are
	// known exactly, so the deltas asserted below are the whole story rather
	// than "some number that happens to look right".
	announceKnowledgeEvent(t, ctx, w, project.id, openPublication.PID)
	waitDeliveries(t, w.aliceID, 4) // the member's web channel: one more
	waitDeliveries(t, w.bobID, 2)   // the non-member's web channel: one more
	waitDrained(t, ctx, w.pool, worker)
	// alice's control owes her one web row; bob's owes him a web row AND an
	// email row (he follows this publication on both channels). Anything
	// beyond that is a delivery the unresolvable pid produced.
	if after := len(deliveriesOf(t, ctx, store, w.pool, w.aliceID)); after != beforeAlice+1 {
		t.Errorf("the member has %d deliveries, want %d: the unresolvable pid produced %d of them",
			after, beforeAlice+1, after-beforeAlice-1)
	}
	if after := len(deliveriesOf(t, ctx, store, w.pool, w.bobID)); after != beforeBob+2 {
		t.Errorf("the non-member has %d deliveries, want %d (the control's web and email rows): "+
			"the unresolvable pid produced %d of them", after, beforeBob+2, after-beforeBob-2)
	}
	// The fan-out's cursor is the observable "this event has been decided":
	// without it the arithmetic above would be vacuous — nothing consumed
	// the event, so nothing could have delivered.
	if !waitForFanOut(t, ctx, w.pool, unknownEvent) {
		t.Fatal("the unresolvable event was never consumed; the counts above would be vacuous")
	}
	level, err := store.TargetAudienceFor(ctx,
		events.Target{Type: events.TargetTypeKnowledge, ID: unknownPID}, w.aliceID)
	if err != nil {
		t.Fatalf("TargetAudienceFor on an unresolvable pid: %v", err)
	}
	if level != events.AudienceNone {
		t.Errorf("the audience of an unresolvable pid is %q, want none", level)
	}
}

// waitDrained waits until the subscription fan-out has consumed every event
// in the outbox, so an assertion made afterwards is about a pipeline that is
// quiet rather than one that is merely early.
//
// The predicate is deliberately about EVERY outbox row rather than about the
// rows the publisher has already stamped: an event that is recorded but not
// yet published is precisely the one that would be delivered to a
// subscription created in the meantime, and a drain that could not see it
// would return vacuously and let the count below be wrong rather than late.
//
// Every outbox row gets a cursor row — subscription_fanout claims published
// events without filtering on type, and marks each one even when it matched
// no subscriber (deliveries=0 is a fanned event, not an unconsumed one) — so
// "no row lacks a cursor row" is exactly "nothing is in flight".
func waitDrained(t *testing.T, ctx context.Context, pool *pgxpool.Pool, worker *workerProcess) {
	t.Helper()
	waitFor(t, 45*time.Second, func() string {
		var pending int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM outbox_events oe
			WHERE NOT EXISTS (
				SELECT 1 FROM subscription_fanned_events s WHERE s.outbox_event_id = oe.id)`).
			Scan(&pending); err != nil {
			t.Fatalf("count pending events: %v", err)
		}
		if pending != 0 {
			return fmt.Sprintf("%d events in the outbox have not been fanned out yet", pending)
		}
		return ""
	}, func() string { return worker.output() })
}

// --------------------------------------------------------------------------
// Rebuild command helpers

// rebuildResult is what `post-worker -search-rebuild` reported, parsed from
// its stdout line: the command's REPORT is part of what is under test — a
// rebuild that ran and said nothing would not be operable.
type rebuildResult struct {
	Removed   int
	Projected int
	Types     string
	Raw       string
}

func runSearchRebuild(t *testing.T, binary, dbURL string) rebuildResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-search-rebuild")
	// A scratch cwd: LoadFromCwd may only see the environment this test
	// built, never a developer's .env.<layer> in the repository.
	cmd.Dir = t.TempDir()
	cmd.Env = workerEnv(t, dbURL)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("post-worker -search-rebuild failed: %v\n%s", err, out)
	}
	line := ""
	for _, l := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(l, "search rebuild: ") {
			line = strings.TrimPrefix(l, "search rebuild: ")
		}
	}
	if line == "" {
		t.Fatalf("the command reported no rebuild; it must not be a silent no-op:\n%s", out)
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		t.Fatalf("unparsable rebuild report %q", line)
	}
	got := rebuildResult{Raw: line}
	for _, field := range fields[:2] {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			t.Fatalf("unparsable rebuild report %q", line)
		}
		n, err := strconv.Atoi(value)
		if err != nil {
			t.Fatalf("rebuild report %q: %v", line, err)
		}
		switch key {
		case "removed":
			got.Removed = n
		case "projected":
			got.Projected = n
		default:
			t.Fatalf("unexpected rebuild report field %q in %q", key, line)
		}
	}
	if len(fields) > 2 {
		// The per-entity-type counts, e.g. "asset=1 knowledge=0 release=1
		// state=1". A rebuild that derived nothing must not look like one
		// that worked, so the list is required.
		got.Types = strings.Join(fields[2:], " ")
	}
	if got.Types == "" {
		t.Fatalf("the rebuild report names no entity type: %q", line)
	}
	return got
}

// --------------------------------------------------------------------------
// Subscription helpers

func deliveriesOf(t *testing.T, ctx context.Context, store *events.SubscriptionStore,
	pool *pgxpool.Pool, userID string) []events.SubscriptionDelivery {
	t.Helper()
	out, err := store.Deliveries(ctx, pool, userID, "", 200)
	if err != nil {
		t.Fatalf("read deliveries: %v", err)
	}
	return out
}

// waitForDocument waits until the worker has projected entityRef, and
// returns the row.
func waitForDocument(t *testing.T, probe documentProbe, entityRef string, worker *workerProcess) projectedRow {
	t.Helper()
	var row projectedRow
	waitFor(t, 45*time.Second, func() string {
		got := probe.doc(t, entityRef)
		if got == nil {
			return "no projected row for " + entityRef
		}
		row = *got
		return ""
	}, func() string { return worker.output() })
	return row
}

// waitForFanOut waits until the subscription fan-out has consumed one outbox
// row — its cursor row is the observable "this event has been decided" — and
// reports whether it got there within the deadline.
func waitForFanOut(t *testing.T, ctx context.Context, pool *pgxpool.Pool, outboxEventID string) bool {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for {
		var fanned bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM subscription_fanned_events WHERE outbox_event_id = $1)`,
			outboxEventID).Scan(&fanned); err != nil {
			t.Fatalf("read the fan-out cursor: %v", err)
		}
		if fanned {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// announceKnowledgeEvent records a knowledge.version_published event with
// the producer's own payload shape
// (internal/persistence/knowledge_publish_store.go:
// knowledgeVersionPublishedPayload) and returns the outbox row's id.
//
// It exists for the cases where the SAME publication has to be announced
// again: a version publishes once, so a second announcement of one
// publication cannot come from the command.
func announceKnowledgeEvent(t *testing.T, ctx context.Context, w *knowledgeWorld, projectID, pid string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"payload_version": 1,
		"publication_id":  pid,
		"public_version":  "v1",
		"project_id":      projectID,
	})
	if err != nil {
		t.Fatalf("render the knowledge event payload: %v", err)
	}
	correlationID := "T0901-knowledge-" + pid
	if err := events.Record(ctx, w.pool, events.Event{
		EventType:     events.EventTypeKnowledgeVersionPublished,
		ActorID:       w.aliceID,
		ProjectID:     projectID,
		Visibility:    "public",
		CorrelationID: correlationID,
		Payload:       payload,
	}); err != nil {
		t.Fatalf("record the knowledge event: %v", err)
	}
	var id string
	if err := w.pool.QueryRow(ctx,
		`SELECT id::text FROM outbox_events WHERE correlation_id = $1 AND event_type = $2`,
		correlationID, events.EventTypeKnowledgeVersionPublished).Scan(&id); err != nil {
		t.Fatalf("read the recorded outbox row: %v", err)
	}
	return id
}

func publicationExists(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid string) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM knowledge_publications WHERE pid = $1)`, pid).Scan(&exists); err != nil {
		t.Fatalf("resolve %s: %v", pid, err)
	}
	return exists
}

// databaseURLOf rebuilds a test database's URL from its open pool.
//
// pgx's ConnConfig.ConnString() returns the ORIGINAL string captured at
// parse time, so a rewritten config does not survive that round trip
// (internal/persistence/testdb.WithDatabase) — the URL has to be rebuilt
// from the name the database actually has.
func databaseURLOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var name string
	if err := pool.QueryRow(ctx, `SELECT current_database()`).Scan(&name); err != nil {
		t.Fatalf("read the test database name: %v", err)
	}
	dbURL, err := testdb.WithDatabase(adminURL(t), name)
	if err != nil {
		t.Fatalf("rebuild the test database url: %v", err)
	}
	return dbURL
}

// --------------------------------------------------------------------------
// Process helpers

var (
	postWorkerOnce sync.Once
	postWorkerBin  string
	postWorkerErr  error
)

// buildPostWorker builds cmd/worker once for the whole test binary. The
// binary IS the deliverable in criteria 4 and 5: those tests drive the
// command, not a function.
func buildPostWorker(t *testing.T) string {
	t.Helper()
	postWorkerOnce.Do(func() {
		root, err := filepath.Abs(filepath.Join("..", ".."))
		if err != nil {
			postWorkerErr = fmt.Errorf("resolve the repository root: %w", err)
			return
		}
		dir, err := os.MkdirTemp("", "post-t0901-worker-")
		if err != nil {
			postWorkerErr = err
			return
		}
		postWorkerBin = filepath.Join(dir, "worker")
		cmd := exec.Command("go", "build", "-o", postWorkerBin, "./cmd/worker")
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			postWorkerErr = fmt.Errorf("go build ./cmd/worker: %v\n%s", err, out)
		}
	})
	if postWorkerErr != nil {
		t.Fatalf("%v", postWorkerErr)
	}
	return postWorkerBin
}

// workerEnv is the validated configuration cmd/worker requires, built from
// the test database's URL — the same POST_* names `make smoke` and
// `make search-rebuild` use.
func workerEnv(t *testing.T, dbURL string) []string {
	t.Helper()
	u, err := url.Parse(dbURL)
	if err != nil {
		t.Fatalf("parse the test database url: %v", err)
	}
	password, _ := u.User.Password()
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	return []string{
		"POST_ENV=test",
		"POST_DB_HOST=" + u.Hostname(),
		"POST_DB_PORT=" + port,
		"POST_DB_USER=" + u.User.Username(),
		"POST_DB_PASSWORD=" + password,
		"POST_DB_NAME=" + strings.TrimPrefix(u.Path, "/"),
		"POST_DB_SSLMODE=disable",
		"POST_BLOB_ACCESS_KEY=t0901-ak",
		"POST_BLOB_SECRET_KEY=t0901-sk",
		"POST_GITEA_TOKEN=t0901-token",
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
	}
}

// workerProcess is a running cmd/worker with its stderr captured, so a
// failing wait can report WHY the worker did not do its job.
type workerProcess struct {
	mu      sync.Mutex
	buf     []byte
	stopped chan struct{}
}

func (w *workerProcess) output() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.buf)
}

func startPostWorker(t *testing.T, binary, dbURL, redisAddr string) *workerProcess {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, binary)
	// A scratch cwd, for the same reason runSearchRebuild uses one.
	cmd.Dir = t.TempDir()
	cmd.Env = append(workerEnv(t, dbURL), "POST_REDIS_ADDR="+redisAddr)
	// The worker shuts down on SIGINT/SIGTERM. exec.CommandContext would
	// kill it instead, so it is asked to stop first and killed only if it
	// does not.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 15 * time.Second
	pipe, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("capture the worker's log: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start post-worker: %v", err)
	}
	w := &workerProcess{stopped: make(chan struct{})}
	go func() {
		defer close(w.stopped)
		buf := make([]byte, 4096)
		for {
			n, err := pipe.Read(buf)
			if n > 0 {
				w.mu.Lock()
				w.buf = append(w.buf, buf[:n]...)
				w.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
		<-w.stopped
	})
	return w
}

// waitFor polls until check reports "" (done), failing the test with the
// last check message and the worker's log once the deadline passes.
func waitFor(t *testing.T, timeout time.Duration, check func() string, log func() string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		last := check()
		if last == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s: %s\nworker log:\n%s", timeout, last, log())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// startMiniRedis starts an in-process Redis-protocol server — the same
// server cmd/devredis runs — and returns it.
func startMiniRedis(t *testing.T) *miniredis.Miniredis {
	t.Helper()
	server := miniredis.NewMiniRedis()
	if err := server.StartAddr(freeAddr(t)); err != nil {
		t.Fatalf("start the Redis-protocol server: %v", err)
	}
	t.Cleanup(server.Close)
	return server
}

// freeAddr reserves a loopback port and releases it, so the server that
// binds it next is not racing another test's range.
func freeAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release the port: %v", err)
	}
	return addr
}

// sameFacets compares two decoded structured objects.
func sameFacets(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
