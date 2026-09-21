package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/aborthttp"
	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/mergehttp"
	"github.com/lichman0405/post/cmd/api/policyhttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/cmd/api/pullrequestshttp"
	"github.com/lichman0405/post/cmd/api/reviewhttp"
	"github.com/lichman0405/post/cmd/api/rsghttp"

	"github.com/lichman0405/post/internal/application/aborts"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/diffs"
	"github.com/lichman0405/post/internal/application/merge"
	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/application/prchecks"
	"github.com/lichman0405/post/internal/application/prdiff"
	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/application/resolutions"
	"github.com/lichman0405/post/internal/application/responsibilities"
	"github.com/lichman0405/post/internal/application/reviews"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rsg/integrity"
	"github.com/lichman0405/post/internal/rsg/schemareg"
)

// T0602's required test — label: `abort tests`.
//
// The abort half of docs/46, end to end, over REAL PostgreSQL: an object
// active on main, proposed for abort over the production route
// (aborthttp.New -> Register -> the real auth guard -> the real command ->
// the real stores), carried by the Research PR the command opens, driven to
// merge_ready by SUBMITTING the required reviews over the review route
// (cmd/api/reviewhttp, POST /api/v1/projects/{id}/pull-requests/{n}/reviews
// — the review_required -> approved -> merge_ready walk is the review
// store's own projection of a satisfied required-review calculation, not a
// state this test assigns), merged onto main through the production merge
// route, and read back out of STORAGE column by column.
//
// docs/46:5's "review/approval if main object" is therefore a fact this
// test observes rather than assumes: two review rows carrying this PR's
// head are what let the merge see merge_ready at all.
//
// Nothing the assertions read is a function's return value or an in-memory
// object: every claim below is a SELECT against the canonical tables, which
// is what makes the two claims that matter — "the abort took effect" and
// "the history was not rewritten" — falsifiable. The task names two
// anti-examples of the shape this test must NOT have
// (tests/integration/freeze_main_e2e_test.go:849-853 and
// tests/integration/pullrequest_test.go:331-336: fixtures that write the
// lifecycle they want to observe straight into storage). Here the lifecycle
// reaches main only through the merge, and the pre-abort state is read out of
// the database before the abort runs so it can be compared byte for byte
// afterwards.
//
// The in-memory adapters are the session store and the rate limiter — the
// same pair every HTTP-level integration fixture in this package uses; Git
// stays nil on the merge service for the reason merge_test.go:105 records
// (the provider saga is T0409's, and the semantic merge records the step as
// not-done rather than claiming the ref moved), which this test does not
// assert about either way.

const abortTaskID = "T0602"

// abortPath is the contract's route: POST
// /projects/{projectId}/objects/{objectId}:abort-proposal
// (specs/api/openapi.yaml:345-355).
func abortPath(projectID, objectID string) string {
	return "/api/v1/projects/" + projectID + "/objects/" + objectID + ":abort-proposal"
}

// abortWire is the abort route's response (aborthttp's own payload).
type abortWire struct {
	ProjectID         string `json:"project_id"`
	ObjectID          string `json:"object_id"`
	AbortedVersionID  string `json:"aborted_version_id"`
	AbortedVersionNo  int    `json:"aborted_version_no"`
	VersionID         string `json:"version_id"`
	VersionNo         int    `json:"version_no"`
	LifecycleState    string `json:"lifecycle_state"`
	BranchID          string `json:"branch_id"`
	BranchName        string `json:"branch_name"`
	PullRequestNumber int64  `json:"pull_request_number"`
	PRState           string `json:"pull_request_state"`
	ReasonCode        string `json:"reason_code"`
	Explanation       string `json:"explanation"`
	ReplacementRef    string `json:"replacement_ref"`
	DecidedBy         string `json:"decided_by"`
	DecidedAt         string `json:"decided_at"`
	Replayed          bool   `json:"replayed"`
}

