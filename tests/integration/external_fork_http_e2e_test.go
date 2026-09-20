// Package integration — T0814 "external fork http e2e" and "fork propose
// authorization e2e": the two production interfaces T0814 wired, driven over
// the real HTTP path.
//
// T0804 built the external fork/contribution GRAPH and proved it by calling
// the services; the routes did not exist yet (the fork surface had none, and
// the open-pull-request route had no command behind it). These two tests are
// the other half: every step below is an HTTP request through the production
// guard — the session cookie and the session-bound CSRF token the signup
// endpoint minted — into the production handlers, over real PostgreSQL and a
// real Gitea. No test-side principal is injected and no service is called
// directly where the criterion names an endpoint.
//
// The acceptance criteria map onto them:
//
//   - TestExternalForkHTTPEndToEnd (label `external fork http e2e`)
//     (1) a signed-in non-member forks a PUBLIC project: 201 with the fork
//     project, the fork branch and the lineage row; the same account on a
//     private project gets the existence-hiding 404.
//     (3) a repeated request is idempotent BY STATE: 200, the same fork, and
//     the project/branch/lineage/ingestion row counts do not grow.
//     (4) two same-named parents in two organizations, forked by one person,
//     both succeed with different slugs, each carrying its own pair digest —
//     the permanent lock-out the naming rule closed.
//     (6) a fork branch whose content is unparseable cannot open a formal PR:
//     the real database refuses the insert (00042's pull_request_semantic_gate
//     raises P0001) and the route answers the contract's code for it. The same
//     test pins the TWO gates' RAISE texts next to each other, which is what
//     the store's discriminator reads.
//   - TestForkProposeAuthorizationEndToEnd (label
//     `fork propose authorization e2e`)
//     (2) open_pr = allow_from_fork: from the actor's own fork the proposal
//     succeeds (201); straight from an upstream branch, and from another
//     actor's fork, it is refused.
//     (4) the anonymous column denies all three endpoints (fork, the fork's
//     scientific-state write, the proposal) with the guard's structural 401.
//     (8) docs/31_MASTER_ACCEPTANCE.md:17 「Public Project 外部用户可
//     fork/contribute」 runs end to end over the real HTTP path: fork → write
//     in the fork → propose to upstream → the proposal is an ordinary pull
//     request of the upstream project, readable through the project's own
//     pull-request surface.
package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
)

// ---- The fork route's wire shape (cmd/api/forkshttp) --------------------

type forkHTTPLineage struct {
	ForkProjectID   string  `json:"fork_project_id"`
	ParentProjectID string  `json:"parent_project_id"`
	ForkedBy        string  `json:"forked_by"`
	RelationType    string  `json:"relation_type"`
	SourceBranchID  string  `json:"source_branch_id"`
	ForkBranchID    string  `json:"fork_branch_id"`
	ForkedSHA       *string `json:"forked_sha"`
	CreatedAt       string  `json:"created_at"`
}

type forkHTTPProject struct {
	ID         string `json:"id"`
	Slug       string `json:"slug"`
	Name       string `json:"name"`
	Purpose    string `json:"purpose"`
	Visibility string `json:"visibility"`
}

type forkHTTPBranch struct {
	ID         string `json:"id"`
	ProjectID  string `json:"project_id"`
	Name       string `json:"name"`
	GitRef     string `json:"git_ref"`
	Visibility string `json:"visibility"`
	Lifecycle  string `json:"lifecycle"`
}

type forkHTTPPayload struct {
	Fork          forkHTTPLineage `json:"fork"`
	Project       forkHTTPProject `json:"project"`
	Branch        forkHTTPBranch  `json:"branch"`
	AlreadyForked bool            `json:"already_forked"`
	Imported      bool            `json:"imported"`
}

// forkPath is the contract's fork route: POST /projects/{projectId}/forks
// (specs/api/openapi.yaml), under the /api/v1 server.
func forkPath(projectID string) string {
	return "/api/v1/projects/" + projectID + "/forks"
}

// forkThroughTheEndpoint issues one fork request exactly as the contract
// defines it — a POST to the fork route over the real guard — and decodes the
// answer when there is one to decode. body=="" sends the JSON object every
// field of which is absent, which is the contract's "fork the parent's main,
// private" request.
func forkThroughTheEndpoint(t *testing.T, uc *testUserClient, projectID, body string) (*http.Response, forkHTTPPayload) {
	t.Helper()
	if body == "" {
		body = `{}`
	}
	resp := uc.do(t, http.MethodPost, forkPath(projectID), body)
	var payload forkHTTPPayload
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			t.Fatalf("fork payload: %v", err)
		}
	}
	return resp, payload
}

