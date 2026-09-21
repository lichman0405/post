package integration

// Task T0510: Knowledge workflow E2E — the required test `knowledge e2e`.
//
// The task's requirement is the chain
//
//	research question → hypothesis → experiment/calculation/dataset
//	  → evidence (including CONFLICTING evidence) → claim → finding
//
// over the provenance base that chain stands on (a material, the sample taken
// from it, and the protocol the experiment follows). Its acceptance is that
// the whole chain is BROWSABLE, VERSIONED, and lands on main through a PR
// MERGE. This test walks that chain over the production HTTP surface: every
// object, every relation, every evidence assertion, the version bump, the
// proposal, the two reviews, the merge and the publication are requests
// served by the wiring cmd/api ships, in front of the same guard, over a real
// PostgreSQL that `testdb.Setup` migrated to head.
//
// Three things this test deliberately does NOT do:
//
//   - It never calls an application service to produce a fact it then
//     asserts on. Services appear only where the platform has no route for
//     the act (seeding the project policy, assigning the routing label,
//     `RequestReview`, which docs/43 defines with no contract operation and
//     no permissions-matrix cell) or where the fact is fixture data (the
//     org/project rows, the two memberships, the two blob rows — this build
//     has no upload route and no "add member" route).
//   - It does not use the provider (Gitea). The merge runs with `Git` nil,
//     the wiring tests/integration/merge_test.go documents on purpose: the
//     saga records the Git step as pending and commits the PLATFORM truth.
//     The provider-side ref update is T0409's own coverage
//     (merge_governance_e2e_test.go), not this task's, and a
//     Gitea-dependent lane would turn this test into a skip in CI.
//   - It does not paper over a gap with a direct read. Every "browsable"
//     claim below is answered by an HTTP read a person or an agent would
//     make: the research outline (JSON and the page), the object detail
//     page's Relations / Provenance / Evidence tabs, the evidence read.
//
// The collision that makes the chain interesting is the evidence: one claim
// version carries a SUPPORTING assertion (from the experiment) and a
// CONTESTING one (from the dataset the experiment produced). The read model
// is forbidden from netting them (docs/10 §4, CLAUDE.md §9.13), so the test
// asserts both stances are present, in their own buckets, before and after
// the merge.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/evidencehttp"
	"github.com/lichman0405/post/cmd/api/knowledgehttp"
	"github.com/lichman0405/post/cmd/api/mergehttp"
	"github.com/lichman0405/post/cmd/api/policyhttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/cmd/api/provenancehttp"
	"github.com/lichman0405/post/cmd/api/pullrequestshttp"
	"github.com/lichman0405/post/cmd/api/reviewhttp"
	"github.com/lichman0405/post/cmd/api/rsghttp"

	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/diffs"
	"github.com/lichman0405/post/internal/application/evidencegraph"
	"github.com/lichman0405/post/internal/application/forks"
	"github.com/lichman0405/post/internal/application/knowledgepublish"
	"github.com/lichman0405/post/internal/application/merge"
	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/application/prchecks"
	"github.com/lichman0405/post/internal/application/prdiff"
	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/application/resolutions"
	"github.com/lichman0405/post/internal/application/responsibilities"
	"github.com/lichman0405/post/internal/application/reviews"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/schemaprofiles"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rights"
	"github.com/lichman0405/post/internal/rsg/integrity"
	"github.com/lichman0405/post/internal/rsg/schemareg"
)

// knowledgeE2ETaskID names the task-scoped database (test_<task>_<run>).
const knowledgeE2ETaskID = "T0510"

// knowledgeE2EResponsibility is the routing label the proposal's changes are
// routed to, and therefore the label the recorded reviews must carry.
const knowledgeE2EResponsibility = "Data Reviewer"

// knowledgeE2EChainTypes are the object types the chain writes. Every one of
// them needs a routing rule: a change no rule routes is Unrouted, and an
// unrouted proposal can never be approved (domain.RequiredReviews has no
// satisfiable verdict for it).
var knowledgeE2EChainTypes = []string{
	"research_question", "hypothesis", "material", "protocol", "sample",
	"experiment", "calculation", "dataset", "claim", "finding",
}

// ---------------------------------------------------------------------------
// The world: the production wiring, the guard, and the fixture facts.

type knowledgeE2EWorld struct {
	ts   *httptest.Server
	pool *pgxpool.Pool

	projectID   string
	aliceID     string
	alice       *testUserClient
	bob         *testUserClient
	carol       *testUserClient
	mainBranch  string
	probeBranch string

	// states is the state read the fixture uses for one thing: resolving
	// main's head, so a branch created after the merge forks from the state
	// acceptance landed on rather than from where main stood when the
	// fixture started.
	states *states.Service

	// prSvc is the pull-request command the routes above call. The test
	// reaches it for exactly one act: RequestReview, the docs/43 transition
	// this build has no route for (see the note at its call site).
	prSvc *pullrequests.Service
}

