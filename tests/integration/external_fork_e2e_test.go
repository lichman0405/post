// Package integration — T0804 "external fork e2e" (blocking): the external
// fork/contribution flow over the real product path — real PostgreSQL, a
// real Gitea instance, the real permission engine (authz.MatrixEngine over
// specs/policies/permissions-matrix.csv) and the real HTTP surface (the
// production guard in front of a mux the production wiring populated, in
// cmd/api/main.go's own order).
//
// Nothing here fabricates fork state: the fork project, its repository, its
// branch, the imported content, the branch's semantic completeness flag and
// the pull request are all produced by the product's own code paths, and
// every assertion reads the canonical rows (or the provider) back.
//
// The acceptance criteria map onto phases of the four tests:
//
//   - TestExternalForkEndToEnd
//     (1) a non-member forks a PUBLIC project; the same account is refused
//     on a private one; (2) write_scientific_state = own_fork_only: the
//     write succeeds in the actor's own fork and is refused upstream and in
//     another actor's fork; (3) open_pr = allow_from_fork: from the fork it
//     succeeds, straight from an upstream branch it is refused (and the
//     database refuses the same insert for any path — 00086's
//     pull_request_fork_gate); (4) the anonymous column denies all three;
//     (8) the lineage is recorded with 'forked_from' and readable;
//     (9) a repeated request and two concurrent requests leave exactly one
//     lineage row, one audit row and one event; (11) docs/31_MASTER_ACCEPTANCE
//     :17 「Public Project 外部用户可 fork/contribute」 runs end to end, and
//     the contribution is read back through the project's real pull-request
//     surface.
//   - TestExternalForkImportDerivesTheSemanticFlag
//     (6) the imported branch's semantic_state is derived from the import's
//     own evidence: an unparseable import is refused by
//     pull_request_semantic_gate and a parseable one reaches its PR;
//     (7) the flag is not a life sentence — a real push that removes the
//     unparseable file clears it, and the SAME proposal call then succeeds.
//   - TestExternalForkCannotObtainRestrictedBlobs
//     (5) restricted blob content is unreachable from the fork, and it is
//     unreachable because no such route exists — not because a UI hides it.
//   - TestExternalForkClaimTransactionIsAtomic
//     (10) the lineage row, the audit row and the event are one
//     transaction's facts: a late failure leaves none of them.
//   - TestExternalForkLineageClaimIsACompareAndSwap
//     (9) the claim itself — the arm a repeated or concurrent request is
//     idempotent BY, asserted at forks.StorePort where it lives: a second
//     claim for the same (parent, actor) pair loses without writing a
//     second lineage row, audit row or event. The service settles repeats
//     on the fork project's unique slug before reaching this arm, so the
//     end-to-end test cannot see it.
//   - TestExternalForkSurvivesATakenDerivedName
//     (1)(9) the fork's derived name is computable from public facts, so a
//     third party — or the actor's own earlier project — can hold it first
//     (personal slugs are unique and nothing is deleted): a foreign holder
//     must not deadlock the fork (it moves to the pair's reserved name and
//     completes there, leaving the holder untouched), and an own holder must
//     be answered accurately rather than as a fork that does not exist.
//   - TestExternalForkEscalationStaysOneForkUnderConcurrency
//     (9) the property that fix rests on: the reserved name is a function of
//     the pair, so two concurrent requests of one pair that both escalate
//     derive the same name, the unique index arbitrates, and the pair ends
//     with exactly one fork project, one lineage row, one audit row and one
//     event.
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/fileshttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/cmd/api/pullrequestshttp"
	"github.com/lichman0405/post/cmd/api/rsghttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/forks"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/pullrequests"
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
	"github.com/lichman0405/post/internal/rights"
	"github.com/lichman0405/post/internal/rsg/schemareg"
)

const externalForkTaskID = "T0804"

// forkAccount is one signed-up user: the browser carrying its session and
// CSRF token (minted by the real signup endpoint), and the user id the
// platform knows it by.
type forkAccount struct {
	client *testUserClient
	id     string
}

// actor is the domain actor the account acts as.
func (a forkAccount) actor() domain.User { return domain.User{ID: a.id} }

// forkRepo is one provisioned repository's coordinates, read back from the
// canonical provision row rather than assumed.
type forkRepo struct {
	owner  string
	name   string
	id     int64
	secret string
}

// externalForkFixture assembles the graph cmd/api builds — the same
// constructors, over the same real pool, behind the same guard — plus the
// provider helpers the T0303/T0305 fixtures already carry. Only the session
// store and the rate limiter are in-memory (Redis semantics are orthogonal
// and covered by the auth e2e suite); every store behind the guard and the
// provider adapter are production.
type externalForkFixture struct {
	*pushIngestionGiteaFixture

	ctx context.Context
	ts  *httptest.Server
	mux *http.ServeMux

	adapter      *gitprovider.GiteaAdapter
	reg          *schemareg.Registry
	orgStore     *persistence.OrgStore
	projectStore *persistence.ProjectStore
	stateStore   *persistence.StateStore
	branchStore  *persistence.BranchStore
	projectSvc   *projects.Service
	stateSvc     *states.Service
	branchSvc    *branches.Service
	rsgSvc       *rsg.Service
	prSvc        *pullrequests.Service
	forkStore    *persistence.ForkStore
	forkSvc      *forks.Service
	provisioner  *gitprovider.Provisioner

	alice, bob, carol, dave, mallory forkAccount

	// privateProject is the fixture's own private project, owned by somebody
	// else: the "same account, private project" control of criterion 1. It
	// is deliberately never provisioned — the read gate refuses the fork
	// before any resource is touched, which is part of what is asserted.
	privateProject domain.Project
}

func newExternalForkFixture(t *testing.T) *externalForkFixture {
	t.Helper()
	ctx := testCtx(t)
	base := requireGitea(t)
	token := giteaServiceToken(t, base)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), externalForkTaskID)

	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("external fork e2e: schema registry: %v", err)
	}
	cfg := gitprovider.Config{
		BaseURL:    base,
		Token:      config.Secret(token),
		WebhookURL: "http://host.invalid/api/v1/git/hooks/gitea",
	}
	adapter := gitprovider.NewGiteaAdapter(cfg)

	// ---- The platform side: the graph cmd/api assembles.
	stateStore := persistence.NewStateStore(pool)
	branchStore := persistence.NewBranchStore(pool)
	stateSvc := states.NewService(stateStore, newCommitGuard(t))
	branchSvc := branches.NewService(branchStore)
	orgStore := persistence.NewOrgStore(pool)
	projectStore := persistence.NewProjectStore(pool)
	projectAPI := projectshttp.New(projectshttp.Deps{
		Store: projectStore,
		Orgs:  orgStore,
		Authz: authz.NewMatrixEngine(),
	})
	projectSvc := projectAPI.Service()
	forkStore := persistence.NewForkStore(pool)
	provisioner := gitprovider.NewProvisioner(adapter, gitprovider.NewProvisionStore(pool), cfg.WebhookURL)
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
		// The own_fork_only cell of write_scientific_state is resolved
		// through this gate — never assumed by the service.
		ForkGate: forkStore,
	})
	prSvc := pullrequests.NewService(persistence.NewPullRequestStore(pool))
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

	// ---- The HTTP surface: the production guard over a mux the production
	// route assemblies populated, in cmd/api/main.go's order.
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
	mux := http.NewServeMux()
	authAPI.Register(mux)
	mux.Handle("/api/v1/projects", projectAPI.Routes())
	mux.Handle("/api/v1/projects/", projectAPI.Routes())
	rsghttp.New(rsghttp.Deps{Service: rsgSvc}).Register(mux)
	pullrequestshttp.New(pullrequestshttp.Deps{PullRequests: prSvc, Projects: projectSvc}).Register(mux)
	// The read-only file surface (T0307): the only place the product serves
	// bytes out of a repository, and therefore the surface criterion 5 has
	// to probe.
	fileshttp.New(fileshttp.Deps{
		Projects: projectSvc,
		Files:    gitprovider.NewFilesReader(adapter, gitprovider.NewUserAccessStore(pool)),
	}).Register(mux)
	ts := httptest.NewServer(authAPI.Guard(mux))
	t.Cleanup(ts.Close)

	// ---- The private control project: owned by an account that is not any
	// of the actors below, so every one of them is a non-member of it.
	ownerUser, err := persistence.NewCredentialStore(pool).CreateWithPassword(
		ctx, "extfork-owner@example.com", "hash", "extfork-owner", "External Fork Owner")
	if err != nil {
		t.Fatalf("external fork e2e: seed the control project's owner: %v", err)
	}
	privateProject, _, err := projectStore.CreateProject(ctx, domain.Project{
		Slug:            "extfork-private",
		Name:            "External Fork Private",
		Purpose:         "T0804 private-project control",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, ownerUser.ID)
	if err != nil {
		t.Fatalf("external fork e2e: create the control project: %v", err)
	}

	inner := &pushIngestionGiteaFixture{
		branchRefGiteaFixture: &branchRefGiteaFixture{
			pool:     pool,
			base:     base,
			token:    token,
			user:     ownerUser,
			project:  privateProject,
			branches: branchSvc,
			cfg:      cfg,
		},
		reg: reg,
	}
	fx := &externalForkFixture{
		pushIngestionGiteaFixture: inner,
		ctx:                       ctx,
		ts:                        ts,
		mux:                       mux,
		adapter:                   adapter,
		reg:                       reg,
		orgStore:                  orgStore,
		projectStore:              projectStore,
		stateStore:                stateStore,
		branchStore:               branchStore,
		projectSvc:                projectSvc,
		stateSvc:                  stateSvc,
		branchSvc:                 branchSvc,
		rsgSvc:                    rsgSvc,
		prSvc:                     prSvc,
		forkStore:                 forkStore,
		forkSvc:                   forkSvc,
		provisioner:               provisioner,
		privateProject:            privateProject,
	}
	fx.alice = fx.signupAccount(t, "extfork-alice@example.com", "extfork-alice")
	fx.bob = fx.signupAccount(t, "extfork-bob@example.com", "extfork-bob")
	fx.carol = fx.signupAccount(t, "extfork-carol@example.com", "extfork-carol")
	fx.dave = fx.signupAccount(t, "extfork-dave@example.com", "extfork-dave")
	fx.mallory = fx.signupAccount(t, "extfork-mallory@example.com", "extfork-mallory")
	return fx
}

// ---- Accounts, projects and repositories ------------------------------

// signupAccount creates one account through the real signup endpoint: the
// session cookie and CSRF token every request below carries are the ones
// that endpoint minted. No test-side principal injection is possible (or
// wanted) — the principal the handlers read is the guard's.
func (fx *externalForkFixture) signupAccount(t *testing.T, email, handle string) forkAccount {
	t.Helper()
	client, id := signup(t, fx.ts.URL, email, handle)
	return forkAccount{client: client, id: id}
}

