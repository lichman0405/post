// Package integration — T0810 `network e2e` (blocking): the MINIMAL Open
// Network closed loop, run from end to end on a bare CI runner.
//
// The task book's chain, verbatim: public project → discover → fork/contribute
// → PR merge → profile credit → external evidence. The acceptance criterion is
// that the WHOLE chain is runnable in CI, which fixes two things about this
// file:
//
//   - It lives in tests/integration, so `make test-integration` — the
//     `migration-integration` CI job, a PostgreSQL service container and
//     nothing else — is what runs it. There is no Gitea and no Redis on that
//     runner, so the suite must not require them (T0814/T0817's own e2e
//     suites do; this one is the CI-visible one).
//   - Every hop that has a product route is driven OVER THAT ROUTE: project
//     creation, the fork, the contributor's write, the proposal, the reviews,
//     the merge, the publication and the evidence assertion are all requests
//     the product serves, behind the production guard, over a real PostgreSQL.
//
// # What is substituted, and what that substitution is not
//
// ONE dependency is replaced: the Git PROVIDER (network_fake_git_test.go's
// in-memory transport). It replaces the transport half of
// gitprovider.GitPort — the object that talks to Gitea — and nothing else.
// The production Provisioner, BranchRefSyncer, ForkImporter and push
// ingestion behind it, the production merge saga in front of it, the fork
// lineage judgment, the semantic gates and every row in PostgreSQL are the
// real ones. The Gitea half of the same chain is G3's:
// specs/orchestrator/gates.json gives this task `gitea-real-services`, and
// tests/integration/external_fork_merge_e2e_test.go (T0817) is the suite that
// runs that half on a real instance.
//
// # The assertions are per hop, not "it did not error"
//
// Each segment below asserts the FACT the hop is supposed to leave behind,
// read back from the canonical rows (a branch head, a version row, a
// pull_requests row, a semantic_merges row, a contribution_events row) rather
// than from the response body alone:
//
//  1. the project is discoverable by an ANONYMOUS explore read — discovery is
//     the public index, not the owner's own view of it;
//  2. the fork is a SEPARATE identity: its own project, its own repository,
//     its own branch, a lineage row that names this (parent, person) pair —
//     and the contributor's branch belongs to the FORK's project, which is
//     the fact the whole external path is built on;
//  3. the merge lands a NEW state on the UPSTREAM main carrying the
//     contributor's version, read version by version, with the payload
//     compared byte for byte against the source version;
//  4. the credit lands on the contributor's own profile and on nobody
//     else's;
//  5. the publication is readable by an anonymous caller, and the evidence
//     another project asserts against it is retrievable there and attributed
//     to the asserting project.
//
// # What the publication node publishes, and why it is not the landed version
//
// The publication read resolves a version through its OBJECT's project
// (internal/persistence/queries/knowledge_publish.sql:61-107), and a merge
// keeps the contributed object in the contributor's fork: it lands new VERSIONS
// of it upstream (asserted in step 7). A cross-project merged version is
// therefore not publishable in the upstream project, which this file pins as a
// refusal rather than papering over, and the loop publishes the upstream's own
// merged result instead — the object the network then cites. This is a
// boundary of the current build, reported in this task's RESULT.
//
// # The positive control for the provider half
//
// Step 9 is a SECOND proposal, this one entirely inside the upstream project.
// Both of its sides sit in one repository, so the merge saga does attempt the
// provider-side merge and this fixture's transport carries it: the merge
// records `updated` and a commit the provider's own main then points at. That
// is what makes step 7's recorded cross-project `failed` a boundary of the
// product rather than a transport this suite never learned to drive.
//
// # The one call that is not a product request
//
// `pullrequests.Service.RequestReview` — docs/43's open → review_required —
// has no route in this build and no cell in
// specs/policies/permissions-matrix.csv, so there is nothing to address a
// product request to. The repository's own precedent for that gap is
// tests/e2e-pr-flows/pr-flows-e2e.mjs:36-44 (and abort_e2e_test.go:605-615 in
// this package): a test-owned call to the production service method the
// missing route would call, touching no state column. This file does the same
// and names the gap in its RESULT. Everything else on the chain — including
// the two reviews, which are submitted over the review route — is product
// surface.
//
// # The negative controls
//
// A chain that only runs forward cannot show that any of its gates is load
// bearing, so the refusals below are pinned alongside it, each read back as a
// NAMED wire outcome: a non-member CANNOT propose from a branch that is not
// their own fork, a MEMBER who never forked cannot either (the sharper half —
// her class may open proposals in this project, so only the source-side rule
// can refuse her), a merge is REFUSED while the proposal is still under review
// (the state machine is not a formality on this path), and the git half of the
// CROSS-PROJECT merge is recorded as failed-with-a-reason rather than silently
// reported as done
// (T0817's documented boundary: one provider-side merge names one
// repository, so a pair that spans two repositories is not attempted and must
// not be recorded as if it were).
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/explorehttp"
	"github.com/lichman0405/post/cmd/api/forkshttp"
	"github.com/lichman0405/post/cmd/api/knowledgehttp"
	"github.com/lichman0405/post/cmd/api/mergegit"
	"github.com/lichman0405/post/cmd/api/mergehttp"
	"github.com/lichman0405/post/cmd/api/policyhttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/cmd/api/pullrequestshttp"
	"github.com/lichman0405/post/cmd/api/researchprofilehttp"
	"github.com/lichman0405/post/cmd/api/reviewhttp"
	"github.com/lichman0405/post/cmd/api/rsghttp"

	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/branches"
	appcontribution "github.com/lichman0405/post/internal/application/contribution"
	"github.com/lichman0405/post/internal/application/diffs"
	"github.com/lichman0405/post/internal/application/forks"
	"github.com/lichman0405/post/internal/application/knowledgepublish"
	"github.com/lichman0405/post/internal/application/merge"
	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/application/prchecks"
	"github.com/lichman0405/post/internal/application/prdiff"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/application/resolutions"
	"github.com/lichman0405/post/internal/application/responsibilities"
	"github.com/lichman0405/post/internal/application/reviews"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/contribution"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/gitprovider"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rights"
	"github.com/lichman0405/post/internal/rsg/integrity"
	"github.com/lichman0405/post/internal/rsg/schemareg"
)

const networkTaskID = "T0810"

// networkHookURL is the webhook address the production provisioner registers
// on every repository it creates. Nothing delivers to it in this suite (the
// chain contains no push: the contributor's writes are RSG requests and the
// fork's content arrives through the production importer), so it is a
// placeholder exactly as the Gitea-backed fixtures' is.
const networkHookURL = "http://host.invalid/api/v1/git/hooks/gitea"