// createOrganization drives the product's own organization-create route and
// returns the organization's id.
func (fx *externalForkFixture) createOrganization(t *testing.T, account forkAccount, slug string) string {
	t.Helper()
	resp := account.client.do(t, http.MethodPost, "/api/v1/organizations",
		fmt.Sprintf(`{"slug":%q,"name":%q}`, slug, slug))
	mustStatus(t, resp, http.StatusCreated)
	var payload struct {
		Organization struct {
			ID string `json:"id"`
		} `json:"organization"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("organization payload: %v", err)
	}
	if payload.Organization.ID == "" {
		t.Fatalf("the organization route answered no id: %+v", payload)
	}
	return payload.Organization.ID
}

// createOrganizationProject creates one project INSIDE an organization through
// the project-create route. It is the only way two projects may hold the same
// slug: a slug is unique per organization (00019), and globally unique only
// for personal projects.
func (fx *externalForkFixture) createOrganizationProject(t *testing.T, account forkAccount, orgID, slug string,
	visibility domain.ProjectVisibility) domain.Project {
	t.Helper()
	resp := account.client.do(t, http.MethodPost, "/api/v1/projects",
		fmt.Sprintf(`{"slug":%q,"name":%q,"purpose":%q,"visibility":%q,"organization_id":%q}`,
			slug, slug, "T0814 same-named parents", string(visibility), orgID))
	mustStatus(t, resp, http.StatusCreated)
	var payload struct {
		Project struct {
			ID string `json:"id"`
		} `json:"project"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("project payload: %v", err)
	}
	project, err := fx.projectSvc.Get(fx.ctx,
		projects.Reader{UserID: account.id, Authenticated: true}, payload.Project.ID)
	if err != nil {
		t.Fatalf("read back the organization project %s: %v", slug, err)
	}
	if project.OrganizationID == nil || *project.OrganizationID != orgID {
		t.Fatalf("project %s landed in %v, want organization %s", slug, project.OrganizationID, orgID)
	}
	return project
}

// ---- Row-count probes ---------------------------------------------------

// forkRowCounts counts what a repeated fork request must NOT add: the actor's
// personal projects, the fork project's branches, and the pair's lineage rows.
func (fx *externalForkFixture) forkRowCounts(t *testing.T, actorID, forkProjectID string) (personal, branches, lineage int) {
	t.Helper()
	if err := fx.pool.QueryRow(fx.ctx,
		`SELECT count(*) FROM projects WHERE created_by = $1 AND organization_id IS NULL`,
		actorID).Scan(&personal); err != nil {
		t.Fatalf("count the actor's personal projects: %v", err)
	}
	if err := fx.pool.QueryRow(fx.ctx,
		`SELECT count(*) FROM branches WHERE project_id = $1`, forkProjectID).Scan(&branches); err != nil {
		t.Fatalf("count the fork project's branches: %v", err)
	}
	if err := fx.pool.QueryRow(fx.ctx,
		`SELECT count(*) FROM project_forks WHERE fork_project_id = $1`, forkProjectID).Scan(&lineage); err != nil {
		t.Fatalf("count the fork's lineage rows: %v", err)
	}
	return personal, branches, lineage
}

// importDeliveries counts the content copies recorded against one repository —
// the deliveries a repeated fork request must not add a second of.
func (fx *externalForkFixture) importDeliveries(t *testing.T, repoID int64) int {
	t.Helper()
	var n int
	if err := fx.pool.QueryRow(fx.ctx,
		`SELECT count(*) FROM git_push_ingestions WHERE gitea_repo_id = $1`, repoID).Scan(&n); err != nil {
		t.Fatalf("count the repository's deliveries: %v", err)
	}
	return n
}

// pairDigestOf is forkPairDigest (internal/application/forks) as this test can
// read it: the first eight hex characters of sha256 over the parent's id, a
// NUL, and the actor's id. It is spelled out here rather than exported from
// the service because the SLUG is a wire fact — what a client sees in the fork
// project's URL — and a test that asked the service for the expected value
// would be checking the service against itself.
func pairDigestOf(parentID, actorID string) string {
	sum := sha256.Sum256([]byte(parentID + "\x00" + actorID))
	return hex.EncodeToString(sum[:4])
}

// expectedForkSlug is the name the pair's fork project must carry: the
// parent's slug, the actor's handle and the pair's digest (forkSlug). The
// digest is part of EVERY fork name, short ones included, which is what makes
// two same-named parents two names.
func expectedForkSlug(parentSlug, handle, parentID, actorID string) string {
	return parentSlug + "-" + handle + "-" + pairDigestOf(parentID, actorID)
}