// createProject drives the product's own project-create route and returns
// the row the rest of the platform sees.
func (fx *externalForkFixture) createProject(t *testing.T, account forkAccount, slug string, visibility domain.ProjectVisibility) domain.Project {
	t.Helper()
	body := fmt.Sprintf(`{"slug":%q,"name":%q,"purpose":%q,"visibility":%q}`,
		slug, slug, "T0804 external fork e2e", string(visibility))
	resp := account.client.do(t, http.MethodPost, "/api/v1/projects", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("external fork e2e: create project %s = %d: %s", slug, resp.StatusCode, readAll(t, resp))
	}
	var payload struct {
		Project struct {
			ID         string `json:"id"`
			Visibility string `json:"visibility"`
		} `json:"project"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("external fork e2e: decode project %s: %v", slug, err)
	}
	if payload.Project.Visibility != string(visibility) {
		t.Fatalf("external fork e2e: project %s visibility = %q, want %q",
			slug, payload.Project.Visibility, visibility)
	}
	project, err := fx.projectSvc.Get(fx.ctx,
		projects.Reader{UserID: account.id, Authenticated: true}, payload.Project.ID)
	if err != nil {
		t.Fatalf("external fork e2e: read back project %s: %v", slug, err)
	}
	return project
}

// provisionProject runs the production provisioner over a project (the work
// the provisioning job does) and reads the repository coordinates back from
// the canonical provision row.
func (fx *externalForkFixture) provisionProject(t *testing.T, projectID string) forkRepo {
	t.Helper()
	if err := fx.provisioner.Provision(fx.ctx, projectID); err != nil {
		t.Fatalf("external fork e2e: provision %s: %v", projectID, err)
	}
	return fx.repoOf(t, projectID)
}

// repoOf reads a project's provisioned repository identity (and registers
// its cleanup).
func (fx *externalForkFixture) repoOf(t *testing.T, projectID string) forkRepo {
	t.Helper()
	owner, err := fx.adapter.Owner(fx.ctx)
	if err != nil {
		t.Fatalf("external fork e2e: resolve the service identity: %v", err)
	}
	var repo forkRepo
	repo.owner = owner
	if err := fx.pool.QueryRow(fx.ctx,
		`SELECT name, gitea_repo_id, webhook_secret FROM git_repository_provisions WHERE project_id = $1`,
		projectID).Scan(&repo.name, &repo.id, &repo.secret); err != nil {
		t.Fatalf("external fork e2e: read the provision row of %s: %v", projectID, err)
	}
	t.Cleanup(func() { deleteGiteaRepo(t, fx.base, fx.token, repo.owner, repo.name) })
	return repo
}

// createBranch creates a branch through the canonical RSG service and syncs
// its provider ref (the work the refsync job does), exactly as
// cmd/api/main.go's graph does. It returns the branch row.
func (fx *externalForkFixture) createBranch(t *testing.T, actor forkAccount, projectID, name, baseRef string) domain.Branch {
	t.Helper()
	branch, err := fx.rsgSvc.CreateBranch(fx.ctx, actor.actor(), projectID, rsg.CreateBranchInput{
		Name:       name,
		BaseRef:    baseRef,
		Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("external fork e2e: create branch %s: %v", name, err)
	}
	if err := fx.syncer().Sync(fx.ctx, branch.ID); err != nil {
		t.Fatalf("external fork e2e: sync the %s ref: %v", name, err)
	}
	return branch
}

// pushAndDeliver pushes one real commit to a branch and replays the
// provider's signed delivery into the production receiver, returning the
// new head.
func (fx *externalForkFixture) pushAndDeliver(t *testing.T, repo forkRepo, branch, base string,
	add map[string]string, remove []string, message, deliveryID string) string {
	t.Helper()
	sha := fx.pushCommit(t, repo.owner, repo.name, branch, base, add, remove, message)
	if code := fx.deliver(t, repo.id, repo.owner, repo.name, "refs/heads/"+branch,
		base, sha, 1, nil, repo.secret, deliveryID); code != http.StatusNoContent {
		t.Fatalf("external fork e2e: delivery for %s@%s = %d, want 204", branch, sha, code)
	}
	return sha
}

// ---- Canonical-row probes --------------------------------------------

// stateIDOfCommit is the project state the ingestion recorded for one
// pushed commit — the state the next branch forks from.
func (fx *externalForkFixture) stateIDOfCommit(t *testing.T, projectID, sha string) string {
	t.Helper()
	var id string
	if err := fx.pool.QueryRow(fx.ctx, `SELECT id::text FROM project_states
		WHERE project_id = $1 AND git_commit_sha = $2
		ORDER BY created_at, id LIMIT 1`, projectID, sha).Scan(&id); err != nil {
		t.Fatalf("external fork e2e: probe the state of commit %s: %v", sha, err)
	}
	return id
}

// forkPoint reads one branch's recorded provider-side fork point
// (git_branch_refs.fork_sha) — the baseline a fork import measures a copy
// of that line against.
func (fx *externalForkFixture) forkPoint(t *testing.T, branchID string) string {
	t.Helper()
	var sha *string
	if err := fx.pool.QueryRow(fx.ctx,
		`SELECT fork_sha FROM git_branch_refs WHERE branch_id = $1`, branchID).Scan(&sha); err != nil {
		t.Fatalf("external fork e2e: probe the fork point of %s: %v", branchID, err)
	}
	if sha == nil {
		return ""
	}
	return *sha
}

// importIngestion is the delivery the fork import recorded for one
// repository ref, with the change rows it was inspected down to.
func (fx *externalForkFixture) importIngestion(t *testing.T, repoID int64, ref string) (ingestionRow, []changeRow) {
	t.Helper()
	var row ingestionRow
	var ingestionID string
	if err := fx.pool.QueryRow(fx.ctx, `SELECT id::text, COALESCE(delivery_id, ''), gitea_repo_id,
		COALESCE(project_id::text, ''), branch_name, git_ref, before_sha, after_sha, commit_count,
		COALESCE(pusher, ''), COALESCE(head_skip_reason, '')
		FROM git_push_ingestions WHERE gitea_repo_id = $1 AND git_ref = $2`, repoID, ref).
		Scan(&ingestionID, &row.deliveryID, &row.giteaRepo, &row.projectID, &row.branchName,
			&row.gitRef, &row.beforeSHA, &row.afterSHA, &row.commits, &row.pusher, &row.headSkipReason); err != nil {
		t.Fatalf("external fork e2e: probe the import's ingestion for %d/%s: %v", repoID, ref, err)
	}
	rows, err := fx.pool.Query(fx.ctx, `SELECT path, change_kind, file_kind,
		COALESCE(schema_id, ''), COALESCE(content_sha256, '')
		FROM git_push_changes WHERE ingestion_id = $1 ORDER BY path`, ingestionID)
	if err != nil {
		t.Fatalf("external fork e2e: probe the import's changes: %v", err)
	}
	defer rows.Close()
	var changes []changeRow
	for rows.Next() {
		var c changeRow
		if err := rows.Scan(&c.path, &c.kind, &c.fileKind, &c.schemaID, &c.contentSHA); err != nil {
			t.Fatalf("external fork e2e: scan the import's change: %v", err)
		}
		changes = append(changes, c)
	}
	return row, changes
}

// forkRecordCounts counts the three facts one fork must leave exactly one
// of: the lineage row, its audit row and the outbox event.
func (fx *externalForkFixture) forkRecordCounts(t *testing.T, parentID, actorID string) (lineage, audits, eventsCount int) {
	t.Helper()
	if err := fx.pool.QueryRow(fx.ctx,
		`SELECT count(*) FROM project_forks WHERE parent_project_id = $1 AND forked_by = $2`,
		parentID, actorID).Scan(&lineage); err != nil {
		t.Fatalf("external fork e2e: count lineage rows: %v", err)
	}
	if err := fx.pool.QueryRow(fx.ctx,
		`SELECT count(*) FROM audit_log WHERE project_id = $1 AND actor_id = $2 AND action = $3`,
		parentID, actorID, forks.ActionProjectForked).Scan(&audits); err != nil {
		t.Fatalf("external fork e2e: count audit rows: %v", err)
	}
	if err := fx.pool.QueryRow(fx.ctx,
		`SELECT count(*) FROM outbox_events
		  WHERE event_type = 'branch.created'
		    AND project_id = (SELECT fork_project_id FROM project_forks
		                       WHERE parent_project_id = $1 AND forked_by = $2)`,
		parentID, actorID).Scan(&eventsCount); err != nil {
		t.Fatalf("external fork e2e: count fork events: %v", err)
	}
	return lineage, audits, eventsCount
}

// prCount counts a project's pull requests.
func (fx *externalForkFixture) prCount(t *testing.T, projectID string) int {
	t.Helper()
	var n int
	if err := fx.pool.QueryRow(fx.ctx,
		`SELECT count(*) FROM pull_requests WHERE project_id = $1`, projectID).Scan(&n); err != nil {
		t.Fatalf("external fork e2e: count pull requests: %v", err)
	}
	return n
}

// isSemanticGateError reports whether err is the database's P0001 refusal —
// what 00042's gates and 00086's fork gate raise (the shape
// unstructured_change_state_test.go pins).
func isSemanticGateError(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "P0001"
}

// eventTypeInVocabulary reports whether eventType is a name of
// specs/events/event-types.yaml — the machine-readable vocabulary, not a
// prose spelling.
func eventTypeInVocabulary(t *testing.T, eventType string) bool {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("external fork e2e: resolve the repository root: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "specs", "events", "event-types.yaml"))
	if err != nil {
		t.Fatalf("external fork e2e: read the event vocabulary: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "- "+eventType {
			return true
		}
	}
	return false
}

// TestExternalForkEndToEnd is the master-acceptance path of
// docs/31_MASTER_ACCEPTANCE.md:17 「Public Project 外部用户可 fork/contribute」:
// a non-member forks a public project, writes scientific state in their own
// fork, and proposes the contribution to the upstream project — every step
// through the product's own code, and the proposal read back through the
// project's real pull-request surface.
func TestExternalForkEndToEnd(t *testing.T) {
	fx := newExternalForkFixture(t)
	ctx := fx.ctx

	// ---- The public project, created through the product's HTTP surface.
	parent := fx.createProject(t, fx.alice, "mof-gas-ext", domain.VisibilityPublic)
	repo := fx.provisionProject(t, parent.ID)
	main := fx.createBranch(t, fx.alice, parent.ID, "main", "")
	mainSHA := fx.seedMain(t, repo.owner, repo.name)

	// ---- (4) The anonymous column: all three cells are deny. The two
	// actions with an HTTP route are refused before routing (401, the
	// guard's structural answer), the third is refused by the service (the
	// product has no PR-create route yet — pullrequestshttp is read-only —
	// so the service IS its product path).
	anonymous := newTestUserClient(fx.ts.URL)
	for _, tc := range []struct {
		name string
		path string
	}{
		{"create_branch", fmt.Sprintf("/api/v1/projects/%s/branches", parent.ID)},
		{"write_scientific_state", fmt.Sprintf("/api/v1/projects/%s/branches/%s/objects", parent.ID, main.ID)},
	} {
		resp := anonymous.do(t, http.MethodPost, tc.path, `{"name":"anon"}`)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("(4) anonymous %s = %d: %s", tc.name, resp.StatusCode, readAll(t, resp))
		}
	}
	if _, err := fx.forkSvc.OpenExternalPR(ctx, domain.User{}, forks.OpenPRRequest{
		ProjectID: parent.ID, SourceBranchID: main.ID, TargetBranchID: main.ID, Title: "anonymous",
	}); !errors.Is(err, forks.ErrForbidden) {
		t.Errorf("(4) anonymous open_pr = %v, want ErrForbidden", err)
	}
	// The matrix itself, read through the real engine: the anonymous class
	// denies all three cells, so no enforcement site can permit them.
	engine := authz.NewMatrixEngine()
	for _, action := range []authz.Action{authz.ActionCreateBranch, authz.ActionWriteScientificState, authz.ActionOpenPR} {
		decision, err := engine.Authorize(ctx, authz.Request{
			Action: action,
			Class:  authz.ClassOf(false, nil, false),
		})
		if err != nil {
			t.Fatalf("(4) authorize %s for the anonymous class: %v", action, err)
		}
		if decision.Verdict != authz.VerdictDeny || decision.Permits() {
			t.Errorf("(4) %s for public_anonymous = %q, want deny", action, decision.Verdict)
		}
	}

	// ---- (1) A non-member forks the public project.
	bob := fx.bob.actor()
	bobFork, err := fx.forkSvc.Fork(ctx, bob, forks.ForkRequest{ProjectID: parent.ID})
	if err != nil {
		t.Fatalf("(1) fork the public project as a non-member: %v", err)
	}
	if bobFork.AlreadyForked || !bobFork.Imported {
		t.Fatalf("(1) first fork = %+v, want a fresh fork whose content was imported", bobFork)
	}
	if bobFork.Fork.ParentProjectID != parent.ID || bobFork.Fork.ForkedBy != bob.ID {
		t.Fatalf("(1) lineage = %+v, want parent %s forked by %s", bobFork.Fork, parent.ID, bob.ID)
	}
	if bobFork.Fork.ForkProjectID == parent.ID {
		t.Fatal("(1) the fork project is the parent — a fork must be the actor's own project")
	}
	if bobFork.Fork.RelationType != "forked_from" {
		t.Errorf("(1) lineage relation = %q, want the vocabulary's forked_from", bobFork.Fork.RelationType)
	}
	if bobFork.Fork.ForkedSHA == nil || *bobFork.Fork.ForkedSHA != mainSHA {
		t.Errorf("(1) fork point = %v, want the copied commit %s", bobFork.Fork.ForkedSHA, mainSHA)
	}
	if !bobFork.Import.Recorded {
		t.Error("(1) the import's delivery was already recorded — the first fork must be the one that records it")
	}
	// The fork is Bob's own space: he is its owner, not a contributor of the
	// parent (docs/04 §2 says both things at once).
	membership, err := fx.projectSvc.GetMembership(ctx, bob, bobFork.Project.ID)
	if err != nil {
		t.Fatalf("(1) read the fork's membership: %v", err)
	}
	if membership.Role != domain.ProjectRoleOwner {
		t.Errorf("(1) the fork's membership role = %q, want owner", membership.Role)
	}
	if _, err := fx.projectSvc.GetMembership(ctx, bob, parent.ID); !errors.Is(err, projects.ErrMemberNotFound) {
		t.Errorf("(1) the forker's membership in the parent = %v, want ErrMemberNotFound (a fork grants no upstream role)", err)
	}
	forkRepo := fx.repoOf(t, bobFork.Project.ID)
	// The content really landed on the provider: the fork's repository
	// carries the copied branch at the copied commit.
	if code, sha := fx.giteaBranch(t, forkRepo.owner, forkRepo.name, bobFork.Branch.Name); code != http.StatusOK || sha != mainSHA {
		t.Errorf("(1) the fork's ref = %d@%s, want 200 at the copied commit %s", code, sha, mainSHA)
	}

	// ---- (1b) The same account on a PRIVATE project is refused, and the
	// refusal is existence-hiding: nothing about that project is created —
	// not a fork project (whose row could never be deleted) and not a
	// lineage row.
	if _, err := fx.forkSvc.Fork(ctx, bob, forks.ForkRequest{ProjectID: fx.privateProject.ID}); !errors.Is(err, forks.ErrProjectNotFound) {
		t.Fatalf("(1) fork a private project as a non-member = %v, want ErrProjectNotFound", err)
	}
	var privateChildren, privateLineage int
	if err := fx.pool.QueryRow(ctx,
		`SELECT count(*) FROM projects WHERE slug LIKE $1`, fx.privateProject.Slug+"-%").
		Scan(&privateChildren); err != nil {
		t.Fatalf("(1) count the refused fork's projects: %v", err)
	}
	if err := fx.pool.QueryRow(ctx,
		`SELECT count(*) FROM project_forks WHERE parent_project_id = $1`, fx.privateProject.ID).
		Scan(&privateLineage); err != nil {
		t.Fatalf("(1) count the refused fork's lineage rows: %v", err)
	}
	if privateChildren != 0 || privateLineage != 0 {
		t.Errorf("(1) a refused fork left %d project(s) and %d lineage row(s)", privateChildren, privateLineage)
	}

	// ---- Carol's fork of the same public project: the "somebody else's
	// fork" target of criterion 2, and an actor the write refusals can name.
	// Carol's fork of the same public project: the "somebody else's fork"
	// target of criterion 2, and an actor the write refusals can name. Her
	// fork is PUBLIC on purpose — a private one would be answered by the
	// project read gate (an existence-hiding 404) and the write cell would
	// never be reached. A readable fork owned by somebody else is where
	// own_fork_only has to do the refusing.
	carolFork, err := fx.forkSvc.Fork(ctx, fx.carol.actor(), forks.ForkRequest{
		ProjectID: parent.ID, Visibility: domain.VisibilityPublic,
	})
	if err != nil {
		t.Fatalf("(2) fork the public project as a second non-member: %v", err)
	}
	if carolFork.Project.Visibility != domain.VisibilityPublic {
		t.Fatalf("(2) the second fork's visibility = %q, want the public one it asked for", carolFork.Project.Visibility)
	}

	// ---- (2) write_scientific_state = own_fork_only.
	writeObject := func(account forkAccount, projectID, branchID, statement string) (int, string) {
		t.Helper()
		body := fmt.Sprintf(`{"object_type":"claim","payload":%s}`, mergeMainGateClaim(statement))
		resp := account.client.do(t, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/branches/%s/objects", projectID, branchID), body)
		return resp.StatusCode, readAll(t, resp)
	}
	// (2a) In the actor's own fork: allowed. The write goes through the real
	// HTTP route, the real RSG service and the real policy engine.
	status, body := writeObject(fx.bob, bobFork.Project.ID, bobFork.Branch.ID, "an external contributor's claim")
	if status != http.StatusCreated {
		t.Fatalf("(2) write in the actor's own fork = %d: %s", status, body)
	}
	var written struct {
		ID        string `json:"id"`
		VersionID string `json:"version_id"`
		Title     string `json:"title"`
	}
	if err := json.Unmarshal([]byte(body), &written); err != nil || written.ID == "" {
		t.Fatalf("(2) the accepted write did not answer an object: %v (%s)", err, body)
	}
	// (2b) Upstream: refused. The parent is public and readable — this is a
	// permission refusal, not an existence-hidden 404.
	if status, body := writeObject(fx.bob, parent.ID, main.ID, "an upstream claim"); status != http.StatusForbidden {
		t.Errorf("(2) write on the upstream project as a non-member = %d: %s", status, body)
	}
	// (2c) Somebody else's fork: refused too, and refused by the same cell —
	// Carol's fork is public, so the read gate let the request through and
	// own_fork_only is what answered. Bob holds Carol's project and branch
	// ids; holding them buys nothing.
	if status, body := writeObject(fx.bob, carolFork.Project.ID, carolFork.Branch.ID, "somebody else's claim"); status != http.StatusForbidden {
		t.Errorf("(2) write in another actor's fork = %d: %s", status, body)
	}
	// The two refusals left no objects behind: only the accepted write's
	// claim exists in the database.
	var claims int
	if err := fx.pool.QueryRow(ctx,
		`SELECT count(*) FROM scientific_objects WHERE object_type = 'claim'`).Scan(&claims); err != nil || claims != 1 {
		t.Errorf("(2) the project's claims = %d row(s) (%v), want only the accepted write's", claims, err)
	}

	// ---- (3) open_pr = allow_from_fork.
	pr, err := fx.forkSvc.OpenExternalPR(ctx, bob, forks.OpenPRRequest{
		ProjectID:      parent.ID,
		SourceBranchID: bobFork.Branch.ID,
		TargetBranchID: main.ID,
		Title:          "material evidence from an external fork",
		Body:           "Proposed from the contributor's own fork (docs/04 §2).",
	})
	if err != nil {
		t.Fatalf("(3) open a PR from the actor's own fork: %v", err)
	}
	if pr.ProjectID != parent.ID || pr.SourceBranchID != bobFork.Branch.ID || pr.CreatedBy != bob.ID {
		t.Fatalf("(3) the proposal = %+v, want a %s PR from %s by %s", pr, parent.ID, bobFork.Branch.ID, bob.ID)
	}
	// (3b) Straight from an upstream branch (no fork): refused. The source
	// is a real second branch of the parent — a proposal the platform would
	// happily open for a member — so the ONLY thing standing between this
	// call and a PR row is the open_pr cell. (main→main would be refused by
	// the PR service's own "two different branches" check, which would hide
	// a broken cell behind an unrelated refusal.)
	upstreamLine := fx.createBranch(t, fx.alice, parent.ID, "line-upstream", "")
	if _, err := fx.forkSvc.OpenExternalPR(ctx, bob, forks.OpenPRRequest{
		ProjectID: parent.ID, SourceBranchID: upstreamLine.ID, TargetBranchID: main.ID, Title: "not from a fork",
	}); !errors.Is(err, forks.ErrForbidden) {
		t.Errorf("(3) open a PR from an upstream branch as a non-member = %v, want ErrForbidden", err)
	}
	// (3c) From ANOTHER actor's fork: refused.
	if _, err := fx.forkSvc.OpenExternalPR(ctx, bob, forks.OpenPRRequest{
		ProjectID: parent.ID, SourceBranchID: carolFork.Branch.ID, TargetBranchID: main.ID, Title: "from another fork",
	}); !errors.Is(err, forks.ErrForbidden) {
		t.Errorf("(3) open a PR from another actor's fork = %v, want ErrForbidden", err)
	}
	// (3d) The database holds the same rule for any insert path: a
	// cross-project source that is not the creator's own fork is refused by
	// 00086's pull_request_fork_gate, whoever writes the row.
	mainBaseState := fx.baseStateOfBranch(t, main.ID)
	if _, err := fx.pool.Exec(ctx, `INSERT INTO pull_requests
		(project_id, number, source_branch_id, target_branch_id, base_state_id, proposed_state_id, title, created_by)
		VALUES ($1, 9001, $2, $3, $4, $4, 'smuggled fork', $5)`,
		parent.ID, carolFork.Branch.ID, main.ID, mainBaseState, bob.ID); !isSemanticGateError(err) {
		t.Errorf("(3) a smuggled cross-project PR insert = %v, want the fork gate's P0001 refusal", err)
	}

	// ---- (8) The lineage is recorded and readable, with the vocabulary's
	// relation name.
	lineage, err := fx.forkSvc.Lineage(ctx, projects.Reader{UserID: fx.alice.id, Authenticated: true}, parent.ID)
	if err != nil {
		t.Fatalf("(8) read the lineage: %v", err)
	}
	if len(lineage) != 2 {
		t.Fatalf("(8) lineage = %+v, want the two forks of the parent", lineage)
	}
	var seenBob, seenCarol bool
	for _, row := range lineage {
		if row.RelationType != "forked_from" {
			t.Errorf("(8) lineage relation = %q, want forked_from", row.RelationType)
		}
		if row.ForkedSHA == nil {
			t.Errorf("(8) lineage row %s records no fork point", row.ForkProjectID)
		}
		switch row.ForkedBy {
		case bob.ID:
			seenBob = row.ForkProjectID == bobFork.Project.ID && row.ForkBranchID == bobFork.Branch.ID
		case fx.carol.id:
			seenCarol = row.ForkProjectID == carolFork.Project.ID
		}
	}
	if !seenBob || !seenCarol {
		t.Errorf("(8) the lineage is missing a fork: bob=%v carol=%v", seenBob, seenCarol)
	}
	// The anonymous reader of a public project may read its lineage too.
	if _, err := fx.forkSvc.Lineage(ctx, projects.Reader{}, parent.ID); err != nil {
		t.Errorf("(8) the public project's lineage refused an anonymous reader: %v", err)
	}
	// The private control stays hidden.
	if _, err := fx.forkSvc.Lineage(ctx, projects.Reader{}, fx.privateProject.ID); !errors.Is(err, forks.ErrProjectNotFound) {
		t.Errorf("(8) the private project's lineage = %v, want the existence-hiding refusal", err)
	}

	// ---- (9) A repeated request is a no-op by construction.
	repeat, err := fx.forkSvc.Fork(ctx, bob, forks.ForkRequest{ProjectID: parent.ID})
	if err != nil {
		t.Fatalf("(9) repeat the fork request: %v", err)
	}
	if !repeat.AlreadyForked || repeat.Imported {
		t.Errorf("(9) the repeated request = %+v, want the existing fork and no second import", repeat)
	}
	if repeat.Fork.ForkProjectID != bobFork.Fork.ForkProjectID {
		t.Errorf("(9) the repeated request answered a different fork: %s vs %s",
			repeat.Fork.ForkProjectID, bobFork.Fork.ForkProjectID)
	}
	if lineage, audits, events := fx.forkRecordCounts(t, parent.ID, bob.ID); lineage != 1 || audits != 1 || events != 1 {
		t.Errorf("(9) after the repeat: %d lineage, %d audit, %d event(s), want one of each", lineage, audits, events)
	}
	// The event is the vocabulary's name, and the import recorded the copy
	// once (the delivery key collapses a second import onto the same row).
	if !eventTypeInVocabulary(t, "branch.created") {
		t.Fatal("(9) branch.created is not in specs/events/event-types.yaml")
	}
	var eventsForFork int
	if err := fx.pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events
		WHERE event_type = 'branch.created' AND project_id = $1`, bobFork.Project.ID).Scan(&eventsForFork); err != nil {
		t.Fatalf("(9) count the fork's events: %v", err)
	}
	if eventsForFork != 1 {
		t.Errorf("(9) the fork emitted %d branch.created event(s), want 1", eventsForFork)
	}
	imported, changes := fx.importIngestion(t, forkRepo.id, "refs/heads/"+bobFork.Branch.Name)
	if imported.deliveryID != fmt.Sprintf("fork-import-%d-%s", forkRepo.id, mainSHA) {
		t.Errorf("(9) the import's delivery = %q, want the derived one for the copy", imported.deliveryID)
	}
	if imported.pusher != "post-fork-service" {
		t.Errorf("(9) the import's pusher = %q, want the platform's fork service", imported.pusher)
	}
	if len(changes) != 0 {
		t.Errorf("(9) the main-line copy recorded %+v, want nothing attributable to it", changes)
	}

	// ---- (9b) Two CONCURRENT requests: exactly one wins. The two answers
	// are both legitimate — one creates, the other reads the existing row
	// back or is answered ErrForkSlugTaken because the winner's project row
	// exists while its lineage row does not yet (the two writes are two
	// statements, so that window is real, and the answer states exactly what
	// the record shows instead of claiming a fork). What must hold is the
	// end state: one lineage row, one audit row, one event — and one import.
	type forkOutcome struct {
		result forks.ForkResult
		err    error
	}
	outcomes := make([]forkOutcome, 2)
	var wg sync.WaitGroup
	for i := range outcomes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			result, err := fx.forkSvc.Fork(ctx, fx.dave.actor(), forks.ForkRequest{ProjectID: parent.ID})
			outcomes[i] = forkOutcome{result: result, err: err}
		}(i)
	}
	wg.Wait()
	winners := 0
	for i, outcome := range outcomes {
		switch {
		case outcome.err == nil && !outcome.result.AlreadyForked:
			winners++
		case outcome.err == nil && outcome.result.AlreadyForked:
		case errors.Is(outcome.err, forks.ErrForkSlugTaken):
			// The other request was between its project insert and its
			// lineage insert. Nothing is inconsistent — this request wrote
			// no second project, no lineage row, no audit row and no event —
			// and the retry below settles it.
		default:
			t.Errorf("(9) concurrent request %d = %v (result %+v)", i, outcome.err, outcome.result)
		}
	}
	if winners != 1 {
		t.Errorf("(9) %d concurrent request(s) reported creating the fork, want exactly 1", winners)
	}
	if lineage, audits, eventsCount := fx.forkRecordCounts(t, parent.ID, fx.dave.id); lineage != 1 || audits != 1 || eventsCount != 1 {
		t.Errorf("(9) after the concurrent pair: %d lineage, %d audit, %d event(s), want one of each",
			lineage, audits, eventsCount)
	}
	// A later request resolves to that one fork (and completes its content
	// if the winner's copy is what lost the race).
	settled, err := fx.forkSvc.Fork(ctx, fx.dave.actor(), forks.ForkRequest{ProjectID: parent.ID})
	if err != nil {
		t.Fatalf("(9) settle the concurrent fork: %v", err)
	}
	if !settled.AlreadyForked || settled.Fork.ForkedSHA == nil {
		t.Errorf("(9) the settled fork = %+v, want the existing row with its content imported", settled)
	}
	if lineage, audits, eventsCount := fx.forkRecordCounts(t, parent.ID, fx.dave.id); lineage != 1 || audits != 1 || eventsCount != 1 {
		t.Errorf("(9) after settling: %d lineage, %d audit, %d event(s), want one of each", lineage, audits, eventsCount)
	}
	// The concurrent winner's import is the same single delivery.
	daveRepo := fx.repoOf(t, settled.Fork.ForkProjectID)
	daveRow, _ := fx.importIngestion(t, daveRepo.id, "refs/heads/"+settled.Branch.Name)
	if daveRow.afterSHA != mainSHA || daveRow.projectID != settled.Fork.ForkProjectID {
		t.Errorf("(9) the concurrent fork's import = %+v, want the main line's copy into %s",
			daveRow, settled.Fork.ForkProjectID)
	}

	// ---- (11) The contribution is visible on the project's real
	// pull-request surface: the external proposal is a PR of the upstream
	// project, readable by its owner and by any reader of the public
	// project.
	listResp := fx.alice.client.do(t, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/pull-requests", parent.ID), "")
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("(11) read the project's pull requests = %d: %s", listResp.StatusCode, readAll(t, listResp))
	}
	// The route answers the project's proposals as a list (no envelope — the
	// handler writes the slice itself).
	var listed []struct {
		ID             string `json:"id"`
		Number         int64  `json:"number"`
		Title          string `json:"title"`
		State          string `json:"state"`
		SourceBranchID string `json:"source_branch_id"`
		TargetBranchID string `json:"target_branch_id"`
		CreatedBy      string `json:"created_by"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&listed); err != nil {
		t.Fatalf("(11) decode the pull-request list: %v", err)
	}
	found := false
	for _, row := range listed {
		if row.Number == pr.Number && row.SourceBranchID == bobFork.Branch.ID &&
			row.TargetBranchID == main.ID && row.CreatedBy == bob.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("(11) the external proposal is not on the project's pull-request list: %+v", listed)
	}
	// A non-member of the project reads the same public PR through the
	// detail route — the contribution is public exactly as far as the
	// project is.
	detailResp := fx.mallory.client.do(t, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/pull-requests/%d", parent.ID, pr.Number), "")
	if detailResp.StatusCode != http.StatusOK {
		t.Fatalf("(11) a non-member reading the public PR = %d: %s", detailResp.StatusCode, readAll(t, detailResp))
	}
	var detail struct {
		Number         int64  `json:"number"`
		SourceBranchID string `json:"source_branch_id"`
		CreatedBy      string `json:"created_by"`
	}
	if err := json.NewDecoder(detailResp.Body).Decode(&detail); err != nil {
		t.Fatalf("(11) decode the pull request: %v", err)
	}
	if detail.Number != pr.Number || detail.SourceBranchID != bobFork.Branch.ID || detail.CreatedBy != bob.ID {
		t.Errorf("(11) the detail route answered %+v, want the external proposal", detail)
	}
}

// baseStateOfBranch reads a branch's base state id — the state a PR row
// naming this branch as its target must carry. (states are not tagged with
// a branch id when a branch is created from one; the branch's own
// base_state_id is the record of what it stands on.)
func (fx *externalForkFixture) baseStateOfBranch(t *testing.T, branchID string) string {
	t.Helper()
	var id string
	if err := fx.pool.QueryRow(fx.ctx,
		`SELECT base_state_id::text FROM branches WHERE id = $1`, branchID).Scan(&id); err != nil {
		t.Fatalf("external fork e2e: probe the base state of branch %s: %v", branchID, err)
	}
	return id
}

// TestExternalForkImportDerivesTheSemanticFlag is the criterion-6 pair: the
// imported branch's semantic_state comes from the import's own evidence. An
// unparseable copy is refused by 00042's pull_request_semantic_gate; a
// parseable one reaches its pull request. Only testing the refusal would
// pass just as well if every fork were marked unstructured_changes, which is
// why both halves are here — and criterion 7's clearing step shows the mark
// is a derived state, not a life sentence.
func TestExternalForkImportDerivesTheSemanticFlag(t *testing.T) {
	fx := newExternalForkFixture(t)
	ctx := fx.ctx

	parent := fx.createProject(t, fx.alice, "mof-gas-lines", domain.VisibilityPublic)
	repo := fx.provisionProject(t, parent.ID)
	main := fx.createBranch(t, fx.alice, parent.ID, "main", "")
	bootstrap := fx.seedMain(t, repo.owner, repo.name)

	// ---- The parent's own research line, carried forward by a real push:
	// the pushed commit becomes a project state, and the lines forked from
	// it record that commit as their fork point.
	r1 := fx.createBranch(t, fx.alice, parent.ID, "line-r1", "")
	c1 := fx.pushAndDeliver(t, repo, r1.Name, bootstrap,
		map[string]string{"manifests/mat-r1.json": pushMatDoc()}, nil, "add the material manifest", "extfork-r1")
	if got := fx.semanticFlag(t, ctx, r1.ID); got != "semantic_complete" {
		t.Fatalf("the parent's line after a parseable push = %q, want semantic_complete", got)
	}
	baseState := fx.stateIDOfCommit(t, parent.ID, c1)

	// ---- Two lines forked from that state: one carries an unparseable
	// file, one carries a manifest. Their refs record the fork point the
	// fork import measures a copy against.
	r2 := fx.createBranch(t, fx.alice, parent.ID, "line-r2", baseState)
	c2 := fx.pushAndDeliver(t, repo, r2.Name, c1,
		map[string]string{"data/raw.csv": "raw,unstructured\n1,2\n"}, nil, "attach raw measurements", "extfork-r2")
	r3 := fx.createBranch(t, fx.alice, parent.ID, "line-r3", baseState)
	c3 := fx.pushAndDeliver(t, repo, r3.Name, c1,
		map[string]string{"manifests/mat-r3.json": pushMatDoc()}, nil, "add another manifest", "extfork-r3")
	if got := fx.semanticFlag(t, ctx, r2.ID); got != "unstructured_changes" {
		t.Fatalf("the line with raw data = %q, want unstructured_changes", got)
	}
	if got := fx.semanticFlag(t, ctx, r3.ID); got != "semantic_complete" {
		t.Fatalf("the line with a manifest = %q, want semantic_complete", got)
	}
	for _, line := range []domain.Branch{r2, r3} {
		if got := fx.forkPoint(t, line.ID); got != c1 {
			t.Fatalf("the fork point of %s = %q, want the pushed state %s (without it the import has no baseline)",
				line.Name, got, c1)
		}
	}

	// ---- (6a) The unparseable import: the flag is derived from the copy's
	// own evidence, and the copy cannot open a formal PR.
	bobFork, err := fx.forkSvc.Fork(ctx, fx.bob.actor(), forks.ForkRequest{
		ProjectID: parent.ID, SourceBranchID: r2.ID,
	})
	if err != nil {
		t.Fatalf("(6) fork the line with raw data: %v", err)
	}
	bobRepo := fx.repoOf(t, bobFork.Project.ID)
	if got := fx.semanticFlag(t, ctx, bobFork.Branch.ID); got != "unstructured_changes" {
		t.Fatalf("(6) the imported branch = %q, want the value the import's evidence derives", got)
	}
	row, changes := fx.importIngestion(t, bobRepo.id, "refs/heads/"+bobFork.Branch.Name)
	if row.afterSHA != c2 || row.projectID != bobFork.Project.ID {
		t.Fatalf("(6) the import's delivery = %+v, want the copied commit %s recorded against %s",
			row, c2, bobFork.Project.ID)
	}
	if len(changes) != 1 || changes[0].path != "data/raw.csv" ||
		changes[0].kind != "added" || changes[0].fileKind != "unstructured" {
		t.Fatalf("(6) the import inspected %+v, want the raw data file it could not parse", changes)
	}
	blocked, err := fx.forkSvc.OpenExternalPR(ctx, fx.bob.actor(), forks.OpenPRRequest{
		ProjectID: parent.ID, SourceBranchID: bobFork.Branch.ID, TargetBranchID: main.ID,
		Title: "proposal from an unparseable fork", Body: "docs/16 §4",
	})
	if !isSemanticGateError(err) {
		t.Fatalf("(6) a PR from a branch with unstructured changes = %v (%+v), want the semantic gate's P0001 refusal", err, blocked)
	}
	if !strings.Contains(err.Error(), "unstructured_changes") {
		t.Errorf("(6) the refusal %q does not name the branch's semantic state", err)
	}
	if n := fx.prCount(t, parent.ID); n != 0 {
		t.Fatalf("(6) the refused proposal left %d PR row(s) behind", n)
	}

	// ---- (7) The flag is not a life sentence. A real push that removes the
	// unparseable file is judged by the same rule every push is judged by,
	// and clears it (docs/16 §4: the branch may propose again once the
	// evidence is complete).
	c4 := fx.pushAndDeliver(t, bobRepo, bobFork.Branch.Name, c2, nil,
		[]string{"data/raw.csv"}, "drop the raw file", "extfork-clear")
	if got := fx.semanticFlag(t, ctx, bobFork.Branch.ID); got != "semantic_complete" {
		t.Fatalf("(7) the branch after the repair push = %q, want the flag cleared", got)
	}
	_, changes = fx.importIngestion(t, bobRepo.id, "refs/heads/"+bobFork.Branch.Name)
	_ = changes
	pr, err := fx.forkSvc.OpenExternalPR(ctx, fx.bob.actor(), forks.OpenPRRequest{
		ProjectID: parent.ID, SourceBranchID: bobFork.Branch.ID, TargetBranchID: main.ID,
		Title: "proposal from a repaired fork", Body: "the raw data file was removed",
	})
	if err != nil {
		t.Fatalf("(7) the SAME proposal after the evidence was filled in: %v", err)
	}
	if pr.ProjectID != parent.ID || pr.SourceBranchID != bobFork.Branch.ID {
		t.Errorf("(7) the accepted proposal = %+v", pr)
	}

	// ---- (6b) The parseable import reaches its pull request: the same
	// call, a different copy, the other outcome.
	carolFork, err := fx.forkSvc.Fork(ctx, fx.carol.actor(), forks.ForkRequest{
		ProjectID: parent.ID, SourceBranchID: r3.ID,
	})
	if err != nil {
		t.Fatalf("(6) fork the line with the manifest: %v", err)
	}
	carolRepo := fx.repoOf(t, carolFork.Project.ID)
	if got := fx.semanticFlag(t, ctx, carolFork.Branch.ID); got != "semantic_complete" {
		t.Fatalf("(6) the parseable import = %q, want semantic_complete", got)
	}
	carolRow, carolChanges := fx.importIngestion(t, carolRepo.id, "refs/heads/"+carolFork.Branch.Name)
	if carolRow.afterSHA != c3 {
		t.Errorf("(6) the parseable import copied %s, want %s", carolRow.afterSHA, c3)
	}
	if len(carolChanges) != 1 || carolChanges[0].path != "manifests/mat-r3.json" ||
		carolChanges[0].fileKind != "semantic_manifest" {
		t.Fatalf("(6) the parseable import inspected %+v, want the classified manifest", carolChanges)
	}
	carolPR, err := fx.forkSvc.OpenExternalPR(ctx, fx.carol.actor(), forks.OpenPRRequest{
		ProjectID: parent.ID, SourceBranchID: carolFork.Branch.ID, TargetBranchID: main.ID,
		Title: "proposal from a parseable fork", Body: "docs/16 §4",
	})
	if err != nil {
		t.Fatalf("(6) a PR from a branch whose import parsed must open: %v", err)
	}
	if carolPR.ProjectID != parent.ID || carolPR.SourceBranchID != carolFork.Branch.ID {
		t.Errorf("(6) the parseable import's proposal = %+v", carolPR)
	}
	if n := fx.prCount(t, parent.ID); n != 2 {
		t.Errorf("(6) the parent has %d PR(s), want the repaired fork's and the parseable one's", n)
	}
	// The copy is content, not a pointer: the fork's repository serves the
	// copied commit, and the fork's own record of it is the import's.
	if code, sha := fx.giteaBranch(t, carolRepo.owner, carolRepo.name, carolFork.Branch.Name); code != http.StatusOK || sha != c3 {
		t.Errorf("(6) the parseable fork's ref = %d@%s, want 200 at %s", code, sha, c3)
	}
	if carolRow.pusher != "post-fork-service" {
		t.Errorf("(6) the parseable import's pusher = %q, want the platform's fork service", carolRow.pusher)
	}
	_ = fx.semanticFlag(t, ctx, carolFork.Branch.ID)
	_ = c4
}

// TestExternalForkImportOfAnUnparseableLineWithNoRecordedForkPoint is
// criterion 6's negative half for a copy that has no baseline: at the
// moment of the import the copied branch's flag is never CLEANER than the
// source line's.
//
// A line the platform created without a base state's commit has no recorded
// fork point — the ref syncer's default-branch fallback (refsync.go's
// pre-T0305 path) leaves git_branch_refs.fork_sha NULL — so there is no
// content baseline to diff the copy against. Judged against nothing the
// copy comes out semantic_complete whatever the source line carries: a line
// whose own push put content the platform cannot parse on it cannot open a
// formal PR (00042's pull_request_semantic_gate), while a fork of that very
// line opens one — the same content, from a branch marked clean — and
// merges it. What the copy carries instead is the source line's OWN
// recorded evidence at the copied commit: the records that line's own
// completeness flag is derived from.
func TestExternalForkImportOfAnUnparseableLineWithNoRecordedForkPoint(t *testing.T) {
	fx := newExternalForkFixture(t)
	ctx := fx.ctx

	parent := fx.createProject(t, fx.alice, "mof-gas-nofork", domain.VisibilityPublic)
	repo := fx.provisionProject(t, parent.ID)
	main := fx.createBranch(t, fx.alice, parent.ID, "main", "")
	bootstrap := fx.seedMain(t, repo.owner, repo.name)

	// The line is created BEFORE any push lands, and that is the shape under
	// test: the ref syncer records a fork point from the base state's
	// git_commit_sha, and a branch created while the project has no ingested
	// commit has none to record — it forks from the repository's default
	// branch (refsync.go's pre-T0305 fallback) and git_branch_refs.fork_sha
	// stays NULL. A line created after the push below would carry that state
	// as its fork point and exercise the recorded-baseline route instead.
	dirty := fx.createBranch(t, fx.alice, parent.ID, "line-r1", "")
	if got := fx.forkPoint(t, dirty.ID); got != "" {
		t.Fatalf("the recorded fork point of %s = %q, want none: the scenario is the line with no baseline",
			dirty.Name, got)
	}
	c1 := fx.pushAndDeliver(t, repo, dirty.Name, bootstrap,
		map[string]string{"data/raw.csv": "raw,unstructured\n1,2\n"}, nil, "attach raw measurements", "extfork-nofork-r1")
	if got := fx.semanticFlag(t, ctx, dirty.ID); got != "unstructured_changes" {
		t.Fatalf("the line itself after the unparseable push = %q, want unstructured_changes", got)
	}
	// The line cannot open a formal PR with that content — the comparison
	// the fork below must not escape.
	if _, err := fx.forkSvc.OpenExternalPR(ctx, fx.alice.actor(), forks.OpenPRRequest{
		ProjectID: parent.ID, SourceBranchID: dirty.ID, TargetBranchID: main.ID,
		Title: "proposal from the unparseable line itself", Body: "docs/16 §4",
	}); !isSemanticGateError(err) {
		t.Fatalf("a PR from the unparseable line = %v, want the semantic gate's P0001 refusal", err)
	}

	bobFork, err := fx.forkSvc.Fork(ctx, fx.bob.actor(), forks.ForkRequest{
		ProjectID: parent.ID, SourceBranchID: dirty.ID,
	})
	if err != nil {
		t.Fatalf("fork the line with no fork point: %v", err)
	}
	bobRepo := fx.repoOf(t, bobFork.Project.ID)
	if got := fx.semanticFlag(t, ctx, bobFork.Branch.ID); got != "unstructured_changes" {
		t.Fatalf("the imported branch = %q, want the source line's own judgement: a copy must never be cleaner than its source", got)
	}
	row, changes := fx.importIngestion(t, bobRepo.id, "refs/heads/"+bobFork.Branch.Name)
	if row.afterSHA != c1 {
		t.Errorf("the import copied %s, want the pushed commit %s", row.afterSHA, c1)
	}
	if len(changes) != 1 || changes[0].path != "data/raw.csv" ||
		changes[0].kind != "added" || changes[0].fileKind != "unstructured" {
		t.Fatalf("the import carried %+v, want exactly the source line's own record", changes)
	}
	blocked, err := fx.forkSvc.OpenExternalPR(ctx, fx.bob.actor(), forks.OpenPRRequest{
		ProjectID: parent.ID, SourceBranchID: bobFork.Branch.ID, TargetBranchID: main.ID,
		Title: "proposal from a fork of an unparseable line", Body: "docs/16 §4",
	})
	if !isSemanticGateError(err) {
		t.Fatalf("a PR from the fork = %v (%+v), want the semantic gate's P0001 refusal", err, blocked)
	}
	if n := fx.prCount(t, parent.ID); n != 0 {
		t.Fatalf("the refused proposals left %d PR row(s) behind", n)
	}

	// ---- The carried evidence is ORDINARY evidence, not a flag written by
	// the import: the rule that clears a push-recorded mark clears this one
	// too, because the mark is a projection of the same rows. A real push
	// that removes the unparseable file has a record nearer to the head,
	// and the SAME proposal call then succeeds.
	c3 := fx.pushAndDeliver(t, bobRepo, bobFork.Branch.Name, c1, nil,
		[]string{"data/raw.csv"}, "drop the raw file", "extfork-nofork-clear")
	if got := fx.semanticFlag(t, ctx, bobFork.Branch.ID); got != "semantic_complete" {
		t.Fatalf("the branch after the repair push = %q, want the flag cleared by the nearer record", got)
	}
	repaired, err := fx.forkSvc.OpenExternalPR(ctx, fx.bob.actor(), forks.OpenPRRequest{
		ProjectID: parent.ID, SourceBranchID: bobFork.Branch.ID, TargetBranchID: main.ID,
		Title: "proposal from a repaired fork", Body: "the raw data file was removed",
	})
	if err != nil {
		t.Fatalf("the SAME proposal after the evidence was filled in: %v", err)
	}
	if repaired.ProjectID != parent.ID || repaired.SourceBranchID != bobFork.Branch.ID {
		t.Errorf("the accepted proposal = %+v", repaired)
	}
	_ = c3
}

// TestExternalForkImportOfACleanLineWithNoRecordedForkPoint is criterion
// 6's positive half for that same copy, and it is not optional: the
// negative half alone passes just as well under a blanket
// unstructured_changes mark, and that mark is cleared only by a real push —
// a legitimate fork would be locked for good (docs/16 §4.1 forbids exactly
// that).
//
// The line here has the same shape as the one above: created before any
// push landed, so its ref carries no fork point and the copy has no
// baseline to diff against. Its content is one the platform understands, so
// the copy must be clean — and it is clean precisely because what travels
// is the line's own manifest record and not the copied tree, whose
// platform bootstrap file no push ever wrote and no push can remove.
func TestExternalForkImportOfACleanLineWithNoRecordedForkPoint(t *testing.T) {
	fx := newExternalForkFixture(t)
	ctx := fx.ctx

	parent := fx.createProject(t, fx.alice, "mof-gas-nofork-clean", domain.VisibilityPublic)
	repo := fx.provisionProject(t, parent.ID)
	main := fx.createBranch(t, fx.alice, parent.ID, "main", "")
	bootstrap := fx.seedMain(t, repo.owner, repo.name)

	clean := fx.createBranch(t, fx.alice, parent.ID, "line-r1", "")
	if got := fx.forkPoint(t, clean.ID); got != "" {
		t.Fatalf("the recorded fork point of %s = %q, want none: the scenario is the line with no baseline",
			clean.Name, got)
	}
	c1 := fx.pushAndDeliver(t, repo, clean.Name, bootstrap,
		map[string]string{"manifests/mat-nofork.json": pushMatDoc()}, nil, "add the material manifest", "extfork-nofork-r2")
	if got := fx.semanticFlag(t, ctx, clean.ID); got != "semantic_complete" {
		t.Fatalf("the clean line after the parseable push = %q, want semantic_complete", got)
	}

	carolFork, err := fx.forkSvc.Fork(ctx, fx.carol.actor(), forks.ForkRequest{
		ProjectID: parent.ID, SourceBranchID: clean.ID,
	})
	if err != nil {
		t.Fatalf("fork the clean line with no fork point: %v", err)
	}
	carolRepo := fx.repoOf(t, carolFork.Project.ID)
	if got := fx.semanticFlag(t, ctx, carolFork.Branch.ID); got != "semantic_complete" {
		t.Fatalf("the imported branch = %q, want the clean line's own judgement — the mark must not lock a legitimate fork", got)
	}
	carolRow, carolChanges := fx.importIngestion(t, carolRepo.id, "refs/heads/"+carolFork.Branch.Name)
	if carolRow.afterSHA != c1 {
		t.Errorf("the import copied %s, want the pushed commit %s", carolRow.afterSHA, c1)
	}
	if len(carolChanges) != 1 || carolChanges[0].path != "manifests/mat-nofork.json" ||
		carolChanges[0].fileKind != "semantic_manifest" {
		t.Fatalf("the import carried %+v, want exactly the line's own manifest record", carolChanges)
	}
	carolPR, err := fx.forkSvc.OpenExternalPR(ctx, fx.carol.actor(), forks.OpenPRRequest{
		ProjectID: parent.ID, SourceBranchID: carolFork.Branch.ID, TargetBranchID: main.ID,
		Title: "proposal from a fork of a clean line", Body: "docs/16 §4",
	})
	if err != nil {
		t.Fatalf("a PR from the fork of a clean line must open (the flag is derived evidence, not a life sentence): %v", err)
	}
	if carolPR.ProjectID != parent.ID || carolPR.SourceBranchID != carolFork.Branch.ID {
		t.Errorf("the accepted proposal = %+v", carolPR)
	}
	if n := fx.prCount(t, parent.ID); n != 1 {
		t.Errorf("the parent has %d PR(s), want the clean fork's one", n)
	}
	// The copy is content, not a pointer: the fork's repository serves the
	// copied commit.
	if code, sha := fx.giteaBranch(t, carolRepo.owner, carolRepo.name, carolFork.Branch.Name); code != http.StatusOK || sha != c1 {
		t.Errorf("the clean fork's ref = %d@%s, want 200 at %s", code, sha, c1)
	}
}

// TestExternalForkCannotObtainRestrictedBlobs is criterion 5's negative
// test: a blob attached to the parent's canonical state at access_level
// restricted is unreachable from the fork — and it is unreachable because
// no route serves blob bytes at all (cmd/api/fileshttp registers five
// reads, none of them a blob fetch; internal/assets/page.go:104 records
// the same fact), not because a UI hides it. docs/57_DEMO_UAT.md:7 lists
// "restricted blob download" as a scenario that must fail; docs/23 §
// and CLAUDE.md §9.7 ("Knowledge visibility != Blob accessibility") are
// the rule behind it. The judgment uses internal/rights' vocabulary — the
// access levels the storage layer already has — and adds no tier.
func TestExternalForkCannotObtainRestrictedBlobs(t *testing.T) {
	fx := newExternalForkFixture(t)
	ctx := fx.ctx

	parent := fx.createProject(t, fx.alice, "mof-gas-rights", domain.VisibilityPublic)
	repo := fx.provisionProject(t, parent.ID)
	main := fx.createBranch(t, fx.alice, parent.ID, "main", "")
	fx.seedMain(t, repo.owner, repo.name)

	// ---- The restricted artifact: a blob attached to a version of the
	// parent's canonical line. There is no API that creates one (no blob
	// upload path exists yet), so the fixture writes the canonical rows the
	// storage layer's own vocabulary describes.
	created, err := fx.rsgSvc.CreateObject(ctx, fx.alice.actor(), parent.ID, main.ID, rsg.CreateObjectInput{
		ObjectType: "claim",
		Payload:    json.RawMessage(mergeMainGateClaim("the claim the restricted artifact belongs to")),
	})
	if err != nil {
		t.Fatalf("(5) create the object the artifact belongs to: %v", err)
	}
	const artifactName = "restricted-crystal-structure.pdf"
	blobID, accessLevel := fx.attachRestrictedBlob(t, created.Version.ID, fx.alice.id, artifactName)
	access, ok := rights.ParseDataAccess(accessLevel)
	if !ok || access != rights.DataAccessRestricted {
		t.Fatalf("(5) the attachment's access level %q is not the rights vocabulary's restricted", accessLevel)
	}
	// Control: the platform can see the restricted artifact where it lives,
	// so the negative below is not measuring an empty fixture.
	if n := fx.projectBlobAttachments(t, parent.ID); n != 1 {
		t.Fatalf("(5) the parent holds %d attachment(s), want the restricted one", n)
	}

	// ---- The fork. What travels is the project's git line; the canonical
	// store's blob rows do not travel with it, and there is no route that
	// could fetch their bytes.
	fork, err := fx.forkSvc.Fork(ctx, fx.bob.actor(), forks.ForkRequest{ProjectID: parent.ID})
	if err != nil {
		t.Fatalf("(5) fork the project holding the restricted artifact: %v", err)
	}
	// The fork's repository is resolved (and registered for cleanup)
	// whether or not the content assertions below need its coordinates.
	fx.repoOf(t, fork.Project.ID)

	// ---- (5a) The fork's canonical scope holds nothing to fetch: no
	// object, no version, no attachment of the parent's restricted blob.
	if got := fx.projectBlobAttachments(t, fork.Project.ID); got != 0 {
		t.Errorf("(5) the fork project reaches %d blob attachment(s), want none", got)
	}
	var forkObjects, forkVersions int
	if err := fx.pool.QueryRow(ctx,
		`SELECT count(*) FROM scientific_objects WHERE project_id = $1`, fork.Project.ID).Scan(&forkObjects); err != nil {
		t.Fatalf("(5) count the fork's objects: %v", err)
	}
	if err := fx.pool.QueryRow(ctx,
		`SELECT count(*) FROM scientific_object_versions v
		   JOIN scientific_objects o ON o.id = v.object_id
		  WHERE o.project_id = $1`, fork.Project.ID).Scan(&forkVersions); err != nil {
		t.Fatalf("(5) count the fork's object versions: %v", err)
	}
	if forkObjects != 0 || forkVersions != 0 {
		t.Errorf("(5) the fork holds %d object(s) and %d version(s), want none", forkObjects, forkVersions)
	}

	// ---- (5b) The fork's file surface — the product's only byte-serving
	// reader — lists the copied line and does not serve the restricted
	// artifact, because it is not content of the repository at all.
	treeResp := fx.bob.client.do(t, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/files/tree", fork.Project.ID), "")
	if treeResp.StatusCode != http.StatusOK {
		t.Fatalf("(5) the fork's file tree = %d: %s", treeResp.StatusCode, readAll(t, treeResp))
	}
	tree := readAll(t, treeResp)
	if !strings.Contains(tree, "README.md") {
		t.Fatalf("(5) the fork's tree does not list the copied bootstrap file — the fixture is not measuring the fork: %s", tree)
	}
	if strings.Contains(tree, artifactName) || strings.Contains(tree, blobID) {
		t.Errorf("(5) the restricted artifact appears in the fork's file tree: %s", tree)
	}
	rawResp := fx.bob.client.do(t, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/files/raw?path=%s", fork.Project.ID, artifactName), "")
	if rawResp.StatusCode != http.StatusNotFound {
		t.Errorf("(5) fetching the restricted artifact through the fork's file surface = %d, want 404", rawResp.StatusCode)
	}
	// The same surface DOES serve a file that is really there (the positive
	// control for the two probes above).
	okRaw := fx.bob.client.do(t, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/files/raw?path=README.md", fork.Project.ID), "")
	if okRaw.StatusCode != http.StatusOK {
		t.Errorf("(5) fetching the fork's README = %d: %s", okRaw.StatusCode, readAll(t, okRaw))
	}
	// And the artifact is not repository content ANYWHERE — the parent's own
	// surface refuses it too. A blob's bytes are behind the blob store's
	// access level, not behind the repository the fork copies; the fork does
	// not carry it because no repository does.
	parentRaw := fx.alice.client.do(t, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/files/raw?path=%s", parent.ID, artifactName), "")
	if parentRaw.StatusCode != http.StatusNotFound {
		t.Errorf("(5) the parent's own file surface served the artifact = %d, want 404 (it is a blob, not a file)",
			parentRaw.StatusCode)
	}

	// ---- (5c) And there is no blob download route to reach it through —
	// the claim is "there is no way", not "a page hides it". Every shape a
	// blob fetch would take is probed on the assembled product surface (the
	// guarded mux the production wiring populated); the mux answers 404 and
	// the body never carries the artifact. The control at the end shows the
	// instrument distinguishes a route that exists.
	probes := []string{
		"/api/v1/blobs/" + blobID,
		"/api/v1/blobs/" + blobID + "/content",
		"/api/v1/blobs/" + blobID + "/download",
		fmt.Sprintf("/api/v1/projects/%s/blobs/%s", parent.ID, blobID),
		fmt.Sprintf("/api/v1/projects/%s/blobs/%s", fork.Project.ID, blobID),
		fmt.Sprintf("/api/v1/projects/%s/objects/%s/content", fork.Project.ID, created.Object.ID),
		fmt.Sprintf("/api/v1/projects/%s/blobs/%s/content", fork.Project.ID, blobID),
	}
	for _, path := range probes {
		resp := fx.bob.client.do(t, http.MethodGet, path, "")
		body := readAll(t, resp)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("(5) %s = %d (body %q), want 404 — a route serving blob bytes must not exist", path, resp.StatusCode, body)
		}
		if strings.Contains(body, blobID) || strings.Contains(body, artifactName) {
			t.Errorf("(5) %s answered the artifact's identity: %q", path, body)
		}
	}
	// The instrument: a path the product DOES route is not answered 404 by
	// the same probe, so the 404s above are the absence of a route rather
	// than a probe that can never see anything.
	control := fx.bob.client.do(t, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/files/tree", fork.Project.ID), "")
	if control.StatusCode == http.StatusNotFound {
		t.Fatal("(5) the probe answers 404 for a route that exists — the negative above proves nothing")
	}
	// The mux itself, for the shapes outside any registered subtree: no
	// pattern matches them at all.
	for _, path := range []string{"/api/v1/blobs/" + blobID, "/api/v1/blobs/" + blobID + "/content"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if _, pattern := fx.mux.Handler(req); pattern != "" {
			t.Errorf("(5) %s is served by pattern %q", path, pattern)
		}
	}
}

// projectBlobAttachments counts the blob attachments reachable from a
// project: its objects' versions' attachments. Zero is the negative's
// claim; one is the fixture's control.
func (fx *externalForkFixture) projectBlobAttachments(t *testing.T, projectID string) int {
	t.Helper()
	var n int
	if err := fx.pool.QueryRow(fx.ctx,
		`SELECT count(*) FROM blob_attachments a
		   JOIN scientific_object_versions v ON v.id = a.scientific_object_version_id
		   JOIN scientific_objects o ON o.id = v.object_id
		  WHERE o.project_id = $1`, projectID).Scan(&n); err != nil {
		t.Fatalf("external fork e2e: count the project's blob attachments: %v", err)
	}
	return n
}

// attachRestrictedBlob writes one blob and one restricted attachment for it
// (the canonical shape blob_attachments already has) and returns their ids.
func (fx *externalForkFixture) attachRestrictedBlob(t *testing.T, versionID, actorID, name string) (blobID, accessLevel string) {
	t.Helper()
	if err := fx.pool.QueryRow(fx.ctx, `INSERT INTO blobs
		(content_hash, size_bytes, media_type, storage_key, integrity_state, created_by)
		VALUES ($1, 4096, 'application/pdf', $2, 'pending', $3) RETURNING id::text`,
		"sha256:extfork-restricted", "s3://post-blobs/restricted/"+name, actorID).Scan(&blobID); err != nil {
		t.Fatalf("external fork e2e: seed the restricted blob: %v", err)
	}
	if err := fx.pool.QueryRow(fx.ctx, `INSERT INTO blob_attachments
		(blob_id, scientific_object_version_id, attachment_role, access_level, state_id)
		VALUES ($1, $2, 'raw_data', 'restricted',
		        (SELECT state_id FROM scientific_object_versions WHERE id = $2))
		RETURNING access_level`,
		blobID, versionID).Scan(&accessLevel); err != nil {
		t.Fatalf("external fork e2e: attach the restricted blob: %v", err)
	}
	return blobID, accessLevel
}

// TestExternalForkClaimTransactionIsAtomic is criterion 10: the lineage row,
// its audit row and its outbox event are one transaction's facts. The audit
// row is written after the lineage row and before the event, so an audit
// insert that cannot succeed — the entry names a project with no projects
// row, and audit_log.project_id is a foreign key — is a deterministic late
// failure (the shape tests/integration/freeze_main_e2e_test.go:579-594 uses,
// moved one level down: an actor with no users row would fail the lineage
// insert itself, since project_forks.forked_by is a foreign key too).
// Nothing is left behind. The same claim with a real audit project is then
// accepted, which is what shows the failure was the named project and not
// the fixture.
func TestExternalForkClaimTransactionIsAtomic(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), externalForkTaskID)

	// A real user, a real project pair and a real branch pair: everything
	// the claim needs except the actor its audit entry names.
	owner, err := persistence.NewCredentialStore(pool).CreateWithPassword(
		ctx, "extfork-atomic@example.com", "hash", "extfork-atomic", "External Fork Atomic")
	if err != nil {
		t.Fatalf("(10) seed the user: %v", err)
	}
	projectStore := persistence.NewProjectStore(pool)
	branchSvc := branches.NewService(persistence.NewBranchStore(pool))
	stateSvc := states.NewService(persistence.NewStateStore(pool), newCommitGuard(t))
	mkProject := func(slug string) domain.Project {
		t.Helper()
		project, _, err := projectStore.CreateProject(ctx, domain.Project{
			Slug: slug, Name: slug, Purpose: "T0804 atomicity", Visibility: domain.VisibilityPrivate,
			ProvisionStatus: domain.ProvisionPending,
		}, owner.ID)
		if err != nil {
			t.Fatalf("(10) create project %s: %v", slug, err)
		}
		return project
	}
	mkBranch := func(project domain.Project, name string) domain.Branch {
		t.Helper()
		genesis, err := stateSvc.CreateInitialState(ctx, states.CreateInitialStateParams{
			ProjectID: project.ID, ManifestVersion: "v1",
		})
		if err != nil {
			t.Fatalf("(10) create the genesis state of %s: %v", project.ID, err)
		}
		branch, err := branchSvc.Create(ctx, branches.CreateBranchParams{
			ProjectID:   project.ID,
			Name:        name,
			Visibility:  domain.BranchVisibilityPrivate,
			BaseStateID: genesis.ID,
			CreatedBy:   owner.ID,
		})
		if err != nil {
			t.Fatalf("(10) create branch %s: %v", name, err)
		}
		return branch
	}
	parent := mkProject("extfork-atomic-parent")
	forkProject := mkProject("extfork-atomic-fork")
	source := mkBranch(parent, "main")
	forkBranch := mkBranch(forkProject, "fork/main")

	store := persistence.NewForkStore(pool)
	// The ghost is a PROJECT, not an actor: project_forks.forked_by is a
	// foreign key to users too, so an actor with no users row fails the
	// lineage insert itself — before the transaction boundary this test is
	// about. The audit entry instead names a well-formed project id with no
	// projects row, and the audit statement is the one that runs AFTER the
	// lineage insert (ClaimFork: insert → visibility → audit → event). The
	// failure is therefore a late one, and what it leaves behind is the
	// question.
	const ghostProject = "00000000-0000-4000-8000-0000000000ff"
	claim := func(auditProjectID string) (forks.Fork, bool, error) {
		t.Helper()
		return store.ClaimFork(ctx, forks.ClaimRequest{
			ForkProjectID:   forkProject.ID,
			ParentProjectID: parent.ID,
			ActorID:         owner.ID,
			SourceBranchID:  source.ID,
			ForkBranchID:    forkBranch.ID,
			Audit: domain.AuditEntry{
				ActorID:   owner.ID,
				Action:    forks.ActionProjectForked,
				TargetRef: "project:" + forkProject.ID,
				ProjectID: auditProjectID,
				AfterSummary: map[string]any{
					"fork_project_id":   forkProject.ID,
					"parent_project_id": parent.ID,
				},
				Metadata: map[string]any{"relation_type": "forked_from"},
			},
		})
	}

	_, inserted, err := claim(ghostProject)
	if err == nil || inserted {
		t.Fatalf("(10) the claim whose audit cannot be written = %v (inserted %v), want a failure", err, inserted)
	}
	// The failure must be the AUDIT statement's foreign key on the table it
	// names — not the lineage insert (which used two real projects) and not
	// anything earlier. A failure anywhere else would not test the
	// transaction boundary at all.
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" ||
		pgErr.TableName != "audit_log" || pgErr.ConstraintName != "audit_log_project_id_fkey" {
		t.Fatalf("(10) the claim failed on %v, want audit_log.project_id's foreign key", err)
	}
	// The three facts the claim is responsible for. The audit count is
	// scoped to the fork action — signing a user up audits too, and this
	// test is about what the CLAIM writes — while the event count is not
	// scoped at all: the claim is the only thing in this fixture that may
	// emit one, so any row here would be its doing.
	counts := func() (lineage, audits, outbox int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM project_forks`).Scan(&lineage); err != nil {
			t.Fatalf("(10) count lineage rows: %v", err)
		}
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE action = $1`, forks.ActionProjectForked).Scan(&audits); err != nil {
			t.Fatalf("(10) count the fork's audit rows: %v", err)
		}
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events`).Scan(&outbox); err != nil {
			t.Fatalf("(10) count events: %v", err)
		}
		return lineage, audits, outbox
	}
	if lineage, audits, outbox := counts(); lineage != 0 || audits != 0 || outbox != 0 {
		t.Fatalf("(10) the failed claim left %d lineage row(s), %d audit row(s), %d event(s), want none",
			lineage, audits, outbox)
	}

	// The control: the same claim, identical but for the audit entry's
	// project, is accepted and does leave all three facts.
	fork, inserted, err := claim(parent.ID)
	if err != nil || !inserted {
		t.Fatalf("(10) the same claim with a real audit project = %v (inserted %v), want it accepted", err, inserted)
	}
	if fork.ForkProjectID != forkProject.ID || fork.RelationType != "forked_from" || fork.ForkedSHA != nil {
		t.Fatalf("(10) the claimed fork = %+v, want the fork project with its relation and no fork point yet", fork)
	}
	if lineage, audits, outbox := counts(); lineage != 1 || audits != 1 || outbox != 1 {
		t.Fatalf("(10) the accepted claim left %d lineage row(s), %d audit row(s), %d event(s), want one of each",
			lineage, audits, outbox)
	}
	// The event is the vocabulary's name and its visibility is fail-closed:
	// both projects are private, so the event is private too.
	var eventType, visibility string
	if err := pool.QueryRow(ctx,
		`SELECT event_type, visibility FROM outbox_events`).Scan(&eventType, &visibility); err != nil {
		t.Fatalf("(10) read the fork's event: %v", err)
	}
	if eventType != "branch.created" {
		t.Errorf("(10) the fork's event = %q, want the vocabulary's branch.created", eventType)
	}
	if !eventTypeInVocabulary(t, eventType) {
		t.Errorf("(10) %q is not in specs/events/event-types.yaml", eventType)
	}
	if visibility != "private" {
		t.Errorf("(10) the event's visibility = %q, want private (fail closed for two private projects)", visibility)
	}
}