// networkWorld is the graph cmd/api assembles — the same constructors, in the
// same order, over the same real pool, behind the same production guard — with
// the provider transport swapped for netGit.
type networkWorld struct {
	ctx  context.Context
	pool *pgxpool.Pool
	git  *netGit
	reg  *schemareg.Registry
	ts   *httptest.Server

	projectStore *persistence.ProjectStore
	policyStore  *persistence.PolicyStore
	stateStore   *persistence.StateStore
	branchStore  *persistence.BranchStore
	forkStore    *persistence.ForkStore
	provisioner  *gitprovider.Provisioner

	projectSvc *projects.Service
	stateSvc   *states.Service
	branchSvc  *branches.Service
	rsgSvc     *rsg.Service
	prSvc      *pullrequests.Service
	forkSvc    *forks.Service
	routingSvc *responsibilities.Service
	reviewsSvc *reviews.Service
	mergeSvc   *merge.Service

	// know carries the knowledge/evidence helpers of this package, over THIS
	// world's server: one composition of the routes, so the read the evidence
	// suite makes and the read this loop makes cannot drift apart.
	know *knowledgeWorld

	alice, bob, carol       *testUserClient
	aliceID, bobID, carolID string
}

func newNetworkWorld(t *testing.T) *networkWorld {
	t.Helper()
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), networkTaskID)

	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("network e2e: schema registry: %v", err)
	}
	git := newNetGit()

	// ---- The platform side: the graph cmd/api/main.go assembles, in its order.
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
	stateSvc := states.NewService(stateStore, newCommitGuard(t))
	forkStore := persistence.NewForkStore(pool)
	rsgSvc := rsg.NewService(rsg.Deps{
		Projects:  projectSvc,
		Branches:  branchSvc,
		States:    stateSvc,
		Latest:    stateStore,
		Objects:   persistence.NewScientificObjectStore(pool),
		Relations: persistence.NewRelationStore(pool),
		Authz:     authz.NewMatrixEngine(),
		Schemas:   reg,
		Events:    events.Recorder{},
		// write_scientific_state's own_fork_only cell is resolved through this
		// gate, never assumed by the service.
		ForkGate: forkStore,
		// The evidence network's write side (T0806): the assertion command
		// resolves both version pins and the target's publication through this
		// adapter. Without it the command fails closed (cmd/api/main.go:610
		// wires the same store).
		Evidence: persistence.NewEvidenceStore(pool),
	})
	prStore := persistence.NewPullRequestStore(pool)
	prSvc := pullrequests.NewService(prStore)
	provisioner := gitprovider.NewProvisioner(git, gitprovider.NewProvisionStore(pool), networkHookURL)
	// The importer takes the SAME ingester the webhook receiver is served by
	// in production: the copy a fork lands is inspected by the one
	// push-inspection implementation (T0305/T0817), so the fork's branch is
	// judged by the same rules as any other push.
	pushIngester := gitprovider.NewPushIngester(git, gitprovider.NewPushIngestStore(pool), reg)
	forkSvc := forks.NewService(forks.Deps{
		Projects:     projectSvc,
		Branches:     branchSvc,
		BranchWriter: rsgSvc,
		Forks:        forkStore,
		Repos:        provisioner,
		Imports: gitprovider.NewForkImporter(git, gitprovider.NewForkImportStore(pool),
			pushIngester),
		PullRequests: prSvc,
		Authz:        authz.NewMatrixEngine(),
	})
	diffSvc := diffs.NewService(stateStore, persistence.NewManifestStore(pool), prStore)
	resolutionSvc := resolutions.NewService(diffSvc, resolutions.NewPGStore(pool), projectSvc, authz.NewMatrixEngine())
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
		Commits:   stateSvc,
		Objects:   persistence.NewScientificObjectStore(pool),
		Relations: persistence.NewRelationStore(pool),
		Projects:  projectSvc,
		Authz:     authz.NewMatrixEngine(),
		Aborts:    persistence.NewScientificObjectStore(pool),
		// The fork lineage the merge's source-side read asks (T0817): the
		// source branch of an external proposal is admitted exactly when this
		// says it is the proposal author's own fork of the proposal's project.
		Forks: forkStore,
		Checks: prchecks.NewService(prchecks.Deps{
			PRs:      prStore,
			Projects: projectStore,
			States:   stateStore,
			Branches: branchStore,
			Manifest: persistence.NewManifestStore(pool),
			Policies: policyStore,
			Engine:   integrity.New(reg),
		}),
		Policies: policyAPI.Service(),
		Rules:    policy.NewRuleEvaluator(),
		Events:   events.Recorder{},
		Git:      mergegit.New(git, gitprovider.NewUserAccessStore(pool)),
		// The identity the provider-reconciliation rule accepts a main update
		// from. In production it is the provider's service account
		// (gitprovider.RefGuard over the provider's own login); here it is the
		// identity netGit reports as the actor of a provider-side merge, which
		// is the same thing one layer down.
		RefGuard: gitprovider.RefGuard{MergeService: netGitMergeLogin},
	})

	// ---- The HTTP surface: the production route assemblies behind the
	// production guard, in cmd/api/main.go's own order.
	v1 := http.NewServeMux()
	authAPI.Register(v1)
	researchProfileAPI := researchprofilehttp.New(researchprofilehttp.Deps{
		Reader: persistence.NewResearchProfileStore(pool),
	})
	researchProfileAPI.Register(v1)
	projectRoutes := projectAPI.Routes()
	v1.Handle("/api/v1/projects", projectRoutes)
	v1.Handle("/api/v1/projects/", projectRoutes)
	rsghttp.New(rsghttp.Deps{Service: rsgSvc}).Register(v1)
	checksSvc := prchecks.NewService(prchecks.Deps{
		PRs:      prStore,
		Projects: projectStore,
		States:   stateStore,
		Branches: branchStore,
		Manifest: persistence.NewManifestStore(pool),
		Policies: policyStore,
		Engine:   integrity.New(reg),
	})
	pullrequestshttp.New(pullrequestshttp.Deps{
		PullRequests: prSvc,
		Create:       forkSvc,
		Checks:       checksSvc,
		Projects:     projectSvc,
	}).Register(v1)
	forkshttp.New(forkshttp.Deps{
		Forks:    forkSvc,
		Projects: projectSvc,
		Missing:  nil,
	}).Register(v1)
	reviewhttp.New(reviewhttp.Deps{Service: reviewsSvc, Projects: projectSvc}).Register(v1)
	mergehttp.New(mergehttp.Deps{Command: mergeSvc, Projects: projectSvc}).Register(v1)
	// The Explore index (T0802): the anonymous read that makes a public
	// project DISCOVERABLE, built from the same public reads the surfaces it
	// aggregates already serve.
	explorehttp.New(explorehttp.Deps{
		Reader: explorehttp.Sources{
			ProjectSource:      explorehttp.ProjectSource{Service: projectSvc},
			AssetSource:        explorehttp.AssetSource{Pages: persistence.NewAssetPageStore(pool)},
			ContributionSource: explorehttp.ContributionSource{Service: appcontribution.NewService(contribution.NewOpportunityStore(pool))},
			Store:              explorehttp.NewStore(pool),
		},
	}).Register(v1)
	// Published knowledge (T0605) with the origin/network evidence read
	// (T0806) served by the same adapter the evidence write path uses.
	knowledgePublishStore := persistence.NewKnowledgePublishStore(pool)
	knowledgehttp.New(knowledgehttp.Deps{
		Publish: knowledgepublish.NewCommand(knowledgepublish.Deps{
			Members: persistence.NewProjectStore(pool),
			Store:   knowledgePublishStore,
			Authz:   authz.NewMatrixEngine(),
		}),
		Read:     knowledgePublishStore,
		Projects: projectSvc,
		Members:  projectSvc,
		Evidence: persistence.NewEvidenceStore(pool),
	}).Register(v1)

	rootMux := http.NewServeMux()
	rootMux.Handle("/api/v1/", authAPI.Guard(v1))
	ts := httptest.NewServer(rootMux)
	t.Cleanup(ts.Close)

	w := &networkWorld{
		ctx: ctx, pool: pool, git: git, reg: reg, ts: ts,
		projectStore: projectStore, policyStore: policyStore,
		stateStore: stateStore, branchStore: branchStore, forkStore: forkStore,
		provisioner: provisioner,
		projectSvc:  projectSvc, stateSvc: stateSvc, branchSvc: branchSvc,
		rsgSvc: rsgSvc, prSvc: prSvc, forkSvc: forkSvc,
		routingSvc: routingSvc, reviewsSvc: reviewsSvc, mergeSvc: mergeSvc,
	}
	w.alice, w.aliceID = signup(t, ts.URL, "network-alice@example.com", "network-alice")
	w.bob, w.bobID = signup(t, ts.URL, "network-bob@example.com", "network-bob")
	w.carol, w.carolID = signup(t, ts.URL, "network-carol@example.com", "network-carol")
	w.know = &knowledgeWorld{
		ts: ts, pool: pool, svc: rsgSvc,
		alice: w.alice, bob: w.bob, carol: w.carol,
		aliceID: w.aliceID, bobID: w.bobID, carolID: w.carolID,
	}
	return w
}

