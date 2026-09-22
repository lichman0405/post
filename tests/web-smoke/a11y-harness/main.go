// Command a11y-harness is a lightweight API fixture server for the
// tests/web-smoke a11y suite. It composes the same production handlers and
// stores that cmd/api mounts, over a real PostgreSQL database, but it seeds
// enough state for the a11y scanner to exercise every docs/42 core page that
// exists in apps/web with real data rather than service-down shells.
//
// What is fixture (the same rule as tests/e2e-release):
//   - organization, project, membership and routing rules (no public route
//     creates these in this build);
//   - one main branch, one feature branch, one project state, one release,
//     one research asset/version and one pull request — enough for the seven
//     docs/42 pages that have routes to render real content.
//
// The process prints one JSON line ("READY {...}") on stdout once every route
// is serving and the fixture is seeded; the caller waits for it and then
// drives the web app with the ids it contains.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/aborthttp"
	"github.com/lichman0405/post/cmd/api/assetshttp"
	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/mergehttp"
	"github.com/lichman0405/post/cmd/api/policyhttp"
	"github.com/lichman0405/post/cmd/api/profilehttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/cmd/api/pullrequestshttp"
	"github.com/lichman0405/post/cmd/api/releasehttp"
	"github.com/lichman0405/post/cmd/api/researchprofilehttp"
	"github.com/lichman0405/post/cmd/api/reviewhttp"
	"github.com/lichman0405/post/cmd/api/rsghttp"
	"github.com/lichman0405/post/cmd/api/searchhttp"

	"github.com/lichman0405/post/internal/application/aborts"
	"github.com/lichman0405/post/internal/application/assetpublish"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/diffs"
	"github.com/lichman0405/post/internal/application/forks"
	"github.com/lichman0405/post/internal/application/manifests"
	"github.com/lichman0405/post/internal/application/merge"
	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/application/prchecks"
	"github.com/lichman0405/post/internal/application/prdiff"
	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/application/releases"
	"github.com/lichman0405/post/internal/application/resolutions"
	"github.com/lichman0405/post/internal/application/responsibilities"
	"github.com/lichman0405/post/internal/application/reviews"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/states"
	appvalidation "github.com/lichman0405/post/internal/application/validation"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rsg/integrity"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
	"github.com/lichman0405/post/internal/search/answer"
	"github.com/lichman0405/post/internal/search/ranking"
	"github.com/lichman0405/post/internal/search/retrieval"
)

const harnessTaskID = "T1104"

const reviewerPassword = "long-enough-password-1"

type human struct {
	Email  string `json:"email"`
	Handle string `json:"handle"`
	Role   string `json:"role"`
}

var humans = []human{
	{Email: "a11y-owner@example.com", Handle: "a11y-owner", Role: "owner"},
}

func main() {
	adminURL := envDefault("POSTGRES_TEST_ADMIN_URL", "postgres://postgres:postgres_dev_pw@127.0.0.1:5432/post")
	addr := envDefault("A11Y_API_ADDR", "127.0.0.1:18192")
	webOrigin := envDefault("A11Y_WEB_ORIGIN", "http://127.0.0.1:31108")
	if err := run(adminURL, addr, webOrigin); err != nil {
		fmt.Fprintf(os.Stderr, "a11y-harness: %v\n", err)
		os.Exit(1)
	}
}