// ---- (1)(3)(4)(6) The HTTP fork surface ---------------------------------

// TestExternalForkHTTPEndToEnd is T0814's required e2e, label
// `external fork http e2e`.
func TestExternalForkHTTPEndToEnd(t *testing.T) {
	fx := newExternalForkFixture(t)
	ctx := fx.ctx
	// The test's OWN handle, next to the one t.Run's closure parameter
	// shadows it with. fx.repoOf registers a cleanup that DELETES the provider
	// repository when the handle it was given ends — so a subtest that merely
	// PROBES the fork's repository on its own handle would tear that
	// repository down when the subtest ends, and the next subtest would push
	// into a repository Gitea no longer has. Repository probes shared across
	// subtests go through this handle.
	ownT := t

	// ---- The public parent, created and provisioned through the product's
	// own paths, with a canonical line carrying content to copy.
	parent := fx.createProject(t, fx.alice, "mof-http-fork", domain.VisibilityPublic)
	repo := fx.provisionProject(t, parent.ID)
	main := fx.createBranch(t, fx.alice, parent.ID, "main", "")
	mainSHA := fx.seedMain(t, repo.owner, repo.name)

	t.Run("a non-member forks a public project over HTTP", func(t *testing.T) {
		resp, fork := forkThroughTheEndpoint(t, fx.bob.client, parent.ID,
			`{"name":"MOF HTTP Fork","purpose":"external contribution"}`)
		mustStatus(t, resp, http.StatusCreated)

		// The answer names the fork project, its branch and the lineage row.
		if fork.Project.ID == "" || fork.Project.ID == parent.ID {
			t.Fatalf("fork project = %+v, want a project of its own", fork.Project)
		}
		if fork.Branch.ID == "" || fork.Branch.ProjectID != fork.Project.ID {
			t.Fatalf("fork branch = %+v, want a branch of the fork project", fork.Branch)
		}
		if fork.Branch.Name != "fork/main" || fork.Branch.Lifecycle != "active" {
			t.Fatalf("fork branch = %+v, want the copy on fork/main, active", fork.Branch)
		}
		if fork.Fork.ForkProjectID != fork.Project.ID || fork.Fork.ParentProjectID != parent.ID ||
			fork.Fork.ForkedBy != fx.bob.id || fork.Fork.RelationType != "forked_from" ||
			fork.Fork.SourceBranchID != main.ID || fork.Fork.ForkBranchID != fork.Branch.ID {
			t.Fatalf("lineage = %+v, want the row this fork wrote", fork.Fork)
		}
		if fork.Fork.ForkedSHA == nil || *fork.Fork.ForkedSHA == "" {
			t.Fatalf("lineage records no fork point: %+v", fork.Fork)
		}
		if fork.AlreadyForked || !fork.Imported {
			t.Fatalf("already_forked/imported = %v/%v, want false/true", fork.AlreadyForked, fork.Imported)
		}

		// Fail closed: the fork's content is the forker's until they publish
		// it, so the default visibility is private.
		if fork.Project.Visibility != string(domain.VisibilityPrivate) {
			t.Fatalf("fork project visibility = %q, want private (the fail-closed default)",
				fork.Project.Visibility)
		}

		// The SLUG is derived from the pair and carries its digest.
		wantSlug := expectedForkSlug(parent.Slug, "extfork-bob", parent.ID, fx.bob.id)
		if fork.Project.Slug != wantSlug {
			t.Fatalf("fork slug = %q, want %q", fork.Project.Slug, wantSlug)
		}
		if fork.Project.Slug == parent.Slug+"-extfork-bob" {
			t.Fatalf("the fork slug is the digest-free shape %q: two same-named parents would collide "+
				"and lock the second fork out permanently", fork.Project.Slug)
		}

		// The canonical rows agree with the answer: the lineage row, the fork
		// project and the branch are all there.
		var lineageParent, lineageActor, lineageSource, lineageBranch string
		if err := fx.pool.QueryRow(ctx,
			`SELECT parent_project_id::text, forked_by::text, source_branch_id::text, fork_branch_id::text
			   FROM project_forks WHERE fork_project_id = $1`, fork.Project.ID).
			Scan(&lineageParent, &lineageActor, &lineageSource, &lineageBranch); err != nil {
			t.Fatalf("read the lineage row back: %v", err)
		}
		if lineageParent != parent.ID || lineageActor != fx.bob.id ||
			lineageSource != main.ID || lineageBranch != fork.Branch.ID {
			t.Fatalf("stored lineage = (%s, %s, %s, %s), want the request's own facts",
				lineageParent, lineageActor, lineageSource, lineageBranch)
		}
		var storedVisibility string
		var storedOrg *string
		if err := fx.pool.QueryRow(ctx,
			`SELECT visibility, organization_id::text FROM projects WHERE id = $1`, fork.Project.ID).
			Scan(&storedVisibility, &storedOrg); err != nil {
			t.Fatalf("read the fork project back: %v", err)
		}
		if storedVisibility != string(domain.VisibilityPrivate) || storedOrg != nil {
			t.Fatalf("stored fork project = (%s, org %v), want a private personal project",
				storedVisibility, storedOrg)
		}

		// The content really was copied: the fork's repository carries the
		// import's delivery of the parent line's commit.
		forkRepo := fx.repoOf(ownT, fork.Project.ID)
		row, _ := fx.importIngestion(t, forkRepo.id, "refs/heads/"+fork.Branch.Name)
		if row.afterSHA != mainSHA || row.projectID != fork.Project.ID {
			t.Fatalf("the import's delivery = %+v, want %s copied into %s", row, mainSHA, fork.Project.ID)
		}

		// One fork leaves exactly one lineage row, one audit row and one
		// event (docs/53): the row is not the only record of the act.
		if lineage, audits, eventsCount := fx.forkRecordCounts(t, parent.ID, fx.bob.id); lineage != 1 || audits != 1 || eventsCount != 1 {
			t.Fatalf("fork records: %d lineage, %d audit, %d event(s), want one of each",
				lineage, audits, eventsCount)
		}
	})

	t.Run("a repeated request is idempotent by state", func(t *testing.T) {
		var forkProjectID, forkBranchID string
		if err := fx.pool.QueryRow(ctx,
			`SELECT fork_project_id::text, fork_branch_id::text FROM project_forks
			  WHERE parent_project_id = $1 AND forked_by = $2`, parent.ID, fx.bob.id).
			Scan(&forkProjectID, &forkBranchID); err != nil {
			t.Fatalf("read the existing fork: %v", err)
		}
		personalBefore, branchesBefore, lineageBefore := fx.forkRowCounts(t, fx.bob.id, forkProjectID)
		forkRepo := fx.repoOf(ownT, forkProjectID)
		deliveriesBefore := fx.importDeliveries(t, forkRepo.id)

		resp, fork := forkThroughTheEndpoint(t, fx.bob.client, parent.ID,
			`{"name":"MOF HTTP Fork","purpose":"external contribution"}`)
		mustStatus(t, resp, http.StatusOK) // the contract's "Already forked"

		if !fork.AlreadyForked || fork.Imported {
			t.Fatalf("already_forked/imported = %v/%v, want true/false", fork.AlreadyForked, fork.Imported)
		}
		if fork.Project.ID != forkProjectID || fork.Branch.ID != forkBranchID {
			t.Fatalf("the repeat answered %s/%s, want the pair's existing fork %s/%s",
				fork.Project.ID, fork.Branch.ID, forkProjectID, forkBranchID)
		}
		// The ROW COUNTS are the assertion: no second project, no second
		// branch, no second lineage row, and no second content copy.
		personalAfter, branchesAfter, lineageAfter := fx.forkRowCounts(t, fx.bob.id, forkProjectID)
		if personalAfter != personalBefore || branchesAfter != branchesBefore || lineageAfter != lineageBefore {
			t.Fatalf("the repeat grew the record: personal %d→%d, branches %d→%d, lineage %d→%d",
				personalBefore, personalAfter, branchesBefore, branchesAfter, lineageBefore, lineageAfter)
		}
		if after := fx.importDeliveries(t, forkRepo.id); after != deliveriesBefore {
			t.Fatalf("the repeat copied the content again: %d→%d deliveries", deliveriesBefore, after)
		}
		if lineage, audits, eventsCount := fx.forkRecordCounts(t, parent.ID, fx.bob.id); lineage != 1 || audits != 1 || eventsCount != 1 {
			t.Fatalf("after the repeat: %d lineage, %d audit, %d event(s), want one of each",
				lineage, audits, eventsCount)
		}
	})

	t.Run("the same account on a private project gets the existence-hiding 404", func(t *testing.T) {
		// The control project is private and nobody below is a member of it.
		resp, _ := forkThroughTheEndpoint(t, fx.carol.client, fx.privateProject.ID, `{}`)
		if resp.StatusCode != http.StatusNotFound {
			body := readAll(t, resp)
			t.Fatalf("forking a private project as a non-member = %d, want 404 (never 403: a 403 "+
				"confirms the project exists): %s", resp.StatusCode, body)
		}
		var env errorEnvelope
		raw := readAll(t, resp)
		_ = json.Unmarshal([]byte(raw), &env)
		if env.Code != "PROJECT_NOT_FOUND" {
			t.Fatalf("code = %q, want PROJECT_NOT_FOUND: %s", env.Code, raw)
		}
		// Refused before anything was resolved, and nothing was written.
		var n int
		if err := fx.pool.QueryRow(ctx,
			`SELECT count(*) FROM project_forks WHERE parent_project_id = $1 AND forked_by = $2`,
			fx.privateProject.ID, fx.carol.id).Scan(&n); err != nil {
			t.Fatalf("count the refused fork's rows: %v", err)
		}
		if n != 0 {
			t.Fatalf("the refused fork left %d lineage row(s) behind", n)
		}
	})

	t.Run("one actor forks two same-named parents in two organizations", func(t *testing.T) {
		orgA := fx.createOrganization(t, fx.alice, "mof-http-lab-a")
		orgB := fx.createOrganization(t, fx.alice, "mof-http-lab-b")
		first := fx.createOrganizationProject(t, fx.alice, orgA, "mof-dupe", domain.VisibilityPublic)
		second := fx.createOrganizationProject(t, fx.alice, orgB, "mof-dupe", domain.VisibilityPublic)
		if first.ID == second.ID {
			t.Fatal("the two organizations hold the same project; the case needs two")
		}
		for _, p := range []domain.Project{first, second} {
			repo := fx.provisionProject(t, p.ID)
			fx.createBranch(t, fx.alice, p.ID, "main", "")
			fx.seedMain(t, repo.owner, repo.name)
		}

		// One person, both parents, over HTTP.
		respA, forkA := forkThroughTheEndpoint(t, fx.carol.client, first.ID, `{}`)
		mustStatus(t, respA, http.StatusCreated)
		respB, forkB := forkThroughTheEndpoint(t, fx.carol.client, second.ID, `{}`)
		mustStatus(t, respB, http.StatusCreated)

		if forkA.Project.ID == forkB.Project.ID {
			t.Fatalf("both forks landed on one project: %s", forkA.Project.ID)
		}
		if forkA.Project.Slug == forkB.Project.Slug {
			t.Fatalf("two same-named parents produced one slug %q: the second fork would have been "+
				"refused against the actor's own first fork, permanently", forkA.Project.Slug)
		}
		// Each name carries ITS pair's digest — the fact the collision test
		// above rests on.
		wantA := expectedForkSlug("mof-dupe", "extfork-carol", first.ID, fx.carol.id)
		wantB := expectedForkSlug("mof-dupe", "extfork-carol", second.ID, fx.carol.id)
		if forkA.Project.Slug != wantA || forkB.Project.Slug != wantB {
			t.Fatalf("slugs = (%q, %q), want (%q, %q)", forkA.Project.Slug, forkB.Project.Slug, wantA, wantB)
		}
		if pairDigestOf(first.ID, fx.carol.id) == pairDigestOf(second.ID, fx.carol.id) {
			t.Fatal("the two pairs share a digest; the case cannot tell them apart")
		}
	})

	t.Run("an unparseable fork branch cannot open a formal PR", func(t *testing.T) {
		var forkProjectID, forkBranchID string
		if err := fx.pool.QueryRow(ctx,
			`SELECT fork_project_id::text, fork_branch_id::text FROM project_forks
			  WHERE parent_project_id = $1 AND forked_by = $2`, parent.ID, fx.bob.id).
			Scan(&forkProjectID, &forkBranchID); err != nil {
			t.Fatalf("read bob's fork: %v", err)
		}
		var forkBranchName string
		if err := fx.pool.QueryRow(ctx,
			`SELECT name FROM branches WHERE id = $1`, forkBranchID).Scan(&forkBranchName); err != nil {
			t.Fatalf("read the fork branch: %v", err)
		}
		forkRepo := fx.repoOf(ownT, forkProjectID)

		// A real push into the fork carrying content the platform cannot
		// parse: the branch's semantic state is derived from the copy's own
		// evidence, not set by this test.
		fx.pushAndDeliver(t, forkRepo, forkBranchName, mainSHA,
			map[string]string{"data/raw.csv": "raw,unstructured\n1,2\n"}, nil,
			"attach raw measurements", "t0814-raw")
		if got := fx.semanticFlag(t, ctx, forkBranchID); got != "unstructured_changes" {
			t.Fatalf("the fork branch after the raw push = %q, want unstructured_changes", got)
		}

		// ---- The route answers the contract's code for that state. Today
		// the whole chain used to collapse into SERVICE_UNAVAILABLE, which
		// tells the contributor the service is down instead of telling them
		// what to fix.
		resp := fx.bob.client.doKeyed(t, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/pull-requests", parent.ID),
			fmt.Sprintf(`{"source_branch_id":%q,"target_branch_id":%q,"title":"proposal from raw content","body":"docs/16 §4"}`,
				forkBranchID, main.ID),
			"t0814-unstructured-key")
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("opening a PR from an unparseable branch = %d: %s", resp.StatusCode, readAll(t, resp))
		}
		var env errorEnvelope
		raw := readAll(t, resp)
		if err := json.Unmarshal([]byte(raw), &env); err != nil {
			t.Fatalf("decode the refusal: %v (%s)", err, raw)
		}
		if env.Code != "BRANCH_UNSTRUCTURED_CHANGES" {
			t.Fatalf("code = %q, want BRANCH_UNSTRUCTURED_CHANGES: %s", env.Code, raw)
		}
		if n := fx.prCount(t, parent.ID); n != 0 {
			t.Fatalf("the refused proposal left %d PR row(s) behind", n)
		}

		// ---- The database really is what refused it, and it refused it with
		// the text the store's discriminator reads. The insert below bypasses
		// every application-layer check: only 00042's
		// pull_request_semantic_gate stands between it and a PR row.
		baseState := fx.baseStateOfBranch(t, main.ID)
		sourceState := fx.baseStateOfBranch(t, forkBranchID)
		_, err := fx.pool.Exec(ctx, `INSERT INTO pull_requests
			(project_id, number, source_branch_id, target_branch_id, base_state_id, proposed_state_id, title, created_by)
			VALUES ($1, 9001, $2, $3, $4, $5, 'smuggled from raw content', $6)`,
			parent.ID, forkBranchID, main.ID, baseState, sourceState, fx.bob.id)
		if !isSemanticGateError(err) {
			t.Fatalf("a smuggled PR insert from an unparseable branch = %v, want 00042's P0001 refusal", err)
		}
		if !strings.HasPrefix(err.Error(), "ERROR: pull request cannot be opened from branch ") {
			t.Fatalf("00042's RAISE text is %q; the store discriminates on the prefix "+
				"\"pull request cannot be opened from branch \"", err.Error())
		}

		// ---- The OTHER gate raises the same SQLSTATE from the same table,
		// which is why the two are told apart by their text. A cross-project
		// source that is nobody's fork of this project is 00086's
		// pull_request_fork_gate.
		foreign := fx.createProject(t, fx.alice, "mof-http-foreign", domain.VisibilityPublic)
		fx.provisionProject(t, foreign.ID)
		aliceLine := fx.createBranch(t, fx.alice, foreign.ID, "line-main", "")
		_, err = fx.pool.Exec(ctx, `INSERT INTO pull_requests
			(project_id, number, source_branch_id, target_branch_id, base_state_id, proposed_state_id, title, created_by)
			VALUES ($1, 9002, $2, $3, $4, $4, 'smuggled from a foreign project', $5)`,
			parent.ID, aliceLine.ID, main.ID, baseState, fx.bob.id)
		if !isSemanticGateError(err) {
			t.Fatalf("a smuggled cross-project PR insert = %v, want 00086's P0001 refusal", err)
		}
		if !strings.HasPrefix(err.Error(), "ERROR: pull request on project ") {
			t.Fatalf("00086's RAISE text is %q; the store discriminates on the prefix "+
				"\"pull request on project \"", err.Error())
		}
	})
}

