// Package integration — T0817 "external fork merge e2e" (blocking): the
// MERGE half of the external contribution path. T0814's AC8 was split: that
// task kept "the proposal opens on the real HTTP path", this one owns "the
// proposal's MERGE reads the source side in the project the source branch
// itself lives in" (docs/04 §2: a non-member forks a public project into
// their own space and proposes from there).
//
// What the task is about, in one sentence: until T0817 the merge service
// pinned BOTH sides of a proposal to the PR's project — `s.branch(ctx,
// in.ProjectID, pr.SourceBranchID)` and the diff triple's ProjectID — so an
// external proposal could be opened (T0804/T0814 made the row representable
// and the gate honest) but never merged: the merge would look for the
// contributor's branch inside the upstream project and answer "not found".
// The fix is that each side carries its OWN project: the SOURCE branch is
// read by its own id (the row says which project it belongs to) while the
// target branch, the PR, the policy, the commit and the accepted state stay
// the PR's project's.
//
// Nothing here fabricates the fork: the parent project, its repository, its
// main branch, the fork project, the fork's repository, its branch, the
// imported content, the contributor's claimed objects, the governance routing
// rule, the two reviews that walk the proposal to merge_ready and the merge
// itself are all produced by the product's own code paths (the production
// route assemblies in cmd/api's own order, behind the production auth
// guard). Every assertion reads the canonical rows back.
//
// One row is written by the fixture rather than by a route, and it is named
// where it is written (abortVersion): the withdrawn version's abort record.
// The product's abort command proposes a branch of its own, so a fixture that
// needs the abort to be CONTENT of this proposal has to append that row
// itself — in the shape migration 00100 defines and the abort command writes.
//
// # The two tests
//
//   - TestExternalForkMergeEndToEnd — the closed loop (criteria 1, 2, 4):
//     A (public) is forked by B from B's own space, B writes scientific
//     state on the fork branch, a PR to A is opened and approved by review,
//     and the merge lands a NEW state on A's main carrying B's proposed
//     object versions AND the version-pinned relation between them — item by
//     item (payloads byte for byte, the edge's pins re-pointed at the
//     versions the merge wrote), not "a state id changed". The merge
//     record proves the source side was read in the FORK's project
//     (semantic_merges.project_id is A's while its source_state_id and
//     source_branch_id are the fork's), the fork branch is closed as merged,
//     and the content moved upstream keeps the source version's visibility
//     policy verbatim (docs/12 §3: a cross-project merge is not a reason to
//     widen rights) and carries the contributor's own abort record — reason
//     code, explanation and the DECIDING actor — on the version the merge
//     wrote in its own name.
//
//   - TestExternalForkMergeRefusesANonForkSource — the negative evidence
//     (criterion 3), and it names WHICH layer refuses. A cross-project PR
//     whose source branch is not the opener's own fork is refused by the
//     STORE first (the existence-hiding not-found), by migration 00086's
//     pull_request_fork_gate at the INSERT behind it (SQLSTATE and message
//     asserted, reached by a raw INSERT), and the MERGE holds the rule a
//     third time — a layer that is unreachable through the product and is
//     reached here only by staging the post-gate row shape (the trigger
//     suspended for one INSERT, restored immediately), where the merge
//     refuses to read the foreign branch and writes nothing.
//
// # What had to change for the loop to close
//
// The merge read both sides in the PR's project; three further reads on the
// same path had the same assumption, and all four now ask which side the
// value belongs to:
//
//   - the merge service (internal/application/merge): the source branch is
//     read by its own id and its project carried through the diff triple and
//     the lock scope;
//   - the fork import (internal/gitprovider + internal/application/forks):
//     the copied commit's state was recorded ON the fork branch without
//     moving that branch's head pointer and without a state_commits row, so
//     the branch carried a state that was neither its head nor named by any
//     commit — integrity's provenance checks read that as "2 heads" and as
//     an unnamed state. The import is now a recorded transition like any
//     other: the pushed-head state chains to the branch's base state, the
//     head moves by a guarded compare-and-swap, and one state_commits row
//     names the imported state (via 'git_compat', actor = the forker). Both
//     of those rules ride the copy's own delivery — the import arm — and the
//     generic push path keeps its documented semantics: a commit that is
//     already a state of the project is REUSED when another branch pushes it
//     (pinned by TestPushIngestionReusesAStateOnASecondBranch), and the
//     transaction is otherwise the same code;
//   - the integrity pre-flight (internal/application/prchecks): a chain's
//     boundary is judged in the project of the chain's OWN branch, so the
//     fork project's root state is a legitimate exit of the fork's source
//     chain while the target side still resolves in the PR's project.
//
// None of the gates moved: 00086's pull_request_fork_gate, the PR state
// machine, the frozen-main rule and the semantic gate are the same code, and
// both tests below read their verdicts out of the same layers they always
// did.
//
// # The in-memory adapters
//
// The session store and the rate limiter (memstore) are in-memory, exactly as
// the external fork and merge governance e2e suites compose them: Redis
// session semantics are orthogonal and covered by the auth e2e suite. Every
// store behind the guard, the provider adapter, and the provisioner are the
// production ones.
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/mergegit"
	"github.com/lichman0405/post/cmd/api/mergehttp"
	"github.com/lichman0405/post/cmd/api/policyhttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/cmd/api/pullrequestshttp"
	"github.com/lichman0405/post/cmd/api/reviewhttp"
	"github.com/lichman0405/post/cmd/api/rsghttp"

	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/diffs"
	"github.com/lichman0405/post/internal/application/forks"
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
	"github.com/lichman0405/post/internal/config"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/gitprovider"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rsg/integrity"
	"github.com/lichman0405/post/internal/rsg/schemareg"
)

const externalForkMergeTaskID = "T0817"

// forkMergePlatform is the graph cmd/api builds — the same constructors, over
// the same real pool, behind the same guard — plus the provider helpers the
// T0409 / T0804 fixtures carry. It is shared by the two tests below because
// both exercise the same command over the same surface; only the fixture
// rows they build differ.
type forkMergePlatform struct {
	ctx   context.Context
	base  string
	token string
	owner string
	pool  *pgxpool.Pool
	cfg   gitprovider.Config
	reg   *schemareg.Registry

	adapter *gitprovider.GiteaAdapter

	orgStore     *persistence.OrgStore
	projectStore *persistence.ProjectStore
	policyStore  *persistence.PolicyStore
	stateStore   *persistence.StateStore
	branchStore  *persistence.BranchStore

	projectSvc  *projects.Service
	stateSvc    *states.Service
	branchSvc   *branches.Service
	rsgSvc      *rsg.Service
	prSvc       *pullrequests.Service
	forkSvc     *forks.Service
	provisioner *gitprovider.Provisioner
	diffSvc     *diffs.Service
	mergeSvc    *merge.Service
	routingSvc  *responsibilities.Service
	reviewsSvc  *reviews.Service

	ts *httptest.Server

	alice, bob, carol, maintainer, viewer forkAccount
}