// TestExternalForkLineageClaimIsACompareAndSwap is criterion 9's CAS clause,
// asserted at the layer that owns it: forks.StorePort.ClaimFork.
//
// Through the service, a repeated or concurrent request never reaches the
// claim's conflict arm: the fork project's derived name is unique, so the
// second request's project insert loses on it and the service resolves the
// loss by the record — the pair's lineage row if there is one, the holder's
// creator if there is not, and ErrForkSlugTaken between the winner's two
// writes, when the holder is a project the pair's own actor created. Delete
// the arm — ON CONFLICT (parent_project_id, forked_by) DO NOTHING — and
// TestExternalForkEndToEnd stays green; measured, not assumed (T0804's
// mutation battery, MB3). The contract the service relies on is therefore
// asserted directly here, in the shape main_freeze_store's CAS has: the
// second claim for the same pair must answer the WINNER'S row, write
// nothing at all, and report inserted=false.
func TestExternalForkLineageClaimIsACompareAndSwap(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), externalForkTaskID)

	owner, err := persistence.NewCredentialStore(pool).CreateWithPassword(
		ctx, "extfork-cas@example.com", "hash", "extfork-cas", "External Fork CAS")
	if err != nil {
		t.Fatalf("(9) seed the user: %v", err)
	}
	projectStore := persistence.NewProjectStore(pool)
	branchSvc := branches.NewService(persistence.NewBranchStore(pool))
	stateSvc := states.NewService(persistence.NewStateStore(pool), newCommitGuard(t))
	mkProject := func(slug string) domain.Project {
		t.Helper()
		project, _, err := projectStore.CreateProject(ctx, domain.Project{
			Slug: slug, Name: slug, Purpose: "T0804 claim CAS", Visibility: domain.VisibilityPrivate,
			ProvisionStatus: domain.ProvisionPending,
		}, owner.ID)
		if err != nil {
			t.Fatalf("(9) create project %s: %v", slug, err)
		}
		return project
	}
	mkBranch := func(project domain.Project, name string) domain.Branch {
		t.Helper()
		genesis, err := stateSvc.CreateInitialState(ctx, states.CreateInitialStateParams{
			ProjectID: project.ID, ManifestVersion: "v1",
		})
		if err != nil {
			t.Fatalf("(9) create the genesis state of %s: %v", project.ID, err)
		}
		branch, err := branchSvc.Create(ctx, branches.CreateBranchParams{
			ProjectID:   project.ID,
			Name:        name,
			Visibility:  domain.BranchVisibilityPrivate,
			BaseStateID: genesis.ID,
			CreatedBy:   owner.ID,
		})
		if err != nil {
			t.Fatalf("(9) create branch %s: %v", name, err)
		}
		return branch
	}
	parent := mkProject("extfork-cas-parent")
	firstFork := mkProject("extfork-cas-first")
	secondFork := mkProject("extfork-cas-second")
	source := mkBranch(parent, "main")
	firstBranch := mkBranch(firstFork, "main")
	secondBranch := mkBranch(secondFork, "main")

	store := persistence.NewForkStore(pool)
	claim := func(forkProject domain.Project, forkBranch domain.Branch) (forks.Fork, bool, error) {
		t.Helper()
		return store.ClaimFork(ctx, forks.ClaimRequest{
			ForkProjectID:   forkProject.ID,
			ParentProjectID: parent.ID,
			ActorID:         owner.ID,
			SourceBranchID:  source.ID,
			ForkBranchID:    forkBranch.ID,
			Audit: domain.AuditEntry{
				ActorID:   owner.ID,
				Action:    forks.ActionProjectForked,
				TargetRef: "project:" + forkProject.ID,
				ProjectID: parent.ID,
				AfterSummary: map[string]any{
					"fork_project_id":   forkProject.ID,
					"parent_project_id": parent.ID,
				},
				Metadata: map[string]any{"relation_type": "forked_from"},
			},
		})
	}
	// The end state every claim below is judged against: one lineage row,
	// one audit row (this pair's) and one branch.created event, counted
	// across BOTH candidate fork projects — a losing claim that wrote an
	// event for its own project would be invisible to a count scoped to the
	// winner.
	counts := func() (lineage, audits, events int) {
		t.Helper()
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM project_forks WHERE parent_project_id = $1 AND forked_by = $2`,
			parent.ID, owner.ID).Scan(&lineage); err != nil {
			t.Fatalf("(9) count lineage rows: %v", err)
		}
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE project_id = $1 AND actor_id = $2 AND action = $3`,
			parent.ID, owner.ID, forks.ActionProjectForked).Scan(&audits); err != nil {
			t.Fatalf("(9) count audit rows: %v", err)
		}
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM outbox_events
			  WHERE event_type = 'branch.created' AND project_id = ANY($1::uuid[])`,
			[]string{firstFork.ID, secondFork.ID}).Scan(&events); err != nil {
			t.Fatalf("(9) count events: %v", err)
		}
		return lineage, audits, events
	}

	won, inserted, err := claim(firstFork, firstBranch)
	if err != nil || !inserted {
		t.Fatalf("(9) the first claim = %+v, inserted %v, %v — want the winner", won, inserted, err)
	}
	if won.ForkProjectID != firstFork.ID || won.ForkBranchID != firstBranch.ID {
		t.Fatalf("(9) the winner = %+v, want the project and branch it claimed", won)
	}
	if lineage, audits, events := counts(); lineage != 1 || audits != 1 || events != 1 {
		t.Fatalf("(9) after the first claim: %d lineage, %d audit, %d event(s), want one of each",
			lineage, audits, events)
	}

	// The claim that must lose. It names a DIFFERENT fork project — the
	// shape of a second request that got as far as its own project — and
	// the pair is the same, so the unique key decides.
	lost, inserted, err := claim(secondFork, secondBranch)
	if err != nil {
		t.Fatalf("(9) the second claim for the same pair returned an error instead of the winner's row: %v", err)
	}
	if inserted {
		t.Fatalf("(9) the second claim reported creating the lineage: %+v", lost)
	}
	if lost.ForkProjectID != firstFork.ID || lost.ForkBranchID != firstBranch.ID || lost.ForkedSHA != nil {
		t.Errorf("(9) the losing claim answered %+v, want the winner's row (%s, %s) and no fork point",
			lost, firstFork.ID, firstBranch.ID)
	}
	if lineage, audits, events := counts(); lineage != 1 || audits != 1 || events != 1 {
		t.Errorf("(9) after the losing claim: %d lineage, %d audit, %d event(s), want one of each — a claim that lost writes nothing",
			lineage, audits, events)
	}
	// And the losing claim's project has no lineage of its own: the failing
	// claim did not quietly create a second fork.
	if fork, ok, err := store.ForkOfProject(ctx, secondFork.ID); err != nil || ok {
		t.Errorf("(9) the losing claim's project has a lineage row (%+v, ok=%v, err=%v), want none", fork, ok, err)
	}
	// A third claim is the same no-op as the second: the arm is stable, not
	// a one-shot guard that degrades after the first conflict.
	if _, inserted, err = claim(secondFork, secondBranch); err != nil || inserted {
		t.Errorf("(9) the third claim inserted=%v, err=%v — want the same no-op", inserted, err)
	}
	if lineage, audits, events := counts(); lineage != 1 || audits != 1 || events != 1 {
		t.Errorf("(9) after the third claim: %d lineage, %d audit, %d event(s), want one of each",
			lineage, audits, events)
	}
	// The lineage is readable through the branch, which is the read
	// allow_from_fork resolves (criterion 8's shape, on the row this test
	// wrote).
	fork, ok, err := store.ForkOfBranch(ctx, firstBranch.ID)
	if err != nil || !ok {
		t.Fatalf("(9) read the lineage through the branch: ok=%v, err=%v", ok, err)
	}
	if fork.ParentProjectID != parent.ID || fork.ForkedBy != owner.ID || fork.RelationType != "forked_from" {
		t.Errorf("(9) the lineage read through the branch = %+v, want the parent, the actor and 'forked_from'", fork)
	}
}

// derivedForkSlug is the name the fork service derives for (parent, actor),
// as the test side must see it to occupy that name: the parent's slug, a
// dash, and the actor's identity — their handle, or their id compacted when
// they carry none (which is what these fixture actors pass: the service is
// called with domain.User{ID}).
//
// It is deliberately a re-derivation and not a call into the service: the
// point of the tests below is that SOMEBODY ELSE can compute this name from
// public facts. If this re-derivation ever drifted from the service's, the
// fork would simply succeed at the derived name and the tests would fail on
// the very assertion that says it did not.
func derivedForkSlug(t *testing.T, parent domain.Project, actor domain.User) string {
	t.Helper()
	handle := strings.ToLower(strings.TrimSpace(actor.Handle))
	if handle == "" {
		handle = strings.ReplaceAll(actor.ID, "-", "")
	}
	slug := parent.Slug + "-" + handle
	if !domain.ValidProjectSlug(slug) {
		t.Fatalf("external fork e2e: the fixture's derived slug %q is not a valid project slug — this test needs the unshortened shape", slug)
	}
	return slug
}

// TestExternalForkSurvivesATakenDerivedName is F1 end to end, over the real
// database: the name a fork derives is computable from public facts, so a
// third party — or the actor's own earlier project — can be holding it when
// the fork arrives, and personal project slugs are globally unique (00019)
// with nothing ever deleted. A fork that only tried its derived name would
// be permanently deadlocked by a name its actor cannot free; the flow must
// instead (a) still succeed when the holder is somebody else's project and
// (b) answer accurately, without inventing a fork, when the holder is the
// actor's own.
func TestExternalForkSurvivesATakenDerivedName(t *testing.T) {
	fx := newExternalForkFixture(t)
	ctx := fx.ctx

	parent := fx.createProject(t, fx.alice, "extfork-name", domain.VisibilityPublic)
	repo := fx.provisionProject(t, parent.ID)
	fx.createBranch(t, fx.alice, parent.ID, "main", "")
	mainSHA := fx.seedMain(t, repo.owner, repo.name)

	// ---- (F1a) Another account holds the name Bob's fork derives.
	bobName := derivedForkSlug(t, parent, fx.bob.actor())
	squatter := fx.createProject(t, fx.mallory, bobName, domain.VisibilityPrivate)

	bobFork, err := fx.forkSvc.Fork(ctx, fx.bob.actor(), forks.ForkRequest{ProjectID: parent.ID})
	if err != nil {
		t.Fatalf("(F1a) fork with the derived name held by another account: %v", err)
	}
	if bobFork.AlreadyForked || !bobFork.Imported {
		t.Fatalf("(F1a) the fork past a taken name = %+v, want a fresh fork whose content was imported", bobFork)
	}
	if bobFork.Project.Slug == bobName {
		t.Fatalf("(F1a) the fork project took the squatted name %q", bobName)
	}
	if !strings.HasPrefix(bobFork.Project.Slug, bobName) || !strings.HasSuffix(bobFork.Project.Slug, "-2") {
		t.Errorf("(F1a) the fork's slug = %q, want the pair's reserved name (the derived %q with the reserve mark)",
			bobFork.Project.Slug, bobName)
	}
	if !domain.ValidProjectSlug(bobFork.Project.Slug) {
		t.Errorf("(F1a) the fork's slug %q is not a valid project slug", bobFork.Project.Slug)
	}
	if bobFork.Project.ID == squatter.ID {
		t.Fatal("(F1a) the fork IS the squatter's project — the fork must be the actor's own new project")
	}
	// The squatter is untouched: still exactly one personal project under
	// that slug, still its creator's.
	var holders int
	var holderCreator string
	if err := fx.pool.QueryRow(ctx,
		`SELECT count(*), min(created_by::text) FROM projects WHERE organization_id IS NULL AND slug = $1`,
		bobName).Scan(&holders, &holderCreator); err != nil {
		t.Fatalf("(F1a) read the holder of %s: %v", bobName, err)
	}
	if holders != 1 || holderCreator != fx.mallory.id {
		t.Errorf("(F1a) the name %s is held by %d project(s) created by %q, want the squatter's one project by %s",
			bobName, holders, holderCreator, fx.mallory.id)
	}
	// The fork is one fork: one lineage row, one audit row, one event, and
	// the content really arrived in the fork's own repository.
	if lineage, audits, events := fx.forkRecordCounts(t, parent.ID, fx.bob.id); lineage != 1 || audits != 1 || events != 1 {
		t.Errorf("(F1a) after forking past a taken name: %d lineage, %d audit, %d event(s), want one of each",
			lineage, audits, events)
	}
	bobRepo := fx.repoOf(t, bobFork.Project.ID)
	if code, sha := fx.giteaBranch(t, bobRepo.owner, bobRepo.name, bobFork.Branch.Name); code != http.StatusOK || sha != mainSHA {
		t.Errorf("(F1a) the fork's ref = %d@%s, want 200 at the copied commit %s", code, sha, mainSHA)
	}

	// ---- (F1a') The repeated request: the derived name is STILL taken
	// (nothing is deleted) and the pair now has a fork — the answer is that
	// fork, and nothing new is written.
	bobRepeat, err := fx.forkSvc.Fork(ctx, fx.bob.actor(), forks.ForkRequest{ProjectID: parent.ID})
	if err != nil {
		t.Fatalf("(F1a) repeat the fork: %v", err)
	}
	if !bobRepeat.AlreadyForked || bobRepeat.Fork.ForkProjectID != bobFork.Fork.ForkProjectID {
		t.Errorf("(F1a) the repeat = %+v, want the fork %s it already has",
			bobRepeat, bobFork.Fork.ForkProjectID)
	}
	if lineage, audits, events := fx.forkRecordCounts(t, parent.ID, fx.bob.id); lineage != 1 || audits != 1 || events != 1 {
		t.Errorf("(F1a) after the repeat: %d lineage, %d audit, %d event(s), want one of each",
			lineage, audits, events)
	}
	// Two projects carry the parent's name: the squatter's and Bob's one.
	var children int
	if err := fx.pool.QueryRow(ctx,
		`SELECT count(*) FROM projects WHERE organization_id IS NULL AND slug LIKE $1`,
		parent.Slug+"-%").Scan(&children); err != nil {
		t.Fatalf("(F1a) count the parent's namesakes: %v", err)
	}
	if children != 2 {
		t.Errorf("(F1a) %d project(s) carry the parent's name, want the squatter's and the fork's (2)", children)
	}

	// ---- (F1b) Carol holds the name her OWN fork derives: a project of
	// hers, created before she ever asked to fork. This is the case where a
	// fork project between its two writes and a project of hers look the
	// same in the record, so the fork neither escalates (which could leave
	// two fork projects for one pair) nor claims a fork exists.
	carolName := derivedForkSlug(t, parent, fx.carol.actor())
	carolProject := fx.createProject(t, fx.carol, carolName, domain.VisibilityPrivate)
	_, err = fx.forkSvc.Fork(ctx, fx.carol.actor(), forks.ForkRequest{ProjectID: parent.ID})
	if !errors.Is(err, forks.ErrForkSlugTaken) {
		t.Fatalf("(F1b) fork with the actor's own project holding the name = %v, want ErrForkSlugTaken", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, carolName) {
		t.Errorf("(F1b) the refusal does not name what it lost the name to: %q", msg)
	}
	if strings.Contains(msg, "already being created") {
		t.Errorf("(F1b) the refusal claims a fork is being created, which no row shows: %q", msg)
	}
	// Nothing was written: no lineage row, no audit row, no event — and no
	// second project.
	if lineage, audits, events := fx.forkRecordCounts(t, parent.ID, fx.carol.id); lineage != 0 || audits != 0 || events != 0 {
		t.Errorf("(F1b) the refusal left %d lineage, %d audit, %d event(s), want none",
			lineage, audits, events)
	}
	var carolHolders int
	var carolHolderID string
	if err := fx.pool.QueryRow(ctx,
		`SELECT count(*), min(id::text) FROM projects WHERE organization_id IS NULL AND slug = $1`,
		carolName).Scan(&carolHolders, &carolHolderID); err != nil {
		t.Fatalf("(F1b) read the holder of %s: %v", carolName, err)
	}
	if carolHolders != 1 || carolHolderID != carolProject.ID {
		t.Errorf("(F1b) the name %s is held by %d project(s), want only Carol's own", carolName, carolHolders)
	}
	// The answer is stable: the same request again changes nothing.
	if _, err := fx.forkSvc.Fork(ctx, fx.carol.actor(), forks.ForkRequest{ProjectID: parent.ID}); !errors.Is(err, forks.ErrForkSlugTaken) {
		t.Errorf("(F1b) the repeated refusal = %v, want the same ErrForkSlugTaken", err)
	}
	if lineage, audits, events := fx.forkRecordCounts(t, parent.ID, fx.carol.id); lineage != 0 || audits != 0 || events != 0 {
		t.Errorf("(F1b) after the repeated refusal: %d lineage, %d audit, %d event(s), want none",
			lineage, audits, events)
	}
}

// TestExternalForkEscalationStaysOneForkUnderConcurrency is the property the
// F1 fix rests on, measured rather than argued: when the derived name is
// taken, BOTH concurrent requests of one pair take the escalation path — and
// because the reserved name is derived from the pair alone, they derive the
// SAME one, so the unique index arbitrates exactly as it does on the derived
// name. The pair ends with one fork project, one lineage row, one audit row
// and one event; a fallback name that varied per call would leave two
// projects, one of them a row that can never be deleted.
func TestExternalForkEscalationStaysOneForkUnderConcurrency(t *testing.T) {
	fx := newExternalForkFixture(t)
	ctx := fx.ctx

	parent := fx.createProject(t, fx.alice, "extfork-race", domain.VisibilityPublic)
	repo := fx.provisionProject(t, parent.ID)
	fx.createBranch(t, fx.alice, parent.ID, "main", "")
	fx.seedMain(t, repo.owner, repo.name)

	daveName := derivedForkSlug(t, parent, fx.dave.actor())
	fx.createProject(t, fx.mallory, daveName, domain.VisibilityPrivate)

	type outcome struct {
		result forks.ForkResult
		err    error
	}
	outcomes := make([]outcome, 2)
	var wg sync.WaitGroup
	for i := range outcomes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			result, err := fx.forkSvc.Fork(ctx, fx.dave.actor(), forks.ForkRequest{ProjectID: parent.ID})
			outcomes[i] = outcome{result: result, err: err}
		}(i)
	}
	wg.Wait()
	winners := 0
	for i, out := range outcomes {
		switch {
		case out.err == nil && !out.result.AlreadyForked:
			winners++
		case out.err == nil && out.result.AlreadyForked:
		case errors.Is(out.err, forks.ErrForkSlugTaken):
			// The other request won the reserved name (or had already
			// claimed the lineage). Nothing is inconsistent: this request
			// wrote no second project, no lineage row, no audit row and no
			// event.
		default:
			t.Errorf("(F1) concurrent escalated request %d = %v (result %+v)", i, out.err, out.result)
		}
	}
	if winners != 1 {
		t.Errorf("(F1) %d concurrent request(s) reported creating the fork, want exactly 1", winners)
	}
	if lineage, audits, events := fx.forkRecordCounts(t, parent.ID, fx.dave.id); lineage != 1 || audits != 1 || events != 1 {
		t.Errorf("(F1) after the concurrent pair: %d lineage, %d audit, %d event(s), want one of each",
			lineage, audits, events)
	}
	// One fork project and the squatter's, and nothing else: pre-fix this is
	// where a per-call fallback name would show up as a second project.
	var children int
	if err := fx.pool.QueryRow(ctx,
		`SELECT count(*) FROM projects WHERE organization_id IS NULL AND slug LIKE $1`,
		parent.Slug+"-%").Scan(&children); err != nil {
		t.Fatalf("(F1) count the parent's namesakes: %v", err)
	}
	if children != 2 {
		t.Errorf("(F1) %d project(s) carry the parent's name, want the squatter's and the ONE fork project (2)", children)
	}
	// A later request settles on that one fork, with its content imported and
	// its fork point recorded.
	settled, err := fx.forkSvc.Fork(ctx, fx.dave.actor(), forks.ForkRequest{ProjectID: parent.ID})
	if err != nil {
		t.Fatalf("(F1) settle the concurrent fork: %v", err)
	}
	if !settled.AlreadyForked || settled.Fork.ForkedSHA == nil {
		t.Errorf("(F1) the settled fork = %+v, want the existing row with its content imported", settled)
	}
	if settled.Project.Slug == daveName || !strings.HasSuffix(settled.Project.Slug, "-2") {
		t.Errorf("(F1) the settled fork's slug = %q, want the reserved name derived from the pair", settled.Project.Slug)
	}
	if lineage, audits, events := fx.forkRecordCounts(t, parent.ID, fx.dave.id); lineage != 1 || audits != 1 || events != 1 {
		t.Errorf("(F1) after settling: %d lineage, %d audit, %d event(s), want one of each", lineage, audits, events)
	}
}