// abortOnce issues one abort proposal and returns its status, raw body and
// decoded payload. It calls no testing helper on the way out, so the
// concurrency case below can drive it from goroutines.
func abortOnce(client *http.Client, server, projectID, objectID, csrf, key, body string) (int, string, abortWire, error) {
	req, err := http.NewRequest(http.MethodPost, server+abortPath(projectID, objectID), bytes.NewReader([]byte(body)))
	if err != nil {
		return 0, "", abortWire{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", abortWire{}, err
	}
	defer resp.Body.Close()
	raw := make([]byte, 0, 512)
	buf := make([]byte, 512)
	for {
		n, rerr := resp.Body.Read(buf)
		raw = append(raw, buf[:n]...)
		if rerr != nil {
			break
		}
	}
	var out abortWire
	if resp.StatusCode == http.StatusCreated {
		if err := json.Unmarshal(raw, &out); err != nil {
			return resp.StatusCode, string(raw), abortWire{}, fmt.Errorf("decode abort payload: %w (%s)", err, raw)
		}
	}
	return resp.StatusCode, string(raw), out, nil
}

// abortThroughTheEndpoint issues one abort exactly as the contract defines
// it — a POST to the literal path with the required Idempotency-Key, over the
// real guard (the session's cookie and CSRF token ride along) — and asserts
// the 201.
func abortThroughTheEndpoint(t *testing.T, uc *testUserClient, projectID, objectID, key, body string) abortWire {
	t.Helper()
	status, raw, out, err := abortOnce(uc.client, uc.server, projectID, objectID, uc.csrf, key, body)
	if err != nil {
		t.Fatalf("abort %s: %v", objectID, err)
	}
	if status != http.StatusCreated {
		t.Fatalf("abort %s = %d: %s", objectID, status, raw)
	}
	return out
}

// abortBody renders the contract's body. The field names are the MCP tool's
// argument names (specs/mcp/tools.json:23) plus docs/46:7's optional
// replacement ref — see cmd/api/aborthttp's package doc.
func abortBody(versionID, reasonCode, explanation, replacementRef string) string {
	b := fmt.Sprintf(`{"object_version_ref":%q,"reason_code":%q,"explanation":%q`,
		"object_version:"+versionID, reasonCode, explanation)
	if replacementRef != "" {
		b += fmt.Sprintf(`,"replacement_ref":%q`, replacementRef)
	}
	return b + "}"
}

// ---------------------------------------------------------------- storage

// versionRow is one scientific_object_versions row read back column by
// column. Every column of the canonical table is here, including the six
// migration 00100 added, so "byte-identical" below is a claim about the whole
// row rather than about the columns this task happens to care about.
type versionRow struct {
	id                 string
	objectID           string
	versionNo          int
	stateID            string
	branchID           *string
	schemaID           string
	schemaVersion      string
	title              string
	lifecycleState     string
	payload            []byte
	visibilityPolicyID *string
	integrityHash      string
	createdBy          string
	createdAt          time.Time
	abortReasonCode    *string
	abortExplanation   *string
	abortReplacement   *string
	abortedBy          *string
	abortedAt          *time.Time
	abortRequestKey    *string
}

const versionRowColumns = `id::text, object_id::text, version_no, state_id::text, branch_id::text,
	schema_id, schema_version, title, lifecycle_state, payload, visibility_policy_id::text,
	integrity_hash, created_by::text, created_at,
	abort_reason_code, abort_explanation, abort_replacement_ref, aborted_by::text,
	aborted_at, abort_request_key`

// readVersion reads one version row straight out of the canonical table.
func readVersion(t *testing.T, ctx context.Context, pool *pgxpool.Pool, versionID string) versionRow {
	t.Helper()
	var r versionRow
	if err := pool.QueryRow(ctx,
		`SELECT `+versionRowColumns+` FROM scientific_object_versions WHERE id = $1::uuid`,
		versionID).Scan(&r.id, &r.objectID, &r.versionNo, &r.stateID, &r.branchID, &r.schemaID,
		&r.schemaVersion, &r.title, &r.lifecycleState, &r.payload, &r.visibilityPolicyID,
		&r.integrityHash, &r.createdBy, &r.createdAt, &r.abortReasonCode, &r.abortExplanation,
		&r.abortReplacement, &r.abortedBy, &r.abortedAt, &r.abortRequestKey); err != nil {
		t.Fatalf("read version %s: %v", versionID, err)
	}
	return r
}

// describe renders every column of a version row as text, so two reads of the
// same row can be compared byte for byte without the comparison depending on
// which columns a caller remembered to name.
func (r versionRow) describe() string {
	return fmt.Sprintf("id=%s object_id=%s version_no=%d state_id=%s branch_id=%v schema_id=%s "+
		"schema_version=%s title=%s lifecycle_state=%s payload=%s visibility_policy_id=%v "+
		"integrity_hash=%s created_by=%s created_at=%s abort_reason_code=%v abort_explanation=%v "+
		"abort_replacement_ref=%v aborted_by=%v aborted_at=%v abort_request_key=%v",
		r.id, r.objectID, r.versionNo, r.stateID, ptrText(r.branchID), r.schemaID, r.schemaVersion,
		r.title, r.lifecycleState, string(r.payload), ptrText(r.visibilityPolicyID),
		r.integrityHash, r.createdBy, r.createdAt.UTC().Format(time.RFC3339Nano),
		ptrText(r.abortReasonCode), ptrText(r.abortExplanation), ptrText(r.abortReplacement),
		ptrText(r.abortedBy), r.abortedAt, ptrText(r.abortRequestKey))
}

func ptrText(p *string) string {
	if p == nil {
		return "<NULL>"
	}
	return *p
}

// versionsOfObject reads every version row of one object, oldest first.
func versionsOfObject(t *testing.T, ctx context.Context, pool *pgxpool.Pool, objectID string) []versionRow {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT `+versionRowColumns+` FROM scientific_object_versions WHERE object_id = $1::uuid ORDER BY version_no`,
		objectID)
	if err != nil {
		t.Fatalf("read the versions of %s: %v", objectID, err)
	}
	defer rows.Close()
	var out []versionRow
	for rows.Next() {
		var r versionRow
		if err := rows.Scan(&r.id, &r.objectID, &r.versionNo, &r.stateID, &r.branchID, &r.schemaID,
			&r.schemaVersion, &r.title, &r.lifecycleState, &r.payload, &r.visibilityPolicyID,
			&r.integrityHash, &r.createdBy, &r.createdAt, &r.abortReasonCode, &r.abortExplanation,
			&r.abortReplacement, &r.abortedBy, &r.abortedAt, &r.abortRequestKey); err != nil {
			t.Fatalf("scan a version row of %s: %v", objectID, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the versions of %s: %v", objectID, err)
	}
	return out
}

// abortCounts counts the abort command's two records for one object: the
// audit rows it wrote and the domain events it recorded. Both are counts, not
// existence checks, because the idempotency criterion is about how MANY were
// written.
func abortCounts(t *testing.T, ctx context.Context, pool *pgxpool.Pool, objectID string) (audits, outbox int) {
	t.Helper()
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE action = $1 AND target_ref = $2`,
		domain.ActionScientificObjectAborted, "object:"+objectID).Scan(&audits); err != nil {
		t.Fatalf("count the abort audit rows: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM outbox_events WHERE event_type = $1 AND payload->>'object_id' = $2`,
		"scientific_object.aborted", objectID).Scan(&outbox); err != nil {
		t.Fatalf("count the scientific_object.aborted events: %v", err)
	}
	return audits, outbox
}

// abortAuditRow is the one audit row an abort appended, as the archive holds
// it.
type abortAuditRow struct {
	actorID string
	via     string
	target  string
	before  []byte
	after   []byte
}

func readAbortAudit(t *testing.T, ctx context.Context, pool *pgxpool.Pool, objectID string) abortAuditRow {
	t.Helper()
	var r abortAuditRow
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(actor_id::text, ''), via, COALESCE(target_ref, ''),
		        COALESCE(before_summary, 'null'::jsonb), COALESCE(after_summary, 'null'::jsonb)
		   FROM audit_log WHERE action = $1 AND target_ref = $2`,
		domain.ActionScientificObjectAborted, "object:"+objectID).Scan(&r.actorID, &r.via, &r.target, &r.before, &r.after); err != nil {
		t.Fatalf("read the abort audit row: %v", err)
	}
	return r
}

// ----------------------------------------------------------------- fixture

// abortFixture is the platform graph, wired the way cmd/api wires it, over
// real PostgreSQL and behind the real auth guard.
type abortFixture struct {
	pool       *pgxpool.Pool
	server     string
	project    domain.Project
	org        domain.Organization
	mainBranch domain.Branch
	alice      *testUserClient
	aliceID    string
	// clients holds every acting browser by the email it signed up with.
	clients map[string]*testUserClient
	prs     *pullrequests.Service
	merges  *merge.Service
	aborts  *aborts.Service
	// routing is the Research Owners stack the review submissions ride on
	// (rules + assignments): the proposal's required-review calculation is
	// read from it, and the conditional verdict for a viewer's review is
	// resolved by it.
	routing *responsibilities.Service
}

func newAbortFixture(t *testing.T, ctx context.Context) *abortFixture {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), abortTaskID)

	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	orgStore := persistence.NewOrgStore(pool)
	projectStore := persistence.NewProjectStore(pool)
	projectAPI := projectshttp.New(projectshttp.Deps{
		Store: projectStore,
		Orgs:  orgStore,
		Authz: authz.NewMatrixEngine(),
	})
	projectSvc := projectAPI.Service()
	policyStore := persistence.NewPolicyStore(pool)
	policyAPI := policyhttp.New(policyhttp.Deps{
		Store:    policyStore,
		Orgs:     orgStore,
		Projects: projectStore,
	})
	stateStore := persistence.NewStateStore(pool)
	branchSvc := branches.NewService(persistence.NewBranchStore(pool))
	statesSvc := states.NewService(stateStore, newCommitGuard(t))
	objects := persistence.NewScientificObjectStore(pool)
	rsgSvc := rsg.NewService(rsg.Deps{
		Projects:  projectSvc,
		Branches:  branchSvc,
		States:    statesSvc,
		Latest:    stateStore,
		Objects:   objects,
		Relations: persistence.NewRelationStore(pool),
		Authz:     authz.NewMatrixEngine(),
		Schemas:   reg,
		Events:    events.Recorder{},
	})
	diffSvc := diffs.NewService(stateStore, persistence.NewManifestStore(pool), persistence.NewPullRequestStore(pool))
	resolutionSvc := resolutions.NewService(diffSvc, resolutions.NewPGStore(pool), projectSvc, authz.NewMatrixEngine())
	branchStore := persistence.NewBranchStore(pool)
	prStore := persistence.NewPullRequestStore(pool)
	prSvc := pullrequests.NewService(prStore)
	// The Research Owners stack and the review submission command, wired as
	// cmd/api wires them. Without them the proposal could only reach
	// merge_ready by a state assignment this test performs — which is exactly
	// the shortcut the task book names as an anti-example.
	routingSvc := responsibilities.NewService(responsibilities.Deps{
		Rules:     persistence.NewResponsibilityStore(pool),
		Projects:  projectStore,
		Members:   projectSvc,
		PRs:       prStore,
		Branches:  branchStore,
		Diffs:     prdiff.NewService(prStore, branchStore, diffSvc),
		Policies:  policyStore,
		Evaluator: policy.NewRuleEvaluator(),
	})
	reviewsSvc := reviews.NewService(reviews.Deps{
		Repo:           persistence.NewReviewStore(pool),
		Projects:       projectSvc,
		Authz:          authz.NewMatrixEngine(),
		Responsibility: routingSvc,
		Routing:        routingSvc,
	})
	mergeSvc := merge.NewService(merge.Deps{
		Store:     persistence.NewSemanticMergeStore(pool),
		Diffs:     diffSvc,
		Plans:     resolutionSvc,
		Commits:   statesSvc,
		Objects:   objects,
		Relations: persistence.NewRelationStore(pool),
		Projects:  projectSvc,
		Authz:     authz.NewMatrixEngine(),
		Checks: prchecks.NewService(prchecks.Deps{
			PRs:      persistence.NewPullRequestStore(pool),
			Projects: projectStore,
			States:   stateStore,
			Branches: persistence.NewBranchStore(pool),
			Manifest: persistence.NewManifestStore(pool),
			Policies: policyStore,
			Engine:   integrity.New(reg),
		}),
		Policies: policyAPI.Service(),
		Rules:    policy.NewRuleEvaluator(),
		Events:   events.Recorder{},
		// The abort reader is wired exactly as cmd/api wires it: the merge
		// copies docs/46:7's record onto the row it lands on main.
		Aborts: objects,
	})
	abortSvc := aborts.NewService(aborts.Deps{
		Members:      persistence.NewProjectStore(pool),
		Authz:        authz.NewMatrixEngine(),
		Objects:      objects,
		Branches:     branchSvc,
		PullRequests: prSvc,
		Commits:      statesSvc,
		Events:       events.Recorder{},
	})

	authAPI := authhttp.New(authhttp.Deps{
		Users:    persistence.NewCredentialStore(pool),
		Sessions: memstore.NewSessions(),
		Limiter:  memstore.NewLimiter(),
		Cfg: authn.Config{
			WebOrigin:          "http://web.test",
			SessionTTL:         time.Hour,
			LoginLimitPerEmail: 1000,
			LoginLimitPerIP:    10000,
			// The suite signs up several users per fixture against one
			// loopback address; the signup limiter is production's, and
			// leaving it at its production value would rate-limit the
			// fixture rather than the product (the freeze e2e's value).
			SignupLimitPerIP: 10000,
			LoginWindow:      time.Minute,
		},
		Secure: false,
		Audit:  persistence.NewAuditStore(pool),
	})
	apiMux := http.NewServeMux()
	authAPI.Register(apiMux)
	apiMux.Handle("/api/v1/projects", projectAPI.Routes())
	apiMux.Handle("/api/v1/projects/", projectAPI.Routes())
	rsghttp.New(rsghttp.Deps{Service: rsgSvc}).Register(apiMux)
	pullrequestshttp.New(pullrequestshttp.Deps{
		PullRequests: prSvc,
		Checks: prchecks.NewService(prchecks.Deps{
			PRs:      persistence.NewPullRequestStore(pool),
			Projects: projectStore,
			States:   stateStore,
			Branches: persistence.NewBranchStore(pool),
			Manifest: persistence.NewManifestStore(pool),
			Policies: policyStore,
			Engine:   integrity.New(reg),
		}),
	}).Register(apiMux)
	mergehttp.New(mergehttp.Deps{Command: mergeSvc, Projects: projectSvc}).Register(apiMux)
	aborthttp.New(aborthttp.Deps{Command: abortSvc}).Register(apiMux)
	reviewhttp.New(reviewhttp.Deps{Service: reviewsSvc, Projects: projectSvc}).Register(apiMux)
	ts := httptest.NewServer(authAPI.Guard(apiMux))
	t.Cleanup(ts.Close)

	// Every acting browser is created through the real signup endpoint: the
	// cookies and CSRF tokens the refusals below ride on are the ones that
	// endpoint minted, not values this test made up.
	alice, aliceID := signup(t, ts.URL, "abort-owner@example.com", "abort-owner")
	viewer, viewerID := signup(t, ts.URL, "abort-viewer@example.com", "abort-viewer")
	contrib, contribID := signup(t, ts.URL, "abort-contrib@example.com", "abort-contrib")
	stranger, _ := signup(t, ts.URL, "abort-stranger@example.com", "abort-stranger")

	org, _, err := orgStore.CreateOrganization(ctx, domain.Organization{
		Slug: "abort-gov", Name: "Abort Governance",
	}, aliceID, todayUTC())
	if err != nil {
		t.Fatalf("create the fixture organization: %v", err)
	}
	project, _, err := projectStore.CreateProject(ctx, domain.Project{
		OrganizationID:  &org.ID,
		Slug:            "abort-gov",
		Name:            "Abort Governance",
		Purpose:         "T0602 abort main object e2e",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, aliceID)
	if err != nil {
		t.Fatalf("create the fixture project: %v", err)
	}
	for _, seed := range []struct{ userID, role string }{{viewerID, "viewer"}, {contribID, "contributor"}} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, $3)`,
			project.ID, seed.userID, seed.role); err != nil {
			t.Fatalf("seed the %s membership: %v", seed.role, err)
		}
	}
	// The routing data the required-review calculation runs on: the changes
	// this project's abort proposals carry are claims, and the viewer holds
	// the label the rule routes them to. Both are fixture data — no route in
	// this build writes a Research Owners rule or an assignment (the same
	// note merge_governance_e2e_test.go:430 records); the REVIEWS themselves
	// below are HTTP submissions.
	//
	// The label has to be held by BOTH reviewers below: the scientific
	// requirement is signed under the routed responsibility, and the
	// whole-proposal integrity requirement accepts only a review recorded
	// under a label the routing resolved (domain.EvaluateRequiredReviews:
	// "never an empty label — a reviewer who holds no responsibility did not
	// sign under one"), which is the same reason the governance e2e assigns
	// the label to both of its reviewers.
	aliceUser := domain.User{ID: aliceID}
	for _, userID := range []string{viewerID, aliceID} {
		if _, err := routingSvc.Assign(ctx, aliceUser, project.ID, userID, "Data Reviewer"); err != nil {
			t.Fatalf("assign the reviewer responsibility to %s: %v", userID, err)
		}
	}
	if _, err := routingSvc.AddRule(ctx, aliceUser, project.ID, responsibilities.AddRuleInput{
		MatchKind:      domain.ResearchOwnerMatchObjectType,
		MatchValue:     "claim",
		Responsibility: "Data Reviewer",
	}); err != nil {
		t.Fatalf("route claim changes to the reviewer: %v", err)
	}
	// The merge's governance check reads main_protected from the policy in
	// force and refuses a policy that does not mention it.
	seedProjectPolicy(t, ctx, policyStore, project, aliceID)

	mainBranch, err := rsgSvc.CreateBranch(ctx, domain.User{ID: aliceID}, project.ID, rsg.CreateBranchInput{
		Name: "main", BaseRef: "", Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create the main branch: %v", err)
	}

	return &abortFixture{
		pool:       pool,
		server:     ts.URL,
		project:    project,
		org:        org,
		mainBranch: mainBranch,
		alice:      alice,
		aliceID:    aliceID,
		clients: map[string]*testUserClient{
			"abort-owner@example.com":    alice,
			"abort-viewer@example.com":   viewer,
			"abort-contrib@example.com":  contrib,
			"abort-stranger@example.com": stranger,
		},
		prs:     prSvc,
		merges:  mergeSvc,
		aborts:  abortSvc,
		routing: routingSvc,
	}
}