func run(adminURL, addr, webOrigin string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	name := testdb.DatabaseName(harnessTaskID)
	if err := createDatabase(ctx, adminURL, name); err != nil {
		return fmt.Errorf("create database %s: %w", name, err)
	}
	defer dropDatabase(adminURL, name)
	dbURL, err := testdb.WithDatabase(adminURL, name)
	if err != nil {
		return err
	}
	if _, err := persistence.Migrate(ctx, dbURL); err != nil {
		return fmt.Errorf("migrate %s: %w", name, err)
	}
	pool, err := persistence.Open(ctx, dbURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	api, err := buildAPI(ctx, pool, webOrigin)
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	server := &http.Server{Handler: api.guarded, ReadHeaderTimeout: 15 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutCtx)
	}()

	base := "http://" + listener.Addr().String()

	ids := make([]string, len(humans))
	for i, h := range humans {
		id, err := signup(ctx, base, h.Email, h.Handle)
		if err != nil {
			return err
		}
		ids[i] = id
	}
	fixture, err := api.seed(ctx, ids)
	if err != nil {
		return err
	}

	line, err := json.Marshal(map[string]any{
		"ready":         true,
		"api_base":      base,
		"web_origin":    webOrigin,
		"project_id":    fixture.projectID,
		"project_name":  fixture.projectName,
		"org_id":        fixture.orgID,
		"user_id":       ids[0],
		"user_email":    humans[0].Email,
		"password":      reviewerPassword,
		"pr_number":     fixture.prNumber,
		"release_id":    fixture.releaseID,
		"asset_pid":     fixture.assetPID,
		"asset_version": fixture.assetVersion,
	})
	if err != nil {
		return err
	}
	fmt.Println("READY " + string(line))

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	return nil
}

// seedFixture is the seeded substrate the browser starts from.
type seedFixture struct {
	projectID    string
	projectName  string
	orgID        string
	prNumber     int64
	releaseID    string
	assetPID     string
	assetVersion string
}

// api is the composed surface plus the state the harness routes need.
type api struct {
	mux     *http.ServeMux
	guarded http.Handler
	pool    *pgxpool.Pool
}