func newForkMergePlatform(t *testing.T) *forkMergePlatform {
	t.Helper()
	ctx := testCtx(t)
	base := requireGitea(t)
	token := giteaServiceToken(t, base)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), externalForkMergeTaskID)

	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("external fork merge e2e: schema registry: %v", err)
	}
	cfg := gitprovider.Config{
		BaseURL:    base,
		Token:      config.Secret(token),
		WebhookURL: "http://host.invalid/api/v1/git/hooks/gitea",
	}
	adapter := gitprovider.NewGiteaAdapter(cfg)
	owner, err := adapter.Owner(ctx)
	if err != nil {
		t.Fatalf("external fork merge e2e: resolve the service identity: %v", err)
	}

	// ---- The platform side: the graph cmd/api assembles, in its order.
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
	stateSvc := states.NewService(stateStore, newCommitGuard(t))
	branchSvc := branches.NewService(branchStore)
	forkStore := persistence.NewForkStore(pool)
	prStore := persistence.NewPullRequestStore(pool)
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
		// write_scientific_state's own_fork_only cell is resolved through
		// this gate, never assumed by the service.
		ForkGate: forkStore,
	})
	prSvc := pullrequests.NewService(prStore)
	provisioner := gitprovider.NewProvisioner(adapter, gitprovider.NewProvisionStore(pool), cfg.WebhookURL)
	forkSvc := forks.NewService(forks.Deps{
		Projects:     projectSvc,
		Branches:     branchSvc,
		BranchWriter: rsgSvc,
		Forks:        forkStore,
		Repos:        provisioner,
		Imports: gitprovider.NewForkImporter(adapter, gitprovider.NewForkImportStore(pool),
			gitprovider.NewPushIngester(adapter, gitprovider.NewPushIngestStore(pool), reg)),
		PullRequests: prSvc,
		Authz:        authz.NewMatrixEngine(),
	})
	// The diff the merge and the review routing both read. Its third port is
	// the T0817 rule: a state of another project is admitted exactly when a
	// pull request of the diff's project proposes it (the external fork's
	// source state), so prdiff — which the merge, the responsibilities
	// routing and the review machine all go through inside allowed_scope-free
	// packages — needs no change to serve a cross-project proposal.
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
		// The abort-record reader (T0602), wired exactly as cmd/api wires it:
		// without it the merge refuses to materialize an aborted source
		// version at all (its fail-closed rule), so the fixture that stages a
		// withdrawn version has to be wired like production to merge it.
		Aborts: persistence.NewScientificObjectStore(pool),
		// The fork lineage the merge's own source-side judgment asks (T0817).
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
		Git:      mergegit.New(adapter, gitprovider.NewUserAccessStore(pool)),
		RefGuard: gitprovider.RefGuard{MergeService: owner},
	})

	// ---- The HTTP surface: the production guard over a mux the production
	// route assemblies populated, in cmd/api/main.go's own order.
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
	rsghttp.New(rsghttp.Deps{Service: rsgSvc}).Register(apiMux)
	pullrequestshttp.New(pullrequestshttp.Deps{
		PullRequests: prSvc,
		Create:       forkSvc,
		Projects:     projectSvc,
	}).Register(apiMux)
	reviewhttp.New(reviewhttp.Deps{Service: reviewsSvc, Projects: projectSvc}).Register(apiMux)
	mergehttp.New(mergehttp.Deps{Command: mergeSvc, Projects: projectSvc}).Register(apiMux)
	ts := httptest.NewServer(authAPI.Guard(apiMux))
	t.Cleanup(ts.Close)

	p := &forkMergePlatform{
		ctx: ctx, base: base, token: token, owner: owner, pool: pool, cfg: cfg, reg: reg,
		adapter:  adapter,
		orgStore: orgStore, projectStore: projectStore, policyStore: policyStore,
		stateStore: stateStore, branchStore: branchStore,
		projectSvc: projectSvc, stateSvc: stateSvc, branchSvc: branchSvc, rsgSvc: rsgSvc,
		prSvc: prSvc, forkSvc: forkSvc, provisioner: provisioner, diffSvc: diffSvc,
		mergeSvc: mergeSvc, routingSvc: routingSvc, reviewsSvc: reviewsSvc,
		ts: ts,
	}
	p.alice = p.signupAccount(t, "forkmerge-alice@example.com", "forkmerge-alice")
	p.bob = p.signupAccount(t, "forkmerge-bob@example.com", "forkmerge-bob")
	p.carol = p.signupAccount(t, "forkmerge-carol@example.com", "forkmerge-carol")
	p.maintainer = p.signupAccount(t, "forkmerge-maintainer@example.com", "forkmerge-maintainer")
	p.viewer = p.signupAccount(t, "forkmerge-viewer@example.com", "forkmerge-viewer")
	return p
}

// ---- Accounts, projects, repositories ----------------------------------

func (p *forkMergePlatform) signupAccount(t *testing.T, email, handle string) forkAccount {
	t.Helper()
	client, id := signup(t, p.ts.URL, email, handle)
	return forkAccount{client: client, id: id}
}

// createProject drives the product's own create-project route and returns the
// row the rest of the platform sees.
func (p *forkMergePlatform) createProject(t *testing.T, account forkAccount, slug string, visibility domain.ProjectVisibility) domain.Project {
	t.Helper()
	body := fmt.Sprintf(`{"slug":%q,"name":%q,"purpose":%q,"visibility":%q}`,
		slug, slug, "T0817 external fork merge e2e", string(visibility))
	resp := account.client.do(t, http.MethodPost, "/api/v1/projects", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("external fork merge e2e: create project %s = %d: %s", slug, resp.StatusCode, readAll(t, resp))
	}
	var payload struct {
		Project struct {
			ID         string `json:"id"`
			Visibility string `json:"visibility"`
		} `json:"project"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("external fork merge e2e: decode project %s: %v", slug, err)
	}
	if payload.Project.Visibility != string(visibility) {
		t.Fatalf("external fork merge e2e: project %s visibility = %q, want %q",
			slug, payload.Project.Visibility, visibility)
	}
	project, err := p.projectSvc.Get(p.ctx, projects.Reader{UserID: account.id, Authenticated: true}, payload.Project.ID)
	if err != nil {
		t.Fatalf("external fork merge e2e: read back project %s: %v", slug, err)
	}
	return project
}

// provisionProject runs the production provisioner (the work the provisioning
// job does) and returns the repository coordinates read back from the
// canonical provision row.
func (p *forkMergePlatform) provisionProject(t *testing.T, projectID string) forkRepo {
	t.Helper()
	if err := p.provisioner.Provision(p.ctx, projectID); err != nil {
		t.Fatalf("external fork merge e2e: provision %s: %v", projectID, err)
	}
	repo := forkRepo{owner: p.owner}
	if err := p.pool.QueryRow(p.ctx,
		`SELECT name, gitea_repo_id, webhook_secret FROM git_repository_provisions WHERE project_id = $1`,
		projectID).Scan(&repo.name, &repo.id, &repo.secret); err != nil {
		t.Fatalf("external fork merge e2e: read the provision row of %s: %v", projectID, err)
	}
	t.Cleanup(func() { deleteGiteaRepo(t, p.base, p.token, repo.owner, repo.name) })
	return repo
}

// createBranch creates a branch through the canonical RSG service and syncs
// its provider ref (the work the refsync job does), exactly as
// cmd/api/main.go's graph does.
func (p *forkMergePlatform) createBranch(t *testing.T, actor forkAccount, projectID, name, baseRef string) domain.Branch {
	t.Helper()
	branch, err := p.rsgSvc.CreateBranch(p.ctx, actor.actor(), projectID, rsg.CreateBranchInput{
		Name:       name,
		BaseRef:    baseRef,
		Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("external fork merge e2e: create branch %s: %v", name, err)
	}
	syncer := gitprovider.NewBranchRefSyncer(p.adapter, gitprovider.NewBranchRefStore(p.pool))
	if err := syncer.Sync(p.ctx, branch.ID); err != nil {
		t.Fatalf("external fork merge e2e: sync the %s ref: %v", name, err)
	}
	return branch
}

// writeClaim writes one main-gate-complete claim through the production RSG
// route (the real guard, the real service, the real policy engine) and
// returns the object id and the version row the write produced.
func (p *forkMergePlatform) writeClaim(t *testing.T, account forkAccount, projectID, branchID, statement string) (objectID, versionID string) {
	t.Helper()
	body := fmt.Sprintf(`{"object_type":"claim","payload":%s}`, mergeMainGateClaim(statement))
	resp := account.client.do(t, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/branches/%s/objects", projectID, branchID), body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("external fork merge e2e: write a claim on branch %s = %d: %s",
			branchID, resp.StatusCode, readAll(t, resp))
	}
	var written struct {
		ID        string `json:"id"`
		VersionID string `json:"version_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&written); err != nil || written.ID == "" {
		t.Fatalf("external fork merge e2e: the accepted write did not answer an object: %v", err)
	}
	return written.ID, written.VersionID
}