// TestForkProposeAuthorizationEndToEnd is T0814's required e2e, label
// `fork propose authorization e2e`: the three matrix cells this task wired,
// driven over HTTP, plus the master-acceptance journey.
func TestForkProposeAuthorizationEndToEnd(t *testing.T) {
	fx := newExternalForkFixture(t)
	ctx := fx.ctx

	parent := fx.createProject(t, fx.alice, "mof-authz", domain.VisibilityPublic)
	repo := fx.provisionProject(t, parent.ID)
	main := fx.createBranch(t, fx.alice, parent.ID, "main", "")
	fx.seedMain(t, repo.owner, repo.name)

	// ---- The actor's own fork, over HTTP.
	resp, bobFork := forkThroughTheEndpoint(t, fx.bob.client, parent.ID, `{}`)
	mustStatus(t, resp, http.StatusCreated)

	// ---- (4) The anonymous column: three cells, three endpoints, one
	// structural answer. The guard refuses the write before routing, so none
	// of these three requests reaches a handler at all.
	anonymous := newTestUserClient(fx.ts.URL)
	for _, tc := range []struct {
		name string
		path string
		body string
	}{
		{"create_branch (fork)", forkPath(parent.ID), `{}`},
		{"write_scientific_state", fmt.Sprintf("/api/v1/projects/%s/branches/%s/objects",
			bobFork.Project.ID, bobFork.Branch.ID), `{"object_type":"claim"}`},
		{"open_pr", fmt.Sprintf("/api/v1/projects/%s/pull-requests", parent.ID),
			`{"source_branch_id":"x","target_branch_id":"y","title":"anonymous"}`},
	} {
		resp := anonymous.do(t, http.MethodPost, tc.path, tc.body)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("anonymous %s = %d: %s", tc.name, resp.StatusCode, readAll(t, resp))
			continue
		}
		var env errorEnvelope
		raw := readAll(t, resp)
		_ = json.Unmarshal([]byte(raw), &env)
		if env.Code != "AUTH_UNAUTHENTICATED" {
			t.Errorf("anonymous %s code = %q: %s", tc.name, env.Code, raw)
		}
	}

	// ---- (2) write_scientific_state = own_fork_only, over the real RSG
	// route. The matrix is what decides; the handler resolves nothing.
	writeObject := func(account forkAccount, projectID, branchID, statement string) (int, string) {
		t.Helper()
		resp := account.client.do(t, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/branches/%s/objects", projectID, branchID),
			fmt.Sprintf(`{"object_type":"claim","payload":%s}`, mergeMainGateClaim(statement)))
		return resp.StatusCode, readAll(t, resp)
	}
	if status, body := writeObject(fx.bob, bobFork.Project.ID, bobFork.Branch.ID,
		"an external contributor's claim"); status != http.StatusCreated {
		t.Fatalf("write in the actor's own fork = %d: %s", status, body)
	}
	if status, body := writeObject(fx.bob, parent.ID, main.ID, "an upstream claim"); status != http.StatusForbidden {
		t.Errorf("write on the upstream project as a non-member = %d: %s", status, body)
	}

	// ---- (2)(3) Somebody else's fork: a full fork of the same parent, made
	// by another account, so the refusal is the open_pr / own_fork_only cell
	// and not an unreadable project. Carol's fork is asked for PUBLIC on
	// purpose — a private one would be answered by the project read gate (the
	// existence-hiding 404) and the two cells below would never be reached.
	// The parent is public, so asking for a public fork of it is allowed.
	carolResp, carolFork := forkThroughTheEndpoint(t, fx.carol.client, parent.ID, `{"visibility":"public"}`)
	mustStatus(t, carolResp, http.StatusCreated)
	if carolFork.Project.Visibility != string(domain.VisibilityPublic) {
		t.Fatalf("the second fork's visibility = %q, want the public one it asked for — with a private "+
			"fork the two refusals below would be the read gate's 404, not the cells under test",
			carolFork.Project.Visibility)
	}
	if status, body := writeObject(fx.bob, carolFork.Project.ID, carolFork.Branch.ID,
		"somebody else's claim"); status != http.StatusForbidden {
		t.Errorf("write in another actor's fork = %d: %s", status, body)
	}
	if !carolFork.AlreadyForked && carolFork.Project.ID == bobFork.Project.ID {
		t.Fatal("two actors share one fork project")
	}

	// ---- (3) open_pr = allow_from_fork, over the real proposal route.
	openPR := func(account forkAccount, sourceBranchID, targetBranchID, key string) *http.Response {
		t.Helper()
		return account.client.doKeyed(t, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/pull-requests", parent.ID),
			fmt.Sprintf(`{"source_branch_id":%q,"target_branch_id":%q,"title":"material evidence from an external fork","body":"Proposed from the contributor's own fork (docs/04 §2)."}`,
				sourceBranchID, targetBranchID),
			key)
	}
	resp = openPR(fx.bob, bobFork.Branch.ID, main.ID, "t0814-pr-own-fork")
	mustStatus(t, resp, http.StatusCreated)
	var pr struct {
		ID              string `json:"id"`
		Number          int64  `json:"number"`
		SourceBranchID  string `json:"source_branch_id"`
		TargetBranchID  string `json:"target_branch_id"`
		BaseStateID     string `json:"base_state_id"`
		ProposedStateID string `json:"proposed_state_id"`
		CreatedBy       string `json:"created_by"`
		State           string `json:"state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		t.Fatalf("decode the proposal: %v", err)
	}
	if pr.SourceBranchID != bobFork.Branch.ID || pr.TargetBranchID != main.ID || pr.CreatedBy != fx.bob.id {
		t.Fatalf("the proposal = %+v, want %s → %s by %s", pr, bobFork.Branch.ID, main.ID, fx.bob.id)
	}
	if pr.State != "open" {
		t.Fatalf("the proposal's state = %q, want the machine's first state", pr.State)
	}
	// The row is what the EXISTING path downstream consumes, pinned the way
	// that path reads it: the base state is the TARGET branch's head and the
	// proposed state is the SOURCE branch's — the two states the review
	// machine shows and the merge later plans with. An external proposal is
	// not a special kind of pull request (docs/04 §2): if it were pinned
	// differently, review and merge would have to know about forks.
	if want := fx.baseStateOfBranch(t, main.ID); pr.BaseStateID != want {
		t.Fatalf("base_state_id = %q, want the target branch's head %q", pr.BaseStateID, want)
	}
	if want := fx.baseStateOfBranch(t, bobFork.Branch.ID); pr.ProposedStateID != want {
		t.Fatalf("proposed_state_id = %q, want the source branch's head %q", pr.ProposedStateID, want)
	}

	// Straight from an upstream branch: refused. The source is a real second
	// branch of the parent — a proposal a member could open — so the open_pr
	// cell is the only thing between this call and a PR row.
	upstreamLine := fx.createBranch(t, fx.alice, parent.ID, "line-upstream-authz", "")
	resp = openPR(fx.bob, upstreamLine.ID, main.ID, "t0814-pr-upstream")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a proposal from an upstream branch as a non-member = %d: %s", resp.StatusCode, readAll(t, resp))
	}
	// From ANOTHER actor's fork: refused, with the same answer.
	resp = openPR(fx.bob, carolFork.Branch.ID, main.ID, "t0814-pr-other-fork")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a proposal from another actor's fork = %d: %s", resp.StatusCode, readAll(t, resp))
	}
	// The two refusals opened nothing.
	if n := fx.prCount(t, parent.ID); n != 1 {
		t.Fatalf("the project has %d pull request(s), want only the accepted one", n)
	}

	// ---- (8) The master-acceptance journey, over the HTTP path end to end:
	// fork → write in the fork → propose upstream → the proposal is an
	// ORDINARY pull request of the upstream project, readable through the
	// project's own pull-request surface by the project's owner and by a
	// non-member, and NOT applied to main (merge controls acceptance).
	detail := fx.alice.client.do(t, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/pull-requests/%d", parent.ID, pr.Number), "")
	mustStatus(t, detail, http.StatusOK)
	var readBack struct {
		Number         int64  `json:"number"`
		SourceBranchID string `json:"source_branch_id"`
		CreatedBy      string `json:"created_by"`
	}
	if err := json.NewDecoder(detail.Body).Decode(&readBack); err != nil {
		t.Fatalf("decode the proposal read back: %v", err)
	}
	if readBack.Number != pr.Number || readBack.SourceBranchID != bobFork.Branch.ID || readBack.CreatedBy != fx.bob.id {
		t.Fatalf("the upstream project's own surface answered %+v, want the contribution", readBack)
	}
	list := fx.mallory.client.do(t, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/pull-requests", parent.ID), "")
	mustStatus(t, list, http.StatusOK)
	var listed []struct {
		Number         int64  `json:"number"`
		SourceBranchID string `json:"source_branch_id"`
	}
	if err := json.NewDecoder(list.Body).Decode(&listed); err != nil {
		t.Fatalf("decode the proposal list: %v", err)
	}
	found := false
	for _, row := range listed {
		if row.Number == pr.Number && row.SourceBranchID == bobFork.Branch.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("the contribution is not on the project's pull-request list: %+v", listed)
	}

	// The contribution is PROPOSED, not applied: the parent's canonical line
	// still ends at the state it had before, and the fork's branch is what
	// carries the change (docs/09 §3 — main advances only through a merge).
	var mainState, parentStateAfter string
	if err := fx.pool.QueryRow(ctx,
		`SELECT COALESCE(base_state_id::text, '') FROM branches WHERE id = $1`, main.ID).Scan(&mainState); err != nil {
		t.Fatalf("read main's head: %v", err)
	}
	if err := fx.pool.QueryRow(ctx,
		`SELECT COALESCE(base_state_id::text, '') FROM branches WHERE id = $1`, bobFork.Branch.ID).Scan(&parentStateAfter); err != nil {
		t.Fatalf("read the fork branch's head: %v", err)
	}
	if mainState == parentStateAfter {
		t.Fatal("the parent's main and the fork's branch share a head; the claim written in the fork is not " +
			"on its own line, so the proposal has nothing to propose")
	}
}
