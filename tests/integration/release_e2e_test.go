package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/orgshttp"
	"github.com/lichman0405/post/cmd/api/policyhttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/cmd/api/releasehttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/manifests"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/releases"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/states"
	appvalidation "github.com/lichman0405/post/internal/application/validation"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/sqlc"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// Package integration — T0606-TEST-01 "release e2e" (blocking): the
// immutable release API exercised end to end against REAL PostgreSQL —
// real migrations, real pgx stores, the real auth guard (session + CSRF)
// and the real releasehttp/projectshttp/policyhttp handlers, composed
// exactly as cmd/api/main.go composes them. Only the session store is
// in-memory (Redis semantics are orthogonal and covered elsewhere).
//
// The acceptance criteria each map to a phase of the test:
//
//   - "同 version 重复 idempotent/conflict" → TestReleaseE2E: the same
//     version with the same Idempotency-Key replays the first create's
//     row (same id, nothing re-written — audit/event counts prove it);
//     with a different or missing key it answers 409
//     RELEASE_VERSION_TAKEN.
//   - "旧 release 不受 current state 改变" → TestReleaseE2E: after main's
//     head moves to a new state, the stored release keeps its pinned
//     state id and its manifest export is byte-identical.
//   - no edit/delete → PUT/PATCH/DELETE answer 405 (no route exists);
//   - release gate → without policy and without approved
//     scientific+integrity reviews the create answers 409
//     RELEASE_GATE_BLOCKED (server re-validates — docs/22 §7);
//   - authz → a contributor's create answers 403 RELEASE_FORBIDDEN, an
//     anonymous create 401, and an anonymous read of a private project's
//     releases 404 (existence hiding, docs/45);
//   - durability → each create writes the release row, its idempotency
//     ledger entry, its audit row and its release.published research
//     event + outbox row in one transaction (docs/53).

const releaseE2ETaskID = "T0606"

// releaseE2EPayload is the wire shape of one release (releasehttp
// releasePayload).
type releaseE2EPayload struct {
	ID                 string  `json:"id"`
	ProjectID          string  `json:"project_id"`
	Version            string  `json:"version"`
	Title              string  `json:"title"`
	StateID            string  `json:"state_id"`
	PolicyVersionID    *string `json:"policy_version_id"`
	OrgPolicyVersionID *string `json:"org_policy_version_id"`
	ManifestHash       string  `json:"manifest_hash"`
	CreatedBy          string  `json:"created_by"`
	CreatedAt          string  `json:"created_at"`
}

// releaseE2EList is the list envelope ({"releases":[...]}).
type releaseE2EList struct {
	Releases []releaseE2EPayload `json:"releases"`
}

// doKeyed sends a request with an Idempotency-Key header alongside the
// session's CSRF token — the create's replay contract rides on that key
// (docs/22).
func (uc *testUserClient) doKeyed(t *testing.T, method, path, body, idempotencyKey string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, uc.server+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-CSRF-Token", uc.csrf)
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := uc.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// createRelease posts one release create; key == "" sends no
// Idempotency-Key.
func createRelease(t *testing.T, uc *testUserClient, projectID, version, title, key string) *http.Response {
	t.Helper()
	body := fmt.Sprintf(`{"version":%q,"title":%q}`, version, title)
	return uc.doKeyed(t, http.MethodPost, "/api/v1/projects/"+projectID+"/releases", body, key)
}

// decodeRelease reads one release payload off the wire.
func decodeRelease(t *testing.T, resp *http.Response) releaseE2EPayload {
	t.Helper()
	var r releaseE2EPayload
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("release payload: %v", err)
	}
	return r
}

// getManifestBytes downloads the manifest export and returns its raw
// bytes (the byte-identity assertions compare these) plus the headers.
func getManifestBytes(t *testing.T, uc *testUserClient, projectID, releaseID string) ([]byte, http.Header) {
	t.Helper()
	resp := uc.do(t, http.MethodGet,
		"/api/v1/projects/"+projectID+"/releases/"+releaseID+"/manifest", "")
	mustStatus(t, resp, http.StatusOK)
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	return b, resp.Header
}