// activeMainObject writes an object onto main through the production RSG
// route — the state acceptance criterion 1 starts from — and returns its id
// and version id.
func (f *abortFixture) activeMainObject(t *testing.T, ctx context.Context, statement string) (objectID, versionID string) {
	t.Helper()
	status, body := wirePost(t, f.alice,
		branchObjectsPath(f.project.ID, f.mainBranch.ID),
		fmt.Sprintf(`{"object_type":"claim","payload":%s}`, mergeMainGateClaim(statement)))
	if status != http.StatusCreated {
		t.Fatalf("write the fixture object onto main = %d: %s", status, body)
	}
	var obj struct {
		ID        string `json:"id"`
		VersionID string `json:"version_id"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode the object payload: %v (%s)", err, body)
	}
	if obj.ID == "" || obj.VersionID == "" {
		t.Fatalf("the object route answered without an id or a version id: %s", body)
	}
	_ = ctx
	return obj.ID, obj.VersionID
}

// driveToMergeReady takes the proposal the abort command opened to the only
// state the merge engine accepts BY SUBMITTING ITS REVIEWS.
//
// It used to be two SetState calls (approved, merge_ready) after a direct
// RequestReview — the shape the task book names as an anti-example
// (freeze_main_e2e_test.go:849-853, pullrequest_test.go:331-336), and which
// would have let a proposal nobody reviewed reach main: docs/46:5's
// "review/approval if main object" is part of the record this test exists to
// observe. The walk now happens where production performs it: the first
// submission satisfies the scientific requirement the routing resolved, the
// second (the whole-proposal integrity judgment) is the one that satisfies
// the calculation, and the review store's own projection performs
// review_required -> approved -> merge_ready inside it.
//
// The one direct call left is RequestReview, for the reason
// merge_governance_e2e_test.go:511-514 records: docs/43's open ->
// review_required transition has no route in this build (no contract
// operation, no request_review cell in specs/policies/permissions-matrix.csv
// — inventing one would be inventing a product action).
func (f *abortFixture) driveToMergeReady(t *testing.T, ctx context.Context, number int64) {
	t.Helper()
	if _, err := f.prs.RequestReview(ctx, f.project.ID, number); err != nil {
		t.Fatalf("request review on PR %d: %v", number, err)
	}
	// The two review dimensions the calculation asks for, each submitted over
	// the review route by a caller the matrix admits: the viewer signs the
	// scientific judgment under the responsibility the rule routed the change
	// to, and the owner signs the proposal-wide integrity judgment. Both rows
	// record the head they reviewed, which is what the projection reads.
	for _, submission := range []struct {
		client *testUserClient
		kind   string
	}{
		{f.viewer(t), "scientific"},
		{f.alice, "integrity"},
	} {
		body := fmt.Sprintf(`{"kind":%q,"decision":"approved","body":"reviewed for the T0602 abort e2e"}`, submission.kind)
		resp := submission.client.do(t, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/pull-requests/%d/reviews", f.project.ID, number), body)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("submit the %s review of PR %d = %d: %s", submission.kind, number, resp.StatusCode, readAll(t, resp))
		}
	}
	// The state the merge will read is the store's, not this test's: if the
	// reviews did not satisfy the calculation the proposal is still parked
	// and the merge below would refuse it. It is read out of the ROW, the way
	// every other assertion in this file reads its subject — not out of a
	// service's return value, which would answer from the same row but leave
	// "the machine walked" resting on the call rather than on the record.
	var state string
	if err := f.pool.QueryRow(ctx,
		`SELECT state FROM pull_requests WHERE project_id = $1::uuid AND number = $2`,
		f.project.ID, number).Scan(&state); err != nil {
		t.Fatalf("read PR %d after its reviews: %v", number, err)
	}
	if state != string(domain.PullRequestStateMergeReady) {
		t.Fatalf("PR %d is %q after its reviews, want %q: the review submissions did not walk the machine",
			number, state, domain.PullRequestStateMergeReady)
	}
	if f.routing == nil {
		t.Fatal("the fixture has no routing stack; the reviews above could not have been routed")
	}
}

// -------------------------------------------------------------------- test

// TestAbortProposalEndToEnd is T0602's required end-to-end test.
func TestAbortProposalEndToEnd(t *testing.T) {
	ctx := testCtx(t)
	f := newAbortFixture(t, ctx)
	pool, project, alice := f.pool, f.project, f.alice

	objectID, v1ID := f.activeMainObject(t, ctx, "a claim that will be aborted")

	// The state the abort must NOT change, read out of storage before it
	// runs. Every column of v1 is captured here and compared after the
	// abort, in criterion 4 below.
	v1Before := readVersion(t, ctx, pool, v1ID)
	if v1Before.lifecycleState != string(domain.LifecycleActive) {
		t.Fatalf("the fixture object is not active on main: lifecycle_state = %q", v1Before.lifecycleState)
	}
	if v1Before.branchID == nil || *v1Before.branchID != f.mainBranch.ID {
		t.Fatalf("the fixture object's version is not on main: branch_id = %v", v1Before.branchID)
	}
	// Main's head pointer, read out of storage after the object was written
	// and before the abort runs (branches.base_state_id is the head pointer
	// the platform's own GetBranchHead reads — see branch_store.go:210).
	mainHeadBefore := branchHead(t, ctx, pool, f.mainBranch.ID)

	const (
		reason      = "superseded_by_better_evidence"
		explanation = "The 2026-08-14 re-run of the sorption measurement contradicts this claim's band gap, and the sample was later found to be oxidised."
		replacement = "object_version:11111111-1111-4111-8111-111111111111"
		key         = "abort-e2e-proposal-0001"
	)

	// ---- (1) + (2) The abort proposal, over the production route. The
	// lifecycle it reports is the ROW's; the object on main is not aborted
	// until the PR below merges, and the response says which PR that is.
	wire := abortThroughTheEndpoint(t, alice, project.ID, objectID, key,
		abortBody(v1ID, reason, explanation, replacement))
	if wire.Replayed {
		t.Fatal("(1) the first abort reported itself as a replay")
	}
	if wire.LifecycleState != string(domain.LifecycleAborted) {
		t.Fatalf("(1) the proposal's version row is in lifecycle %q, want aborted", wire.LifecycleState)
	}
	if wire.AbortedVersionID != v1ID || wire.AbortedVersionNo != v1Before.versionNo {
		t.Fatalf("(2) the proposal names version %s v%d as the aborted one, want %s v%d",
			wire.AbortedVersionID, wire.AbortedVersionNo, v1ID, v1Before.versionNo)
	}
	if wire.VersionID == v1ID || wire.VersionNo <= v1Before.versionNo {
		t.Fatalf("(1) the abort did not append: it reported version %s v%d against the aborted %s v%d",
			wire.VersionID, wire.VersionNo, v1ID, v1Before.versionNo)
	}
	if wire.BranchID == "" || wire.BranchName == "" {
		t.Fatalf("(1) the proposal names no branch: %+v", wire)
	}
	if wire.PullRequestNumber == 0 {
		t.Fatalf("(1) the proposal opened no Research PR: %+v", wire)
	}
	// (2) docs/46:7's fields, asserted per item.
	if wire.ReasonCode != reason {
		t.Fatalf("(2) reason_code = %q, want %q", wire.ReasonCode, reason)
	}
	if wire.Explanation != explanation {
		t.Fatalf("(2) explanation = %q, want %q", wire.Explanation, explanation)
	}
	if wire.ReplacementRef != replacement {
		t.Fatalf("(2) replacement_ref = %q, want %q", wire.ReplacementRef, replacement)
	}
	if wire.DecidedBy != f.aliceID {
		t.Fatalf("(2) decided_by = %q, want the aborting actor %q", wire.DecidedBy, f.aliceID)
	}
	if wire.DecidedAt == "" {
		t.Fatalf("(2) decided_at is empty: %+v", wire)
	}

	// ---- (2) The same fields as STORED, read column by column.
	stored := readVersion(t, ctx, pool, wire.VersionID)
	if stored.lifecycleState != string(domain.LifecycleAborted) {
		t.Fatalf("(2) the stored version's lifecycle_state = %q, want aborted", stored.lifecycleState)
	}
	if stored.abortReasonCode == nil || *stored.abortReasonCode != reason {
		t.Fatalf("(2) stored abort_reason_code = %v, want %q", stored.abortReasonCode, reason)
	}
	if stored.abortExplanation == nil || *stored.abortExplanation != explanation {
		t.Fatalf("(2) stored abort_explanation = %v, want the explanation", stored.abortExplanation)
	}
	if stored.abortReplacement == nil || *stored.abortReplacement != replacement {
		t.Fatalf("(2) stored abort_replacement_ref = %v, want %q", stored.abortReplacement, replacement)
	}
	if stored.abortedBy == nil || *stored.abortedBy != f.aliceID {
		t.Fatalf("(2) stored aborted_by = %v, want the aborting actor %q", stored.abortedBy, f.aliceID)
	}
	if stored.abortedAt == nil {
		t.Fatal("(2) stored aborted_at is NULL: docs/46:7's time was not recorded")
	}
	if stored.abortRequestKey == nil || *stored.abortRequestKey != key {
		t.Fatalf("(2) stored abort_request_key = %v, want the request's Idempotency-Key %q", stored.abortRequestKey, key)
	}
	if !bytes.Equal(stored.payload, v1Before.payload) {
		t.Fatalf("(2) the abort changed the payload: %s vs %s", stored.payload, v1Before.payload)
	}

	// ---- (1) The abort reached no further than the proposal branch. Main's
	// head pointer has not moved and v1's lifecycle on main is still
	// 'active': docs/46:9's branch → PR → merge is not a formality, and
	// until the PR below merges there is no abort on main at all.
	//
	// Note what is NOT asserted here: the OBJECT's version counter
	// (scientific_objects.current_version_no) does advance, because it
	// numbers the object's whole version log and the log just grew a row on
	// the proposal branch. That is the log's numbering, not main's state,
	// and the branch head is what docs/46:9 governs.
	if mainHeadNow := branchHead(t, ctx, pool, f.mainBranch.ID); mainHeadNow != mainHeadBefore {
		t.Fatalf("(1) main's head moved to %s during the proposal; the abort must not write to main", mainHeadNow)
	}
	if row := readVersion(t, ctx, pool, v1ID); row.lifecycleState != string(domain.LifecycleActive) {
		t.Fatalf("(1) v1's lifecycle on main is now %q; the proposal must not change it", row.lifecycleState)
	}

	// ---- (2b) "no replacement" is NULL, not "". A second object is aborted
	// with no replacement_ref at all: the row must distinguish never-given
	// from given-and-empty.
	otherID, otherV1 := f.activeMainObject(t, ctx, "a claim with no replacement")
	noRepl := abortThroughTheEndpoint(t, alice, project.ID, otherID, "abort-e2e-norepl-0001",
		abortBody(otherV1, "withdrawn", "Withdrawn after the group's own replication failed to reproduce it.", ""))
	if noRepl.ReplacementRef != "" {
		t.Fatalf("(2b) an abort with no replacement_ref answered %q, want empty", noRepl.ReplacementRef)
	}
	if row := readVersion(t, ctx, pool, noRepl.VersionID); row.abortReplacement != nil {
		t.Fatalf("(2b) an abort with no replacement_ref stored %q, want NULL (\"no replacement\" and \"a replacement that is nothing\" are different facts)",
			*row.abortReplacement)
	}

	// ---- (8) The proposal branch and the PR are the ONLY new rows on the
	// main side of the ledger: one aborted version, one audit row, one event.
	if audits, outbox := abortCounts(t, ctx, pool, objectID); audits != 1 || outbox != 1 {
		t.Fatalf("(8) one abort wrote %d audit rows and %d events, want 1 and 1", audits, outbox)
	}

	// ---- (9) The idempotent replay: the same key answers with the proposal
	// it already created and writes nothing at all.
	replay := abortThroughTheEndpoint(t, alice, project.ID, objectID, key,
		abortBody(v1ID, reason, explanation, replacement))
	if !replay.Replayed {
		t.Fatal("(9) the repeated request did not report itself as a replay")
	}
	if replay.VersionID != wire.VersionID || replay.VersionNo != wire.VersionNo {
		t.Fatalf("(9) the replay answered version %s v%d, want the first answer's %s v%d",
			replay.VersionID, replay.VersionNo, wire.VersionID, wire.VersionNo)
	}
	if replay.PullRequestNumber != wire.PullRequestNumber {
		t.Fatalf("(9) the replay answered PR %d, want the first answer's %d", replay.PullRequestNumber, wire.PullRequestNumber)
	}
	if replay.BranchID != wire.BranchID || replay.BranchName != wire.BranchName {
		t.Fatalf("(9) the replay answered branch %s/%s, want %s/%s",
			replay.BranchID, replay.BranchName, wire.BranchID, wire.BranchName)
	}
	if replay.AbortedVersionID != v1ID || replay.ReasonCode != reason ||
		replay.Explanation != explanation || replay.ReplacementRef != replacement ||
		replay.DecidedBy != f.aliceID {
		t.Fatalf("(9) the replay answered a different record: %+v", replay)
	}
	if audits, outbox := abortCounts(t, ctx, pool, objectID); audits != 1 || outbox != 1 {
		t.Fatalf("(9) the replay wrote a second record: %d audit rows and %d events, want 1 and 1", audits, outbox)
	}
	if n := len(versionsOfObject(t, ctx, pool, objectID)); n != 2 {
		t.Fatalf("(9) the object has %d version rows after the replay, want 2 (v1 + one abort)", n)
	}

	// ---- (4) History is byte-identical. v1 is read back column by column
	// and compared with the snapshot taken before the abort, and the abort is
	// shown to have produced a NEW row rather than an edit of v1.
	v1After := readVersion(t, ctx, pool, v1ID)
	if v1After.describe() != v1Before.describe() {
		t.Fatalf("(4) the aborted version was REWRITTEN by the abort:\n before: %s\n  after: %s",
			v1Before.describe(), v1After.describe())
	}
	all := versionsOfObject(t, ctx, pool, objectID)
	if len(all) != 2 || all[0].id != v1ID || all[1].id != wire.VersionID {
		t.Fatalf("(4) the abort did not append a new version row: got %d rows", len(all))
	}
	if all[0].versionNo != v1Before.versionNo {
		t.Fatalf("(4) v1's version number changed: %d -> %d", v1Before.versionNo, all[0].versionNo)
	}

	// ---- (7) The user-visible audit row: docs/26 lists abort among the
	// highest-risk audited actions and docs/53 makes the row part of the
	// action.
	audit := readAbortAudit(t, ctx, pool, objectID)
	if audit.actorID != f.aliceID {
		t.Fatalf("(7) the audit row's actor = %q, want %q", audit.actorID, f.aliceID)
	}
	var after map[string]any
	if err := json.Unmarshal(audit.after, &after); err != nil {
		t.Fatalf("(7) decode the audit row's after_summary: %v (%s)", err, audit.after)
	}
	if after["reason_code"] != reason {
		t.Fatalf("(7) the audit row's reason_code = %v, want %q", after["reason_code"], reason)
	}
	if after["explanation"] != explanation {
		t.Fatalf("(7) the audit row's explanation = %v, want the explanation", after["explanation"])
	}
	if after["replacement_ref"] != replacement {
		t.Fatalf("(7) the audit row's replacement_ref = %v, want %q", after["replacement_ref"], replacement)
	}
	if after["lifecycle_state"] != string(domain.LifecycleAborted) {
		t.Fatalf("(7) the audit row's after lifecycle_state = %v, want aborted", after["lifecycle_state"])
	}
	if after["idempotency_key"] != key {
		t.Fatalf("(7) the audit row does not name the request that produced it: %v", after["idempotency_key"])
	}
	// The other object's audit row, which had no replacement ref, must not
	// carry the key at all — the same distinction the version row's NULL
	// keeps.
	var noReplAfter map[string]any
	noReplAudit := readAbortAudit(t, ctx, pool, otherID)
	if err := json.Unmarshal(noReplAudit.after, &noReplAfter); err != nil {
		t.Fatalf("(7) decode the no-replacement audit row: %v (%s)", err, noReplAudit.after)
	}
	if _, present := noReplAfter["replacement_ref"]; present {
		t.Fatalf("(7) an abort with no replacement ref wrote one into its audit row: %v", noReplAfter["replacement_ref"])
	}

	// ---- (5) Every class the matrix does not admit is refused, before any
	// target lookup, with a permission-class code; and an abort of an object
	// that does not exist is indistinguishable from an abort of one that
	// does.
	missing := "00000000-0000-4000-8000-00000000dead"
	for _, tc := range []struct {
		name   string
		client *testUserClient
		want   int
	}{
		{"viewer", f.viewer(t), http.StatusForbidden},
		{"contributor", f.contributor(t), http.StatusForbidden},
		{"non-member", f.stranger(t), http.StatusForbidden},
		{"anonymous", newTestUserClient(f.server), http.StatusUnauthorized},
	} {
		t.Run("abort refused for the "+tc.name, func(t *testing.T) {
			status, raw, _, err := abortOnce(tc.client.client, f.server, project.ID, objectID,
				tc.client.csrf, "abort-denied-key-0001", abortBody(v1ID, reason, explanation, ""))
			if err != nil {
				t.Fatalf("abort as the %s: %v", tc.name, err)
			}
			if status != tc.want {
				t.Fatalf("the %s abort = %d: %s", tc.name, status, raw)
			}
			if tc.want == http.StatusForbidden {
				var env errorEnvelope
				if err := json.Unmarshal([]byte(raw), &env); err != nil {
					t.Fatalf("decode the %s refusal: %v (%s)", tc.name, err, raw)
				}
				if env.Code != aborts.CodeForbidden {
					t.Fatalf("the %s refusal code = %q, want %q", tc.name, env.Code, aborts.CodeForbidden)
				}
			}
		})
	}

	// The existence-hiding pair: the same unauthorized caller, the same
	// request shape, one object that exists and one that does not. The two
	// answers must be identical — same status, same code — or the refusal
	// would be a lookup wearing a permission's clothes.
	realStatus, realBody, _, err := abortOnce(f.viewer(t).client, f.server, project.ID, objectID,
		f.viewer(t).csrf, "abort-hiding-key-0001", abortBody(v1ID, reason, explanation, ""))
	if err != nil {
		t.Fatalf("probe the refusal for a real object: %v", err)
	}
	ghostStatus, ghostBody, _, err := abortOnce(f.viewer(t).client, f.server, project.ID, missing,
		f.viewer(t).csrf, "abort-hiding-key-0001", abortBody(v1ID, reason, explanation, ""))
	if err != nil {
		t.Fatalf("probe the refusal for a missing object: %v", err)
	}
	if realStatus != ghostStatus || realBody != ghostBody {
		t.Fatalf("(5) an unauthorized abort of a real object (%d: %s) is distinguishable from one of a missing object (%d: %s)",
			realStatus, realBody, ghostStatus, ghostBody)
	}
	// The same pair for a caller the matrix ADMITS: an owner aborting an
	// object that does not exist has to answer "not found" rather than a
	// permission refusal, or the refusal above would be hiding nothing but
	// would still be reported for the wrong reason.
	if status, _, _, err := abortOnce(alice.client, f.server, project.ID, missing, alice.csrf,
		"abort-owner-missing-0001", abortBody(v1ID, reason, explanation, "")); err != nil {
		t.Fatalf("probe the owner's abort of a missing object: %v", err)
	} else if status != http.StatusNotFound {
		t.Fatalf("(5) an owner aborting a missing object = %d, want 404", status)
	}

	// ---- (5) The agent half, on both lines of defence. The FIRST line is
	// the domain backstop, asked directly with the project's OWNER as the
	// agent's user — an actor the matrix admits — so the refusal cannot be
	// the matrix's doing. The SECOND is the matrix's own answer for the
	// agent class, asked independently.
	agentActor := aborts.Actor{User: domain.User{ID: f.aliceID}, IsAgent: true}
	if _, err := f.aborts.AbortProposal(ctx, agentActor, aborts.Input{
		ProjectID:        project.ID,
		ObjectID:         otherID,
		ObjectVersionRef: otherV1,
		ReasonCode:       "agent_attempt",
		Explanation:      "an agent must not be able to reach this",
		IdempotencyKey:   "abort-agent-key-0001",
	}); err == nil {
		t.Fatal("(5) an agent aborted a main object — the domain backstop did not fire")
	} else {
		var refused *aborts.AgentNotPermittedError
		if !errors.As(err, &refused) {
			t.Fatalf("(5) the agent refusal is %v, want *aborts.AgentNotPermittedError", err)
		}
		if refused.Code() != aborts.CodeAgentDenied {
			t.Fatalf("(5) the agent refusal code = %q, want %q", refused.Code(), aborts.CodeAgentDenied)
		}
	}
	ownerRole := domain.ProjectRoleOwner
	decision, err := authz.NewMatrixEngine().Authorize(ctx, authz.Request{
		Action: authz.ActionAbortMainObject,
		Class:  authz.ClassOf(true, &ownerRole, true),
	})
	if err != nil {
		t.Fatalf("(5) authorize abort_main_object for an agent: %v", err)
	}
	if decision.Permits() {
		t.Fatal("(5) the permission matrix permits abort_main_object for an agent — the second line of defence is gone")
	}
	// What the agent emitted is not an abort: no version of either object
	// carries an abort it produced, and the counts above are unchanged.
	for _, id := range []string{objectID, otherID} {
		for _, row := range versionsOfObject(t, ctx, pool, id) {
			if row.abortRequestKey != nil && *row.abortRequestKey == "abort-agent-key-0001" {
				t.Fatalf("(5) an agent's refused proposal left a version row in storage: %s", row.id)
			}
		}
	}

	// ---- (6) The proposal merges onto main, and the abort takes effect
	// there. The merge runs over the production route with the real guard —
	// mergeThroughTheEndpoint is this package's shared helper for that
	// request (merge_governance_e2e_test.go), so the route, the header
	// contract and the status are checked once, the same way, everywhere.
	f.driveToMergeReady(t, ctx, wire.PullRequestNumber)
	merged := mergeThroughTheEndpoint(t, alice, project.ID, wire.PullRequestNumber, "abort-merge-key-0001")
	if merged.Replayed {
		t.Fatal("(6) the merge of the abort proposal reported itself as a replay")
	}

	// The accepted state carries the abort: a NEW version row on main, in
	// lifecycle 'aborted', with docs/46:7's record COPIED onto it — the merge
	// materializes a version it did not decide, so it must carry the decision
	// rather than invent one.
	mainVersions := versionsOfObject(t, ctx, pool, objectID)
	if len(mainVersions) != 3 {
		t.Fatalf("(6) the object has %d version rows after the merge, want 3 (v1 + proposal + accepted)", len(mainVersions))
	}
	accepted := mainVersions[2]
	if accepted.lifecycleState != string(domain.LifecycleAborted) {
		t.Fatalf("(6) the accepted version's lifecycle_state = %q, want aborted", accepted.lifecycleState)
	}
	if accepted.branchID == nil || *accepted.branchID != f.mainBranch.ID {
		t.Fatalf("(6) the accepted version is not on main: branch_id = %v", accepted.branchID)
	}
	if accepted.abortReasonCode == nil || *accepted.abortReasonCode != reason {
		t.Fatalf("(6) the accepted version lost the reason code: %v", accepted.abortReasonCode)
	}
	if accepted.abortExplanation == nil || *accepted.abortExplanation != explanation {
		t.Fatalf("(6) the accepted version lost the explanation: %v", accepted.abortExplanation)
	}
	if accepted.abortedBy == nil || *accepted.abortedBy != f.aliceID {
		t.Fatalf("(6) the accepted version lost the actor: %v", accepted.abortedBy)
	}
	if accepted.abortedAt == nil {
		t.Fatalf("(6) the accepted version lost the decision time")
	}
	if accepted.abortRequestKey != nil {
		t.Fatalf("(6) the accepted version carries the request's Idempotency-Key (%v); the key belongs to the request that was made, not to the row the merge landed",
			*accepted.abortRequestKey)
	}
	if !bytes.Equal(accepted.payload, v1Before.payload) {
		t.Fatalf("(6) the accepted version's payload is not the aborted version's")
	}

	// ---- (4) v1 is STILL byte-identical after the merge. The whole point of
	// an append-only log is that the merge does not reach backwards either.
	if after := readVersion(t, ctx, pool, v1ID); after.describe() != v1Before.describe() {
		t.Fatalf("(4) the merge rewrote v1:\n before: %s\n  after: %s", v1Before.describe(), after.describe())
	}

	// ---- (6) The event the merge added is the merge's; the abort's event
	// count is still exactly one.
	if audits, outbox := abortCounts(t, ctx, pool, objectID); audits != 1 || outbox != 1 {
		t.Fatalf("(6) after the merge the abort's records number %d audits and %d events, want 1 and 1", audits, outbox)
	}
	var payload map[string]any
	var rawPayload []byte
	if err := pool.QueryRow(ctx,
		`SELECT payload FROM outbox_events WHERE event_type = $1 AND payload->>'object_id' = $2`,
		"scientific_object.aborted", objectID).Scan(&rawPayload); err != nil {
		t.Fatalf("(6) read the abort event's payload: %v", err)
	}
	if err := json.Unmarshal(rawPayload, &payload); err != nil {
		t.Fatalf("(6) decode the abort event's payload: %v (%s)", err, rawPayload)
	}
	if payload["reason_code"] != reason {
		t.Fatalf("(6) the event's reason_code = %v, want %q", payload["reason_code"], reason)
	}
	if payload["object_type"] != "claim" {
		t.Fatalf("(6) the event's object_type = %v, want claim", payload["object_type"])
	}
	if _, present := payload["explanation"]; present {
		t.Fatalf("(6) the event payload carries the free-text explanation; it carries identity and reference only: %v", payload)
	}
}

// viewer/contributor/stranger are the fixture's non-owner clients, resolved
// once. They are read back out of the fixture rather than re-signed-up, so
// every refusal below is issued by the same session the fixture's memberships
// were seeded for.
func (f *abortFixture) viewer(t *testing.T) *testUserClient {
	t.Helper()
	return f.clientFor(t, "abort-viewer@example.com")
}
func (f *abortFixture) contributor(t *testing.T) *testUserClient {
	t.Helper()
	return f.clientFor(t, "abort-contrib@example.com")
}
func (f *abortFixture) stranger(t *testing.T) *testUserClient {
	t.Helper()
	return f.clientFor(t, "abort-stranger@example.com")
}

func (f *abortFixture) clientFor(t *testing.T, email string) *testUserClient {
	t.Helper()
	c, ok := f.clients[email]
	if !ok {
		t.Fatalf("no client for %s", email)
	}
	return c
}

// branchHead reads a branch's head state id straight out of the canonical
// row — the same pointer states.Service.GetBranchHead answers from.
func branchHead(t *testing.T, ctx context.Context, pool *pgxpool.Pool, branchID string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(base_state_id::text, '') FROM branches WHERE id = $1::uuid`, branchID).Scan(&id); err != nil {
		t.Fatalf("read branch %s's head: %v", branchID, err)
	}
	return id
}