func newKnowledgeE2EWorld(t *testing.T, ctx context.Context) *knowledgeE2EWorld {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), knowledgeE2ETaskID)

	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}

	// ---- the platform graph, composed the way cmd/api composes it.
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
	branchStore := persistence.NewBranchStore(pool)
	branchSvc := branches.NewService(branchStore)
	statesSvc := states.NewService(stateStore, newCommitGuard(t))
	profilesSvc := schemaprofiles.NewService(schemaprofiles.Deps{
		Store:    persistence.NewSchemaProfileStore(pool),
		Projects: projectSvc,
		Schemas:  reg,
	})
	rsgSvc := rsg.NewService(rsg.Deps{
		Projects: projectSvc,
		Branches: branchSvc,
		States:   statesSvc,
		Latest:   stateStore,
		Objects:  persistence.NewScientificObjectStore(pool),
		// Queries is the branch-slice read the outline, the query route and
		// the object detail page are built from. Without it the research
		// outline answers 503 — a service with no query port knows nothing
		// about the project, and that is the fail-closed answer.
		Queries:  persistence.NewRSGQueryStore(pool),
		Profiles: persistence.NewProfileStore(pool),
		// The evidence-assertion slice (T0806). Wired because this chain
		// HAS evidence — a service without it refuses the write, which is
		// the fail-closed answer a chain with no evidence never meets.
		Evidence:       persistence.NewEvidenceStore(pool),
		Relations:      persistence.NewRelationStore(pool),
		SchemaProfiles: profilesSvc,
		Authz:          authz.NewMatrixEngine(),
		Schemas:        reg,
		Events:         events.Recorder{},
	})
	diffSvc := diffs.NewService(stateStore, persistence.NewManifestStore(pool), persistence.NewPullRequestStore(pool))
	resolutionSvc := resolutions.NewService(diffSvc, resolutions.NewPGStore(pool), projectSvc, authz.NewMatrixEngine())
	prStore := persistence.NewPullRequestStore(pool)
	prSvc := pullrequests.NewService(prStore)
	prDiffSvc := prdiff.NewService(prStore, branchStore, diffSvc)
	checksSvc := prchecks.NewService(prchecks.Deps{
		PRs:      prStore,
		Projects: projectStore,
		States:   stateStore,
		Branches: branchStore,
		Manifest: persistence.NewManifestStore(pool),
		Policies: policyStore,
		Engine:   integrity.New(reg),
	})
	// The open-pull-request route's command: the fork service, which
	// resolves the open_pr cell against the real membership and proposes
	// through the same pull-request command (T0410/T0804). Its two provider
	// ports stay nil — this build has no fork route, so nothing reaches
	// them.
	forksSvc := forks.NewService(forks.Deps{
		Projects:     projectSvc,
		Branches:     branchSvc,
		BranchWriter: rsgSvc,
		Forks:        persistence.NewForkStore(pool),
		PullRequests: prSvc,
		Authz:        authz.NewMatrixEngine(),
	})
	routingSvc := responsibilities.NewService(responsibilities.Deps{
		Rules:     persistence.NewResponsibilityStore(pool),
		Projects:  projectStore,
		Members:   projectSvc,
		PRs:       prStore,
		Branches:  branchStore,
		Diffs:     prDiffSvc,
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
	// The merge command. Git and RefGuard are nil ON PURPOSE, exactly as
	// merge_test.go's fixture documents: the provider-side merge adapter is
	// T0409's, so the saga records the step as not-done and commits the
	// platform truth rather than claiming a ref moved. What this test needs
	// from the merge is the accepted state on main, and that half is the
	// platform's own.
	mergeSvc := merge.NewService(merge.Deps{
		Store:     persistence.NewSemanticMergeStore(pool),
		Diffs:     diffSvc,
		Plans:     resolutionSvc,
		Commits:   statesSvc,
		Objects:   persistence.NewScientificObjectStore(pool),
		Relations: persistence.NewRelationStore(pool),
		Projects:  projectSvc,
		Authz:     authz.NewMatrixEngine(),
		Checks:    checksSvc,
		Policies:  policyAPI.Service(),
		Rules:     policy.NewRuleEvaluator(),
		Events:    events.Recorder{},
	})
	provenanceStore := provenancehttp.NewProjectionStore(pool)
	evidenceGraphSvc := evidencegraph.New(evidencegraph.Deps{
		Objects:    persistence.NewScientificObjectStore(pool),
		Assertions: persistence.NewEvidenceGraphStore(pool),
		Relations:  persistence.NewRelationStore(pool),
	})
	// The publication surface (T0805), wired the way cmd/api/main.go wires
	// it: the store answers the facts AND the publication read, so the
	// preview, the publish and the published document cannot disagree about
	// what the rows say. Its evidence port is the same evidence store the
	// assertion write path uses.
	knowledgePublishStore := persistence.NewKnowledgePublishStore(pool)
	knowledgeAPI := knowledgehttp.New(knowledgehttp.Deps{
		Publish: knowledgepublish.NewCommand(knowledgepublish.Deps{
			Members: persistence.NewProjectStore(pool),
			Store:   knowledgePublishStore,
			Authz:   authz.NewMatrixEngine(),
		}),
		Read:     knowledgePublishStore,
		Projects: projectAPI.Service(),
		Members:  projectAPI.Service(),
		Evidence: persistence.NewEvidenceStore(pool),
	})

	// ---- the HTTP server: the production guard in front of a mux the
	// production route assemblies populated, in cmd/api/main.go's order.
	// Only the session store and the rate limiter are the in-memory
	// implementations (release_e2e_test.go records the same exception).
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
		Secure: false,
		Audit:  persistence.NewAuditStore(pool),
	})
	apiMux := http.NewServeMux()
	authAPI.Register(apiMux)
	apiMux.Handle("/api/v1/projects", projectAPI.Routes())
	apiMux.Handle("/api/v1/projects/", projectAPI.Routes())
	pullrequestshttp.New(pullrequestshttp.Deps{
		PullRequests: prSvc,
		Create:       forksSvc,
		Checks:       checksSvc,
		Diff:         prDiffSvc,
		Projects:     projectSvc,
	}).Register(apiMux)
	reviewhttp.New(reviewhttp.Deps{Service: reviewsSvc, Projects: projectSvc}).Register(apiMux)
	mergehttp.New(mergehttp.Deps{Command: mergeSvc, Projects: projectSvc}).Register(apiMux)
	provenancehttp.New(provenancehttp.Deps{Store: provenanceStore, Gate: projectSvc}).Register(apiMux)
	evidencehttp.New(evidencehttp.Deps{Service: evidenceGraphSvc, Gate: projectSvc}).Register(apiMux)
	knowledgeAPI.Register(apiMux)
	rsghttp.New(rsghttp.Deps{
		Service:    rsgSvc,
		Provenance: provenanceStore,
		Evidence:   evidenceGraphSvc,
	}).Register(apiMux)
	ts := httptest.NewServer(authAPI.Guard(apiMux))
	t.Cleanup(ts.Close)

	// ---- the people, created through the real signup endpoint. Their
	// sessions and CSRF tokens are the ones every request below carries.
	alice, aliceID := signup(t, ts.URL, "knowledge-e2e@example.com", "knowledge-e2e")
	bob, bobID := signup(t, ts.URL, "knowledge-e2e-reviewer@example.com", "knowledge-e2e-reviewer")
	carol, carolID := signup(t, ts.URL, "knowledge-e2e-integrity@example.com", "knowledge-e2e-integrity")

	// ---- the project. The org and project rows are fixture data (this
	// build's provisioning is the provider-backed path T0302 owns); every
	// scientific fact below is written through the API.
	org, _, err := orgStore.CreateOrganization(ctx, domain.Organization{
		Slug: "knowledge-e2e", Name: "Knowledge E2E",
	}, aliceID, todayUTC())
	if err != nil {
		t.Fatalf("create the fixture organization: %v", err)
	}
	project, _, err := projectStore.CreateProject(ctx, domain.Project{
		OrganizationID:  &org.ID,
		Slug:            "knowledge-e2e",
		Name:            "Knowledge E2E",
		Purpose:         "T0510 knowledge workflow",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, aliceID)
	if err != nil {
		t.Fatalf("create the fixture project: %v", err)
	}

	// ---- the reviewer memberships and the routing rules: fixtures because
	// this build has no route that adds a member or writes a Research
	// Owners rule. The REVIEWS themselves are HTTP submissions below.
	for _, m := range []struct {
		user domain.User
		role domain.ProjectRole
	}{
		{domain.User{ID: bobID}, domain.ProjectRoleViewer},
		{domain.User{ID: carolID}, domain.ProjectRoleMaintainer},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, $3)`,
			project.ID, m.user.ID, m.role); err != nil {
			t.Fatalf("seed the %s membership: %v", m.role, err)
		}
		if _, err := routingSvc.Assign(ctx, domain.User{ID: aliceID}, project.ID, m.user.ID, knowledgeE2EResponsibility); err != nil {
			t.Fatalf("assign the responsibility label: %v", err)
		}
	}
	for _, objectType := range knowledgeE2EChainTypes {
		if _, err := routingSvc.AddRule(ctx, domain.User{ID: aliceID}, project.ID, responsibilities.AddRuleInput{
			MatchKind:      domain.ResearchOwnerMatchObjectType,
			MatchValue:     objectType,
			Responsibility: knowledgeE2EResponsibility,
		}); err != nil {
			t.Fatalf("route %s changes to a reviewer: %v", objectType, err)
		}
	}
	// The project policy the merge's fail-closed governance check reads: a
	// policy that does not mention main is refused, so the fixture declares
	// main protected.
	seedProjectPolicy(t, ctx, policyStore, project, aliceID)

	// ---- the two branches. main is the frozen acceptance line; the
	// research branch forks from main's head, so the proposal has a base on
	// the target's own chain.
	mainBranch, err := rsgSvc.CreateBranch(ctx, domain.User{ID: aliceID}, project.ID, rsg.CreateBranchInput{
		Name: "main", BaseRef: "", Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create the main branch: %v", err)
	}
	mainHead, err := statesSvc.GetBranchHead(ctx, mainBranch.ID)
	if err != nil {
		t.Fatalf("read main's head state: %v", err)
	}
	probeBranch, err := rsgSvc.CreateBranch(ctx, domain.User{ID: aliceID}, project.ID, rsg.CreateBranchInput{
		Name: "knowledge-r1", BaseRef: mainHead.ID, Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create the research branch: %v", err)
	}

	return &knowledgeE2EWorld{
		ts: ts, pool: pool,
		projectID: project.ID,
		aliceID:   aliceID, alice: alice, bob: bob, carol: carol,
		mainBranch: mainBranch.ID, probeBranch: probeBranch.ID,
		states: statesSvc,
		prSvc:  prSvc,
	}
}

// ---------------------------------------------------------------------------
// Wire helpers.

// kObject is the objectPayload rsghttp answers with.
type kObject struct {
	ID             string          `json:"id"`
	ObjectType     string          `json:"object_type"`
	CurrentVersion int             `json:"current_version"`
	VersionID      string          `json:"version_id"`
	VersionNo      int             `json:"version_no"`
	StateID        string          `json:"state_id"`
	Title          string          `json:"title"`
	LifecycleState string          `json:"lifecycle_state"`
	Payload        json.RawMessage `json:"payload"`
}

func (w *knowledgeE2EWorld) objectsPath() string {
	return "/api/v1/projects/" + w.projectID + "/branches/" + w.probeBranch + "/objects"
}

// objectPath is the read/create-version path of one object on the research
// branch; a non-empty tab asks for the detail page (HTML) instead of JSON.
func (w *knowledgeE2EWorld) objectPath(objectID, tab string) string {
	path := w.objectsPath() + "/" + objectID
	if tab != "" {
		path += "?tab=" + tab
	}
	return path
}

// objectPathAtVersion is objectPath with an explicit version. The detail page
// answers about the object's newest version unless one is named, and the
// graph tabs are pinned to the version the page shows — so a claim whose
// newest version is not the one carrying evidence has to be asked for the
// version it means.
func (w *knowledgeE2EWorld) objectPathAtVersion(objectID, tab string, versionNo int) string {
	return fmt.Sprintf("%s?tab=%s&version=%d", w.objectPath(objectID, ""), tab, versionNo)
}

// mainObjectPath is the same path on main — the branch acceptance lands on.
// The chain has to be readable THERE, not only where it was written: a merge
// that leaves the work readable only on the branch it came from has accepted
// nothing.
func (w *knowledgeE2EWorld) mainObjectPath(objectID, tab string) string {
	path := "/api/v1/projects/" + w.projectID + "/branches/" + w.mainBranch + "/objects/" + objectID
	if tab != "" {
		path += "?tab=" + tab
	}
	return path
}

// knowledgeE2ERights is the rights document the publication is stored under:
// internal/rights' fail-closed default, serialised. It is what the other
// publication tests publish under, and it widens nothing — publishing records
// a state, it does not change an audience.
func knowledgeE2ERights(t *testing.T) string {
	t.Helper()
	b, err := rights.New().Marshal()
	if err != nil {
		t.Fatalf("marshal the rights document: %v", err)
	}
	return string(b)
}

// mustRefusalReason asserts a refused publish answered the blocked envelope
// and names the reason asked for. The refusal is read as a REPORT, not as a
// status: the code says which rule refused, and a status alone would accept
// any refusal at all — including one about a different version.
func mustRefusalReason(t *testing.T, resp *http.Response, want string) {
	t.Helper()
	var refusal struct {
		Code    string   `json:"code"`
		Reasons []string `json:"reasons"`
	}
	decodeJSON(t, resp, &refusal)
	if refusal.Code != knowledgepublish.CodePublishBlocked {
		t.Fatalf("the refusal's code = %q, want %q (body: %+v)", refusal.Code, knowledgepublish.CodePublishBlocked, refusal)
	}
	for _, reason := range refusal.Reasons {
		if strings.HasPrefix(reason, want+": ") {
			return
		}
	}
	t.Fatalf("the refusal names %v, want the reason %q", refusal.Reasons, want)
}

// createBranch forks one research branch from main's CURRENT head through the
// contract's route (the fixture uses the service for the two branches it lays
// down before anything is read; this one is created in the middle of the
// story, so it goes through HTTP like every other act below step 1).
//
// Forking from main's head is what makes it the next round of work on
// accepted state. The branch the chain was written on cannot be reused: the
// merge closed it (active → merged), and a write to a closed branch is
// refused with BRANCH_NOT_ACTIVE — which this helper's caller would see as a
// failure, not as a silent no-op.
func (w *knowledgeE2EWorld) createBranch(t *testing.T, ctx context.Context, uc *testUserClient, name string) string {
	t.Helper()
	head, err := w.states.GetBranchHead(ctx, w.mainBranch)
	if err != nil {
		t.Fatalf("read main's head state: %v", err)
	}
	body := fmt.Sprintf(`{"name":%q,"base_ref":%q,"visibility":%q}`,
		name, head.ID, domain.BranchVisibilityPrivate)
	resp := uc.do(t, http.MethodPost, "/api/v1/projects/"+w.projectID+"/branches", body)
	mustStatus(t, resp, http.StatusCreated)
	var created struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		BaseStateID string `json:"base_state_id"`
	}
	decodeJSON(t, resp, &created)
	if created.ID == "" || created.Name != name {
		t.Fatalf("the created branch = %+v, want the named branch %q", created, name)
	}
	if created.BaseStateID != head.ID {
		t.Fatalf("the created branch forks from state %s, want main's head %s", created.BaseStateID, head.ID)
	}
	return created.ID
}
func (w *knowledgeE2EWorld) create(t *testing.T, uc *testUserClient, objectType, payload string) kObject {
	t.Helper()
	body := fmt.Sprintf(`{"object_type":%q,"payload":%s}`, objectType, payload)
	resp := uc.do(t, http.MethodPost, w.objectsPath(), body)
	mustStatus(t, resp, http.StatusCreated)
	var got kObject
	decodeJSON(t, resp, &got)
	if got.ID == "" || got.VersionID == "" {
		t.Fatalf("create %s: the response names no object/version: %+v", objectType, got)
	}
	if got.ObjectType != objectType {
		t.Fatalf("create %s: the response reports object_type %q", objectType, got.ObjectType)
	}
	return got
}

// relate writes one relation between two pinned object versions.
func (w *knowledgeE2EWorld) relate(t *testing.T, uc *testUserClient, relationType, sourceVersionID, targetVersionID string) {
	t.Helper()
	body := fmt.Sprintf(`{"relation_type":%q,"source_object_version_id":%q,"target_object_version_id":%q}`,
		relationType, sourceVersionID, targetVersionID)
	resp := uc.do(t, http.MethodPost,
		"/api/v1/projects/"+w.projectID+"/branches/"+w.probeBranch+"/relations", body)
	mustStatus(t, resp, http.StatusCreated)
}

// knowledgeE2EEvidenceBody is one evidence assertion's request body: the two
// version pins, the relation, and the fields docs/10 requires of an
// assertion. It is a function so the refusal case can post the SAME body to a
// branch that must reject it — a refusal proved with a different body would
// prove nothing about this one.
func knowledgeE2EEvidenceBody(relation, targetVersionID, evidenceVersionID, evidenceType, note string) string {
	return fmt.Sprintf(`{"target_version_ref":"object_version:%s","evidence_version_ref":"object_version:%s",`+
		`"relation":%q,"evidence_type":%q,"scope":{"conditions":"298 K, 1 bar CO2, 90%% RH"},`+
		`"directness":"direct","inference_nature":"observational","reasoning_note":%q}`,
		targetVersionID, evidenceVersionID, relation, evidenceType, note)
}

// assertEvidence writes one evidence assertion about a pinned target version.
func (w *knowledgeE2EWorld) assertEvidence(t *testing.T, uc *testUserClient, branchID, relation, targetVersionID, evidenceVersionID, evidenceType, note string) {
	t.Helper()
	body := knowledgeE2EEvidenceBody(relation, targetVersionID, evidenceVersionID, evidenceType, note)
	resp := uc.do(t, http.MethodPost,
		"/api/v1/projects/"+w.projectID+"/branches/"+branchID+"/evidence-assertions", body)
	mustStatus(t, resp, http.StatusCreated)
}

// getJSON issues a read as uc and decodes it, failing with the envelope when
// the status is not the expected one.
func (w *knowledgeE2EWorld) getJSON(t *testing.T, uc *testUserClient, path string, want int, dst any) {
	t.Helper()
	resp := uc.do(t, http.MethodGet, path, "")
	if resp.StatusCode != want {
		t.Fatalf("GET %s = %d, want %d: %s", path, resp.StatusCode, want, readAll(t, resp))
	}
	if dst != nil {
		decodeJSON(t, resp, dst)
	}
}

// getHTML issues a browser-style read (Accept: text/html) and returns the
// page source.
func (w *knowledgeE2EWorld) getHTML(t *testing.T, uc *testUserClient, path string, want int) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, w.ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := uc.client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body := readAll(t, resp)
	if resp.StatusCode != want {
		t.Fatalf("GET %s (html) = %d, want %d: %s", path, resp.StatusCode, want, body)
	}
	return body
}

func decodeJSON(t *testing.T, resp *http.Response, dst any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The read models this test asserts on.

type kResearch struct {
	ProjectID string `json:"project_id"`
	Counts    struct {
		Questions    int `json:"questions"`
		Findings     int `json:"findings"`
		Hypotheses   int `json:"hypotheses"`
		Claims       int `json:"claims"`
		OtherObjects int `json:"other_objects"`
	} `json:"counts"`
	Questions  []kQuestion `json:"questions"`
	Findings   []kFinding  `json:"findings"`
	Unassigned []kRef      `json:"unassigned"`
}

type kQuestion struct {
	ObjectID      string      `json:"object_id"`
	VersionID     string      `json:"version_id"`
	Title         string      `json:"title"`
	Statement     string      `json:"statement"`
	QuestionState string      `json:"question_state"`
	Hypotheses    []kRef      `json:"hypotheses"`
	Findings      []kRef      `json:"findings"`
	OtherObjects  []kRef      `json:"other_objects"`
	Children      []kQuestion `json:"children"`
}

type kFinding struct {
	ObjectID    string   `json:"object_id"`
	VersionID   string   `json:"version_id"`
	Title       string   `json:"title"`
	Statement   string   `json:"statement"`
	FindingType string   `json:"finding_type"`
	Assessment  string   `json:"assessment"`
	Claims      []kClaim `json:"claims"`
	Questions   []kRef   `json:"questions"`
}

type kClaim struct {
	ObjectID  string `json:"object_id"`
	VersionID string `json:"version_id"`
	Title     string `json:"title"`
	Resolved  bool   `json:"resolved"`
}

type kRef struct {
	ObjectID   string `json:"object_id"`
	ObjectType string `json:"object_type"`
	Title      string `json:"title"`
}

type kEvidence struct {
	ProjectID string `json:"project_id"`
	Object    struct {
		ObjectID   string `json:"object_id"`
		ObjectType string `json:"object_type"`
		Title      string `json:"title"`
	} `json:"object"`
	Groups []kEvidenceGroup `json:"groups"`
}

type kEvidenceGroup struct {
	Target     kEvidenceTarget      `json:"target"`
	Supporting []kEvidenceAssertion `json:"supporting"`
	Contesting []kEvidenceAssertion `json:"contesting"`
	Neutral    []kEvidenceAssertion `json:"neutral"`
	Unlabeled  []kEvidenceAssertion `json:"unlabeled"`
}

type kEvidenceTarget struct {
	ObjectVersionID string `json:"object_version_id"`
	VersionNo       *int   `json:"version_no"`
	Title           string `json:"title"`
}

type kEvidenceAssertion struct {
	ID                      string `json:"id"`
	Relation                string `json:"relation"`
	EvidenceType            string `json:"evidence_type"`
	TargetObjectVersionID   string `json:"target_object_version_id"`
	EvidenceObjectVersionID string `json:"evidence_object_version_id"`
}

type kQuery struct {
	ProjectID string `json:"project_id"`
	Objects   []struct {
		ID         string `json:"id"`
		ObjectType string `json:"object_type"`
		VersionID  string `json:"version_id"`
		Title      string `json:"title"`
	} `json:"objects"`
	Relations []struct {
		ID           string `json:"id"`
		RelationType string `json:"relation_type"`
	} `json:"relations"`
}

// index returns the object ids a branch-slice query carries, keyed by type.
func (q kQuery) index() map[string]string {
	out := make(map[string]string, len(q.Objects))
	for _, o := range q.Objects {
		out[o.ObjectType] = o.ID
	}
	return out
}

// ---------------------------------------------------------------------------
// The test.

func TestKnowledgeWorkflowEndToEnd(t *testing.T) {
	ctx := testCtx(t)
	w := newKnowledgeE2EWorld(t, ctx)
	alice := w.alice

	// Two blobs are named by the dataset and the calculation. Their ids
	// have to exist BEFORE the payloads that reference them, so they are
	// drawn from the database up front; the rows are written once the
	// versions they attach to exist. A payload whose blob reference
	// resolves to nothing would be refused at the merge (the integrity
	// engine's blob_refs_resolve), so the fixture cannot fake this.
	datasetBlobID := w.newUUID(t)
	calcBlobID := w.newUUID(t)

	// ---- 1. The chain, written through the contract's routes, on the
	// research branch. Every payload is MAIN-GATE COMPLETE: the merge into
	// frozen main is the last chance to complete the docs/08 domain fields
	// (internal/rsg/validation's ladder), so a chain that lacked them could
	// be browsed but never accepted — and "PR merge" is half of this task's
	// acceptance.
	protocol := w.create(t, alice, "protocol", `{
		"purpose": "hold the sorbent at 298 K and 1 bar CO2 through 100 humid cycles",
		"domain": "materials",
		"steps": [{"id": "s1", "action": "humid cycling"}, {"id": "s2", "action": "CO2 uptake measurement"}],
		"parameters": {"cycles": 100, "relative_humidity_pct": 90},
		"requirements": ["dry glovebox", "calibrated sorption analyser"]}`)
	// The material the sample is a specimen of. It is not decoration: the
	// sample's OWN schema (specs/schemas/sample.schema.json) requires
	// material_id, and the merge's schema_payload_conforms check re-validates
	// every proposed version against its registered schema — a chain that
	// skipped the material would be refused at the gate, which is how this
	// fixture learned the field was missing.
	material := w.create(t, alice, "material", `{
		"name": "Amine-functionalised UiO-66 (MOF-AMINE-A)",
		"formula": "Zr6O4(OH)4(BDC-NH2)6",
		"material_family": "metal-organic framework",
		"identifiers": {"project_code": "MOF-AMINE-A-001", "batch": "B-2026-03"}}`)
	sample := w.create(t, alice, "sample", fmt.Sprintf(`{
		"sample_code": "MOF-AMINE-A-001",
		"material_id": %q,
		"protocol_version_id": %q,
		"physical_form": "powder"}`, material.ID, protocol.VersionID))
	question := w.create(t, alice, "research_question", `{
		"statement": "Does the amine-functionalised framework retain its CO2 uptake after 100 humid cycles?",
		"purpose": "Decide whether the sorbent survives humidity cycling before any scale-up is proposed.",
		"question_state": "open"}`)
	hypothesis := w.create(t, alice, "hypothesis", fmt.Sprintf(`{
		"statement": "If the amine sites are retained, uptake after 100 humid cycles stays within 10%% of the fresh value.",
		"question_id": %q,
		"hypothesis_type": "mechanistic",
		"assessment": "under_test",
		"scope": {"system": "MOF-AMINE-A", "conditions": "298 K, 1 bar CO2, 90%% RH"}}`, question.ID))
	experiment := w.create(t, alice, "experiment", fmt.Sprintf(`{
		"objective": "Measure CO2 uptake before and after 100 humid cycles on the same sample.",
		"operator_ids": [%q],
		"sample_ids": [%q],
		"protocol_version_id": %q,
		"conditions": {"temperature_k": 298, "pressure_bar": 1, "relative_humidity_pct": 90},
		"result_summary": "Uptake fell 12%% after 100 cycles, more than the 10%% the hypothesis allows."}`,
		w.aliceID, sample.ID, protocol.VersionID))
	calculation := w.create(t, alice, "calculation", fmt.Sprintf(`{
		"objective": "Fit the isotherm and derive the retained-capacity fraction.",
		"software": "python3.12 + scipy 1.14",
		"method": "Langmuir fit over the 0.05-1.0 bar isotherm points",
		"input_object_ids": [%q],
		"parameters": {"model": "langmuir", "bootstrap_samples": 200},
		"output_blob_ids": [%q],
		"result_summary": "Retained fraction 0.88 (95%% CI 0.84-0.91) - below the 0.90 the hypothesis names.",
		"protocol_version_id": %q}`, sample.ID, calcBlobID, protocol.VersionID))
	dataset := w.create(t, alice, "dataset", fmt.Sprintf(`{
		"purpose": "The measured uptake isotherms behind the retained-capacity claim.",
		"data_type": "adsorption_isotherm",
		"blob_ids": [%q],
		"access_level": "restricted",
		"quality_notes": "Two repeats per point; the protocol_version_id names the cycling procedure."}`,
		datasetBlobID))
	claim := w.create(t, alice, "claim", fmt.Sprintf(`{
		"statement": "MOF-AMINE-A retains 88%% CO2 uptake after 100 humid cycles.",
		"claim_type": "quantitative",
		"subject_ref": %q,
		"property": "co2_uptake_retained_fraction",
		"value": {"number": 0.88, "unit": "dimensionless"},
		"scope": {"temperature_k": 298, "pressure_bar": 1, "relative_humidity_pct": 90},
		"assessment": "contested"}`, sample.ID))
	finding := w.create(t, alice, "finding", fmt.Sprintf(`{
		"statement": "Humid cycling costs the sorbent 12%% of its CO2 uptake; the loss is real and repeated.",
		"finding_type": "observation",
		"claim_version_refs": [%q],
		"assessment": "contested"}`, claim.VersionID))

	// ---- 2. The links that make it ONE chain rather than ten rows: the
	// provenance edges (what produced what) and the knowledge edges (what
	// addresses the question, what tests the hypothesis). Every edge is
	// version-pinned: it names the exact version it was written about.
	w.relate(t, alice, "addresses_question", hypothesis.VersionID, question.VersionID)
	w.relate(t, alice, "addresses_question", experiment.VersionID, question.VersionID)
	w.relate(t, alice, "addresses_question", calculation.VersionID, question.VersionID)
	w.relate(t, alice, "addresses_question", dataset.VersionID, question.VersionID)
	w.relate(t, alice, "addresses_question", claim.VersionID, question.VersionID)
	w.relate(t, alice, "addresses_question", finding.VersionID, question.VersionID)
	w.relate(t, alice, "tests_hypothesis", experiment.VersionID, hypothesis.VersionID)
	w.relate(t, alice, "follows_protocol", experiment.VersionID, protocol.VersionID)
	w.relate(t, alice, "performed_on", experiment.VersionID, sample.VersionID)
	w.relate(t, alice, "produces", experiment.VersionID, dataset.VersionID)
	w.relate(t, alice, "uses", calculation.VersionID, dataset.VersionID)
	w.relate(t, alice, "derived_from", claim.VersionID, calculation.VersionID)

	// ---- 3. The two blob rows the dataset and the calculation name, each
	// attached to the version that uses it, in that version's own state:
	// which is what makes the blob a member of the state's manifest.
	w.attachBlob(t, ctx, datasetBlobID, dataset.VersionID, dataset.StateID, "data", "sha256:knowledge-e2e-dataset", "s3://post-blobs/knowledge-e2e/isotherms.csv")
	w.attachBlob(t, ctx, calcBlobID, calculation.VersionID, calculation.StateID, "output", "sha256:knowledge-e2e-calc", "s3://post-blobs/knowledge-e2e/fit.json")

	// The evidence phase is NOT here, and its absence is a rule rather than
	// an ordering preference: the assertion write path resolves its target
	// through the publication read, so an assertion can only be written
	// about knowledge the repository has already published, and a version
	// only becomes publishable through a reviewed merge. Both of those are
	// steps 7 and 8 below; the conflicting evidence is step 9.

	// ---- 4. BROWSABLE (i): the research outline, the surface a reader
	// starts from. The chain hangs off the question: the hypothesis in its
	// own section, the finding in its own section, and the experiment,
	// calculation, dataset and claim as the question's other objects.
	outlinePath := "/api/v1/projects/" + w.projectID + "/research"
	var outline kResearch
	w.getJSON(t, alice, outlinePath, http.StatusOK, &outline)
	if len(outline.Questions) != 1 {
		t.Fatalf("the outline carries %d questions, want the one this chain wrote", len(outline.Questions))
	}
	q := outline.Questions[0]
	if q.ObjectID != question.ID || q.VersionID != question.VersionID {
		t.Fatalf("the outline's question is %s v%s, want %s v%s", q.ObjectID, q.VersionID, question.ID, question.VersionID)
	}
	if q.QuestionState != "open" {
		t.Errorf("the outline reports question_state %q, want %q", q.QuestionState, "open")
	}
	mustRef(t, q.Hypotheses, "hypothesis", hypothesis.ID, "the question's hypotheses")
	mustRef(t, q.Findings, "finding", finding.ID, "the question's findings")
	for _, want := range []kObject{experiment, calculation, dataset, claim} {
		mustRef(t, q.OtherObjects, want.ObjectType, want.ID, "the question's other objects")
	}
	if len(outline.Findings) != 1 || outline.Findings[0].ObjectID != finding.ID {
		t.Fatalf("the outline's findings = %+v, want the one this chain wrote (%s)", outline.Findings, finding.ID)
	}
	f := outline.Findings[0]
	if f.FindingType != "observation" || f.Assessment != "contested" {
		t.Errorf("the finding renders as type %q assessment %q, want observation/contested", f.FindingType, f.Assessment)
	}
	if len(f.Claims) != 1 {
		t.Fatalf("the finding carries %d claim refs, want the one claim version it pins", len(f.Claims))
	}
	if !f.Claims[0].Resolved || f.Claims[0].VersionID != claim.VersionID || f.Claims[0].ObjectID != claim.ID {
		t.Fatalf("the finding's claim ref = %+v, want the pinned claim version %s resolved to object %s",
			f.Claims[0], claim.VersionID, claim.ID)
	}
	// The protocol and the sample address no question — they hang off the
	// experiment by provenance, not off the question by knowledge. The
	// outline says so instead of hiding them.
	mustRef(t, outline.Unassigned, "protocol", protocol.ID, "the outline's unassigned objects")
	mustRef(t, outline.Unassigned, "sample", sample.ID, "the outline's unassigned objects")

	// The same outline rendered as the page a person reads, in both of its
	// views: the question view the URL defaults to (the chain hanging off
	// the question) and the by-finding view its tab switches to.
	researchHTML := w.getHTML(t, alice, outlinePath, http.StatusOK)
	mustContain(t, researchHTML, `data-question-id="`+question.ID+`"`, "the research page's question node")
	mustContain(t, researchHTML, hypothesis.Title, "the research page's hypothesis link")
	findingsHTML := w.getHTML(t, alice, outlinePath+"?view=findings", http.StatusOK)
	mustContain(t, findingsHTML, `data-finding-id="`+finding.ID+`"`, "the research page's finding node")
	mustContain(t, findingsHTML, claim.Title, "the pinned claim on the research page's finding")

	// ---- 5. BROWSABLE (ii): hop by hop, through the object detail pages
	// and the two graph tabs. Each hop is asserted in both directions where
	// the edge is rendered from both ends — an edge that renders on one
	// side only is a reference the reader cannot follow.
	questionPage := w.getHTML(t, alice, w.objectPath(question.ID, "relations"), http.StatusOK)
	mustContain(t, questionPage, "<code>addresses_question</code>", "the question's relation rows")
	mustContain(t, questionPage, hypothesis.Title, "the hypothesis on the question's page")
	mustContain(t, questionPage, finding.Title, "the finding on the question's page")
	experimentPage := w.getHTML(t, alice, w.objectPath(experiment.ID, "relations"), http.StatusOK)
	mustContain(t, experimentPage, "<code>tests_hypothesis</code>", "the experiment's relation rows")
	mustContain(t, experimentPage, hypothesis.Title, "the tested hypothesis on the experiment's page")
	mustContain(t, experimentPage, "<code>produces</code>", "the experiment's producing edge")
	mustContain(t, experimentPage, dataset.Title, "the produced dataset on the experiment's page")
	datasetProvenance := w.getHTML(t, alice, w.objectPath(dataset.ID, "provenance"), http.StatusOK)
	mustContain(t, datasetProvenance, `data-graph="provenance"`, "the dataset's provenance tab")
	mustContain(t, datasetProvenance, `data-row-relation="produces"`, "the producing edge in the dataset's lineage walk")
	mustContain(t, datasetProvenance, experiment.Title, "the producing experiment in the dataset's lineage")
	// The claim's evidence tab, read BEFORE any assertion exists: the page
	// renders, and it reports no stance it was never told about. That is the
	// "before" half of the instrument step 10 closes — a tab that showed an
	// assertion here would be showing one that does not exist.
	claimEvidenceBefore := w.getHTML(t, alice, w.objectPath(claim.ID, "evidence"), http.StatusOK)
	if strings.Contains(claimEvidenceBefore, `data-row-relation="supports"`) ||
		strings.Contains(claimEvidenceBefore, `data-row-relation="contradicts"`) {
		t.Fatal("the claim's evidence tab renders an assertion before any was written")
	}
	// The finding's last hop: it addresses the question through an RSG
	// relation, and it names the claim version it judges through its PAYLOAD
	// pin (claim_version_refs), not through a relation — a finding's judgment
	// is about a version, and the pin is what says which. The page shows the
	// pin as the exact version id; the outline (step 4) is the surface that
	// resolves it to that version's title.
	findingRelations := w.getHTML(t, alice, w.objectPath(finding.ID, "relations"), http.StatusOK)
	mustContain(t, findingRelations, "<code>addresses_question</code>", "the finding's relation rows")
	mustContain(t, findingRelations, question.Title, "the question the finding addresses")
	findingPage := w.getHTML(t, alice, w.objectPath(finding.ID, "metadata"), http.StatusOK)
	mustContain(t, findingPage, claim.VersionID, "the claim version the finding pins, on the finding's page")

	// ---- 6. VERSIONED. A new claim version that restates the number: the
	// chain must show the change without rewriting what the earlier version
	// meant, and above all without moving the finding's pinned ref (a
	// finding is a judgment about a version, not about "the claim"). The
	// revision is written BEFORE the proposal, the way a branch is revised
	// before it is offered for review — so the merge in step 7 accepts THIS
	// version as the branch's head, and the publication in step 8 attaches
	// to the version the repository accepted rather than to an earlier one.
	claimV2Statement := "MOF-AMINE-A retains 88% CO2 uptake after 100 humid cycles (re-fitted from three repeats)."
	v2Resp := alice.do(t, http.MethodPost, w.objectPath(claim.ID, "")+":version",
		fmt.Sprintf(`{"expected_version":1,"patch":{"statement":%q}}`, claimV2Statement))
	mustStatus(t, v2Resp, http.StatusCreated)
	var claimV2 kObject
	decodeJSON(t, v2Resp, &claimV2)
	if claimV2.VersionNo != 2 || claimV2.Title != claimV2Statement {
		t.Fatalf("the claim's second version = v%d titled %q, want v2 with the new statement", claimV2.VersionNo, claimV2.Title)
	}
	if claimV2.VersionID == claim.VersionID {
		t.Fatal("the second version reused the first version's id — the log appended nothing")
	}

	// The claim object now reads as v2 …
	var claimNow kObject
	w.getJSON(t, alice, w.objectPath(claim.ID, ""), http.StatusOK, &claimNow)
	if claimNow.CurrentVersion != 2 {
		t.Fatalf("the claim reads as current_version %d, want 2", claimNow.CurrentVersion)
	}
	// … while the finding still pins v1, and the outline still resolves
	// that pin to v1's own title.
	w.getJSON(t, alice, outlinePath, http.StatusOK, &outline)
	if len(outline.Findings) != 1 || len(outline.Findings[0].Claims) != 1 {
		t.Fatalf("the outline's finding lost its claim ref after the new version: %+v", outline.Findings)
	}
	pinned := outline.Findings[0].Claims[0]
	if pinned.VersionID != claim.VersionID {
		t.Fatalf("the finding's pin moved to %s when the claim advanced to v2 — an append-only version log must not rewrite a finding's meaning",
			pinned.VersionID)
	}
	if pinned.Title != claim.Title {
		t.Fatalf("the finding's pinned claim renders as %q, want v1's own title %q", pinned.Title, claim.Title)
	}
	// The detail page a reader lands on says which version of how many it is
	// showing and lists the others: the page is version-addressable, not a
	// mutable document that quietly became something else.
	claimPage := w.getHTML(t, alice, w.objectPath(claim.ID, "metadata"), http.StatusOK)
	mustContain(t, claimPage, "Version <strong>2</strong> of 2", "the claim page's version summary")
	mustContain(t, claimPage, `<span class="vnum">v1</span>`, "v1 in the claim's version list")
	mustContain(t, claimPage, `<span class="vnum">v2</span>`, "v2 in the claim's version list")
	mustContain(t, claimPage, claim.Title, "v1's title in the claim's version list")

	// ---- 7. The proposal. main carries NOTHING of the chain yet: the read
	// is the branch-slice query, the same read that answers "what is
	// accepted on main" for the UI.
	mainSlicePath := "/api/v1/projects/" + w.projectID + "/query?branch_id=" + w.mainBranch
	var before kQuery
	w.getJSON(t, alice, mainSlicePath, http.StatusOK, &before)
	if len(before.Objects) != 0 {
		t.Fatalf("main already carries %d objects before the merge: %+v", len(before.Objects), before.index())
	}

	openBody := fmt.Sprintf(`{"source_branch_id":%q,"target_branch_id":%q,`+
		`"title":"knowledge-r1: humid-cycling chain","body":"T0510 knowledge workflow e2e"}`,
		w.probeBranch, w.mainBranch)
	openResp := alice.doKeyed(t, http.MethodPost, "/api/v1/projects/"+w.projectID+"/pull-requests",
		openBody, "knowledge-e2e-open-000001")
	if openResp.StatusCode != http.StatusCreated {
		t.Fatalf("open the pull request = %d: %s", openResp.StatusCode, readAll(t, openResp))
	}
	var opened struct {
		Number int64  `json:"number"`
		State  string `json:"state"`
	}
	decodeJSON(t, openResp, &opened)
	if opened.State != string(domain.PullRequestStateOpen) {
		t.Fatalf("the opened proposal is %q, want %q", opened.State, domain.PullRequestStateOpen)
	}

	// The proposal's own diff is the Research State Diff: every object type
	// the chain wrote is what the reviewer is being asked about.
	var prDiff struct {
		ObjectChanges []struct {
			ObjectType string `json:"object_type"`
		} `json:"object_changes"`
	}
	w.getJSON(t, alice, fmt.Sprintf("/api/v1/projects/%s/pull-requests/%d/diff", w.projectID, opened.Number),
		http.StatusOK, &prDiff)
	changed := map[string]bool{}
	for _, c := range prDiff.ObjectChanges {
		changed[c.ObjectType] = true
	}
	for _, want := range knowledgeE2EChainTypes {
		if !changed[want] {
			t.Fatalf("the proposal's diff carries no %s change (it carries %v)", want, changed)
		}
	}

	// The state walk is NOT performed by this test. docs/43's open →
	// review_required transition has no route in this build (no contract
	// operation, no request_review cell in the permissions matrix), so the
	// one direct call below IS that missing route — everything after it is
	// HTTP again.
	if _, err := w.prSvc.RequestReview(ctx, w.projectID, opened.Number); err != nil {
		t.Fatalf("request review: %v", err)
	}
	// The two required dimensions, submitted over the review route by the
	// two members who hold the routing label. The second submission
	// satisfies the calculation and walks the proposal to merge_ready
	// inside the store's own projection.
	for _, submission := range []struct {
		client *testUserClient
		kind   string
		why    string
	}{
		{w.bob, "scientific", "the chain's experiment, calculation, dataset and claim read as one line of reasoning"},
		{w.carol, "integrity", "the proposal as a whole is internally consistent and its provenance is traceable"},
	} {
		body := fmt.Sprintf(`{"kind":%q,"decision":"approved","body":%q}`, submission.kind, submission.why)
		resp := submission.client.do(t, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/pull-requests/%d/reviews", w.projectID, opened.Number), body)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("submit the %s review = %d: %s", submission.kind, resp.StatusCode, readAll(t, resp))
		}
	}
	pr, err := w.prSvc.Get(ctx, w.projectID, opened.Number)
	if err != nil {
		t.Fatalf("read the proposal after the reviews: %v", err)
	}
	if pr.State != domain.PullRequestStateMergeReady {
		t.Fatalf("the reviewed proposal is %q, want %q — the review submissions did not walk the machine",
			pr.State, domain.PullRequestStateMergeReady)
	}

	// ---- 7b. PR MERGE. The route, the guard, the Idempotency-Key
	// contract, the policy check, the checks re-run and the state write —
	// one request, no service called.
	merged := mergeThroughTheEndpoint(t, alice, w.projectID, opened.Number, "knowledge-e2e-merge-000001")
	if merged.Replayed {
		t.Fatal("the first merge reports itself as a replay")
	}
	// The Git step is pending because no provider adapter is wired here
	// (see this file's header): the platform truth is what the merge
	// commits, and the saga says so instead of claiming a ref moved.
	if merged.GitState != string(domain.GitStatePending) {
		t.Fatalf("merge git_state = %q (error %q), want %q on a Gitea-free wiring",
			merged.GitState, merged.GitError, domain.GitStatePending)
	}

	// main now carries the chain — the same read that was empty above.
	var after kQuery
	w.getJSON(t, alice, mainSlicePath, http.StatusOK, &after)
	onMain := after.index()
	for _, o := range []kObject{protocol, material, sample, question, hypothesis, experiment, calculation, dataset, claim, finding} {
		if got, ok := onMain[o.ObjectType]; !ok || got != o.ID {
			t.Fatalf("main's slice does not carry the %s %s (it carries %v)", o.ObjectType, o.ID, onMain)
		}
	}
	if len(after.Relations) != 12 {
		t.Fatalf("main's slice carries %d relations, want the chain's 12 edges", len(after.Relations))
	}
	// main's claim is a version of its OWN chain (v3): the merge APPENDS the
	// accepted content to main rather than pointing main at the branch's
	// version (internal/application/merge/ports.go: only appends exist,
	// never edits — invariant 8). What must hold is that the appended
	// version carries what the reviewers approved, so the assertion is on
	// the CONTENT: main's claim is the revised statement, not v1's.
	var claimOnMain kObject
	w.getJSON(t, alice,
		"/api/v1/projects/"+w.projectID+"/branches/"+w.mainBranch+"/objects/"+claim.ID,
		http.StatusOK, &claimOnMain)
	if claimOnMain.Title != claimV2Statement {
		t.Fatalf("the claim main accepted reads %q, want the revised statement the reviews approved (%q)",
			claimOnMain.Title, claimV2Statement)
	}
	if claimOnMain.CurrentVersion != claimV2.VersionNo+1 {
		t.Fatalf("main's claim is at version %d, want the append past the branch's v%d (the merge appends its own version to main)",
			claimOnMain.CurrentVersion, claimV2.VersionNo)
	}
	// The page a reader opens on MAIN is the accepted chain's claim, at the
	// version the merge appended.
	claimOnMainPage := w.getHTML(t, alice, w.mainObjectPath(claim.ID, "metadata"), http.StatusOK)
	mustContain(t, claimOnMainPage, claimV2Statement, "the claim page a reader opens on main")
	// … and the finding's pin still names v1 (the version it was written
	// about), not the object's new head.
	w.getJSON(t, alice, outlinePath, http.StatusOK, &outline)
	if len(outline.Findings) != 1 || len(outline.Findings[0].Claims) != 1 ||
		outline.Findings[0].Claims[0].VersionID != claim.VersionID {
		t.Fatalf("after the merge the finding's pin is %+v, want the original claim version %s",
			outline.Findings, claim.VersionID)
	}
	// ---- 8. PUBLISHED. The evidence step below CANNOT come first, and that
	// is the platform's rule rather than this test's ordering taste: the
	// assertion write path resolves its target through the publication read
	// (GetKnowledgePublicationForVersion) and refuses a target the network
	// does not carry. Publication has a gate of its own, and it is the
	// release gate's own query: knowledgepublish.Judge approves a version
	// only when the lineage of its state into main carries the approved
	// reviews of a research PR (ListReleaseReviews). Two rows below carry
	// that record and both publish: the branch head the reviewers pinned
	// (claim v2), and main's own appended version of the same object — the
	// lineage of the state main holds it at contains the merge's result
	// state, so the merge edge (semantic_merges.result_state_id →
	// pull_request_id, which T0611 made readable) puts the accepted PR's
	// approved reviews in it as well. v1 is not on main's side of that
	// merge at all, does not carry the record, and stays refused.
	//
	// The two refusals around the successes are what make that a checked
	// statement rather than a story: v1 is refused as unreviewed, and the
	// published version is refused the second time because a version is
	// published once. A gate nobody walked into is a gate nobody checked.
	const publishedName = "MOF-AMINE-A humid-cycling chain (claim v2)"
	publishResp := alice.doKeyed(t, http.MethodPost,
		"/api/v1/projects/"+w.projectID+"/knowledge:publish",
		fmt.Sprintf(`{"knowledge_version_ref":"object_version:%s","rights":%s,"public_version":%q}`,
			claimV2.VersionID, knowledgeE2ERights(t), publishedName),
		"knowledge-e2e-publish-000001")
	if publishResp.StatusCode != http.StatusCreated {
		t.Fatalf("publish the accepted claim version = %d: %s", publishResp.StatusCode, readAll(t, publishResp))
	}
	var publication struct {
		PID             string `json:"pid"`
		ObjectVersionID string `json:"object_version_id"`
		PublicVersion   string `json:"public_version"`
	}
	decodeJSON(t, publishResp, &publication)
	if publication.PID == "" || publication.ObjectVersionID != claimV2.VersionID {
		t.Fatalf("the publication = %+v, want a pid and the version the merge accepted (%s)", publication, claimV2.VersionID)
	}
	if publication.PublicVersion != publishedName {
		t.Errorf("the publication is named %q, want the name the publisher sent (%q)", publication.PublicVersion, publishedName)
	}

	// The earlier version of the same object is refused: the reviewed merge
	// accepted the branch head, and Judge reads the record of the VERSION's
	// lineage into main, which v1's state is not on the main side of.
	unreviewed := alice.doKeyed(t, http.MethodPost,
		"/api/v1/projects/"+w.projectID+"/knowledge:publish",
		fmt.Sprintf(`{"knowledge_version_ref":"object_version:%s","rights":%s,"public_version":"%s (v1)"}`,
			claim.VersionID, knowledgeE2ERights(t), publishedName),
		"knowledge-e2e-publish-000002")
	if unreviewed.StatusCode != http.StatusConflict {
		t.Fatalf("publishing the pre-merge-revision version = %d, want %d: %s",
			unreviewed.StatusCode, http.StatusConflict, readAll(t, unreviewed))
	}
	mustRefusalReason(t, unreviewed, knowledgepublish.ReasonReviewRequired)

	// main's own appended version publishes too, and what approves it is the
	// same record rather than a second rule: since T0611 ListReleaseReviews
	// reads the merge edge (semantic_merges.result_state_id →
	// pull_request_id), the lineage of the state the merge wrote on main
	// contains that merge's result state and therefore the PR the merge
	// executed — the reviewers' approval IS this version's acceptance
	// record. Before that fix the edge went unread, the query matched
	// proposals only, and no proposal is ever an ancestor of a merged state,
	// so this row was refused as unreviewed: the defect was in the read.
	// Judge is unchanged, so the release gate and the publication gate still
	// reach one conclusion over one record, and the row that publishes here
	// is main's own row and nothing else.
	mainVersionPublished := alice.doKeyed(t, http.MethodPost,
		"/api/v1/projects/"+w.projectID+"/knowledge:publish",
		fmt.Sprintf(`{"knowledge_version_ref":"object_version:%s","rights":%s,"public_version":"%s (main v%d)"}`,
			claimOnMain.VersionID, knowledgeE2ERights(t), publishedName, claimOnMain.CurrentVersion),
		"knowledge-e2e-publish-000004")
	if mainVersionPublished.StatusCode != http.StatusCreated {
		t.Fatalf("publishing main's appended version = %d, want %d: %s",
			mainVersionPublished.StatusCode, http.StatusCreated, readAll(t, mainVersionPublished))
	}
	var mainPublication struct {
		PID             string `json:"pid"`
		ObjectVersionID string `json:"object_version_id"`
		PublicVersion   string `json:"public_version"`
	}
	decodeJSON(t, mainVersionPublished, &mainPublication)
	if mainPublication.PID == "" || mainPublication.ObjectVersionID != claimOnMain.VersionID {
		t.Fatalf("the publication = %+v, want a pid and main's own appended version %s",
			mainPublication, claimOnMain.VersionID)
	}
	mainPublicName := fmt.Sprintf("%s (main v%d)", publishedName, claimOnMain.CurrentVersion)
	if mainPublication.PublicVersion != mainPublicName {
		t.Errorf("the publication is named %q, want the name the publisher sent (%q)",
			mainPublication.PublicVersion, mainPublicName)
	}

	// And a version is published once: the accepted version, again, under a
	// fresh key (a repeated key would be the idempotent replay, which is a
	// different answer to a different request).
	republished := alice.doKeyed(t, http.MethodPost,
		"/api/v1/projects/"+w.projectID+"/knowledge:publish",
		fmt.Sprintf(`{"knowledge_version_ref":"object_version:%s","rights":%s,"public_version":%q}`,
			claimV2.VersionID, knowledgeE2ERights(t), publishedName),
		"knowledge-e2e-publish-000003")
	if republished.StatusCode != http.StatusConflict {
		t.Fatalf("publishing the accepted version a second time = %d, want %d: %s",
			republished.StatusCode, http.StatusConflict, readAll(t, republished))
	}
	mustRefusalReason(t, republished, knowledgepublish.ReasonAlreadyPublished)

	// ---- 9. The evidence, including the conflict. The experiment's own
	// measurement supports the claim the network now carries; the dataset
	// the experiment produced is asserted AGAINST it. Two stances about ONE
	// target version is the case the read model must never net (docs/10 §4,
	// CLAUDE.md §9.13).
	//
	// The assertions are written on a NEW branch forked from main's head, and
	// that is forced rather than chosen: the branch the chain was written on
	// was closed by its own merge, main only moves by review, and an
	// assertion is new work. The refusal of the closed branch is exercised
	// first, with the very body the accepted write below uses — a branch that
	// stopped accepting writes at its merge is exactly the kind of rule that
	// is only real if something tried it.
	closedBranchWrite := alice.do(t, http.MethodPost,
		"/api/v1/projects/"+w.projectID+"/branches/"+w.probeBranch+"/evidence-assertions",
		knowledgeE2EEvidenceBody("supports", claimV2.VersionID, experiment.VersionID, "experimental",
			"the 100-cycle measurement reproduces the retention the revised claim states"))
	if closedBranchWrite.StatusCode != http.StatusConflict {
		t.Fatalf("writing an assertion on the merged branch = %d, want %d: %s",
			closedBranchWrite.StatusCode, http.StatusConflict, readAll(t, closedBranchWrite))
	}
	var closedRefusal struct {
		Code string `json:"code"`
	}
	decodeJSON(t, closedBranchWrite, &closedRefusal)
	if closedRefusal.Code != "BRANCH_NOT_ACTIVE" {
		t.Fatalf("the closed branch's refusal = %q, want BRANCH_NOT_ACTIVE", closedRefusal.Code)
	}

	// The accepted write lands on a branch of its own — new work on a new
	// branch, forked from the head the merge left main on — and the target
	// stays the published version, because that is the version the network
	// carries and the row the reviewers pinned.
	followUp := w.createBranch(t, ctx, alice, "knowledge-r2")
	w.assertEvidence(t, alice, followUp, "supports", claimV2.VersionID, experiment.VersionID, "experimental",
		"the 100-cycle measurement reproduces the retention the revised claim states")
	w.assertEvidence(t, alice, followUp, "contradicts", claimV2.VersionID, dataset.VersionID, "dataset",
		"the isotherm the experiment produced reads 88%, not the 90% the hypothesis' scope implies")

	// ---- 10. BROWSABLE (iii): the evidence READ — why the claim is
	// believed and why it is doubted, from one read, with the two stances in
	// their own buckets and nothing netted. The read is read in BOTH
	// directions: the version the assertions name carries them, and the two
	// versions they do NOT name (the finding's pinned v1, and main's own
	// appended version) answer their own empty groups — a read that served
	// v2's assertions under any other version's number would be reporting
	// evidence about one version as evidence about another.
	var evidence kEvidence
	w.getJSON(t, alice,
		fmt.Sprintf("/api/v1/projects/%s/objects/%s/evidence?version_no=%d",
			w.projectID, claim.ID, claimV2.VersionNo),
		http.StatusOK, &evidence)
	if len(evidence.Groups) != 1 {
		t.Fatalf("the evidence read answers %d target groups, want the one version the assertions pin: %+v",
			len(evidence.Groups), evidence.Groups)
	}
	group := evidence.Groups[0]
	if group.Target.ObjectVersionID != claimV2.VersionID {
		t.Fatalf("the evidence group is about version %s, want the asserted %s", group.Target.ObjectVersionID, claimV2.VersionID)
	}
	if len(group.Supporting) != 1 || group.Supporting[0].EvidenceObjectVersionID != experiment.VersionID {
		t.Fatalf("the supporting bucket = %+v, want the experiment version %s", group.Supporting, experiment.VersionID)
	}
	if len(group.Contesting) != 1 || group.Contesting[0].EvidenceObjectVersionID != dataset.VersionID {
		t.Fatalf("the contesting bucket = %+v, want the dataset version %s", group.Contesting, dataset.VersionID)
	}
	if group.Supporting[0].Relation != "supports" || group.Contesting[0].Relation != "contradicts" {
		t.Errorf("the buckets carry relations %q/%q, want supports/contradicts",
			group.Supporting[0].Relation, group.Contesting[0].Relation)
	}
	if len(group.Neutral) != 0 || len(group.Unlabeled) != 0 {
		t.Errorf("the read invented %d neutral and %d unlabeled assertions",
			len(group.Neutral), len(group.Unlabeled))
	}

	// The whole-object read is an inventory of the versions that carry
	// evidence: exactly one, and it is the published v2.
	var inventory kEvidence
	w.getJSON(t, alice,
		"/api/v1/projects/"+w.projectID+"/objects/"+claim.ID+"/evidence",
		http.StatusOK, &inventory)
	if len(inventory.Groups) != 1 || inventory.Groups[0].Target.ObjectVersionID != claimV2.VersionID {
		t.Fatalf("the object's evidence inventory = %+v, want only the asserted version %s",
			inventory.Groups, claimV2.VersionID)
	}
	// v1 answers its own empty group rather than v2's assertions: the
	// finding's pinned version carries no evidence, and the read says so.
	var v1Evidence kEvidence
	w.getJSON(t, alice,
		fmt.Sprintf("/api/v1/projects/%s/objects/%s/evidence?version_no=%d",
			w.projectID, claim.ID, claim.VersionNo),
		http.StatusOK, &v1Evidence)
	if len(v1Evidence.Groups) != 1 {
		t.Fatalf("the v1 evidence read answers %d groups, want v1's own (empty) group", len(v1Evidence.Groups))
	}
	if v1 := v1Evidence.Groups[0]; v1.Target.ObjectVersionID != claim.VersionID ||
		len(v1.Supporting)+len(v1.Contesting)+len(v1.Neutral)+len(v1.Unlabeled) != 0 {
		t.Fatalf("v1's evidence group = %+v, want the pinned v1 with no assertions — the read is not version-pinned", v1)
	}
	// main's appended version is a version of its own, and it carries no
	// assertions either: nothing has been asserted about it (the conflict
	// above is about the version the network published, which is the one the
	// reviewers pinned).
	var mainVersionEvidence kEvidence
	w.getJSON(t, alice,
		fmt.Sprintf("/api/v1/projects/%s/objects/%s/evidence?version_no=%d",
			w.projectID, claim.ID, claimOnMain.CurrentVersion),
		http.StatusOK, &mainVersionEvidence)
	if len(mainVersionEvidence.Groups) != 1 {
		t.Fatalf("main's appended version answers %d evidence groups, want its own", len(mainVersionEvidence.Groups))
	}
	if got := mainVersionEvidence.Groups[0]; got.Target.ObjectVersionID != claimOnMain.VersionID ||
		len(got.Supporting)+len(got.Contesting)+len(got.Neutral)+len(got.Unlabeled) != 0 {
		t.Fatalf("main's appended version carries %+v, want no assertions of its own", got)
	}

	// The page a person reads it on. The evidence panel is pinned to the
	// version the PAGE shows, and the detail page answers about the object's
	// newest version unless one is named — after the merge, the newest
	// version is the one main appended, which carries no assertion of its
	// own. So the version is named, and the default page is checked to
	// answer the empty state rather than borrowing the assertions of a
	// version it is not about.
	latestPage := w.getHTML(t, alice, w.objectPath(claim.ID, "evidence"), http.StatusOK)
	if strings.Contains(latestPage, `data-row-relation="`) {
		t.Fatalf("the claim's evidence tab borrows assertions for the newest version (%s), which carries none: %s",
			claimOnMain.VersionID, latestPage)
	}
	mustContain(t, latestPage, `data-graph-empty="evidence"`,
		"the empty state on the newest version's evidence tab")

	claimEvidencePage := w.getHTML(t, alice, w.objectPathAtVersion(claim.ID, "evidence", claimV2.VersionNo), http.StatusOK)
	mustContain(t, claimEvidencePage, `data-row-relation="supports"`, "the supporting assertion on the claim's page")
	mustContain(t, claimEvidencePage, `data-row-stance="supporting"`, "the supporting stance on the claim's page")
	mustContain(t, claimEvidencePage, `data-row-relation="contradicts"`, "the contesting assertion on the claim's page")
	mustContain(t, claimEvidencePage, `data-row-stance="contesting"`, "the contesting stance on the claim's page")
	mustContain(t, claimEvidencePage, `data-edge-stance="supporting"`, "the supporting edge in the claim's evidence diagram")
	mustContain(t, claimEvidencePage, `data-edge-stance="contesting"`, "the contesting edge in the claim's evidence diagram")
	mustContain(t, claimEvidencePage, experiment.VersionID, "the supporting evidence version on the claim's page")
	mustContain(t, claimEvidencePage, dataset.VersionID, "the contesting evidence version on the claim's page")
	// The version the panel is about, named by id: the two stances above are
	// stances about THIS version, and the page says which version that is.
	mustContain(t, claimEvidencePage, `data-node-id="`+claimV2.VersionID+`"`,
		"the version the evidence on the claim's page is about")
}