// newReleaseServer composes the production tree (like cmd/api/main.go) —
// auth + orgs + projects + policy + releases over one real test database
// — and returns the server plus the pool for SQL-level verification.
func newReleaseServer(t *testing.T, ctx context.Context) (*httptest.Server, *pgxpool.Pool) {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), releaseE2ETaskID)
	sessions := memstore.NewSessions()
	limiter := memstore.NewLimiter()
	cfg := authn.Config{
		WebOrigin:          "http://web.test",
		SessionTTL:         time.Hour,
		LoginLimitPerEmail: 1000,
		LoginLimitPerIP:    10000,
		LoginWindow:        time.Minute,
		SignupLimitPerIP:   10000,
	}
	authAPI := authhttp.New(authhttp.Deps{
		Users:      persistence.NewCredentialStore(pool),
		Sessions:   sessions,
		Limiter:    limiter,
		OIDCClient: nil,
		Cfg:        cfg,
		Secure:     false,
	})
	orgStore := persistence.NewOrgStore(pool)
	orgAPI := orgshttp.New(orgshttp.Deps{Store: orgStore})
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
	stateStore := persistence.NewStateStore(pool)
	releaseStore := persistence.NewReleaseStore(pool)
	policyStore := persistence.NewPolicyStore(pool)
	branchStore := persistence.NewBranchStore(pool)
	releaseBuilder := releases.NewService(
		stateStore,
		manifests.NewService(stateStore, persistence.NewManifestStore(pool)),
		projectStore,
		branchStore,
		policyStore,
		releaseStore,
		reg,
	)
	releaseCommand := releases.NewCommand(
		releaseBuilder,
		projectStore,
		policyStore,
		branchStore,
		stateStore,
		appvalidation.NewService(
			persistence.NewValidationSnapshotRepository(stateStore),
			rsgvalidation.NewValidator(reg),
		),
		releaseStore,
		authz.NewMatrixEngine(),
	)
	releaseAPI := releasehttp.New(releasehttp.Deps{
		Command:  releaseCommand,
		Projects: projectAPI.Service(),
	})
	apiMux := http.NewServeMux()
	apiMux.Handle("/api/v1/auth/", authAPI.Routes())
	apiMux.Handle("/api/v1/organizations", orgAPI.Routes())
	apiMux.Handle("/api/v1/organizations/", orgAPI.Routes())
	apiMux.Handle("/api/v1/projects", projectAPI.Routes())
	apiMux.Handle("/api/v1/projects/", projectAPI.Routes())
	policyAPI.Register(apiMux)
	releaseAPI.Register(apiMux)
	ts := httptest.NewServer(authAPI.Guard(apiMux))
	t.Cleanup(ts.Close)
	return ts, pool
}

// releaseFixture wires the rsg service over the same pool for fixture
// seeding — the branch/object/state lineage the release gates validate.
type releaseFixture struct {
	svc         *rsg.Service
	pool        *pgxpool.Pool
	aliceID     string
	projectID   string
	mainBranch  string
	research    string
	genesis     string
	head        string
	orgPolicyID string
	projPolicy  string
}

// seedAcceptedState creates main plus one full hypothesis on it and
// returns the head state id. The payload carries every field the release
// gate requires (schema required statement/question_id + the docs/08
// hypothesis fields hypothesis_type/scope), so the member checks pass at
// GateRelease. Since migration 00040 the hypothesis question_id must name
// a real research_question of the same project (uuid shape, existence,
// type and project are all enforced by the database guard), so the
// fixture seeds the question first — through the same production
// CreateObject path, not raw SQL.
func (f *releaseFixture) seedAcceptedState(t *testing.T, ctx context.Context, statement string) string {
	t.Helper()
	alice := domain.User{ID: f.aliceID}
	qRes, err := f.svc.CreateObject(ctx, alice, f.projectID, f.mainBranch, rsg.CreateObjectInput{
		ObjectType: "research_question",
		// purpose + question_state are the docs/08 fields the release gate
		// requires beyond the schema's required array (blocking at main).
		Payload: json.RawMessage(fmt.Sprintf(
			`{"statement":%q,"purpose":%q,"question_state":"open"}`,
			statement+"?", "answer "+statement)),
	})
	if err != nil {
		t.Fatalf("CreateObject research question on main: %v", err)
	}
	res, err := f.svc.CreateObject(ctx, alice, f.projectID, f.mainBranch, rsg.CreateObjectInput{
		ObjectType: "hypothesis",
		Payload: json.RawMessage(fmt.Sprintf(
			`{"statement":%q,"question_id":%q,"hypothesis_type":"mechanistic","scope":{"detail":"probe"}}`,
			statement, qRes.Object.ID)),
	})
	if err != nil {
		t.Fatalf("CreateObject on main: %v", err)
	}
	f.head = res.Version.StateID
	return f.head
}