func buildAPI(ctx context.Context, pool *pgxpool.Pool, webOrigin string) (*api, error) {
	reg, err := schemareg.New()
	if err != nil {
		return nil, err
	}
	orgStore := persistence.NewOrgStore(pool)
	projectStore := persistence.NewProjectStore(pool)
	policyStore := persistence.NewPolicyStore(pool)
	projectAPI := projectshttp.New(projectshttp.Deps{
		Store: projectStore,
		Orgs:  orgStore,
		Authz: authz.NewMatrixEngine(),
	})
	projectSvc := projectAPI.Service()
	policyAPI := policyhttp.New(policyhttp.Deps{
		Store:    policyStore,
		Orgs:     orgStore,
		Projects: projectStore,
	})
	stateStore := persistence.NewStateStore(pool)
	branchStore := persistence.NewBranchStore(pool)
	branchSvc := branches.NewService(branchStore)
	statesSvc := states.NewService(stateStore,
		appvalidation.NewGuard(rsgvalidation.NewValidator(reg), persistence.NewValidationTxProbe()))
	objects := persistence.NewScientificObjectStore(pool)
	relations := persistence.NewRelationStore(pool)
	rsgSvc := rsg.NewService(rsg.Deps{
		Projects:  projectSvc,
		Branches:  branchSvc,
		States:    statesSvc,
		Latest:    stateStore,
		Objects:   objects,
		Relations: relations,
		Queries:   persistence.NewRSGQueryStore(pool),
		Profiles:  persistence.NewProfileStore(pool),
		Authz:     authz.NewMatrixEngine(),
		Schemas:   reg,
		Events:    events.Recorder{},
	})
	prStore := persistence.NewPullRequestStore(pool)
	prSvc := pullrequests.NewService(prStore)
	forksSvc := forks.NewService(forks.Deps{
		Projects:     projectSvc,
		Branches:     branchSvc,
		BranchWriter: rsgSvc,
		Forks:        persistence.NewForkStore(pool),
		PullRequests: prSvc,
		Authz:        authz.NewMatrixEngine(),
	})
	diffSvc := diffs.NewService(stateStore, persistence.NewManifestStore(pool), prStore)
	diffOfPR := prdiff.NewService(prStore, branchStore, diffSvc)
	resolutionSvc := resolutions.NewService(diffSvc, resolutions.NewPGStore(pool), projectSvc, authz.NewMatrixEngine())
	checksSvc := prchecks.NewService(prchecks.Deps{
		PRs:      prStore,
		Projects: projectStore,
		States:   stateStore,
		Branches: branchStore,
		Manifest: persistence.NewManifestStore(pool),
		Policies: policyStore,
		Engine:   integrity.New(reg),
	})
	mergeSvc := merge.NewService(merge.Deps{
		Store:     persistence.NewSemanticMergeStore(pool),
		Diffs:     diffSvc,
		Plans:     resolutionSvc,
		Commits:   statesSvc,
		Objects:   objects,
		Relations: relations,
		Projects:  projectSvc,
		Authz:     authz.NewMatrixEngine(),
		Checks:    checksSvc,
		Forks:     persistence.NewForkStore(pool),
		Policies:  policyAPI.Service(),
		Rules:     policy.NewRuleEvaluator(),
		Events:    events.Recorder{},
		Aborts:    objects,
	})
	routingSvc := responsibilities.NewService(responsibilities.Deps{
		Rules:     persistence.NewResponsibilityStore(pool),
		Projects:  projectStore,
		Members:   projectSvc,
		PRs:       prStore,
		Branches:  branchStore,
		Diffs:     diffOfPR,
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
	releaseStore := persistence.NewReleaseStore(pool)
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
	abortSvc := aborts.NewService(aborts.Deps{
		Members:      projectStore,
		Authz:        authz.NewMatrixEngine(),
		Objects:      objects,
		Branches:     branchSvc,
		PullRequests: prSvc,
		Commits:      statesSvc,
		Events:       events.Recorder{},
	})
	publishCommand := assetpublish.NewCommand(assetpublish.Deps{
		Members:  projectStore,
		Policies: policyAPI.Service(),
		Rules:    policy.NewRuleEvaluator(),
		Store:    persistence.NewAssetPublishStore(pool, rsgvalidation.NewValidator(reg)),
		Authz:    authz.NewMatrixEngine(),
	})
	assetsAPI := assetshttp.New(assetshttp.Deps{
		State:        assetshttp.NewPostgresStateStore(pool),
		Projects:     projectSvc,
		Publish:      publishCommand,
		Pages:        persistence.NewAssetPageStore(pool),
		Members:      projectSvc,
		Dependencies: persistence.NewProjectDependencyStore(pool),
	})

	authAPI := authhttp.New(authhttp.Deps{
		Users:    persistence.NewCredentialStore(pool),
		Sessions: memstore.NewSessions(),
		Limiter:  memstore.NewLimiter(),
		Cfg: authn.Config{
			WebOrigin:          webOrigin,
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
	pullrequestshttp.New(pullrequestshttp.Deps{
		PullRequests: prSvc,
		Create:       forksSvc,
		Checks:       checksSvc,
		Diff:         diffOfPR,
		Projects:     projectSvc,
	}).Register(mux)
	reviewhttp.New(reviewhttp.Deps{Service: reviewsSvc, Projects: projectSvc}).Register(mux)
	mergehttp.New(mergehttp.Deps{Command: mergeSvc, Projects: projectSvc}).Register(mux)
	policyAPI.Register(mux)
	releasehttp.New(releasehttp.Deps{Command: releaseCommand, Projects: projectSvc}).Register(mux)
	aborthttp.New(aborthttp.Deps{Command: abortSvc}).Register(mux)
	assetsAPI.Register(mux)
	profilehttp.New(profilehttp.Deps{Profiles: persistence.NewProfileStore(pool)}).Register(mux)
	researchprofilehttp.New(researchprofilehttp.Deps{Reader: persistence.NewResearchProfileStore(pool)}).Register(mux)

	/* The search surface (docs/42 "Search Answer"), wired the way cmd/api
	 * wires it when the deployment has no answer model: answer.Deps.Provider
	 * is nil, which internal/search/answer/generator.go documents as a
	 * SUPPORTED deployment state and not a defect — nothing here fabricates an
	 * answer, and the page scanned is the page a real deployment without a
	 * model serves.
	 *
	 * What this fixture puts in front of that surface is narrower than the
	 * surface itself, and the coverage table should not be read as more than
	 * it is: "catalyst" matches none of the seeded documents, so retrieval
	 * returns zero ranked sources and the generator's zero-source branch
	 * answers first (internal/search/answer/generator.go:133,
	 * ReasonNoSources) — before the provider check is ever reached. The scan
	 * therefore exercises the structured fallback with no sources, not
	 * ReasonNoProvider and not an answer carrying citations. The page says so
	 * itself — its headline is the no_sources one ("the search returned no
	 * source to cite", apps/web/lib/search.ts:180) — which is what makes this
	 * row a measurement of the fallback rather than of the answer path.
	 *
	 * Without it /search renders its error banner and the Search Answer core
	 * page is never actually scanned — the precise "scan the shell and call
	 * it a core page" failure the coverage table exists to prevent. */
	retrievalStore, err := retrieval.NewSQLStore(pool)
	if err != nil {
		return nil, fmt.Errorf("search retrieval store: %w", err)
	}
	rankingStore, err := ranking.NewSQLStore(pool)
	if err != nil {
		return nil, fmt.Errorf("search ranking store: %w", err)
	}
	searcher, err := retrieval.NewRetriever(retrievalStore, nil)
	if err != nil {
		return nil, fmt.Errorf("search retriever: %w", err)
	}
	ranker, err := ranking.NewRanker(rankingStore)
	if err != nil {
		return nil, fmt.Errorf("search ranker: %w", err)
	}
	answerer, err := answer.New(answer.Deps{})
	if err != nil {
		return nil, fmt.Errorf("search answerer: %w", err)
	}
	searchhttp.New(searchhttp.Deps{
		Scope:     persistence.NewProjectStore(pool),
		Retriever: searcher,
		Ranker:    ranker,
		Answerer:  answerer,
		Records:   persistence.NewSearchRecordStore(pool),
	}).Register(mux)

	a := &api{mux: mux, pool: pool}
	a.guarded = authAPI.Guard(mux)
	return a, nil
}

func (a *api) seed(ctx context.Context, ids []string) (*seedFixture, error) {
	ownerID := ids[0]

	org, _, err := a.orgStore(ctx, domain.Organization{
		Slug: "a11y-org", Name: "A11Y Test Org",
	}, ownerID)
	if err != nil {
		return nil, fmt.Errorf("seed org: %w", err)
	}
	project, _, err := a.projectStore(ctx, domain.Project{
		OrganizationID:  &org.ID,
		Slug:            "a11y-project",
		Name:            "A11Y Test Project",
		Purpose:         "Accessibility scan fixture project",
		Visibility:      domain.VisibilityPublic,
		ProvisionStatus: domain.ProvisionPending,
	}, ownerID)
	if err != nil {
		return nil, fmt.Errorf("seed project: %w", err)
	}

	// Seed the branches/state/release/asset/pr fixture directly. These rows
	// are configuration substrate for the scan, not flow state under test.
	f := &seedFixture{projectID: project.ID, projectName: project.Name, orgID: org.ID}
	if err := a.seedBranchesAndState(ctx, project.ID, ownerID); err != nil {
		return nil, fmt.Errorf("seed branches/state: %w", err)
	}
	if err := a.seedRelease(ctx, project.ID, ownerID, f); err != nil {
		return nil, fmt.Errorf("seed release: %w", err)
	}
	if err := a.seedAsset(ctx, project.ID, ownerID, f); err != nil {
		return nil, fmt.Errorf("seed asset: %w", err)
	}
	if err := a.seedPullRequest(ctx, project.ID, ownerID, f); err != nil {
		return nil, fmt.Errorf("seed pull request: %w", err)
	}
	return f, nil
}

func (a *api) orgStore(ctx context.Context, org domain.Organization, ownerID string) (domain.Organization, bool, error) {
	row := a.pool.QueryRow(ctx, `
		INSERT INTO organizations (slug, name)
		VALUES ($1, $2)
		ON CONFLICT (slug) DO UPDATE SET name = EXCLUDED.name
		RETURNING id, slug, name`, org.Slug, org.Name)
	var o domain.Organization
	err := row.Scan(&o.ID, &o.Slug, &o.Name)
	if err != nil {
		return o, false, err
	}
	_, err = a.pool.Exec(ctx, `
		INSERT INTO organization_memberships (organization_id, user_id, role)
		VALUES ($1, $2, 'owner')
		ON CONFLICT (organization_id, user_id) DO NOTHING`, o.ID, ownerID)
	return o, false, err
}

func (a *api) projectStore(ctx context.Context, project domain.Project, ownerID string) (domain.Project, bool, error) {
	row := a.pool.QueryRow(ctx, `
		INSERT INTO projects (organization_id, slug, name, purpose, visibility, provision_status, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (organization_id, slug) DO UPDATE SET name = EXCLUDED.name
		RETURNING id, organization_id, slug, name, purpose, visibility, provision_status`,
		project.OrganizationID, project.Slug, project.Name, project.Purpose,
		string(project.Visibility), string(project.ProvisionStatus), ownerID)
	var p domain.Project
	var vis, prov string
	err := row.Scan(&p.ID, &p.OrganizationID, &p.Slug, &p.Name, &p.Purpose, &vis, &prov)
	if err != nil {
		return p, false, err
	}
	p.Visibility = domain.ProjectVisibility(vis)
	p.ProvisionStatus = domain.ProvisionStatus(prov)
	_, err = a.pool.Exec(ctx, `
		INSERT INTO project_memberships (project_id, user_id, role)
		VALUES ($1, $2, 'owner')
		ON CONFLICT (project_id, user_id) DO NOTHING`, p.ID, ownerID)
	return p, false, err
}

func (a *api) seedBranchesAndState(ctx context.Context, projectID, ownerID string) error {
	stateID, err := a.insertState(ctx, projectID, "", "a11y-state-hash")
	if err != nil {
		return err
	}
	mainID, err := a.insertBranch(ctx, projectID, "main", "public", &stateID, stateID, ownerID)
	if err != nil {
		return err
	}
	if _, err := a.insertCommit(ctx, projectID, mainID, nil, stateID, ownerID); err != nil {
		return err
	}
	_, err = a.insertBranch(ctx, projectID, "a11y-branch", "public", &stateID, stateID, ownerID)
	if err != nil {
		return err
	}
	return nil
}

func (a *api) insertCommit(ctx context.Context, projectID, branchID string, baseStateID *string, resultStateID, actorID string) (string, error) {
	var id string
	err := a.pool.QueryRow(ctx, `
		INSERT INTO state_commits (project_id, branch_id, base_state_id, result_state_id, actor_id, via, message, operation_summary)
		VALUES ($1, $2, $3, $4, $5, 'web', 'A11Y fixture commit', '{}')
		RETURNING id`, projectID, branchID, baseStateID, resultStateID, actorID).Scan(&id)
	return id, err
}

func (a *api) insertState(ctx context.Context, projectID, parentID, hash string) (string, error) {
	var parent interface{}
	if parentID != "" {
		parent = parentID
	}
	var id string
	err := a.pool.QueryRow(ctx, `
		INSERT INTO project_states (project_id, branch_id, parent_state_id, state_hash, manifest_version)
		VALUES ($1, NULL, $2, $3, '1.0')
		RETURNING id`, projectID, parent, hash).Scan(&id)
	return id, err
}

func (a *api) insertBranch(ctx context.Context, projectID, name, visibility string, baseStateID *string, headStateID, ownerID string) (string, error) {
	var id string
	err := a.pool.QueryRow(ctx, `
		INSERT INTO branches (project_id, name, visibility, git_ref, base_state_id, lifecycle_state, created_by)
		VALUES ($1, $2, $3, $4, $5, 'active', $6)
		ON CONFLICT (project_id, name) DO UPDATE SET base_state_id = EXCLUDED.base_state_id
		RETURNING id`, projectID, name, visibility, "refs/heads/"+name, baseStateID, ownerID).Scan(&id)
	if err != nil {
		return "", err
	}
	_, err = a.pool.Exec(ctx, `UPDATE branches SET base_state_id = $1 WHERE id = $2`, headStateID, id)
	return id, err
}

func (a *api) seedRelease(ctx context.Context, projectID, ownerID string, f *seedFixture) error {
	var stateID string
	err := a.pool.QueryRow(ctx, `SELECT id FROM project_states WHERE project_id = $1 LIMIT 1`, projectID).Scan(&stateID)
	if err != nil {
		return err
	}
	manifest := `{"version":1,"asset_type":"dataset","metadata":{"purpose":"a11y scan fixture"},"dependency_pins":[]}`
	err = a.pool.QueryRow(ctx, `
		INSERT INTO releases (project_id, version, title, state_id, manifest, manifest_hash, created_by)
		VALUES ($1, '1.0.0', 'A11Y Test Release', $2, $3, 'a11y-release-hash', $4)
		ON CONFLICT (project_id, version) DO UPDATE SET title = EXCLUDED.title
		RETURNING id`, projectID, stateID, manifest, ownerID).Scan(&f.releaseID)
	return err
}

func (a *api) seedAsset(ctx context.Context, projectID, ownerID string, f *seedFixture) error {
	var releaseID string
	err := a.pool.QueryRow(ctx, `SELECT id FROM releases WHERE project_id = $1 LIMIT 1`, projectID).Scan(&releaseID)
	if err != nil {
		return err
	}
	manifest := `{"version":1,"asset_type":"dataset","metadata":{"purpose":"a11y scan fixture"},"dependency_pins":[]}`
	rights := `{"licenses":[],"holders":[]}`
	var assetID string
	err = a.pool.QueryRow(ctx, `
		INSERT INTO research_assets (asset_type, slug, title, origin_project_id)
		VALUES ('dataset', 'a11y-asset', 'A11Y Test Asset', $1)
		RETURNING id, pid`, projectID).Scan(&assetID, &f.assetPID)
	if err != nil {
		return err
	}
	err = a.pool.QueryRow(ctx, `
		INSERT INTO research_asset_versions (asset_id, version, source_release_id, manifest, rights_json, visibility, integrity_hash, published_by, origin_refs)
		VALUES ($1, '1.0.0', $2, $3, $4, 'public', 'a11y-asset-hash', $5, ARRAY['project:' || $6::text])
		RETURNING version`, assetID, releaseID, manifest, rights, ownerID, projectID).Scan(&f.assetVersion)
	return err
}

func (a *api) seedPullRequest(ctx context.Context, projectID, ownerID string, f *seedFixture) error {
	var mainID, featureID, stateID string
	rows, err := a.pool.Query(ctx, `SELECT id FROM branches WHERE project_id = $1 ORDER BY name`, projectID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		if mainID == "" {
			mainID = id
		} else {
			featureID = id
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if featureID == "" {
		featureID = mainID
	}
	err = a.pool.QueryRow(ctx, `SELECT id FROM project_states WHERE project_id = $1 LIMIT 1`, projectID).Scan(&stateID)
	if err != nil {
		return err
	}
	err = a.pool.QueryRow(ctx, `
		INSERT INTO pull_requests (project_id, number, source_branch_id, target_branch_id, base_state_id, proposed_state_id, title, state, created_by)
		VALUES ($1, 1, $2, $3, $4, $5, 'A11Y Test PR', 'open', $6)
		ON CONFLICT (project_id, number) DO UPDATE SET title = EXCLUDED.title
		RETURNING number`, projectID, featureID, mainID, stateID, stateID, ownerID).Scan(&f.prNumber)
	return err
}

func createDatabase(ctx context.Context, adminURL, name string) error {
	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		return err
	}
	defer admin.Close()
	_, err = admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %q`, name))
	return err
}

func dropDatabase(adminURL, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		return
	}
	defer admin.Close()
	_, _ = admin.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, name))
}

func signup(ctx context.Context, base, email, handle string) (string, error) {
	body := fmt.Sprintf(`{"email":%q,"password":%q,"handle":%q,"display_name":%q}`,
		email, reviewerPassword, handle, handle)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/auth/signup", stringReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("signup %s = %d", handle, resp.StatusCode)
	}
	var payload struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	return payload.User.ID, nil
}

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func stringReader(s string) io.Reader { return strings.NewReader(s) }