// ---------------------------------------------------------------------------
// Fixture helpers.

// newUUID draws a uuid from the database, so a payload can name the blob row
// that does not exist yet without inventing an id the database would refuse.
func (w *knowledgeE2EWorld) newUUID(t *testing.T) string {
	t.Helper()
	var id string
	if err := w.pool.QueryRow(t.Context(), `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
		t.Fatalf("draw a uuid: %v", err)
	}
	return id
}

// attachBlob writes one blob row and attaches it to a version, in that
// version's state. The rows are fixture data — this build has no upload
// route — but they are REAL rows: the merge's integrity engine refuses a
// payload whose blob reference resolves to nothing in the proposal's
// manifest, so a fixture that skipped this would prove the chain merges only
// by not checking.
func (w *knowledgeE2EWorld) attachBlob(t *testing.T, ctx context.Context, blobID, versionID, stateID, role, contentHash, storageKey string) {
	t.Helper()
	if _, err := w.pool.Exec(ctx,
		`INSERT INTO blobs (id, content_hash, size_bytes, media_type, storage_key, integrity_state, created_by)
		 VALUES ($1, $2, 4096, 'text/csv', $3, 'pending', $4)`,
		blobID, contentHash, storageKey, w.aliceID); err != nil {
		t.Fatalf("seed the %s blob: %v", role, err)
	}
	if _, err := w.pool.Exec(ctx,
		`INSERT INTO blob_attachments (blob_id, scientific_object_version_id, attachment_role, access_level, state_id)
		 VALUES ($1, $2, $3, 'restricted', $4)`,
		blobID, versionID, role, stateID); err != nil {
		t.Fatalf("attach the %s blob: %v", role, err)
	}
}

// mustRef asserts one rendered reference names the expected object.
func mustRef(t *testing.T, refs []kRef, objectType, objectID, where string) {
	t.Helper()
	for _, r := range refs {
		if r.ObjectID == objectID {
			if r.ObjectType != objectType {
				t.Fatalf("%s: %s renders as object_type %q, want %q", where, objectID, r.ObjectType, objectType)
			}
			if r.Title == "" {
				t.Fatalf("%s: %s renders with no title — a reference a reader cannot name", where, objectID)
			}
			return
		}
	}
	t.Fatalf("%s: %s is missing (rendered: %+v)", where, objectID, refs)
}

// mustContain asserts a rendered page carries a marker. Page assertions are
// made against the markup the templates emit (the data- attributes), not
// against a string that could appear in an unrelated context.
func mustContain(t *testing.T, page, marker, what string) {
	t.Helper()
	if !strings.Contains(page, marker) {
		t.Fatalf("%s: the page does not contain %q", what, marker)
	}
}