// seedPR seeds one pull_request row proposing proposedState against main
// (source = the research branch) and returns its id.
func (f *releaseFixture) seedPR(t *testing.T, ctx context.Context, number int64, baseState, proposedState string) string {
	t.Helper()
	q := sqlc.New(f.pool)
	row, err := q.CreatePullRequest(ctx, sqlc.CreatePullRequestParams{
		ProjectID:       parseUUIDOrDie(f.projectID),
		Number:          number,
		SourceBranchID:  parseUUIDOrDie(f.research),
		TargetBranchID:  parseUUIDOrDie(f.mainBranch),
		BaseStateID:     parseUUIDOrDie(baseState),
		ProposedStateID: parseUUIDOrDie(proposedState),
		Title:           "release fixture PR",
		Body:            "",
		CreatedBy:       parseUUIDOrDie(f.aliceID),
	})
	if err != nil {
		t.Fatalf("CreatePullRequest: %v", err)
	}
	return pgUUIDTextTest(row.ID)
}

// addReview seeds one review row on a PR (reviewer = bob, the project's
// contributor — the gate requires scientific AND integrity approval).
// reviewedState is the head the review evaluates (migration 00061:
// reviews pin reviewed_state_id to the PR's proposed head; the fixture
// passes it explicitly).
func (f *releaseFixture) addReview(t *testing.T, ctx context.Context, prID, reviewerID, reviewedState, kind, decision string) {
	t.Helper()
	q := sqlc.New(f.pool)
	if _, err := q.CreateReview(ctx, sqlc.CreateReviewParams{
		PullRequestID:   parseUUIDOrDie(prID),
		ReviewerID:      parseUUIDOrDie(reviewerID),
		ReviewKind:      kind,
		Decision:        decision,
		ReviewedStateID: parseUUIDOrDie(reviewedState),
		Responsibility:  "",
		Body:            "",
	}); err != nil {
		t.Fatalf("CreateReview: %v", err)
	}
}