// TestAbortProposalConcurrency is the concurrent half of the idempotency
// criterion: two requests carrying ONE Idempotency-Key race, and exactly one
// abort exists afterwards — one version row, one audit row, one event.
//
// The loser's own outcome is not asserted to be a success: it may be answered
// by the replay read (when the winner committed before the loser looked) or
// refused with the conflict code (when it did not). What is asserted is that
// the loser wrote NOTHING, which is the property the criterion names.
//
// When BOTH requests are answered 201, the two answers must describe the same
// abort — with one exception the contract itself names, the REPLAY GAP: the
// replayed answer may report pull_request_number 0 when it read the version
// before the winner had opened the proposal. sameAbortTwoWays below is that
// rule, spelled out with its citation; it is not a weakening of this branch,
// it is this branch written against the contract instead of against an
// assumption the contract does not make.
func TestAbortProposalConcurrency(t *testing.T) {
	ctx := testCtx(t)
	f := newAbortFixture(t, ctx)
	pool, project, alice := f.pool, f.project, f.alice

	objectID, v1ID := f.activeMainObject(t, ctx, "a claim two requests will race over")
	const key = "abort-concurrent-key-0001"
	body := abortBody(v1ID, "duplicate_run", "A duplicated measurement run supersedes this claim.", "")

	type outcome struct {
		status int
		wire   abortWire
		raw    string
		err    error
	}
	results := make([]outcome, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			status, raw, wire, err := abortOnce(alice.client, f.server, project.ID, objectID, alice.csrf, key, body)
			results[i] = outcome{status: status, wire: wire, raw: raw, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	winners := 0
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("concurrent abort %d: %v", i, r.err)
		}
		switch r.status {
		case http.StatusCreated:
			winners++
		case http.StatusConflict:
			// The loser that looked before the winner committed. Nothing
			// was written by it — asserted below by the counts.
		default:
			t.Fatalf("concurrent abort %d = %d: %s", i, r.status, r.raw)
		}
	}
	if winners == 0 {
		t.Fatal("neither concurrent request produced the proposal")
	}
	if winners == 2 {
		// Both requests were answered 201: one fresh proposal, one replay of
		// the version the other committed. They must be the same abort. The
		// only field they may disagree on is pull_request_number, and only in
		// the one window sameAbortTwoWays names.
		a, b := results[0].wire, results[1].wire
		if err := sameAbortTwoWays(a, b); err != nil {
			t.Fatalf("both concurrent requests reported a proposal, but different ones: %v (a=%+v, b=%+v)", err, a, b)
		}
	}
	if rows := versionsOfObject(t, ctx, pool, objectID); len(rows) != 2 {
		t.Fatalf("the race left %d version rows, want 2 (v1 + exactly one abort)", len(rows))
	}
	if audits, outbox := abortCounts(t, ctx, pool, objectID); audits != 1 || outbox != 1 {
		t.Fatalf("the race wrote %d audit rows and %d events, want exactly 1 and 1", audits, outbox)
	}
	var prs int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pull_requests WHERE project_id = $1::uuid AND creation_key = $2`,
		project.ID, key).Scan(&prs); err != nil {
		t.Fatalf("count the proposals the race opened: %v", err)
	}
	if prs != 1 {
		t.Fatalf("the race opened %d pull requests for one Idempotency-Key, want 1", prs)
	}
}

// sameAbortTwoWays reports whether two 201s answering ONE Idempotency-Key
// describe the same abort. It refuses every disagreement except one the
// contract itself names — the REPLAY GAP.
//
// VersionID, AbortedVersionID and BranchID are the abort's identity, and the
// contract fixes all three for both answers: the fresh one renders them from
// the row it just appended (resultFrom, internal/application/aborts/service.go:885,
// which starts from the replay's own renderer), and the replay renders them
// from that same row read back by key (resultFromRecorded, :906). Nothing
// here can differ without the race having produced two aborts, which is the
// property this test exists to hold.
//
// PullRequestNumber is the exception, and the contract states it verbatim
// (service.go:183-186):
//
//	PullRequestNumber is the Research PR's per-project number. It is 0 only
//	in the replay case where the first attempt committed the version and
//	died before opening the PR (see replay()).
//
// That is the REPLAY GAP, and the race this test drives can land in it: the
// version is committed inside the commit transaction (:535), the proposal is
// opened AFTER it and OUTSIDE that transaction (:600), so a request reading
// the key between the two sees a version and no proposal, replays, and is
// answered 201 with pull_request_number 0 and replayed true (:685-704 leave
// the number at zero and name no PR). The answer is the contract's own, not a
// second abort. So a disagreement is admitted only when the side holding the
// 0 is the REPLAYED one: a 0 on the fresh answer, a non-zero on a replayed
// one, or any other pair of numbers, is a violation and fails here.
func sameAbortTwoWays(a, b abortWire) error {
	switch {
	case a.VersionID != b.VersionID:
		return fmt.Errorf("the two answers name different abort versions (%q vs %q)", a.VersionID, b.VersionID)
	case a.AbortedVersionID != b.AbortedVersionID:
		return fmt.Errorf("the two answers report aborting different versions (%q vs %q)", a.AbortedVersionID, b.AbortedVersionID)
	case a.BranchID != b.BranchID:
		return fmt.Errorf("the two answers committed to different branches (%q vs %q)", a.BranchID, b.BranchID)
	case a.PullRequestNumber == b.PullRequestNumber:
		return nil
	}
	if isReplayGap(a) || isReplayGap(b) {
		return nil
	}
	return fmt.Errorf("the two answers name different proposals (%d vs %d) and neither is the replayed one reporting the gap "+
		"service.go:183-186 allows (replayed=%t carrying %d, replayed=%t carrying %d)",
		a.PullRequestNumber, b.PullRequestNumber, a.Replayed, a.PullRequestNumber, b.Replayed, b.PullRequestNumber)
}

// isReplayGap is the one shape service.go:183-186 admits on a replayed answer:
// it replayed a version whose proposal was not yet readable, so it has no
// proposal number to report.
func isReplayGap(w abortWire) bool { return w.Replayed && w.PullRequestNumber == 0 }

// TestAbortRefusesAKeyBorrowedFromAnotherObject is the fail-closed half of
// the idempotency rule: ONE Idempotency-Key, one project, TWO objects.
//
// The abort's own key index is per object (migration 00100:
// scientific_object_versions (object_id, abort_request_key)), but the same
// key is also the Research PR's creation key, and THAT index is per project
// (migration 00089: pull_requests (project_id, creation_key)) — with an
// adapter that ANSWERS a repeated key with the row the earlier request
// opened rather than refusing it (internal/persistence/pullrequest_store.go
// "the creation replay"). So the second object's abort finds no version
// under (second object, key), writes its own aborted version, and then asks
// for a proposal under a key that already names the FIRST object's PR. The
// answer must be a refusal, not that PR: a 201 carrying somebody else's
// proposal would tell the caller their abort was proposed when nothing
// proposes it.
func TestAbortRefusesAKeyBorrowedFromAnotherObject(t *testing.T) {
	ctx := testCtx(t)
	f := newAbortFixture(t, ctx)
	pool, project, alice := f.pool, f.project, f.alice

	firstID, firstV1 := f.activeMainObject(t, ctx, "the claim the key is first used for")
	secondID, secondV1 := f.activeMainObject(t, ctx, "a second claim in the same project")
	const key = "abort-shared-key-0001"

	first := abortThroughTheEndpoint(t, alice, project.ID, firstID, key,
		abortBody(firstV1, "superseded", "The abort that owns the key.", ""))
	if first.PullRequestNumber == 0 {
		t.Fatalf("the first abort opened no proposal: %+v", first)
	}
	firstBefore := readVersion(t, ctx, pool, first.VersionID)
	var prStateBefore, prBranchBefore string
	if err := pool.QueryRow(ctx,
		`SELECT state, source_branch_id::text FROM pull_requests WHERE project_id = $1::uuid AND number = $2`,
		project.ID, first.PullRequestNumber).Scan(&prStateBefore, &prBranchBefore); err != nil {
		t.Fatalf("read the first proposal: %v", err)
	}

	// The second object's abort, carrying the same key.
	status, raw, second, err := abortOnce(alice.client, f.server, project.ID, secondID, alice.csrf, key,
		abortBody(secondV1, "superseded", "A second object must not borrow the first's key.", ""))
	if err != nil {
		t.Fatalf("the second object's abort: %v", err)
	}
	if status != http.StatusConflict {
		t.Fatalf("a key already used in this project answered the second object's abort with %d: %s", status, raw)
	}
	if second.PullRequestNumber != 0 || second.VersionID != "" {
		t.Fatalf("the refused abort still reported a proposal: %+v", second)
	}

	// The first object's records are exactly as they were: the refusal is not
	// allowed to be a side effect on the proposal the key really names.
	if after := readVersion(t, ctx, pool, first.VersionID); after.describe() != firstBefore.describe() {
		t.Fatalf("the refusal rewrote the first object's aborted version:\n before: %s\n  after: %s",
			firstBefore.describe(), after.describe())
	}
	var prStateAfter, prBranchAfter string
	if err := pool.QueryRow(ctx,
		`SELECT state, source_branch_id::text FROM pull_requests WHERE project_id = $1::uuid AND number = $2`,
		project.ID, first.PullRequestNumber).Scan(&prStateAfter, &prBranchAfter); err != nil {
		t.Fatalf("re-read the first proposal: %v", err)
	}
	if prStateAfter != prStateBefore || prBranchAfter != prBranchBefore {
		t.Fatalf("the refusal moved the first object's proposal: %s/%s -> %s/%s",
			prStateBefore, prBranchBefore, prStateAfter, prBranchAfter)
	}
	var withKey int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pull_requests WHERE project_id = $1::uuid AND creation_key = $2`,
		project.ID, key).Scan(&withKey); err != nil {
		t.Fatalf("count the proposals carrying the key: %v", err)
	}
	if withKey != 1 {
		t.Fatalf("the project has %d proposals under one Idempotency-Key, want 1 (the first object's)", withKey)
	}

	// The residue, recorded rather than blessed: the refusal happens after
	// the version commit (no read-then-write was added ahead of it, because
	// that would break the single-winner property the idempotency criterion
	// rests on), so the second object is left with an aborted version on a
	// proposal branch no pull request proposes — the same shape as the
	// "committed, then died before opening the PR" gap this task's RESULT
	// discloses. Nothing in storage claims otherwise.
	secondVersions := versionsOfObject(t, ctx, pool, secondID)
	if len(secondVersions) != 2 {
		t.Fatalf("the refused object has %d version rows, want 2 (v1 + the aborted version the refusal left behind): %v",
			len(secondVersions), secondVersions)
	}
	residue := secondVersions[1]
	if residue.lifecycleState != string(domain.LifecycleAborted) || residue.branchID == nil {
		t.Fatalf("the residue is %q on branch %v, want an aborted version on the branch it forked", residue.lifecycleState, residue.branchID)
	}
	var proposing int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pull_requests WHERE project_id = $1::uuid AND source_branch_id = $2::uuid`,
		project.ID, *residue.branchID).Scan(&proposing); err != nil {
		t.Fatalf("count the proposals over the residue's branch: %v", err)
	}
	if proposing != 0 {
		t.Fatalf("the refused object's branch is the source of %d proposals; the refusal was supposed to open none", proposing)
	}
}