// ---- The hops -------------------------------------------------------------

// createProject drives the product's create-project route.
func (w *networkWorld) createProject(t *testing.T, uc *testUserClient, slug string) domain.Project {
	t.Helper()
	body := fmt.Sprintf(`{"slug":%q,"name":%q,"purpose":%q,"visibility":"public"}`,
		slug, slug, "T0810 open network closed loop")
	resp := uc.do(t, http.MethodPost, "/api/v1/projects", body)
	mustStatus(t, resp, http.StatusCreated)
	var payload struct {
		Project struct {
			ID         string `json:"id"`
			Visibility string `json:"visibility"`
		} `json:"project"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("network e2e: decode the created project: %v", err)
	}
	if payload.Project.Visibility != string(domain.VisibilityPublic) {
		t.Fatalf("network e2e: the project was created %q, want public — nothing else in this chain is discoverable",
			payload.Project.Visibility)
	}
	project, err := w.projectSvc.Get(w.ctx, projects.Reader{UserID: w.aliceID, Authenticated: true}, payload.Project.ID)
	if err != nil {
		t.Fatalf("network e2e: read back the created project: %v", err)
	}
	return project
}

// provision runs the production provisioner — the work the provisioning job
// does — and returns the repository name it created.
func (w *networkWorld) provision(t *testing.T, projectID string) string {
	t.Helper()
	if err := w.provisioner.Provision(w.ctx, projectID); err != nil {
		t.Fatalf("network e2e: provision %s: %v", projectID, err)
	}
	var name string
	if err := w.pool.QueryRow(w.ctx,
		`SELECT name FROM git_repository_provisions WHERE project_id = $1`, projectID).Scan(&name); err != nil {
		t.Fatalf("network e2e: read the provision row of %s: %v", projectID, err)
	}
	return name
}

// newBranch creates one branch through the canonical RSG service and syncs
// its provider ref (the work the refsync job does). baseRef is the base state
// the line forks ("" = the project's latest state).
func (w *networkWorld) newBranch(t *testing.T, actorID, projectID, name, baseRef string, visibility domain.BranchVisibility) domain.Branch {
	t.Helper()
	branch, err := w.rsgSvc.CreateBranch(w.ctx, domain.User{ID: actorID}, projectID, rsg.CreateBranchInput{
		Name:       name,
		BaseRef:    baseRef,
		Visibility: visibility,
	})
	if err != nil {
		t.Fatalf("network e2e: create branch %s in %s: %v", name, projectID, err)
	}
	syncer := gitprovider.NewBranchRefSyncer(w.git, gitprovider.NewBranchRefStore(w.pool))
	if err := syncer.Sync(w.ctx, branch.ID); err != nil {
		t.Fatalf("network e2e: sync the ref of %s in %s: %v", name, projectID, err)
	}
	return branch
}

// forkRequest is the wire body T0814's route takes, and forkReply the document
// it answers.
type forkReply struct {
	Fork struct {
		ForkProjectID   string `json:"fork_project_id"`
		ParentProjectID string `json:"parent_project_id"`
		ForkedBy        string `json:"forked_by"`
		RelationType    string `json:"relation_type"`
		SourceBranchID  string `json:"source_branch_id"`
		ForkBranchID    string `json:"fork_branch_id"`
	} `json:"fork"`
	Project struct {
		ID         string `json:"id"`
		Slug       string `json:"slug"`
		Visibility string `json:"visibility"`
	} `json:"project"`
	Branch struct {
		ID        string `json:"id"`
		ProjectID string `json:"project_id"`
		Name      string `json:"name"`
	} `json:"branch"`
	AlreadyForked bool `json:"already_forked"`
	Imported      bool `json:"imported"`
}

// fork drives the product's fork route as a NON-MEMBER of the parent.
func (w *networkWorld) fork(t *testing.T, uc *testUserClient, parentID string) forkReply {
	t.Helper()
	resp := uc.do(t, http.MethodPost, "/api/v1/projects/"+parentID+"/forks",
		`{"visibility":"public"}`)
	mustStatus(t, resp, http.StatusCreated)
	var reply forkReply
	raw := readAll(t, resp)
	if err := json.Unmarshal([]byte(raw), &reply); err != nil {
		t.Fatalf("network e2e: decode the fork payload: %v: %s", err, raw)
	}
	return reply
}

// openPR drives the product's open-pull-request route. For the external hop
// the source branch is the contributor's fork branch, which the route admits
// through the fork command (open_pr = allow_from_fork) rather than through
// project membership; the same helper opens the upstream's own proposal from
// a line of the upstream project.
func (w *networkWorld) openPR(t *testing.T, uc *testUserClient, projectID, sourceBranchID, targetBranchID, title, key string) flowPR {
	t.Helper()
	body := fmt.Sprintf(`{"source_branch_id":%q,"target_branch_id":%q,"title":%q,"body":"proposed for the T0810 network e2e"}`,
		sourceBranchID, targetBranchID, title)
	resp := uc.doKeyed(t, http.MethodPost, "/api/v1/projects/"+projectID+"/pull-requests", body, key)
	mustStatus(t, resp, http.StatusCreated)
	return decodeFlow[flowPR](t, resp)
}

// networkRefusal decodes one refusal envelope: the negative controls below
// assert that a refused request carries a NAMED outcome, not merely a
// non-2xx status (a route that answered 500, or 400 for a missing header,
// would satisfy "not created" while proving nothing about the rule).
func networkRefusal(t *testing.T, raw string) struct {
	Code    string `json:"code"`
	Message string `json:"message"`
} {
	t.Helper()
	var refusal struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(raw), &refusal); err != nil {
		t.Fatalf("network e2e: decode the refusal: %v: %s", err, raw)
	}
	if refusal.Code == "" {
		t.Fatalf("network e2e: the refusal carries no wire code: %s", raw)
	}
	return refusal
}

// assertEvidence posts one evidence assertion over the product's RSG route and
// returns the stored row.
func (w *networkWorld) assertEvidence(t *testing.T, uc *testUserClient, projectID, branchID, body string) evidenceAssertionWire {
	t.Helper()
	resp := uc.do(t, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/branches/%s/evidence-assertions", projectID, branchID), body)
	mustStatus(t, resp, http.StatusCreated)
	raw := readAll(t, resp)
	var got evidenceAssertionWire
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("network e2e: decode the stored assertion: %v: %s", err, raw)
	}
	return got
}

// writeQuestion writes one research_question — a publishable knowledge object
// type — on a branch through the product's RSG route.
func (w *networkWorld) writeQuestion(t *testing.T, uc *testUserClient, projectID, branchID, statement string) (objectID, versionID, stateID string) {
	t.Helper()
	body := fmt.Sprintf(
		`{"object_type":"research_question","payload":{"statement":%q,"purpose":%q,"question_state":"open"}}`,
		statement, "close the loop on "+statement)
	resp := uc.do(t, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/branches/%s/objects", projectID, branchID), body)
	mustStatus(t, resp, http.StatusCreated)
	var written struct {
		ID        string `json:"id"`
		VersionID string `json:"version_id"`
		StateID   string `json:"state_id"`
	}
	raw := readAll(t, resp)
	if err := json.Unmarshal([]byte(raw), &written); err != nil {
		t.Fatalf("network e2e: decode the accepted write: %v: %s", err, raw)
	}
	return written.ID, written.VersionID, written.StateID
}

// ---- Canonical-row probes -------------------------------------------------

func (w *networkWorld) branchHead(t *testing.T, branchID string) string {
	t.Helper()
	var id string
	if err := w.pool.QueryRow(w.ctx,
		`SELECT base_state_id::text FROM branches WHERE id = $1`, branchID).Scan(&id); err != nil {
		t.Fatalf("network e2e: read the head of branch %s: %v", branchID, err)
	}
	return id
}

func (w *networkWorld) branchLifecycle(t *testing.T, branchID string) string {
	t.Helper()
	var lifecycle string
	if err := w.pool.QueryRow(w.ctx,
		`SELECT lifecycle_state FROM branches WHERE id = $1`, branchID).Scan(&lifecycle); err != nil {
		t.Fatalf("network e2e: read the lifecycle of branch %s: %v", branchID, err)
	}
	return lifecycle
}

// stateProject answers which project a state belongs to — the fact that makes
// "the source side was read where it lives" checkable.
func (w *networkWorld) stateProject(t *testing.T, stateID string) string {
	t.Helper()
	var projectID string
	if err := w.pool.QueryRow(w.ctx,
		`SELECT project_id::text FROM project_states WHERE id = $1`, stateID).Scan(&projectID); err != nil {
		t.Fatalf("network e2e: read the project of state %s: %v", stateID, err)
	}
	return projectID
}

// versionRowNetwork is the canonical row of one version, plus the version the
// object resolves to in a given state.
type versionRowNetwork struct {
	id             string
	payload        []byte
	title          string
	stateID        string
	branchID       *string
	lifecycleState string
}

func (w *networkWorld) readVersionRow(t *testing.T, versionID string) versionRowNetwork {
	t.Helper()
	var row versionRowNetwork
	if err := w.pool.QueryRow(w.ctx, `
		SELECT id::text, payload, title, state_id::text, branch_id::text, lifecycle_state
		  FROM scientific_object_versions WHERE id = $1`, versionID).
		Scan(&row.id, &row.payload, &row.title, &row.stateID, &row.branchID, &row.lifecycleState); err != nil {
		t.Fatalf("network e2e: read version %s: %v", versionID, err)
	}
	return row
}

// versionOfObjectInState reads the version an object carries in one state —
// the row a reader of that state resolves the object to.
func (w *networkWorld) versionOfObjectInState(t *testing.T, objectID, stateID string) versionRowNetwork {
	t.Helper()
	var id string
	if err := w.pool.QueryRow(w.ctx, `
		SELECT id::text FROM scientific_object_versions
		 WHERE object_id = $1 AND state_id = $2
		 ORDER BY version_no DESC, id LIMIT 1`, objectID, stateID).Scan(&id); err != nil {
		t.Fatalf("network e2e: read object %s's version in state %s: %v", objectID, stateID, err)
	}
	return w.readVersionRow(t, id)
}

// semanticMergeRow is the merge record: which project the merge landed in and
// which project each side of its triple belongs to.
type semanticMergeRow struct {
	projectID       string
	sourceStateID   string
	targetStateID   string
	resultStateID   string
	sourceBranchID  string
	targetBranchID  string
	gitState        string
	gitError        string
	sourceProjectID string
}

func (w *networkWorld) readMergeRow(t *testing.T, mergeID string) semanticMergeRow {
	t.Helper()
	var row semanticMergeRow
	if err := w.pool.QueryRow(w.ctx, `
		SELECT m.project_id::text, m.source_state_id::text, m.target_state_id::text,
		       m.result_state_id::text,
		       m.source_branch_id::text, m.target_branch_id::text,
		       m.git_state, COALESCE(m.git_error, ''),
		       s.project_id::text
		  FROM semantic_merges m
		  JOIN project_states s ON s.id = m.source_state_id
		 WHERE m.id = $1`, mergeID).
		Scan(&row.projectID, &row.sourceStateID, &row.targetStateID, &row.resultStateID,
			&row.sourceBranchID, &row.targetBranchID, &row.gitState, &row.gitError, &row.sourceProjectID); err != nil {
		t.Fatalf("network e2e: read merge %s: %v", mergeID, err)
	}
	return row
}

// contributionRow is one row of a person's Research Profile.
type contributionRow struct {
	EventType string   `json:"event_type"`
	RoleCodes []string `json:"role_codes"`
	Project   *struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
	} `json:"project"`
}

type personProfile struct {
	Person struct {
		ID     string `json:"id"`
		Handle string `json:"handle"`
	} `json:"person"`
	Contributions []contributionRow `json:"contributions"`
}

func (w *networkWorld) researchProfile(t *testing.T, uc *testUserClient, userID string) personProfile {
	t.Helper()
	resp := uc.do(t, http.MethodGet, "/api/v1/users/"+userID+"/research-profile", "")
	mustStatus(t, resp, http.StatusOK)
	var profile personProfile
	raw := readAll(t, resp)
	if err := json.Unmarshal([]byte(raw), &profile); err != nil {
		t.Fatalf("network e2e: decode the research profile: %v: %s", err, raw)
	}
	return profile
}

// ---- The loop -------------------------------------------------------------

// TestOpenNetworkLoopEndToEnd is T0810's required test: label `network e2e`.
func TestOpenNetworkLoopEndToEnd(t *testing.T) {
	w := newNetworkWorld(t)
	ctx := w.ctx

	// ---- 1. A PUBLIC project, owned by Alice, provisioned for real (the
	// repository, the bootstrap main, the webhook and the freeze rule are the
	// production provisioner's work).
	parent := w.createProject(t, w.alice, "open-network-mof")
	parentRepo := w.provision(t, parent.ID)
	mainBranch := w.newBranch(t, w.aliceID, parent.ID, domain.MainBranchName, "", domain.BranchVisibilityPrivate)
	mainBefore := w.branchHead(t, mainBranch.ID)
	// The policy in force is the project's own (the production merge and
	// review reads resolve it).
	seedProjectPolicy(t, ctx, w.policyStore, parent, w.aliceID)

	// ---- 2. DISCOVER: the project is on the anonymous index. Discovery is
	// the public read — an owner's own view of their project would prove
	// nothing about whether anybody else can find it.
	anon := newTestUserClient(w.ts.URL)
	exploreResp := anon.do(t, http.MethodGet, "/api/v1/explore", "")
	mustStatus(t, exploreResp, http.StatusOK)
	exploreRaw := readAll(t, exploreResp)
	if !strings.Contains(exploreRaw, parent.ID) {
		t.Fatalf("network e2e: the public project %s is not on the anonymous Explore index: %s",
			parent.ID, exploreRaw)
	}

	// ---- 3. FORK: a non-member takes the public project into their own
	// space, over the product's own route.
	forked := w.fork(t, w.bob, parent.ID)
	if forked.Fork.ForkProjectID == "" || forked.Fork.ForkProjectID == parent.ID {
		t.Fatalf("network e2e: the fork answered project %q, want a project of its own", forked.Fork.ForkProjectID)
	}
	if forked.Fork.ParentProjectID != parent.ID {
		t.Errorf("network e2e: the lineage names parent %q, want %s", forked.Fork.ParentProjectID, parent.ID)
	}
	if forked.Fork.ForkedBy != w.bobID {
		t.Errorf("network e2e: the lineage names %q as the forker, want the contributor %s", forked.Fork.ForkedBy, w.bobID)
	}
	if forked.Fork.RelationType != "forked_from" {
		t.Errorf("network e2e: the lineage edge is %q, want the canonical vocabulary's 'forked_from'",
			forked.Fork.RelationType)
	}
	if forked.Project.ID != forked.Fork.ForkProjectID {
		t.Fatalf("network e2e: the payload's project %s and lineage %s disagree",
			forked.Project.ID, forked.Fork.ForkProjectID)
	}
	// The fork is an INDEPENDENT identity: its own project, its own
	// repository, its own branch — and the branch belongs to the FORK's
	// project, which is the fact the whole external path rests on.
	if forked.Branch.ProjectID != forked.Fork.ForkProjectID {
		t.Errorf("network e2e: the fork branch belongs to project %s, want the fork's %s",
			forked.Branch.ProjectID, forked.Fork.ForkProjectID)
	}
	var parentProject, forkedBy string
	if err := w.pool.QueryRow(ctx, `
		SELECT parent_project_id::text, forked_by::text FROM project_forks
		 WHERE fork_project_id = $1`, forked.Fork.ForkProjectID).Scan(&parentProject, &forkedBy); err != nil {
		t.Fatalf("network e2e: read the fork lineage row: %v", err)
	}
	if parentProject != parent.ID || forkedBy != w.bobID {
		t.Fatalf("network e2e: the stored lineage is (%s, %s), want (%s, %s)",
			parentProject, forkedBy, parent.ID, w.bobID)
	}
	var forkRepo string
	if err := w.pool.QueryRow(ctx,
		`SELECT name FROM git_repository_provisions WHERE project_id = $1`, forked.Fork.ForkProjectID).
		Scan(&forkRepo); err != nil {
		t.Fatalf("network e2e: the fork project has no repository of its own: %v", err)
	}
	if forkRepo == parentRepo {
		t.Fatalf("network e2e: the fork was provisioned onto the parent's repository %s", forkRepo)
	}
	// The transport carried the copy: the fork's own repository holds the
	// parent's main HEAD on the fork's branch — an import that never happened
	// would leave the ref empty — and the CONTENT that came with it names the
	// parent's repository, so a copy holding it can only have come from there.
	parentMainSHA := w.git.head(netGitServiceLogin, parentRepo, string(domain.MainBranchName))
	if parentMainSHA == "" {
		t.Fatalf("network e2e: the parent's repository carries no main in the transport")
	}
	if got := w.git.head(netGitServiceLogin, forkRepo, forked.Branch.Name); got != parentMainSHA {
		t.Errorf("network e2e: the fork's branch %s is at %q in the transport, the parent's main at %q: the "+
			"content copy did not land", forked.Branch.Name, got, parentMainSHA)
	}
	wantBootstrap := "# " + parentRepo + "\n\nplatform bootstrap commit\n"
	if content, ok := w.git.fileAt(netGitServiceLogin, forkRepo, forked.Branch.Name, netGitBootstrapPath); !ok || content != wantBootstrap {
		t.Errorf("network e2e: the fork's branch carries %q at %s (present=%v), want the parent's own bootstrap "+
			"content", content, netGitBootstrapPath, ok)
	}
	forkRefs := w.git.refNames(netGitServiceLogin, forkRepo)
	if !slices.Contains(forkRefs, forked.Branch.Name) || !slices.Contains(forkRefs, string(domain.MainBranchName)) {
		t.Errorf("network e2e: the fork's repository carries refs %v, want at least %q and %q",
			forkRefs, forked.Branch.Name, domain.MainBranchName)
	}
	if slices.Contains(w.git.refNames(netGitServiceLogin, parentRepo), forked.Branch.Name) {
		t.Errorf("network e2e: the parent's repository carries the fork's branch %s", forked.Branch.Name)
	}

	// ---- 4. CONTRIBUTE: the contributor writes scientific state on the FORK's
	// branch (write_scientific_state = own_fork_only lets him write there and
	// nowhere else).
	const statement = "the external contributor's measured band gap of 0.9 eV"
	objectID, forkVersionID, writtenState := w.writeQuestion(t, w.bob, forked.Project.ID, forked.Branch.ID, statement)
	if got := w.stateProject(t, writtenState); got != forked.Fork.ForkProjectID {
		t.Fatalf("network e2e: the contributor's state belongs to project %s, want his fork's %s",
			got, forked.Fork.ForkProjectID)
	}
	forkHead := w.branchHead(t, forked.Branch.ID)
	if forkHead != writtenState {
		t.Fatalf("network e2e: the fork branch's head is %s, want the state the write produced %s",
			forkHead, writtenState)
	}

	// ---- 5. THE EXTERNAL PROPOSAL: an upstream PR whose source branch lives
	// in the contributor's fork.
	pr := w.openPR(t, w.bob, parent.ID, forked.Branch.ID, mainBranch.ID, "external measurement from a fork",
		"network-e2e-open-0001")
	if pr.CreatedBy != w.bobID {
		t.Fatalf("network e2e: the proposal records author %q, want the contributor %s", pr.CreatedBy, w.bobID)
	}
	var prProject string
	if err := w.pool.QueryRow(ctx,
		`SELECT project_id::text FROM pull_requests WHERE id = $1`, pr.ID).Scan(&prProject); err != nil {
		t.Fatalf("network e2e: read the proposal's project: %v", err)
	}
	if prProject != parent.ID {
		t.Fatalf("network e2e: the proposal belongs to project %s, want the upstream %s", prProject, parent.ID)
	}
	if pr.SourceBranchID != forked.Branch.ID {
		t.Fatalf("network e2e: the proposal's source branch is %s, want the fork's %s", pr.SourceBranchID, forked.Branch.ID)
	}
	if pr.ProposedStateID != forkHead {
		t.Fatalf("network e2e: the proposal pins state %s, want the fork branch's head %s", pr.ProposedStateID, forkHead)
	}
	if got := w.stateProject(t, pr.ProposedStateID); got != forked.Fork.ForkProjectID {
		t.Fatalf("network e2e: the proposed state belongs to project %s, want the fork's %s",
			got, forked.Fork.ForkProjectID)
	}

	// The negative control for the rule that makes this proposal legitimate:
	// the SAME source branch offered by somebody who did not fork the project
	// is refused (docs/04 §2's open_pr = allow_from_fork for the class this
	// caller is in). Without this, "the chain runs" would not distinguish the
	// external path from an open door. Carol is a non-member here — she never
	// forked this project — and the refusal is read back as a named outcome.
	refused := w.carol.doKeyed(t, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/pull-requests", parent.ID),
		fmt.Sprintf(`{"source_branch_id":%q,"target_branch_id":%q,"title":"not mine","body":""}`,
			forked.Branch.ID, mainBranch.ID), "network-e2e-open-stranger-0001")
	if refused.StatusCode == http.StatusCreated {
		t.Fatal("network e2e: a caller who never forked the project opened a proposal from another person's fork branch")
	}
	refusedRaw := readAll(t, refused)
	if refused.StatusCode < 400 || refused.StatusCode >= 500 {
		t.Fatalf("network e2e: the stranger's proposal answered %d, want a refusal: %s", refused.StatusCode, refusedRaw)
	}
	// The refusal is a named outcome, not an empty error: the envelope carries
	// one of docs/45's stable codes, so "refused" is a product answer rather
	// than a 500 in disguise.
	refusal := networkRefusal(t, refusedRaw)
	t.Logf("network e2e: a proposal from a branch that is not the caller's own fork is refused with %d %s: %s",
		refused.StatusCode, refusal.Code, refusal.Message)

	// ---- 6. GOVERNANCE: the upstream project's reviewers walk the proposal
	// through its review dimensions. The members, the responsibility label and
	// the routing rule are the upstream's own governance, not the
	// contributor's: an external fork is not a review venue.
	for _, m := range []struct {
		id   string
		role domain.ProjectRole
	}{{w.carolID, domain.ProjectRoleContributor}, {w.bobID, domain.ProjectRoleMaintainer}} {
		if _, err := w.pool.Exec(ctx,
			`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, $3)`,
			parent.ID, m.id, m.role); err != nil {
			t.Fatalf("network e2e: seed the %s membership: %v", m.role, err)
		}
	}
	// The SHARPER half of the same negative control: a member who may open a
	// proposal in this project (open_pr = allow for her class) still cannot
	// propose FROM a branch that is somebody else's fork. The class-based
	// refusal above would be satisfied by a caller who was simply not allowed
	// to open anything; this one isolates the source-side rule, and migration
	// 00086's trigger is the backstop underneath it.
	foreignSource := w.carol.doKeyed(t, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/pull-requests", parent.ID),
		fmt.Sprintf(`{"source_branch_id":%q,"target_branch_id":%q,"title":"a member, not the forker","body":""}`,
			forked.Branch.ID, mainBranch.ID), "network-e2e-open-member-0001")
	if foreignSource.StatusCode == http.StatusCreated {
		t.Fatal("network e2e: a member who never forked opened a proposal from another person's fork branch")
	}
	foreignRaw := readAll(t, foreignSource)
	memberRefusal := networkRefusal(t, foreignRaw)
	t.Logf("network e2e: a member proposing from somebody else's fork branch is refused with %d %s: %s",
		foreignSource.StatusCode, memberRefusal.Code, memberRefusal.Message)
	// Bob is a member of the UPSTREAM project here, which is a second hat he
	// wears in this fixture only: the membership is what lets him review, and
	// it changes nothing about the fork path he walked above (that path was
	// driven before this insert, as a non-member).
	for _, id := range []string{w.carolID, w.bobID} {
		if _, err := w.routingSvc.Assign(ctx, domain.User{ID: w.aliceID}, parent.ID, id, "Data Reviewer"); err != nil {
			t.Fatalf("network e2e: assign the responsibility label: %v", err)
		}
	}
	if _, err := w.routingSvc.AddRule(ctx, domain.User{ID: w.aliceID}, parent.ID, responsibilities.AddRuleInput{
		MatchKind:      domain.ResearchOwnerMatchObjectType,
		MatchValue:     "research_question",
		Responsibility: "Data Reviewer",
	}); err != nil {
		t.Fatalf("network e2e: route research_question changes to a reviewer: %v", err)
	}

	// docs/43's open → review_required. NO ROUTE EXISTS for this transition
	// (specs/api/openapi.yaml declares no request-review operation and
	// specs/policies/permissions-matrix.csv has no request_review cell), so
	// this is the one call in the loop that is not a product request: it is
	// the production service method the missing route would call, and it
	// touches no state column itself.
	if _, err := w.prSvc.RequestReview(ctx, parent.ID, pr.Number); err != nil {
		t.Fatalf("network e2e: request review: %v", err)
	}
	// The merge is REFUSED while the proposal is under review: the state
	// machine is load bearing on the external path too, not just on the
	// same-project one (the positive control for the merge below).
	underReview := w.alice.doKeyed(t, http.MethodPost, mergePath(parent.ID, pr.Number), "", "network-e2e-merge-too-early")
	if underReview.StatusCode == http.StatusOK {
		t.Fatalf("network e2e: the merge landed while the proposal was still %s", pr.State)
	}
	_ = readAll(t, underReview)
	for _, submission := range []struct {
		uc   *testUserClient
		kind string
	}{{w.carol, "scientific"}, {w.bob, "integrity"}} {
		body := fmt.Sprintf(`{"kind":%q,"decision":"approved","body":"reviewed for the T0810 network e2e"}`,
			submission.kind)
		resp := submission.uc.do(t, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/pull-requests/%d/reviews", parent.ID, pr.Number), body)
		mustStatus(t, resp, http.StatusCreated)
	}
	reviewed, err := w.prSvc.Get(ctx, parent.ID, pr.Number)
	if err != nil {
		t.Fatalf("network e2e: read the proposal after its reviews: %v", err)
	}
	if reviewed.State != domain.PullRequestStateMergeReady {
		t.Fatalf("network e2e: the reviewed proposal is %q, want %q",
			reviewed.State, domain.PullRequestStateMergeReady)
	}

	// ---- 7. PR MERGE, over the product's merge route, by the upstream
	// project's owner.
	merged := mergeThroughTheEndpoint(t, w.alice, parent.ID, pr.Number, "network-e2e-merge-key-0001")
	if merged.Replayed {
		t.Fatal("network e2e: the first merge reports itself as a replay")
	}
	if merged.Number != pr.Number {
		t.Errorf("network e2e: the merge answered for pull request %d, the request named %d", merged.Number, pr.Number)
	}
	// The upstream's own truth: main's head is a NEW state, and it is the one
	// this merge committed.
	mainAfter := w.branchHead(t, mainBranch.ID)
	if mainAfter == mainBefore {
		t.Fatalf("network e2e: the upstream main's head did not move (still %s): the merge accepted nothing", mainAfter)
	}
	if merged.StateID != mainAfter {
		t.Fatalf("network e2e: main's head is %s, the merge answered %s", mainAfter, merged.StateID)
	}
	if got := w.stateProject(t, mainAfter); got != parent.ID {
		t.Fatalf("network e2e: the accepted state belongs to project %s, want the upstream %s", got, parent.ID)
	}
	// The content, item by item: the accepted state carries the contributor's
	// version — an ACTIVE version of the object on the upstream main, whose
	// payload is the source version's byte for byte. "A state id changed" is
	// not evidence that the contribution travelled.
	landed := w.versionOfObjectInState(t, objectID, mainAfter)
	source := w.readVersionRow(t, forkVersionID)
	if landed.id == forkVersionID {
		t.Fatal("network e2e: the accepted state names the fork's own version row — the merge wrote nothing")
	}
	if landed.lifecycleState != string(domain.LifecycleActive) {
		t.Errorf("network e2e: the landed version's lifecycle is %q, want active", landed.lifecycleState)
	}
	if landed.branchID == nil || *landed.branchID != mainBranch.ID {
		t.Errorf("network e2e: the landed version belongs to branch %v, want the upstream main %s",
			landed.branchID, mainBranch.ID)
	}
	if string(landed.payload) != string(source.payload) {
		t.Errorf("network e2e: the landed payload is not the source version's, byte for byte:\n  landed: %s\n  source: %s",
			landed.payload, source.payload)
	}
	if landed.title != source.title {
		t.Errorf("network e2e: the landed version's title is %q, want the source version's %q", landed.title, source.title)
	}
	// The object did not change hands: the merge landed a VERSION of the
	// contributor's object and the object row is still his fork's. Step 10
	// turns on this — it is a fact about the merge, not about this fixture.
	var landedObjectProject string
	if err := w.pool.QueryRow(ctx,
		`SELECT project_id::text FROM scientific_objects WHERE id = $1`, objectID).Scan(&landedObjectProject); err != nil {
		t.Fatalf("network e2e: read the landed object's project: %v", err)
	}
	if landedObjectProject != forked.Fork.ForkProjectID {
		t.Errorf("network e2e: the contributor's object now belongs to project %s, want his fork %s",
			landedObjectProject, forked.Fork.ForkProjectID)
	}
	// The merge record proves WHICH side was read where: the merge belongs to
	// the upstream project while its source state and source branch belong to
	// the contributor's fork.
	mergeRow := w.readMergeRow(t, merged.MergeID)
	if mergeRow.projectID != parent.ID {
		t.Errorf("network e2e: the merge record belongs to project %s, want the upstream %s", mergeRow.projectID, parent.ID)
	}
	if mergeRow.sourceProjectID != forked.Fork.ForkProjectID {
		t.Errorf("network e2e: the merge read its source state in project %s, want the contributor's fork %s",
			mergeRow.sourceProjectID, forked.Fork.ForkProjectID)
	}
	if mergeRow.sourceBranchID != forked.Branch.ID {
		t.Errorf("network e2e: the merge's source branch is %s, want the fork's %s",
			mergeRow.sourceBranchID, forked.Branch.ID)
	}
	if mergeRow.targetBranchID != mainBranch.ID {
		t.Errorf("network e2e: the merge's target branch is %s, want the upstream main %s",
			mergeRow.targetBranchID, mainBranch.ID)
	}
	if mergeRow.resultStateID != mainAfter {
		t.Errorf("network e2e: the merge record's result state is %s, want the upstream main's new head %s",
			mergeRow.resultStateID, mainAfter)
	}
	if got := w.branchLifecycle(t, forked.Branch.ID); got != string(domain.BranchLifecycleMerged) {
		t.Errorf("network e2e: the contributor's branch is %q after the merge, want %q",
			got, domain.BranchLifecycleMerged)
	}
	// The git half of a CROSS-PROJECT merge is recorded as failed with its
	// reason, never as done (T0817): one provider-side merge names one
	// repository, and this pair spans two. The boundary is asserted here so
	// the loop reports what it does — and does not — do on the provider side,
	// instead of leaving it implied.
	if merged.GitState != string(domain.GitStateFailed) {
		t.Errorf("network e2e: the cross-project merge's git state is %q, want the recorded %q",
			merged.GitState, domain.GitStateFailed)
	}
	if !strings.Contains(merged.GitError, "one repository") {
		t.Errorf("network e2e: the git half's recorded reason does not name the reason: %q", merged.GitError)
	}
	if mergeRow.gitState != string(domain.GitStateFailed) {
		t.Errorf("network e2e: the stored merge's git state is %q, want %q", mergeRow.gitState, domain.GitStateFailed)
	}

	// ---- 8. PROFILE CREDIT: the two projections the platform derives from
	// its own events — the outbox dispatcher, then the ledger projector — and
	// then the public profile read.
	if _, err := events.NewDispatcher(w.pool).RunOnce(ctx); err != nil {
		t.Fatalf("network e2e: dispatch the domain events: %v", err)
	}
	if _, err := appcontribution.NewLedgerProjector(contribution.NewLedgerStore(w.pool)).RunOnce(ctx); err != nil {
		t.Fatalf("network e2e: project the contribution ledger: %v", err)
	}
	bobProfile := w.researchProfile(t, newTestUserClient(w.ts.URL), w.bobID)
	if bobProfile.Person.ID != w.bobID {
		t.Fatalf("network e2e: the profile read answered for %s, want %s", bobProfile.Person.ID, w.bobID)
	}
	credited := creditedInProject(bobProfile.Contributions, forked.Fork.ForkProjectID)
	if credited == nil {
		t.Fatalf("network e2e: the contributor's profile carries no credit for his own fork %s: %+v",
			forked.Fork.ForkProjectID, bobProfile.Contributions)
	}
	if credited.EventType != "scientific_object.version_created" {
		t.Errorf("network e2e: the credited event is %q, want the version write", credited.EventType)
	}
	aliceProfile := w.researchProfile(t, newTestUserClient(w.ts.URL), w.aliceID)
	if creditedInProject(aliceProfile.Contributions, forked.Fork.ForkProjectID) != nil {
		t.Error("network e2e: the owner's profile carries credit for the contributor's fork project")
	}

	// ---- 9. The upstream's OWN proposal, same project, the same governance:
	// Alice's question proposed from a line of P and merged into P's main.
	//
	// It is not decoration. It is the POSITIVE CONTROL for the provider half
	// (with both sides in one repository the saga does attempt the
	// provider-side merge, and this fixture's transport carries it), which is
	// what makes step 7's recorded cross-project `failed` a boundary of the
	// product rather than a transport this suite never learned to drive. And
	// it produces an ACTIVE version of an object the UPSTREAM project owns,
	// which is the only kind of version step 10 can publish — see the note
	// there.
	upstreamBranch := w.newBranch(t, w.aliceID, parent.ID, "upstream-band-gap-question", mainAfter, domain.BranchVisibilityPrivate)
	upstreamObject, _, _ := w.writeQuestion(t, w.alice, parent.ID, upstreamBranch.ID,
		"the upstream's own band-gap question, the result the network cites")
	upstreamPR := w.openPR(t, w.alice, parent.ID, upstreamBranch.ID, mainBranch.ID,
		"the upstream's own question", "network-e2e-open-0002")
	if _, err := w.prSvc.RequestReview(ctx, parent.ID, upstreamPR.Number); err != nil {
		t.Fatalf("network e2e: request review on the upstream's own proposal: %v", err)
	}
	for _, submission := range []struct {
		uc   *testUserClient
		kind string
	}{{w.carol, "scientific"}, {w.bob, "integrity"}} {
		body := fmt.Sprintf(`{"kind":%q,"decision":"approved","body":"reviewed for the T0810 network e2e"}`,
			submission.kind)
		resp := submission.uc.do(t, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/pull-requests/%d/reviews", parent.ID, upstreamPR.Number), body)
		mustStatus(t, resp, http.StatusCreated)
	}
	upstreamMerged := mergeThroughTheEndpoint(t, w.alice, parent.ID, upstreamPR.Number, "network-e2e-merge-key-0002")
	if upstreamMerged.GitState != string(domain.GitStateUpdated) {
		t.Fatalf("network e2e: the same-project merge's git state is %q, want %q — with both sides in ONE "+
			"repository the provider-side merge must run, and this fixture's transport implements it; "+
			"without this the cross-project record in step 7 would prove nothing about the product: %s",
			upstreamMerged.GitState, domain.GitStateUpdated, upstreamMerged.GitError)
	}
	if upstreamMerged.GitSHA == nil || *upstreamMerged.GitSHA == "" {
		t.Fatalf("network e2e: the same-project merge recorded git state %q with no commit", upstreamMerged.GitState)
	}
	// The transport itself moved: the merge's recorded commit is the sha the
	// provider's own main now points at — not a value the platform wrote down
	// and never used.
	if want, got := w.git.head(netGitServiceLogin, parentRepo, domain.MainBranchName), *upstreamMerged.GitSHA; got != want {
		t.Errorf("network e2e: the provider's main is at %s, the merge recorded %s", got, want)
	}
	upstreamHead := w.branchHead(t, mainBranch.ID)
	if upstreamHead == mainAfter {
		t.Fatal("network e2e: the upstream's own merge did not move main")
	}
	publishedVersion := w.versionOfObjectInState(t, upstreamObject, upstreamHead)

	// ---- 10. THE PUBLISHED RESULT AND ITS EXTERNAL EVIDENCE.
	//
	// A note on why the published version is the upstream's own and not the
	// one step 7 landed: a merge lands VERSIONS of the contributor's object
	// upstream — the `scientific_objects` row stays in the fork project, which
	// is the fact the cross-project merge rests on (internal/persistence/
	// queries/knowledge_publish.sql:61-107 resolves a version through
	// `so.project_id = @project_id`, so the landed version is not publishable
	// upstream). The negative control for that is asserted right below; the
	// contribution is what the loop carries, and this publication is what the
	// network reads.
	publishResp := w.alice.doKeyed(t, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/knowledge:publish", parent.ID),
		knowledgePublishBody(t, landed.id, "open-network-band-gap", string(rights.MetadataProjectPolicy)),
		"network-e2e-publish-landed")
	if publishResp.StatusCode == http.StatusCreated {
		t.Fatalf("network e2e: the version the cross-project merge landed was published in the upstream "+
			"project — internal/persistence/queries/knowledge_publish.sql:61-107 requires its OBJECT to "+
			"belong to that project and the merge keeps the object in the contributor's fork: %s",
			readAll(t, publishResp))
	}
	if publishResp.StatusCode != http.StatusNotFound {
		t.Errorf("network e2e: publishing the landed version upstream answered %d, want the 404 of "+
			"KNOWLEDGE_VERSION_NOT_FOUND", publishResp.StatusCode)
	}
	t.Logf("network e2e: publishing the cross-project landed version upstream is refused with %d: %s",
		publishResp.StatusCode, readAll(t, publishResp))

	published := w.know.mustKnowledgePublish(t, w.alice, parent.ID,
		knowledgePublishBody(t, publishedVersion.id, "open-network-band-gap", string(rights.MetadataProjectPolicy)),
		"network-e2e-publish-1")
	if published.PID == "" {
		t.Fatal("network e2e: the publication answered no pid")
	}
	// The contributor — from his OWN project, which is what makes the evidence
	// external — brings an independent reproduction to the published result.
	// His fork branch is closed (step 7 merged it), so he opens a new line in
	// his own project: the fork is a research space, not a one-shot upload.
	evidenceBranch := w.newBranch(t, w.bobID, forked.Fork.ForkProjectID, "band-gap-reproduction", "", domain.BranchVisibilityPublic)
	_, evidenceVersion, _ := w.writeQuestion(t, w.bob, forked.Fork.ForkProjectID, evidenceBranch.ID,
		"the contributor's independent reproduction of the measured band gap")
	asserted := w.assertEvidence(t, w.bob, forked.Fork.ForkProjectID, evidenceBranch.ID,
		evidenceAssertionBody(publishedVersion.id, evidenceVersion, "supports"))
	if asserted.EvidenceOrigin != "external" {
		t.Fatalf("network e2e: the assertion from another project is marked %q, want external",
			asserted.EvidenceOrigin)
	}
	readStatus, readRaw := w.know.readKnowledge(t, newTestUserClient(w.ts.URL), published.PID)
	if readStatus != http.StatusOK {
		t.Fatalf("network e2e: the published knowledge read answered %d: %s", readStatus, readRaw)
	}
	var page evidenceReadWire
	if err := json.Unmarshal([]byte(readRaw), &page); err != nil {
		t.Fatalf("network e2e: decode the published page: %v: %s", err, readRaw)
	}
	if page.PID != published.PID {
		t.Errorf("network e2e: the page answers pid %q, want %s", page.PID, published.PID)
	}
	found := false
	for _, row := range page.Evidence.UnreviewedExternal {
		if row.ID == asserted.ID {
			found = true
			if row.AssertingProjectID != forked.Fork.ForkProjectID {
				t.Errorf("network e2e: the external assertion is attributed to project %s, want the contributor's %s",
					row.AssertingProjectID, forked.Fork.ForkProjectID)
			}
			if row.TargetObjectVersionID != publishedVersion.id {
				t.Errorf("network e2e: the external assertion targets version %s, want the published %s",
					row.TargetObjectVersionID, publishedVersion.id)
			}
			if row.Relation != "supports" {
				t.Errorf("network e2e: the external assertion's relation is %q, want supports", row.Relation)
			}
		}
	}
	if !found {
		t.Fatalf("network e2e: the external evidence is not on the published page's unreviewed_external bucket: %+v",
			page.Evidence)
	}

	// The fork's own line: the contributor's work stayed his (his fork project
	// is public, which is what let the credit render) and the upstream's main
	// advanced by the contribution — the two facts the loop exists to make
	// true at once.
	if forkHead == mainAfter {
		t.Fatal("network e2e: the contributor's state and the upstream's accepted state are the same row")
	}
}

// creditedInProject answers the first profile row whose project is projectID.
func creditedInProject(rows []contributionRow, projectID string) *contributionRow {
	for i := range rows {
		if rows[i].Project != nil && rows[i].Project.ID == projectID {
			return &rows[i]
		}
	}
	return nil
}