// TestReleaseE2E drives the full acceptance journey over the wire against
// real PostgreSQL.
func TestReleaseE2E(t *testing.T) {
	ctx := testCtx(t)
	ts, pool := newReleaseServer(t, ctx)

	alice, aliceID := signup(t, ts.URL, "release-alice@example.com", "release-alice")
	bob, bobID := signup(t, ts.URL, "release-bob@example.com", "release-bob")

	// --- alice creates the organization and one private project ---
	resp := alice.do(t, http.MethodPost, "/api/v1/organizations",
		`{"slug":"release-labs","name":"Release Labs"}`)
	mustStatus(t, resp, http.StatusCreated)
	var createdOrg orgResponse
	if err := json.NewDecoder(resp.Body).Decode(&createdOrg); err != nil {
		t.Fatalf("org create payload: %v", err)
	}
	orgID := createdOrg.Organization.ID

	resp = alice.do(t, http.MethodPost, "/api/v1/projects",
		fmt.Sprintf(`{"slug":"release-lab","name":"Release Lab","purpose":"release governance","visibility":"private","organization_id":%q}`, orgID))
	mustStatus(t, resp, http.StatusCreated)
	var createdProj projectResponse
	if err := json.NewDecoder(resp.Body).Decode(&createdProj); err != nil {
		t.Fatalf("project create payload: %v", err)
	}
	projectID := createdProj.Project.ID

	// Bob joins as contributor: he reads the releases, but the matrix
	// (ActionCreateRelease) denies him the create. The membership is
	// seeded directly (the management API surface belongs to T0110; the
	// settings suite seeds the same way).
	if _, err := pool.Exec(ctx,
		`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, $3)`,
		projectID, bobID, "contributor"); err != nil {
		t.Fatalf("seed bob membership: %v", err)
	}

	// --- fixture state: main + one full hypothesis; a research branch
	// as the PR source ---
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	stateStore := persistence.NewStateStore(pool)
	statesSvc := states.NewService(stateStore, appvalidation.NewGuard(rsgvalidation.NewValidator(reg), persistence.NewValidationTxProbe()))
	svc := rsg.NewService(rsg.Deps{
		Projects:  projects.NewService(persistence.NewProjectStore(pool), persistence.NewOrgStore(pool), authz.NewMatrixEngine()),
		Branches:  branches.NewService(persistence.NewBranchStore(pool)),
		States:    statesSvc,
		Latest:    stateStore,
		Objects:   persistence.NewScientificObjectStore(pool),
		Relations: persistence.NewRelationStore(pool),
		Authz:     authz.NewMatrixEngine(),
		Schemas:   reg,
		// The transactional outbox (T1001), like cmd/api/main.go wires it:
		// rsg commits fail without a recorder.
		Events: events.Recorder{},
	})
	aliceUser := domain.User{ID: aliceID}
	mainBranch, err := svc.CreateBranch(ctx, aliceUser, projectID, rsg.CreateBranchInput{
		Name:       "main",
		BaseRef:    "",
		Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create main branch: %v", err)
	}
	var genesis string
	if err := pool.QueryRow(ctx,
		`SELECT id FROM project_states WHERE project_id = $1 AND parent_state_id IS NULL`, projectID).Scan(&genesis); err != nil {
		t.Fatalf("read genesis state: %v", err)
	}
	researchBranch, err := svc.CreateBranch(ctx, aliceUser, projectID, rsg.CreateBranchInput{
		Name:       "research-path",
		BaseRef:    genesis,
		Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create research branch: %v", err)
	}
	f := &releaseFixture{
		svc:        svc,
		pool:       pool,
		aliceID:    aliceID,
		projectID:  projectID,
		mainBranch: mainBranch.ID,
		research:   researchBranch.ID,
		genesis:    genesis,
	}
	head1 := f.seedAcceptedState(t, ctx, "probe-1")

	t.Run("gate refuses before policy and reviews exist", func(t *testing.T) {
		resp := createRelease(t, alice, projectID, "v1.0.0", "First release", "release-key-1")
		mustStatus(t, resp, http.StatusConflict)
		mustEnvelope(t, resp, releases.CodeReleaseGateBlocked)
	})

	t.Run("org and project policy versions pin the rights snapshot", func(t *testing.T) {
		resp := alice.do(t, http.MethodPut, "/api/v1/organizations/"+orgID+"/policy",
			`{"version":"v1","policy":{"main_protected":true,"release_min_reviewers":2}}`)
		mustStatus(t, resp, http.StatusCreated)
		orgPolicy := decodePolicyVersion(t, resp)
		if orgPolicy.ID == "" {
			t.Fatalf("org policy version id = %q", orgPolicy.ID)
		}
		resp = alice.do(t, http.MethodPut, "/api/v1/projects/"+projectID+"/policy",
			`{"version":"v1","policy":{"main_protected":true,"release_min_reviewers":3}}`)
		mustStatus(t, resp, http.StatusCreated)
		projPolicy := decodePolicyVersion(t, resp)
		if projPolicy.ID == "" {
			t.Fatalf("project policy version id = %q", projPolicy.ID)
		}
		f.orgPolicyID = orgPolicy.ID
		f.projPolicy = projPolicy.ID

		// The rights snapshot alone does not open the gate: the review
		// record is still missing — the server re-validates (docs/22 §7).
		resp = createRelease(t, alice, projectID, "v1.0.0", "First release", "release-key-1")
		mustStatus(t, resp, http.StatusConflict)
		mustEnvelope(t, resp, releases.CodeReleaseGateBlocked)
	})

	// --- the review record: one PR proposing head1 against main with
	// approved scientific + integrity reviews (bob reviews) ---
	pr1 := f.seedPR(t, ctx, 1, f.genesis, head1)
	f.addReview(t, ctx, pr1, bobID, head1, "scientific", "approved")
	f.addReview(t, ctx, pr1, bobID, head1, "integrity", "approved")

	var first releaseE2EPayload
	var manifestBytes1 []byte

	t.Run("create fixes the accepted snapshot", func(t *testing.T) {
		resp := createRelease(t, alice, projectID, "v1.0.0", "First release", "release-key-1")
		mustStatus(t, resp, http.StatusCreated)
		first = decodeRelease(t, resp)
		if first.ID == "" || first.Version != "v1.0.0" || first.Title != "First release" {
			t.Fatalf("created release = %+v", first)
		}
		if first.StateID != head1 {
			t.Fatalf("state_id = %s, want the accepted head %s", first.StateID, head1)
		}
		if first.PolicyVersionID == nil || *first.PolicyVersionID != f.projPolicy {
			t.Fatalf("project policy pin = %v, want %s", first.PolicyVersionID, f.projPolicy)
		}
		if first.OrgPolicyVersionID == nil || *first.OrgPolicyVersionID != f.orgPolicyID {
			t.Fatalf("org policy pin = %v, want %s", first.OrgPolicyVersionID, f.orgPolicyID)
		}
		if first.ManifestHash == "" || first.CreatedBy != aliceID {
			t.Fatalf("created release = %+v", first)
		}

		// The manifest export: the stored canonical document, hash-verified
		// server-side, downloadable by name.
		b, hdr := getManifestBytes(t, alice, projectID, first.ID)
		manifestBytes1 = b
		if ct := hdr.Get("Content-Type"); ct != "application/json" {
			t.Fatalf("manifest content type = %q", ct)
		}
		if cd := hdr.Get("Content-Disposition"); cd != `attachment; filename="release-v1.0.0.manifest.json"` {
			t.Fatalf("manifest content disposition = %q", cd)
		}
		var doc map[string]any
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatalf("manifest parse: %v", err)
		}
		if doc["manifest_hash"] != first.ManifestHash {
			t.Fatalf("manifest doc hash = %v, want the row's %s", doc["manifest_hash"], first.ManifestHash)
		}
		if doc["state_id"] != head1 {
			t.Fatalf("manifest state_id = %v, want %s", doc["state_id"], head1)
		}
	})

	t.Run("same version repeats: idempotent with the same key", func(t *testing.T) {
		resp := createRelease(t, alice, projectID, "v1.0.0", "First release", "release-key-1")
		mustStatus(t, resp, http.StatusCreated)
		replay := decodeRelease(t, resp)
		if replay.ID != first.ID || replay.StateID != first.StateID || replay.ManifestHash != first.ManifestHash {
			t.Fatalf("replay = %+v, want the first row %+v", replay, first)
		}
		// The replay wrote nothing: one release row, one ledger entry, one
		// audit row, one event — the idempotency is real, not a facade.
		var releases, creations, audits, events, outbox int
		mustScanCount(t, ctx, pool, `SELECT count(*) FROM releases`, &releases)
		mustScanCount(t, ctx, pool, `SELECT count(*) FROM release_creations`, &creations)
		mustScanCount(t, ctx, pool,
			`SELECT count(*) FROM audit_log WHERE project_id = $1 AND action = $2`, &audits,
			projectID, domain.ActionReleaseCreated)
		mustScanCount(t, ctx, pool,
			`SELECT count(*) FROM research_events WHERE project_id = $1 AND event_type = $2`, &events,
			projectID, "release.published")
		mustScanCount(t, ctx, pool,
			`SELECT count(*) FROM outbox_events WHERE event_type = $1`, &outbox, "release.published")
		if releases != 1 || creations != 1 || audits != 1 || events != 1 || outbox != 1 {
			t.Fatalf("after replay: releases=%d creations=%d audits=%d events=%d outbox=%d, want all 1",
				releases, creations, audits, events, outbox)
		}
	})

	t.Run("same version conflicts: different or missing key", func(t *testing.T) {
		resp := createRelease(t, alice, projectID, "v1.0.0", "First release", "release-key-2")
		mustStatus(t, resp, http.StatusConflict)
		mustEnvelope(t, resp, releases.CodeReleaseVersionTaken)
		resp = createRelease(t, alice, projectID, "v1.0.0", "First release", "")
		mustStatus(t, resp, http.StatusConflict)
		mustEnvelope(t, resp, releases.CodeReleaseVersionTaken)
	})

	t.Run("input shape and immutability of the surface", func(t *testing.T) {
		// A version the label rules refuse (space) is a validation outcome.
		resp := createRelease(t, alice, projectID, "v 1.0", "bad", "release-key-9")
		mustStatus(t, resp, http.StatusBadRequest)
		mustEnvelope(t, resp, releases.CodeReleaseValidationFailed)
		// Immutable: no edit, no delete — the surface registers no such
		// route. In the composed tree the projects subtree answers 404
		// ("no such operation"); on a bare mux the method patterns answer
		// 405 (pinned by the handler unit test) — either way the release
		// command never runs and the row is untouched (the append-only
		// trigger refuses any UPDATE/DELETE at the database, migration
		// 00014).
		for _, tc := range []struct{ method, path string }{
			{http.MethodPut, "/api/v1/projects/" + projectID + "/releases/" + first.ID},
			{http.MethodPatch, "/api/v1/projects/" + projectID + "/releases/" + first.ID},
			{http.MethodDelete, "/api/v1/projects/" + projectID + "/releases/" + first.ID},
		} {
			resp := alice.do(t, tc.method, tc.path, `{"version":"v9.9.9"}`)
			mustStatus(t, resp, http.StatusNotFound)
		}
		// The release still reads back exactly as created.
		resp = alice.do(t, http.MethodGet, "/api/v1/projects/"+projectID+"/releases/"+first.ID, "")
		mustStatus(t, resp, http.StatusOK)
		if after := decodeRelease(t, resp); after.Version != first.Version || after.StateID != first.StateID ||
			after.ManifestHash != first.ManifestHash {
			t.Fatalf("release changed by edit attempts: %+v", after)
		}
	})

	t.Run("the release survives current-state changes byte-identical", func(t *testing.T) {
		// Main moves on: a second hypothesis becomes the accepted head.
		head2 := f.seedAcceptedState(t, ctx, "probe-2")
		if head2 == head1 {
			t.Fatalf("head did not move")
		}
		// The replay still returns the first create's row — pinned state,
		// no gate re-run for the stored snapshot.
		resp := createRelease(t, alice, projectID, "v1.0.0", "First release", "release-key-1")
		mustStatus(t, resp, http.StatusCreated)
		replay := decodeRelease(t, resp)
		if replay.ID != first.ID || replay.StateID != head1 {
			t.Fatalf("replay after head move = %+v, want id %s on state %s", replay, first.ID, head1)
		}
		// The manifest export is byte-identical to the pre-change export.
		b2, _ := getManifestBytes(t, alice, projectID, first.ID)
		if string(b2) != string(manifestBytes1) {
			t.Fatalf("manifest bytes moved after main advanced:\nbefore: %s\nafter:  %s", manifestBytes1, b2)
		}
	})

	t.Run("a second release fixes the new head; the list is newest-first", func(t *testing.T) {
		// The previous subtest advanced main's head; the fixture tracked it.
		head2 := f.head
		pr2 := f.seedPR(t, ctx, 2, head1, head2)
		f.addReview(t, ctx, pr2, bobID, head2, "scientific", "approved")
		f.addReview(t, ctx, pr2, bobID, head2, "integrity", "approved")
		resp := createRelease(t, alice, projectID, "v2.0.0", "", "release-key-3")
		mustStatus(t, resp, http.StatusCreated)
		second := decodeRelease(t, resp)
		if second.StateID != head2 || second.Title != "v2.0.0" {
			t.Fatalf("second release = %+v, want state %s and the version as title", second, head2)
		}

		resp = alice.do(t, http.MethodGet, "/api/v1/projects/"+projectID+"/releases", "")
		mustStatus(t, resp, http.StatusOK)
		var list releaseE2EList
		if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
			t.Fatalf("list payload: %v", err)
		}
		if len(list.Releases) != 2 || list.Releases[0].Version != "v2.0.0" || list.Releases[1].Version != "v1.0.0" {
			t.Fatalf("list = %+v, want [v2.0.0 v1.0.0]", list.Releases)
		}
	})

	t.Run("durable ledger: audit rows and release.published events", func(t *testing.T) {
		rows, err := pool.Query(ctx,
			`SELECT via, actor_id::text, target_ref, correlation_id
			   FROM audit_log WHERE project_id = $1 AND action = $2 ORDER BY occurred_at`,
			projectID, domain.ActionReleaseCreated)
		if err != nil {
			t.Fatalf("audit rows: %v", err)
		}
		defer rows.Close()
		var count int
		for rows.Next() {
			var via, actor, target, correlation string
			if err := rows.Scan(&via, &actor, &target, &correlation); err != nil {
				t.Fatalf("audit scan: %v", err)
			}
			count++
			if via != domain.ViaSession || actor != aliceID {
				t.Fatalf("audit row = via %s actor %s, want session %s", via, actor, aliceID)
			}
			if !strings.HasPrefix(target, "release:") {
				t.Fatalf("audit target = %q, want release:<id>", target)
			}
			if correlation == "" {
				t.Fatalf("audit row has no correlation id")
			}
		}
		if count != 2 {
			t.Fatalf("audit rows = %d, want 2 (one per create; replays write nothing)", count)
		}
		var events int
		mustScanCount(t, ctx, pool,
			`SELECT count(*) FROM research_events
			  WHERE project_id = $1 AND event_type = $2 AND visibility = $3`,
			&events, projectID, "release.published", domain.VisibilityPrivate)
		if events != 2 {
			t.Fatalf("research events = %d, want 2 private release.published rows", events)
		}
		var payloadVersion int
		if err := pool.QueryRow(ctx,
			`SELECT count(DISTINCT payload->>'payload_version') FROM research_events
			  WHERE project_id = $1 AND event_type = $2`,
			projectID, "release.published").Scan(&payloadVersion); err != nil {
			t.Fatalf("event payload version: %v", err)
		}
		if payloadVersion != 1 {
			t.Fatalf("event payload_version = %d, want 1 (payload_version travels inside the payload, docs/53)", payloadVersion)
		}
		var outbox int
		mustScanCount(t, ctx, pool,
			`SELECT count(*) FROM outbox_events WHERE event_type = $1`, &outbox, "release.published")
		if outbox != 2 {
			t.Fatalf("outbox rows = %d, want 2", outbox)
		}
	})

	t.Run("authz: contributor reads but cannot create; anonymous is hidden", func(t *testing.T) {
		// Bob (contributor) reads the releases…
		resp := bob.do(t, http.MethodGet, "/api/v1/projects/"+projectID+"/releases", "")
		mustStatus(t, resp, http.StatusOK)
		// …but the matrix refuses his create (403, never a lookup leak).
		resp = createRelease(t, bob, projectID, "v3.0.0", "", "release-key-4")
		mustStatus(t, resp, http.StatusForbidden)
		mustEnvelope(t, resp, releases.CodeReleaseForbidden)
		// Anonymous: writes are 401 at the guard; reads of the private
		// project answer 404 (existence hiding, docs/45).
		anon := newTestUserClient(ts.URL)
		resp = anon.do(t, http.MethodPost, "/api/v1/projects/"+projectID+"/releases", `{"version":"v3.0.0"}`)
		mustStatus(t, resp, http.StatusUnauthorized)
		resp = anon.do(t, http.MethodGet, "/api/v1/projects/"+projectID+"/releases", "")
		mustStatus(t, resp, http.StatusNotFound)
		mustEnvelope(t, resp, releases.CodeReleaseProjectNotFound)
	})
}

// mustScanCount runs one COUNT query and fails the test unless it
// resolves to exactly one row scanning into *int.
func mustScanCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string, dst *int, args ...any) {
	t.Helper()
	if err := pool.QueryRow(ctx, query, args...).Scan(dst); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}