// writeRelation draws one typed, version-pinned edge between two object
// versions through the production RSG route (the real guard, the real
// service, the real relation catalog) and returns the relation row's id and
// the version the write produced.
func (p *forkMergePlatform) writeRelation(t *testing.T, account forkAccount, projectID, branchID, relationType, sourceVersionID, targetVersionID, payload string) (relationID, versionID string) {
	t.Helper()
	body := fmt.Sprintf(
		`{"relation_type":%q,"source_object_version_id":%q,"target_object_version_id":%q,"payload":%s}`,
		relationType, sourceVersionID, targetVersionID, payload)
	resp := account.client.do(t, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/branches/%s/relations", projectID, branchID), body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("external fork merge e2e: write a %s relation on branch %s = %d: %s",
			relationType, branchID, resp.StatusCode, readAll(t, resp))
	}
	var written struct {
		ID        string `json:"id"`
		VersionID string `json:"version_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&written); err != nil || written.ID == "" {
		t.Fatalf("external fork merge e2e: the accepted relation write did not answer a relation: %v", err)
	}
	return written.ID, written.VersionID
}

// abortVersion appends the version row an abort records: a NEW version of
// the same object in the same state (docs/43's lifecycle "active → aborted",
// the append-only rule of migration 00014), carrying docs/46:7's reason code,
// human explanation, deciding actor and deciding time in its own columns
// (migration 00100). The object's counter is advanced with it so the row is
// the version the object resolves to in that state (the diff engine resolves
// an object to its highest version_no in a snapshot).
//
// In production this row is written by the abort command, which appends it to
// a proposal branch of its OWN and opens its own pull request
// (internal/application/aborts, step 6). The fixture writes the same row
// shape directly because what this suite needs is the abort as CONTENT of the
// external proposal: the product's route would propose a second branch
// instead of putting the abort on the fork branch the external PR is opened
// from. Every other row in this fixture comes from a product path.
func (p *forkMergePlatform) abortVersion(t *testing.T, objectID, versionID, actorID, reason, explanation string) string {
	t.Helper()
	var abortedID string
	if err := p.pool.QueryRow(p.ctx, `
		INSERT INTO scientific_object_versions
			(object_id, version_no, state_id, branch_id, schema_id, schema_version, title,
			 lifecycle_state, payload, visibility_policy_id, integrity_hash, created_by,
			 abort_reason_code, abort_explanation, aborted_by, aborted_at)
		SELECT object_id, version_no + 1, state_id, branch_id, schema_id, schema_version, title,
		       'aborted', payload, visibility_policy_id, integrity_hash, created_by,
		       $2, $3, $4, now()
		FROM scientific_object_versions WHERE id = $1
		RETURNING id::text`, versionID, reason, explanation, actorID).Scan(&abortedID); err != nil {
		t.Fatalf("external fork merge e2e: abort version %s: %v", versionID, err)
	}
	tag, err := p.pool.Exec(p.ctx,
		`UPDATE scientific_objects SET current_version_no = current_version_no + 1
		  WHERE id = $1 AND current_version_no = (
		    SELECT version_no - 1 FROM scientific_object_versions WHERE id = $2)`, objectID, abortedID)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("external fork merge e2e: advance the aborted object's counter = %d rows (%v), want 1",
			tag.RowsAffected(), err)
	}
	return abortedID
}

// ---- Canonical-row probes ----------------------------------------------

// branchHead reads a branch's head state id (the pointer the platform's own
// GetBranchHead reads).
func (p *forkMergePlatform) branchHead(t *testing.T, branchID string) string {
	t.Helper()
	var id string
	if err := p.pool.QueryRow(p.ctx,
		`SELECT base_state_id::text FROM branches WHERE id = $1`, branchID).Scan(&id); err != nil {
		t.Fatalf("external fork merge e2e: read the head of branch %s: %v", branchID, err)
	}
	return id
}

// versionOfObjectInState reads the version an object carries in one state —
// the row a reader of that state resolves the object to (the diff engine
// resolves an object to its highest version_no in a snapshot). It reads the
// whole row (abort_e2e_test.go's versionRow, every canonical column) so the
// "the content travelled" assertions below are about the row, not about the
// columns this task happens to name.
func (p *forkMergePlatform) versionOfObjectInState(t *testing.T, objectID, stateID string) versionRow {
	t.Helper()
	var id string
	if err := p.pool.QueryRow(p.ctx, `
		SELECT id::text FROM scientific_object_versions
		 WHERE object_id = $1 AND state_id = $2
		 ORDER BY version_no DESC, id LIMIT 1`, objectID, stateID).Scan(&id); err != nil {
		t.Fatalf("external fork merge e2e: read object %s's version in state %s: %v", objectID, stateID, err)
	}
	return readVersion(t, p.ctx, p.pool, id)
}

// relationVersionInState reads the version a relation carries in one state —
// the row a reader of that state resolves the edge to (the highest
// version_no there, through the package's shared relationVersionRow, every
// canonical column of it), so the "the edge travelled" assertions below are
// about the row and not about the columns this task happens to name.
func (p *forkMergePlatform) relationVersionInState(t *testing.T, relationID, stateID string) relationVersionRow {
	t.Helper()
	var r relationVersionRow
	if err := p.pool.QueryRow(p.ctx, `
		SELECT id::text, version_no, state_id::text, relation_type,
		       source_object_version_id::text, target_object_version_id::text, payload, created_by::text
		  FROM relation_versions
		 WHERE relation_id = $1 AND state_id = $2
		 ORDER BY version_no DESC, id LIMIT 1`, relationID, stateID).
		Scan(&r.ID, &r.VersionNo, &r.StateID, &r.RelationType,
			&r.SourceObjectVersionID, &r.TargetObjectVersionID, &r.Payload, &r.CreatedBy); err != nil {
		t.Fatalf("external fork merge e2e: read relation %s's version in state %s: %v", relationID, stateID, err)
	}
	return r
}

// pinVisibilityPolicy gives one version a visibility axis of its own
// (scientific_object_versions.visibility_policy_id).
//
// NOTHING in V1 writes that column: it is in no RSG CreateObject input and no
// service path (the same gap tests/integration/knowledge_publish_test.go
// records), so a fixture that needs a version with rights attached has to
// write it. It INSERTs rather than updates because the version table is
// append-only (migration 00014 — an UPDATE is rejected by the database
// itself), so a pinned version is a NEW row copying the routed one. The copy
// carries version_no + 1 in the SAME state, which is what makes it the object's
// representative version there (the diff engine resolves an object to its
// highest version_no in a snapshot), and the object's own counter is bumped
// with it so the platform's head agrees with the row it now points at. That
// agreement is not decoration: the merge reads the counter for its
// materialize compare-and-swap, so leaving it behind would make the pinned
// fixture unmergeable rather than merely inconsistent.
//
// policyID is an EXISTING policy version of the proposal's own project (the
// fixture seeds the project policy over the product's real store and pins the
// version that wrote): the integrity review resolves every pin against the
// project's and its organization's version history (docs/12 §5), so a pin
// naming a policy that does not exist would make the fixture — not the
// subject under test — the reason a merge is refused.
func (p *forkMergePlatform) pinVisibilityPolicy(t *testing.T, objectID, versionID, policyID string) (versionIDOut string) {
	t.Helper()
	if err := p.pool.QueryRow(p.ctx, `
		INSERT INTO scientific_object_versions
			(object_id, version_no, state_id, branch_id, schema_id, schema_version, title,
			 lifecycle_state, payload, visibility_policy_id, integrity_hash, created_by)
		SELECT object_id, version_no + 1, state_id, branch_id, schema_id, schema_version, title,
		       lifecycle_state, payload, $2, integrity_hash, created_by
		FROM scientific_object_versions WHERE id = $1
		RETURNING id::text`, versionID, policyID).Scan(&versionIDOut); err != nil {
		t.Fatalf("external fork merge e2e: pin a visibility policy on version %s: %v", versionID, err)
	}
	tag, err := p.pool.Exec(p.ctx,
		`UPDATE scientific_objects SET current_version_no = current_version_no + 1
		  WHERE id = $1 AND current_version_no = (
		    SELECT version_no - 1 FROM scientific_object_versions WHERE id = $2)`, objectID, versionIDOut)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("external fork merge e2e: advance the pinned object's counter = %d rows (%v), want 1",
			tag.RowsAffected(), err)
	}
	return versionIDOut
}

// ---- The closed loop ----------------------------------------------------

// TestExternalForkMergeEndToEnd is T0817's required e2e: label
// `external fork merge e2e`. It runs the whole external contribution loop on
// real PostgreSQL and a real Gitea and asserts the merged content item by
// item.
func TestExternalForkMergeEndToEnd(t *testing.T) {
	p := newForkMergePlatform(t)
	ctx := p.ctx

	// ---- The upstream project: public, owned by Alice, provisioned for real
	// (repository, bootstrap main, webhook and T0302's protection rule all
	// applied by the production provisioner).
	parent := p.createProject(t, p.alice, "mof-gas-open", domain.VisibilityPublic)
	p.provisionProject(t, parent.ID)
	main := p.createBranch(t, p.alice, parent.ID, "main", "")
	mainBefore := p.branchHead(t, main.ID)

	// ---- The external contribution: a non-member forks the public project
	// into their own space (docs/04 §2). The fork project, its repository,
	// its branch, the copied content and the lineage row are all the product's
	// work.
	bobFork, err := p.forkSvc.Fork(ctx, p.bob.actor(), forks.ForkRequest{
		ProjectID: parent.ID, Visibility: domain.VisibilityPublic,
	})
	if err != nil {
		t.Fatalf("fork the public project as a non-member: %v", err)
	}
	if bobFork.Fork.ForkProjectID == parent.ID || bobFork.Fork.ParentProjectID != parent.ID {
		t.Fatalf("the fork lineage = %+v, want a fork project of %s", bobFork.Fork, parent.ID)
	}
	if bobFork.Fork.ForkedBy != p.bob.id {
		t.Fatalf("the fork lineage names %s as the forker, want %s", bobFork.Fork.ForkedBy, p.bob.id)
	}
	forRepo := forkRepo{owner: p.owner}
	if err := p.pool.QueryRow(ctx,
		`SELECT name, gitea_repo_id, webhook_secret FROM git_repository_provisions WHERE project_id = $1`,
		bobFork.Project.ID).Scan(&forRepo.name, &forRepo.id, &forRepo.secret); err != nil {
		t.Fatalf("read the fork's provision row: %v", err)
	}
	t.Cleanup(func() { deleteGiteaRepo(t, p.base, p.token, forRepo.owner, forRepo.name) })

	// ---- The contributor writes scientific state on the FORK branch, over
	// the production RSG route (own_fork_only lets an external contributor
	// write in their own fork and nowhere else).
	const statement = "the external contributor's measured band gap of 0.9 eV"
	bobObject, bobVersion := p.writeClaim(t, p.bob, bobFork.Project.ID, bobFork.Branch.ID, statement)

	// The rights axis the merge must not widen (docs/12 §3): the contributor's
	// version pins a visibility policy of its own, and the version that lands
	// on the upstream main has to carry that same policy — not the upstream
	// project's default, and not nothing. The upstream project's policy is
	// seeded first (over the product's own store) so the pin names a version
	// that really exists and the integrity review has nothing to refuse it on.
	seedProjectPolicy(t, ctx, p.policyStore, parent, p.alice.id)
	versions, err := p.policyStore.List(ctx, domain.PolicyScope{ProjectID: parent.ID})
	if err != nil || len(versions) == 0 {
		t.Fatalf("the upstream project's policy versions = %d (%v), want the seeded one", len(versions), err)
	}
	bobPolicy := versions[len(versions)-1].ID
	bobVersion = p.pinVisibilityPolicy(t, bobObject, bobVersion, bobPolicy)

	// A second object the contributor withdraws before proposing. An abort is
	// governance attached to a version (docs/46:7), and a cross-project merge
	// is no more a reason to drop it than the rights pin above: the accepted
	// state must carry the record, or main would show an aborted version
	// nobody can account for.
	const abortedStatement = "the external contributor's first band gap estimate, withdrawn by its author"
	abortedObject, abortedWritten := p.writeClaim(t, p.bob, bobFork.Project.ID, bobFork.Branch.ID, abortedStatement)
	const (
		abortReason      = "measurement_superseded"
		abortExplanation = "the reading was re-measured on a second instrument; the superseded estimate must not be presented as accepted state"
	)
	abortedVersion := p.abortVersion(t, abortedObject, abortedWritten, p.bob.id, abortReason, abortExplanation)

	// A second ACTIVE claim and the edge the contributor draws from it to the
	// first. A relation is a version-pinned row of its own (docs/09 §4), not a
	// field of the objects it connects, so the merge has to carry the EDGE as
	// well — and its two pins are ids of the FORK's version rows, which the
	// accepted state must not keep: a reader of main resolving an edge into
	// another project's version rows would be following the proposal's private
	// state, exactly the leak the re-pointing prevents (the plan rewrites an
	// endpoint whose object this merge writes a version of).
	const supportingStatement = "the external contributor's independent reproduction of the band gap"
	supportingObject, supportingVersion := p.writeClaim(t, p.bob, bobFork.Project.ID, bobFork.Branch.ID, supportingStatement)
	const (
		relationType = "supports"
		relationBody = `{"note":"the reproduction supports the measured band gap","reproduced":true}`
	)
	bobRelation, bobRelationVersion := p.writeRelation(t, p.bob, bobFork.Project.ID, bobFork.Branch.ID,
		relationType, supportingVersion, bobVersion, relationBody)

	// ---- The proposal: an external PR from the fork into the upstream
	// project, opened by the contributor.
	pr, err := p.forkSvc.OpenExternalPR(ctx, p.bob.actor(), forks.OpenPRRequest{
		ProjectID:      parent.ID,
		SourceBranchID: bobFork.Branch.ID,
		TargetBranchID: main.ID,
		Title:          "external measurement from a fork",
		Body:           "Proposed from the contributor's own fork (docs/04 §2).",
	})
	if err != nil {
		t.Fatalf("open a pull request from the fork: %v", err)
	}
	if pr.ProjectID != parent.ID || pr.CreatedBy != p.bob.id {
		t.Fatalf("the proposal = %+v, want a %s pull request opened by %s", pr, parent.ID, p.bob.id)
	}
	if pr.SourceBranchID != bobFork.Branch.ID {
		t.Fatalf("the proposal's source branch = %s, want the fork's %s", pr.SourceBranchID, bobFork.Branch.ID)
	}
	// The source state the proposal pins is the fork branch's head — a state
	// of ANOTHER project. That is the fact the merge has to read correctly.
	forkHead := p.branchHead(t, bobFork.Branch.ID)
	if pr.ProposedStateID != forkHead {
		t.Fatalf("the proposal pins source state %s, want the fork branch's head %s", pr.ProposedStateID, forkHead)
	}
	var forkHeadProject string
	if err := p.pool.QueryRow(ctx,
		`SELECT project_id::text FROM project_states WHERE id = $1`, forkHead).Scan(&forkHeadProject); err != nil {
		t.Fatalf("read the proposed state's project: %v", err)
	}
	if forkHeadProject != bobFork.Project.ID {
		t.Fatalf("the proposed source state belongs to project %s, want the fork's %s", forkHeadProject, bobFork.Project.ID)
	}

	// ---- The governance the upstream project requires of any proposal: the
	// claim change routes to a named responsibility, and the two required
	// review dimensions are submitted over the review route. Both reviewers
	// are members of the UPSTREAM project — an external contributor's fork is
	// not a review venue.
	for _, m := range []struct {
		account forkAccount
		role    domain.ProjectRole
	}{{p.maintainer, domain.ProjectRoleMaintainer}, {p.viewer, domain.ProjectRoleViewer}} {
		if _, err := p.pool.Exec(ctx,
			`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, $3)`,
			parent.ID, m.account.id, m.role); err != nil {
			t.Fatalf("seed the %s membership: %v", m.role, err)
		}
		if _, err := p.routingSvc.Assign(ctx, p.alice.actor(), parent.ID, m.account.id, "Data Reviewer"); err != nil {
			t.Fatalf("assign the responsibility label: %v", err)
		}
	}
	if _, err := p.routingSvc.AddRule(ctx, p.alice.actor(), parent.ID, responsibilities.AddRuleInput{
		MatchKind:      domain.ResearchOwnerMatchObjectType,
		MatchValue:     "claim",
		Responsibility: "Data Reviewer",
	}); err != nil {
		t.Fatalf("route claim changes to a reviewer: %v", err)
	}
	if _, err := p.prSvc.RequestReview(ctx, parent.ID, pr.Number); err != nil {
		t.Fatalf("request review: %v", err)
	}
	for _, submission := range []struct {
		account forkAccount
		kind    string
	}{
		{p.viewer, "scientific"},
		{p.maintainer, "integrity"},
	} {
		body := fmt.Sprintf(`{"kind":%q,"decision":"approved","body":"reviewed for the external fork merge e2e"}`,
			submission.kind)
		resp := submission.account.client.do(t, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/pull-requests/%d/reviews", parent.ID, pr.Number), body)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("submit the %s review = %d: %s", submission.kind, resp.StatusCode, readAll(t, resp))
		}
	}
	reviewed, err := p.prSvc.Get(ctx, parent.ID, pr.Number)
	if err != nil {
		t.Fatalf("read the pull request after the reviews: %v", err)
	}
	if reviewed.State != domain.PullRequestStateMergeReady {
		t.Fatalf("the reviewed proposal is %q, want %q — the cross-project diff did not reach the review machine",
			reviewed.State, domain.PullRequestStateMergeReady)
	}

	// ---- The merge: one HTTP request against the production route, as the
	// upstream project's owner, carrying the contract's required
	// Idempotency-Key.
	merged := mergeThroughTheEndpoint(t, p.alice.client, parent.ID, pr.Number, "fork-merge-e2e-key-0001")
	if merged.Replayed {
		t.Fatal("the first merge reports itself as a replay")
	}
	if merged.Number != pr.Number {
		t.Errorf("the merge answered for pull request %d, the request named %d", merged.Number, pr.Number)
	}

	// ---- (1) The accepted state: A's main head is a NEW state, and it is the
	// state this merge committed. Both readings are of the platform's own
	// truth (the branch's head pointer and the merge's answer), never of the
	// response alone.
	mainAfter := p.branchHead(t, main.ID)
	if mainAfter == mainBefore {
		t.Fatalf("main's head state did not move (still %s): the merge accepted nothing", mainAfter)
	}
	if merged.StateID != mainAfter {
		t.Fatalf("main's head state = %s, the merge answered %s — main advanced by something other than this merge",
			mainAfter, merged.StateID)
	}
	var mainAfterBranch string
	var mainAfterParent *string
	if err := p.pool.QueryRow(ctx,
		`SELECT branch_id::text, parent_state_id::text FROM project_states WHERE id = $1`, mainAfter).
		Scan(&mainAfterBranch, &mainAfterParent); err != nil {
		t.Fatalf("read the accepted state: %v", err)
	}
	if mainAfterBranch != main.ID {
		t.Errorf("the accepted state belongs to branch %s, want main %s", mainAfterBranch, main.ID)
	}
	if mainAfterParent == nil || *mainAfterParent != mainBefore {
		t.Errorf("the accepted state's parent = %v, want the previous head %s (main must advance, not be replaced)",
			mainAfterParent, mainBefore)
	}
	if merged.TargetBranchID != main.ID {
		t.Errorf("the merge answered with target branch %q, want main %s", merged.TargetBranchID, main.ID)
	}

	// (1) The content, item by item: the accepted state carries a version of
	// the contributor's object as an ACTIVE version of main, with the
	// contributor's payload — not "a state id changed".
	landed := p.versionOfObjectInState(t, bobObject, mainAfter)
	if landed.objectID != bobObject {
		t.Fatalf("the accepted state's version names object %s, want the contributor's %s", landed.objectID, bobObject)
	}
	if landed.id == bobVersion {
		t.Fatal("the accepted state names the fork's version row itself — the merge did not write a version of its own")
	}
	if landed.lifecycleState != string(domain.LifecycleActive) {
		t.Errorf("the landed version's lifecycle = %q, want active", landed.lifecycleState)
	}
	if landed.branchID == nil || *landed.branchID != main.ID {
		t.Errorf("the landed version belongs to branch %v, want main %s", landed.branchID, main.ID)
	}
	var payload struct {
		Statement string `json:"statement"`
		ClaimType string `json:"claim_type"`
	}
	if err := json.Unmarshal(landed.payload, &payload); err != nil {
		t.Fatalf("the landed version's payload is not JSON: %v (%s)", err, landed.payload)
	}
	source := readVersion(t, p.ctx, p.pool, bobVersion)
	if payload.Statement != statement {
		t.Errorf("the landed version's statement = %q, want the contributor's %q", payload.Statement, statement)
	}
	if string(landed.payload) != string(source.payload) {
		t.Errorf("the landed payload is not the source version's, byte for byte:\n  landed: %s\n  source: %s",
			landed.payload, source.payload)
	}
	if landed.title != source.title {
		t.Errorf("the landed version's title = %q, want the source version's %q", landed.title, source.title)
	}

	// (1b) The abort travelled with the content, record and all. The landed
	// row is the MERGE's write — created_by is the merging actor — while the
	// record inside it is the contributor's DECISION: reason code, human
	// explanation, and aborted_by/aborted_at, which migration 00100 keeps as
	// columns of their own precisely so a merge cannot make the accepting
	// actor look like the actor who decided.
	landedAbort := p.versionOfObjectInState(t, abortedObject, mainAfter)
	sourceAbort := readVersion(t, p.ctx, p.pool, abortedVersion)
	if landedAbort.lifecycleState != string(domain.LifecycleAborted) {
		t.Errorf("the landed version of the withdrawn object has lifecycle %q, want %q",
			landedAbort.lifecycleState, domain.LifecycleAborted)
	}
	if landedAbort.id == abortedVersion {
		t.Error("the accepted state names the fork's aborted row itself — the merge did not write a row of its own")
	}
	if landedAbort.createdBy != p.alice.id {
		t.Errorf("the landed aborted version's created_by = %s, want the merging actor %s", landedAbort.createdBy, p.alice.id)
	}
	if landedAbort.abortReasonCode == nil || *landedAbort.abortReasonCode != abortReason {
		t.Errorf("the landed abort record's reason code = %s, want the contributor's %q", ptrText(landedAbort.abortReasonCode), abortReason)
	}
	if landedAbort.abortExplanation == nil || *landedAbort.abortExplanation != abortExplanation {
		t.Errorf("the landed abort record's explanation = %s, want the contributor's verbatim", ptrText(landedAbort.abortExplanation))
	}
	if landedAbort.abortedBy == nil || *landedAbort.abortedBy != p.bob.id {
		t.Errorf("the landed abort record's aborted_by = %s, want the contributor %s who decided it, not the merging actor (docs/46:7)",
			ptrText(landedAbort.abortedBy), p.bob.id)
	}
	if sourceAbort.abortedAt == nil {
		t.Fatal("the fixture's aborted version carries no aborted_at — the record under test is incomplete")
	}
	if landedAbort.abortedAt == nil || !landedAbort.abortedAt.Equal(*sourceAbort.abortedAt) {
		t.Errorf("the landed abort record's aborted_at = %v, want the decision's time %v verbatim",
			landedAbort.abortedAt, *sourceAbort.abortedAt)
	}
	if string(landedAbort.payload) != string(sourceAbort.payload) {
		t.Errorf("the landed aborted version's payload is not the source version's, byte for byte:\n  landed: %s\n  source: %s",
			landedAbort.payload, sourceAbort.payload)
	}

	// (1c) The relation travelled with the content, and its pins were
	// re-pointed at the versions THIS merge wrote: the accepted state's edge
	// names the landed rows, not the fork's. The assertion is non-vacuous in
	// both directions — the source pins are ids the fork's state resolves to,
	// so "the fork's pins travelled verbatim" fails too.
	landedSupporting := p.versionOfObjectInState(t, supportingObject, mainAfter)
	landedRelation := p.relationVersionInState(t, bobRelation, mainAfter)
	sourceRelation := p.relationVersionInState(t, bobRelation, forkHead)
	if landedRelation.ID == bobRelationVersion {
		t.Error("the accepted state names the fork's relation version row itself — the merge did not write an edge of its own")
	}
	if landedRelation.StateID != mainAfter {
		t.Errorf("the landed relation version sits in state %s, want the accepted state %s", landedRelation.StateID, mainAfter)
	}
	if landedRelation.RelationType != relationType {
		t.Errorf("the landed relation type = %q, want the contributor's %q", landedRelation.RelationType, relationType)
	}
	if landedRelation.CreatedBy != p.alice.id {
		t.Errorf("the landed relation version's created_by = %s, want the merging actor %s", landedRelation.CreatedBy, p.alice.id)
	}
	if string(landedRelation.Payload) != string(sourceRelation.Payload) {
		t.Errorf("the landed relation's payload is not the source's, byte for byte:\n  landed: %s\n  source: %s",
			landedRelation.Payload, sourceRelation.Payload)
	}
	if landedRelation.SourceObjectVersionID != landedSupporting.id {
		t.Errorf("the landed edge's source pin = %s, want the version this merge wrote for the supporting object (%s)",
			landedRelation.SourceObjectVersionID, landedSupporting.id)
	}
	if landedRelation.TargetObjectVersionID != landed.id {
		t.Errorf("the landed edge's target pin = %s, want the version this merge wrote for the contributor's object (%s)",
			landedRelation.TargetObjectVersionID, landed.id)
	}
	if landedRelation.SourceObjectVersionID == sourceRelation.SourceObjectVersionID ||
		landedRelation.TargetObjectVersionID == sourceRelation.TargetObjectVersionID {
		t.Errorf("the landed edge kept a fork-side pin (source %s / target %s): the accepted state would resolve into the fork's rows",
			landedRelation.SourceObjectVersionID, landedRelation.TargetObjectVersionID)
	}

	// (2) The source side was read in the FORK's project. The merge record is
	// the proof: the row's project is the upstream one (that is where the
	// state was committed), while its source branch and source state are the
	// FORK's rows — a merge that had looked the source branch up in the PR's
	// project could not name them, and one that had taken the PR's project's
	// head as the source could not have read the contributor's version at all.
	var mergeProject, mergeSourceBranch, mergeTargetBranch, mergeSourceState, mergeResultState string
	if err := p.pool.QueryRow(ctx, `
		SELECT project_id::text, source_branch_id::text, target_branch_id::text,
		       source_state_id::text, result_state_id::text
		  FROM semantic_merges WHERE id = $1`, merged.MergeID).
		Scan(&mergeProject, &mergeSourceBranch, &mergeTargetBranch, &mergeSourceState, &mergeResultState); err != nil {
		t.Fatalf("read the merge record: %v", err)
	}
	if mergeProject != parent.ID {
		t.Errorf("the merge row's project = %s, want the upstream project %s", mergeProject, parent.ID)
	}
	if mergeSourceBranch != bobFork.Branch.ID || mergeSourceState != forkHead {
		t.Errorf("the merge row's source side = branch %s state %s, want the fork's %s / %s",
			mergeSourceBranch, mergeSourceState, bobFork.Branch.ID, forkHead)
	}
	if mergeTargetBranch != main.ID || mergeResultState != mainAfter {
		t.Errorf("the merge row's target side = branch %s state %s, want main %s / %s",
			mergeTargetBranch, mergeResultState, main.ID, mainAfter)
	}
	var sourceStateProject string
	if err := p.pool.QueryRow(ctx,
		`SELECT project_id::text FROM project_states WHERE id = $1`, mergeSourceState).Scan(&sourceStateProject); err != nil {
		t.Fatalf("read the merge row's source state: %v", err)
	}
	if sourceStateProject != bobFork.Project.ID {
		t.Errorf("the merge row's source state belongs to project %s, want the fork's %s",
			sourceStateProject, bobFork.Project.ID)
	}
	// The fork branch travelled with the merge: the accepted state IS the
	// fork branch's head, so the research path that proposed it has arrived
	// and docs/43's active → merged transition is the recorded outcome.
	var forkLifecycle string
	if err := p.pool.QueryRow(ctx,
		`SELECT lifecycle_state FROM branches WHERE id = $1`, bobFork.Branch.ID).Scan(&forkLifecycle); err != nil {
		t.Fatalf("read the fork branch's lifecycle: %v", err)
	}
	if forkLifecycle != string(domain.BranchLifecycleMerged) {
		t.Errorf("the fork branch's lifecycle = %q, want %q", forkLifecycle, domain.BranchLifecycleMerged)
	}

	// (4) The rights axis travelled with the content: the version that landed
	// upstream carries the SOURCE version's visibility policy, verbatim. The
	// assertion is non-vacuous — the source version's policy is a real row id,
	// so "nothing travelled" and "the upstream default travelled" both fail.
	if landed.visibilityPolicyID == nil || *landed.visibilityPolicyID != bobPolicy {
		t.Errorf("the landed version's visibility_policy_id = %v, want the source version's %s verbatim",
			landed.visibilityPolicyID, bobPolicy)
	}
	if source.visibilityPolicyID == nil || *source.visibilityPolicyID != bobPolicy {
		t.Errorf("the source version's visibility_policy_id = %v, want the pinned %s", source.visibilityPolicyID, bobPolicy)
	}

	// (3) The state machine is untouched by the cross-project path: the
	// proposal is merged, and the merge the route answered with is the row the
	// platform holds (one merge, one accepted state).
	var prState string
	if err := p.pool.QueryRow(ctx,
		`SELECT state FROM pull_requests WHERE id = $1`, pr.ID).Scan(&prState); err != nil {
		t.Fatalf("read the pull request's state: %v", err)
	}
	if prState != string(domain.PullRequestStateMerged) {
		t.Errorf("the pull request's state = %q, want %q", prState, domain.PullRequestStateMerged)
	}
	var merges int
	if err := p.pool.QueryRow(ctx,
		`SELECT count(*) FROM semantic_merges WHERE pull_request_id = $1`, pr.ID).Scan(&merges); err != nil {
		t.Fatalf("count the proposal's merges: %v", err)
	}
	if merges != 1 {
		t.Errorf("the proposal has %d merge record(s), want 1", merges)
	}

	// ---- The Git half of a cross-repository merge. This build's
	// GitMergeRequest names ONE repository (cmd/api/mergegit, whose port
	// internal/application/merge.GitMerger exposes), so the provider-side
	// merge of a fork branch into the upstream repository is not expressible:
	// the saga records the step as failed and STAYS VISIBLE, rather than
	// reporting an updated repository that nothing updated. The database half
	// is the committed research state above; the report names the limitation.
	if merged.GitState != string(domain.GitStateFailed) {
		t.Errorf("the Git step of a cross-project merge = %q, want %q (the provider merge is recorded as not attempted)",
			merged.GitState, domain.GitStateFailed)
	}
	if !strings.Contains(merged.GitError, "across repositories") {
		t.Errorf("the Git step's recorded error does not name the cross-repository limitation: %q", merged.GitError)
	}
}

// ---- The negative evidence ---------------------------------------------

// TestExternalForkMergeRefusesANonForkSource is criterion 3's negative half:
// a cross-project source branch that is NOT the opener's own fork must be
// refused, and the test names the layer that refuses.
//
// THREE layers hold the rule, and the test walks them in the order a caller
// meets them:
//
//   - the STORE (internal/persistence.PullRequestStore.CreatePullRequest)
//     holds it first, on every product path: it reads the source branch
//     without scoping the read to the PR's project, and answers the
//     existence-hiding not-found unless the row's project is a fork of this
//     project forked by this PR's creator. That is what part (a) pins — the
//     layer the product actually meets, and it is a judgment (the read is
//     unscoped, so the branch was SEEN and refused), not a lookup that
//     failed to find anything.
//
//   - migration 00086's pull_request_fork_gate holds it again at the ROW, for
//     every INSERT path, including one that does not go through the store.
//     Part (b) exercises that layer directly (a raw INSERT, no store): the
//     database's own SQLSTATE and message, not merely "an error". It is the
//     backstop behind (a) — the reason a store bug cannot open the hole, and
//     the reason (a) and (b) are not two spellings of one check.
//
//   - the MERGE holds it a third time, when it reads the source side. This
//     layer is UNREACHABLE through the product: a row naming a foreign source
//     either cannot be inserted at all (a, b), or it is the opener's own fork
//     and the merge must accept it (the closed-loop test above). Part (c)
//     reaches it anyway — the post-gate row shape is staged with the gate's
//     trigger suspended for one INSERT and restored immediately — and proves
//     the merge is a judgment and not a hole: it refuses the foreign branch,
//     answers the wire's not-found, and writes nothing at all.
func TestExternalForkMergeRefusesANonForkSource(t *testing.T) {
	p := newForkMergePlatform(t)
	ctx := p.ctx

	// The upstream project (the PR's project) and a project of Carol's that is
	// NOT a fork of it: an ordinary project, in another actor's own space.
	parent := p.createProject(t, p.alice, "mof-gas-gate", domain.VisibilityPublic)
	main := p.createBranch(t, p.alice, parent.ID, "main", "")
	foreign := p.createProject(t, p.carol, "carol-own-line", domain.VisibilityPublic)
	foreignBranch := p.createBranch(t, p.carol, foreign.ID, "line-c", "")
	foreignHead := p.branchHead(t, foreignBranch.ID)
	var lineage int
	if err := p.pool.QueryRow(ctx,
		`SELECT count(*) FROM project_forks WHERE fork_project_id = $1 AND parent_project_id = $2`,
		foreign.ID, parent.ID).Scan(&lineage); err != nil {
		t.Fatalf("count the foreign project's lineage rows: %v", err)
	}
	if lineage != 0 {
		t.Fatalf("the fixture's source project is a fork of the upstream project (%d lineage rows) — the case is not the negative one", lineage)
	}
	mainHead := p.branchHead(t, main.ID)

	// ---- (a) The product path: the store's refusal. A proposal naming the
	// foreign branch as its source is refused, whoever opens it, with the
	// existence-hiding not-found rather than a "this project does not exist"
	// distinction: the caller learns nothing about a branch it may not use.
	_, err := p.prSvc.Create(ctx, pullrequests.CreatePullRequestParams{
		ProjectID:      parent.ID,
		SourceBranchID: foreignBranch.ID,
		TargetBranchID: main.ID,
		Title:          "a proposal from a project that is not my fork",
		CreatedBy:      p.bob.id,
		CreationKey:    "fork-merge-gate-negative-0001",
	})
	if !errors.Is(err, pullrequests.ErrBranchNotFound) {
		t.Fatalf("opening a proposal on a foreign branch = %v (%T), want the store's ErrBranchNotFound", err, err)
	}
	// The refusal is a judgment, not a lookup that found nothing: the branch
	// named as the source exists, and is readable — read here through the
	// product's own branch service, by its owning project, to prove the row
	// the store refused to use is really there.
	if _, err := p.branchSvc.Get(ctx, foreign.ID, foreignBranch.ID); err != nil {
		t.Fatalf("the foreign branch the store refused does not exist: %v", err)
	}
	var rows, foreignRows int
	if err := p.pool.QueryRow(ctx,
		`SELECT count(*) FROM pull_requests WHERE project_id = $1`, parent.ID).Scan(&rows); err != nil {
		t.Fatalf("count the upstream project's proposals: %v", err)
	}
	if rows != 0 {
		t.Errorf("the refused proposal left %d row(s) in the upstream project", rows)
	}
	if err := p.pool.QueryRow(ctx,
		`SELECT count(*) FROM pull_requests WHERE project_id = $1`, foreign.ID).Scan(&foreignRows); err != nil {
		t.Fatalf("count the foreign project's proposals: %v", err)
	}
	if foreignRows != 0 {
		t.Errorf("the refused proposal left %d row(s) in the source project", foreignRows)
	}

	// ---- (b) 00086's gate, the row-level backstop, reached by an INSERT that
	// does not go through the store. The database refuses it with the gate's
	// own SQLSTATE and message — the layer that holds even if the store one
	// day grew a hole.
	var pgErr *pgconn.PgError
	_, insertErr := p.pool.Exec(ctx, `INSERT INTO pull_requests
		(project_id, number, source_branch_id, target_branch_id, base_state_id, proposed_state_id,
		 title, state, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, 'a source branch from a project that is not my fork', $7, $8)`,
		parent.ID, int64(9100), foreignBranch.ID, main.ID, mainHead, foreignHead,
		string(domain.PullRequestStateMergeReady), p.bob.id)
	if insertErr == nil {
		t.Fatal("a raw INSERT smuggled a cross-project proposal past 00086's fork gate")
	}
	if !errors.As(insertErr, &pgErr) || pgErr.Code != "P0001" {
		t.Fatalf("the raw INSERT's refusal = %v (%T), want the fork gate's own P0001", insertErr, insertErr)
	}
	for _, want := range []string{
		"pull request on project " + parent.ID,
		"cannot take its source branch from project " + foreign.ID,
		"must come from the opener's own fork",
	} {
		if !strings.Contains(pgErr.Message, want) {
			t.Errorf("the fork gate's message does not say %q:\n%s", want, pgErr.Message)
		}
	}
	if err := p.pool.QueryRow(ctx,
		`SELECT count(*) FROM pull_requests WHERE project_id = $1`, parent.ID).Scan(&rows); err != nil {
		t.Fatalf("count the upstream project's proposals: %v", err)
	}
	if rows != 0 {
		t.Errorf("the refused raw INSERT left %d proposal row(s)", rows)
	}

	// ---- (c) The post-gate shape: the same pair, inserted with the gate's
	// trigger suspended for this one statement and restored in the same
	// transaction, in merge_ready (the state the merge's own state-machine
	// check demands). The trigger is restored before anything else runs, and
	// the cleanup below restores it again if the insert fails — a run that
	// leaves the gate off would silently weaken every later test in the
	// package.
	const prNumber = int64(9101)
	stagePostGateRow(t, p, parent.ID, foreignBranch.ID, main.ID, mainHead, foreignHead, p.bob.id, prNumber)

	// (c1) The command refuses. The refusal is the same existence-hiding
	// not-found the package answers for a branch it will not read, so a
	// caller learns nothing about a project whose branch it may not use.
	directKey := "fork-merge-negative-direct-1"
	if _, err := p.mergeSvc.Merge(ctx, p.alice.actor(), merge.Input{
		ProjectID: parent.ID, Number: prNumber, IdempotencyKey: &directKey,
	}); !errors.Is(err, merge.ErrBranchNotFound) {
		t.Fatalf("merging a proposal whose source is not the opener's fork = %v, want ErrBranchNotFound", err)
	}

	// (c2) The same refusal on the wire, in the route's own vocabulary: the
	// guard lets the upstream owner through, and the command's refusal maps
	// to the contract's BRANCH_NOT_FOUND envelope.
	resp := p.alice.client.doKeyed(t, http.MethodPost, mergePath(parent.ID, prNumber), "",
		"fork-merge-negative-wire-00001")
	mustStatus(t, resp, http.StatusNotFound)
	mustEnvelope(t, resp, merge.CodeBranchNotFound)

	// (c3) Nothing was written: no merge record, no advance of main, no
	// version created, and the proposal's own state is exactly what the
	// fixture left it in. A refusal that wrote a merge row or moved main
	// would be worse than the refusal this test asserts.
	var mergeRows, stateRows int
	if err := p.pool.QueryRow(ctx,
		`SELECT count(*) FROM semantic_merges WHERE project_id = $1`, parent.ID).Scan(&mergeRows); err != nil {
		t.Fatalf("count the upstream project's merges: %v", err)
	}
	if mergeRows != 0 {
		t.Errorf("the refused merge wrote %d merge record(s)", mergeRows)
	}
	if err := p.pool.QueryRow(ctx,
		`SELECT count(*) FROM project_states WHERE project_id = $1`, parent.ID).Scan(&stateRows); err != nil {
		t.Fatalf("count the upstream project's states: %v", err)
	}
	if stateRows != 1 {
		t.Errorf("the upstream project holds %d state(s), want only the one the fixture created", stateRows)
	}
	if after := p.branchHead(t, main.ID); after != mainHead {
		t.Errorf("main's head moved to %s on a refused merge, want %s", after, mainHead)
	}
	var prState string
	if err := p.pool.QueryRow(ctx,
		`SELECT state FROM pull_requests WHERE project_id = $1 AND number = $2`, parent.ID, prNumber).Scan(&prState); err != nil {
		t.Fatalf("read the staged proposal's state: %v", err)
	}
	if prState != string(domain.PullRequestStateMergeReady) {
		t.Errorf("the refused proposal's state = %q, want %q (the refusal must not consume the proposal)",
			prState, domain.PullRequestStateMergeReady)
	}
}

// stagePostGateRow inserts the row shape that migration 00086's fork gate
// exists to make unreachable: a proposal of projectID whose source branch
// lives in sourceProjectID, which is not the opener's fork. It is the shape
// the merge's own source-side check has to be tested against, and the gate
// has to be suspended for exactly this one statement to produce it.
//
// The suspension, the insert and the restore are ONE transaction — DDL is
// transactional in PostgreSQL — so the gate is never observably off: a
// reader outside this transaction sees it enabled throughout, whether the
// statement commits or rolls back, and a process that dies mid-way leaves
// the transaction (and with it the suspension) rolled back rather than the
// table unguarded. Committing with the trigger still disabled would open the
// gate for EVERY session of this database until the next statement ran, which
// is exactly what this shape rules out. The helper then proves the gate is
// back on — a second raw insert of the same shape must be refused with the
// gate's own SQLSTATE before the caller proceeds — and re-enables it in
// cleanup as a belt-and-braces restore.
func stagePostGateRow(t *testing.T, p *forkMergePlatform, projectID, sourceBranchID, targetBranchID, baseStateID, proposedStateID, createdBy string, number int64) {
	t.Helper()
	ctx := p.ctx
	const insert = `INSERT INTO pull_requests
		(project_id, number, source_branch_id, target_branch_id, base_state_id, proposed_state_id,
		 title, state, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, 'a source branch from a project that is not my fork', $7, $8)`
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("stage the post-gate row: begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `ALTER TABLE pull_requests DISABLE TRIGGER pull_request_fork_gate_trigger`); err != nil {
		t.Fatalf("stage the post-gate row: suspend the fork gate: %v", err)
	}
	if _, err := tx.Exec(ctx, insert,
		projectID, number, sourceBranchID, targetBranchID, baseStateID, proposedStateID,
		string(domain.PullRequestStateMergeReady), createdBy); err != nil {
		t.Fatalf("stage the post-gate row: insert: %v", err)
	}
	if _, err := tx.Exec(ctx, `ALTER TABLE pull_requests ENABLE TRIGGER pull_request_fork_gate_trigger`); err != nil {
		t.Fatalf("stage the post-gate row: restore the fork gate: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("stage the post-gate row: commit: %v", err)
	}
	// The gate is back on, and this is the assertion that says so: the same
	// raw insert — a fresh number, so nothing but the gate can refuse it —
	// must be refused with the trigger's own P0001. Without this probe the
	// only evidence the restore happened would be the absence of an error
	// from the ALTER, which is not the same claim.
	var pgErr *pgconn.PgError
	if _, err := p.pool.Exec(ctx, insert,
		projectID, number+1, sourceBranchID, targetBranchID, baseStateID, proposedStateID,
		string(domain.PullRequestStateMergeReady), createdBy); err == nil {
		t.Fatal("the fork gate did not come back on: a raw cross-project proposal was accepted after the restore")
	} else if !errors.As(err, &pgErr) || pgErr.Code != "P0001" {
		t.Fatalf("the post-restore probe = %v (%T), want the fork gate's own P0001", err, err)
	}
	t.Cleanup(func() {
		if _, err := p.pool.Exec(ctx, `ALTER TABLE pull_requests ENABLE TRIGGER pull_request_fork_gate_trigger`); err != nil {
			t.Errorf("restore the fork gate after the test: %v", err)
		}
	})
}
