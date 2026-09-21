// Package integration — T0818 "external fork merge: the upstream project's
// RSG reads the landed content" (blocking): the READ half of the external
// contribution path.
//
// What the task is about, in one sentence: T0817 made the merge land an
// external fork's proposal correctly, but the landed version rows hang on the
// CONTRIBUTOR's containers (ADR-027 Decision 1) while the project-domain read
// selected rows by `so.project_id = @project_id` — so the upstream project
// could not read its own main back: the object it had just accepted was in
// main, in the manifest, and invisible to `rsg.Service.Query` and
// `ResearchOutline` alike. ADR-027 decides the read instead: a project's RSG
// is the versions ITS STATE LINEAGE carries, whatever container they hang on.
//
// # The tests
//
//   - TestExternalForkMergeLandsTheContentInTheUpstreamRSG — the required
//     e2e, label `external fork merge: the upstream project's RSG reads the
//     landed content`. A (public) is forked by B, the contributor writes
//     scientific state on the fork branch, the proposal is opened to A and
//     approved, the merge lands, and then BOTH projects read their RSG back
//     over the product's own query service:
//     A's read (unpinned AND pinned at the accepted state) carries the landed
//     object versions — payload, title, object type, branch and the version
//     id's own row, item by item — and the landed relation version;
//     B's read of the same object does NOT render the landed version as its
//     latest, and the landed version id appears nowhere in B's slice;
//     neither project gained a `scientific_objects` row;
//     and a version B wrote AFTER the merge, in a fork state A never
//     enumerated, is invisible to A's readers.
//
// The fourth block below is the negative half of the same read rule, run
// through BOTH readers (the query and the outline): the upstream project shows
// what its lineage carries and drops the object B kept to itself, while B's
// own read of that same object still carries it — so "invisible to A" is a
// statement about the reader and not about the row being unreadable by
// everyone.
//
// # What the fixture is, and what it is not
//
// Everything that produces state is a product path: the project, the
// repository, the fork, the imported content, the contributor's writes (the
// real RSG route behind the real guard), the proposal, the review machine
// that walks it to merge_ready and the merge route itself. Nothing fabricates
// a row to set up the case. The one thing the fixture chooses is the FORK's
// visibility: the product's default for a fork is PRIVATE (ForkRequest.
// Visibility: "empty means private. Fail closed: the fork's content is the
// forker's own until they publish it"), so the loop runs both ways — a
// private fork (the default) and a public one — and asserts the same read in
// both. A read that only works when the contributor chose to publish would
// not be "the upstream project reads its own lineage".
package integration

import (
	"fmt"
	"net/http"
	"sort"
	"testing"

	"github.com/lichman0405/post/internal/application/forks"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/responsibilities"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/domain"
)

// externalForkReadLoop is everything one run of the external proposal
// produces, read back from the platform's own rows. Every field is consumed
// by the assertions below; nothing here is a derived convenience value.
type externalForkReadLoop struct {
	parent domain.Project
	main   domain.Branch
	// fork is the lineage answer of the product's own fork command.
	fork forks.ForkResult
	// object/version are the contributor's claim and the version row the
	// write produced; supporting/relation are the second object and the
	// version-pinned edge from it.
	object            string
	version           string
	supporting        string
	supportingVersion string
	relation          string
	relationVersion   string
	// forkHead is the fork branch's head state at the moment the proposal
	// pinned it — the state B's own read is pinned to.
	forkHead string
	pr       domain.PullRequest
	// merge is the merge route's own answer.
	merge mergeE2EPayload
	// mainAfter is A's main head after the merge (read from the branch, not
	// from the response).
	mainAfter string
	// objectsBeforeParent/objectsBeforeFork are the two projects' container
	// counts on the eve of the merge — after both sides' writes, before the
	// merge ran — so a difference is the merge's own doing.
	objectsBeforeParent int
	objectsBeforeFork   int
}

// runExternalForkReadLoop drives the whole external contribution loop over
// the production graph and returns the material. forkVisibility is the fork
// project's own visibility; the loop is otherwise identical, so the two
// visibilities are the same case under the product's two fork shapes.
func runExternalForkReadLoop(t *testing.T, p *forkMergePlatform, slug string, forkVisibility domain.ProjectVisibility, key string) *externalForkReadLoop {
	t.Helper()
	ctx := p.ctx
	l := &externalForkReadLoop{}

	// ---- The upstream project: public, owned by Alice, provisioned for real.
	l.parent = p.createProject(t, p.alice, slug, domain.VisibilityPublic)
	p.provisionProject(t, l.parent.ID)
	l.main = p.createBranch(t, p.alice, l.parent.ID, "main", "")

	// The upstream project's main_protected policy, seeded over the product's
	// own store: without a policy version in force the merge is refused
	// (MERGE_POLICY_REFUSED — "an absent rule is not a permission"), which is
	// the platform protecting main, not a fixture detail.
	seedProjectPolicy(t, ctx, p.policyStore, l.parent, p.alice.id)

	// ---- The external contribution: Bob forks the public project from his
	// own space. The fork project, its repository, its branch, the copied
	// content and the lineage row are the product's own work.
	forked, err := p.forkSvc.Fork(ctx, p.bob.actor(), forks.ForkRequest{
		ProjectID: l.parent.ID, Visibility: forkVisibility,
	})
	if err != nil {
		t.Fatalf("external fork merge read: fork %s as a non-member: %v", slug, err)
	}
	l.fork = forked
	if forked.Fork.ForkProjectID == l.parent.ID || forked.Fork.ParentProjectID != l.parent.ID {
		t.Fatalf("external fork merge read: the fork lineage = %+v, want a fork project of %s", forked.Fork, l.parent.ID)
	}
	var forRepo forkRepo
	if err := p.pool.QueryRow(ctx,
		`SELECT name, gitea_repo_id, webhook_secret FROM git_repository_provisions WHERE project_id = $1`,
		forked.Project.ID).Scan(&forRepo.name, &forRepo.id, &forRepo.secret); err != nil {
		t.Fatalf("external fork merge read: read the fork's provision row: %v", err)
	}
	t.Cleanup(func() { deleteGiteaRepo(t, p.base, p.token, forRepo.owner, forRepo.name) })

	// ---- The contributor's scientific state, written over the production
	// RSG route (own_fork_only lets an external contributor write in their own
	// fork and nowhere else).
	const statement = "the external contributor's measured band gap of 1.4 eV"
	l.object, l.version = p.writeClaim(t, p.bob, forked.Project.ID, forked.Branch.ID, statement)
	const supportingStatement = "the external contributor's second instrument reading"
	l.supporting, l.supportingVersion = p.writeClaim(t, p.bob, forked.Project.ID, forked.Branch.ID, supportingStatement)
	const (
		relationType = "supports"
		relationBody = `{"note":"the second reading supports the first","instrument":"fixture-2"}`
	)
	l.relation, l.relationVersion = p.writeRelation(t, p.bob, forked.Project.ID, forked.Branch.ID,
		relationType, l.supportingVersion, l.version, relationBody)

	// ---- The proposal: an external PR from the fork into the upstream
	// project, opened by the contributor, then walked to merge_ready by the
	// project's own review machine (two members of the UPSTREAM project — a
	// contributor's fork is not a review venue).
	pr, err := p.forkSvc.OpenExternalPR(ctx, p.bob.actor(), forks.OpenPRRequest{
		ProjectID:      l.parent.ID,
		SourceBranchID: forked.Branch.ID,
		TargetBranchID: l.main.ID,
		Title:          "external measurement from a fork",
		Body:           "Proposed from the contributor's own fork (docs/04 §2).",
	})
	if err != nil {
		t.Fatalf("external fork merge read: open a pull request from the fork: %v", err)
	}
	l.pr = pr
	l.forkHead = p.branchHead(t, forked.Branch.ID)
	if pr.ProposedStateID != l.forkHead {
		t.Fatalf("external fork merge read: the proposal pins source state %s, want the fork branch's head %s",
			pr.ProposedStateID, l.forkHead)
	}

	for _, m := range []struct {
		account forkAccount
		role    domain.ProjectRole
	}{{p.maintainer, domain.ProjectRoleMaintainer}, {p.viewer, domain.ProjectRoleViewer}} {
		if _, err := p.pool.Exec(ctx,
			`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, $3)`,
			l.parent.ID, m.account.id, m.role); err != nil {
			t.Fatalf("external fork merge read: seed the %s membership: %v", m.role, err)
		}
		if _, err := p.routingSvc.Assign(ctx, p.alice.actor(), l.parent.ID, m.account.id, "Data Reviewer"); err != nil {
			t.Fatalf("external fork merge read: assign the responsibility label: %v", err)
		}
	}
	if _, err := p.routingSvc.AddRule(ctx, p.alice.actor(), l.parent.ID, claimRoutingRule()); err != nil {
		t.Fatalf("external fork merge read: route claim changes to a reviewer: %v", err)
	}
	if _, err := p.prSvc.RequestReview(ctx, l.parent.ID, pr.Number); err != nil {
		t.Fatalf("external fork merge read: request review: %v", err)
	}
	for _, submission := range []struct {
		account forkAccount
		kind    string
	}{{p.viewer, "scientific"}, {p.maintainer, "integrity"}} {
		body := fmt.Sprintf(`{"kind":%q,"decision":"approved","body":"reviewed for the external fork merge read e2e"}`,
			submission.kind)
		resp := submission.account.client.do(t, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/pull-requests/%d/reviews", l.parent.ID, pr.Number), body)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("external fork merge read: submit the %s review = %d: %s",
				submission.kind, resp.StatusCode, readAll(t, resp))
		}
	}
	reviewed, err := p.prSvc.Get(ctx, l.parent.ID, pr.Number)
	if err != nil {
		t.Fatalf("external fork merge read: read the pull request after the reviews: %v", err)
	}
	if reviewed.State != domain.PullRequestStateMergeReady {
		t.Fatalf("external fork merge read: the reviewed proposal is %q, want %q",
			reviewed.State, domain.PullRequestStateMergeReady)
	}

	// ---- The two projects' container counts on the eve of the merge. The
	// criterion is "the merge created no identity", so the baseline is taken
	// after everything the two sides wrote and before the merge runs.
	l.objectsBeforeParent = p.countObjects(t, l.parent.ID)
	l.objectsBeforeFork = p.countObjects(t, forked.Project.ID)

	// ---- The merge, over the production route as the upstream owner.
	l.merge = mergeThroughTheEndpoint(t, p.alice.client, l.parent.ID, pr.Number, key)
	if l.merge.Replayed {
		t.Fatal("external fork merge read: the first merge reports itself as a replay")
	}
	l.mainAfter = p.branchHead(t, l.main.ID)
	if l.mainAfter == l.forkHead {
		t.Fatalf("external fork merge read: main's head is still %s — the merge accepted nothing", l.mainAfter)
	}
	if l.merge.StateID != l.mainAfter {
		t.Fatalf("external fork merge read: main's head = %s, the merge answered %s", l.mainAfter, l.merge.StateID)
	}
	return l
}

// claimRoutingRule is the routing rule every loop adds: claim changes go to
// the named responsibility (without it the proposal never leaves review).
func claimRoutingRule() responsibilities.AddRuleInput {
	return responsibilities.AddRuleInput{
		MatchKind:      domain.ResearchOwnerMatchObjectType,
		MatchValue:     "claim",
		Responsibility: "Data Reviewer",
	}
}

// countObjects counts one project's scientific_objects rows — the identity
// count the "no new identity" criterion is about.
func (p *forkMergePlatform) countObjects(t *testing.T, projectID string) int {
	t.Helper()
	var n int
	if err := p.pool.QueryRow(p.ctx,
		`SELECT count(*) FROM scientific_objects WHERE project_id = $1`, projectID).Scan(&n); err != nil {
		t.Fatalf("external fork merge read: count the objects of %s: %v", projectID, err)
	}
	return n
}

// querySlice runs one read of a project's RSG through the product's own query
// service, as the given account. It is the read the task is about: everything
// below is asserted on what the service RETURNS, never on a row probe that
// could agree with the query by accident.
func (p *forkMergePlatform) querySlice(t *testing.T, account forkAccount, projectID string, in rsg.QueryInput) rsg.QueryResult {
	t.Helper()
	res, err := p.rsgSvc.Query(p.ctx, projects.Reader{UserID: account.id, Authenticated: true}, projectID, in)
	if err != nil {
		t.Fatalf("external fork merge read: query the RSG of %s (state pin %q): %v", projectID, in.StateID, err)
	}
	return res
}

// queryObject returns the slice row for one object, and whether the object is
// in the slice at all.
func queryObject(res rsg.QueryResult, objectID string) (rsg.ObjectResult, bool) {
	for _, o := range res.Objects {
		if o.Object.ID == objectID {
			return o, true
		}
	}
	return rsg.ObjectResult{}, false
}

// queryRelation returns the slice row for one relation, and whether the edge
// is in the slice at all.
func queryRelation(res rsg.QueryResult, relationID string) (rsg.RelationResult, bool) {
	for _, r := range res.Relations {
		if r.Relation.ID == relationID {
			return r, true
		}
	}
	return rsg.RelationResult{}, false
}

// objectIDs / relationIDs render a slice's ids for failure messages.
func objectIDs(res rsg.QueryResult) []string {
	out := make([]string, 0, len(res.Objects))
	for _, o := range res.Objects {
		out = append(out, o.Object.ID)
	}
	sort.Strings(out)
	return out
}

func relationIDs(res rsg.QueryResult) []string {
	out := make([]string, 0, len(res.Relations))
	for _, r := range res.Relations {
		out = append(out, r.Relation.ID)
	}
	sort.Strings(out)
	return out
}

// outlineObjectRefs flattens every object the outline names, whatever axis it
// landed on (the question tree's nodes and their addressed-by lists, the
// findings' claim refs, and the unassigned remainder).
func outlineObjectRefs(out rsg.ResearchOutline) []rsg.OutlineObjectRef {
	refs := make([]rsg.OutlineObjectRef, 0, len(out.Unassigned))
	refs = append(refs, out.Unassigned...)
	var walk func(qs []rsg.OutlineQuestion)
	walk = func(qs []rsg.OutlineQuestion) {
		for _, q := range qs {
			refs = append(refs, rsg.OutlineObjectRef{ObjectID: q.ObjectID, ObjectType: "research_question", Title: q.Title})
			refs = append(refs, q.Hypotheses...)
			refs = append(refs, q.Findings...)
			refs = append(refs, q.OtherObjects...)
			walk(q.Children)
		}
	}
	walk(out.Questions)
	for _, f := range out.Findings {
		refs = append(refs, rsg.OutlineObjectRef{ObjectID: f.ObjectID, ObjectType: "finding", Title: f.Title})
		for _, c := range f.Claims {
			if c.Resolved {
				refs = append(refs, rsg.OutlineObjectRef{ObjectID: c.ObjectID, ObjectType: "claim", Title: c.Title})
			}
		}
	}
	return refs
}

// outlineNamesObject reports whether the outline names this object at all,
// and with which title.
func outlineNamesObject(out rsg.ResearchOutline, objectID string) (bool, string) {
	for _, r := range outlineObjectRefs(out) {
		if r.ObjectID == objectID {
			return true, r.Title
		}
	}
	return false, ""
}

// readOutline runs one outline read through the product's own service, gated
// exactly as the route gates it.
func (p *forkMergePlatform) readOutline(t *testing.T, account forkAccount, projectID string) rsg.ResearchOutline {
	t.Helper()
	out, err := p.rsgSvc.ResearchOutline(p.ctx, projects.Reader{UserID: account.id, Authenticated: true}, projectID)
	if err != nil {
		t.Fatalf("external fork merge read: build the outline of %s: %v", projectID, err)
	}
	return out
}

// TestExternalForkMergeLandsTheContentInTheUpstreamRSG is T0818's required
// e2e: label `external fork merge: the upstream project's RSG reads the landed
// content`. It runs over the product's DEFAULT fork shape (a private fork) AND
// a public one, because ADR-027's rule is about the reader's state lineage,
// not about the contributor's publication choice.
func TestExternalForkMergeLandsTheContentInTheUpstreamRSG(t *testing.T) {
	p := newForkMergePlatform(t)
	ctx := p.ctx

	for _, tc := range []struct {
		name       string
		visibility domain.ProjectVisibility
		slug       string
		key        string
	}{
		{"private fork (the product's default)", domain.VisibilityPrivate, "mof-fork-read-private", "fork-read-private-0001"},
		{"public fork", domain.VisibilityPublic, "mof-fork-read-public", "fork-read-public-0001"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := runExternalForkReadLoop(t, p, tc.slug, tc.visibility, tc.key)

			// ---- The landed rows, read out of the canonical tables: the
			// merge's own writes, not the fork's rows.
			landed := p.versionOfObjectInState(t, l.object, l.mainAfter)
			landedSupporting := p.versionOfObjectInState(t, l.supporting, l.mainAfter)
			landedRelation := p.relationVersionInState(t, l.relation, l.mainAfter)
			if landed.id == l.version || landedRelation.ID == l.relationVersion {
				t.Fatalf("external fork merge read: the accepted state names the fork's rows themselves "+
					"(object version %s vs %s, relation version %s vs %s)",
					landed.id, l.version, landedRelation.ID, l.relationVersion)
			}
			source := readVersion(t, ctx, p.pool, l.version)
			sourceRelation := p.relationVersionInState(t, l.relation, l.forkHead)

			// ================= (1) THE UPSTREAM DIRECTION =================
			// A's project-domain read carries the landed versions. Asserted
			// item by item: the object is in the slice AT THE LANDED VERSION,
			// with the contributor's payload and title, and the edge is in the
			// slice at the landed relation version with the contributor's
			// payload.
			for _, pinned := range []struct {
				name string
				in   rsg.QueryInput
			}{
				{"project-wide (no pin)", rsg.QueryInput{}},
				{"pinned at the accepted state", rsg.QueryInput{StateID: l.mainAfter}},
				{"pinned at main (the branch)", rsg.QueryInput{BranchID: l.main.ID}},
			} {
				res := p.querySlice(t, p.alice, l.parent.ID, pinned.in)
				got, ok := queryObject(res, l.object)
				if !ok {
					t.Fatalf("external fork merge read: A's read %s does not carry the landed object %s at all; objects=%v",
						pinned.name, l.object, objectIDs(res))
				}
				if got.Version.ID != landed.id {
					t.Errorf("external fork merge read: A's read %s renders the landed object at version %s, want the landed row %s",
						pinned.name, got.Version.ID, landed.id)
				}
				if got.Version.StateID != l.mainAfter {
					t.Errorf("external fork merge read: A's read %s renders the object in state %s, want the accepted state %s",
						pinned.name, got.Version.StateID, l.mainAfter)
				}
				if got.Version.BranchID == nil || *got.Version.BranchID != l.main.ID {
					t.Errorf("external fork merge read: A's read %s renders the object on branch %v, want main %s",
						pinned.name, got.Version.BranchID, l.main.ID)
				}
				if string(got.Version.Payload) != string(source.payload) {
					t.Errorf("external fork merge read: A's read %s carries a payload that is not the contributor's, byte for byte:\n  read:   %s\n  source: %s",
						pinned.name, got.Version.Payload, source.payload)
				}
				if got.Version.Title != source.title {
					t.Errorf("external fork merge read: A's read %s renders title %q, want the contributor's %q",
						pinned.name, got.Version.Title, source.title)
				}
				if got.Object.ObjectType != "claim" {
					t.Errorf("external fork merge read: A's read %s renders object type %q, want claim",
						pinned.name, got.Object.ObjectType)
				}
				// The container travelled with the version: the landed row
				// hangs on the contributor's container (ADR-027 Decision 1),
				// and the read reports it as such rather than inventing an
				// upstream copy of the identity.
				if got.Object.ID != l.object {
					t.Errorf("external fork merge read: A's read %s names container %s, want the contributor's %s",
						pinned.name, got.Object.ID, l.object)
				}
				if got.Object.ProjectID != l.fork.Project.ID {
					t.Errorf("external fork merge read: A's read %s reports the container's project as %s, want the fork's %s "+
						"(the landed version keeps its source identity)",
						pinned.name, got.Object.ProjectID, l.fork.Project.ID)
				}

				rel, ok := queryRelation(res, l.relation)
				if !ok {
					t.Fatalf("external fork merge read: A's read %s does not carry the landed relation %s at all; relations=%v",
						pinned.name, l.relation, relationIDs(res))
				}
				if rel.Version.ID != landedRelation.ID {
					t.Errorf("external fork merge read: A's read %s renders the landed relation at version %s, want %s",
						pinned.name, rel.Version.ID, landedRelation.ID)
				}
				if rel.Version.StateID != l.mainAfter {
					t.Errorf("external fork merge read: A's read %s renders the relation in state %s, want %s",
						pinned.name, rel.Version.StateID, l.mainAfter)
				}
				if rel.Version.RelationType != sourceRelation.RelationType {
					t.Errorf("external fork merge read: A's read %s renders relation type %q, want the contributor's %q",
						pinned.name, rel.Version.RelationType, sourceRelation.RelationType)
				}
				if string(rel.Version.Payload) != string(sourceRelation.Payload) {
					t.Errorf("external fork merge read: A's read %s carries an edge payload that is not the contributor's, byte for byte:\n  read:   %s\n  source: %s",
						pinned.name, rel.Version.Payload, sourceRelation.Payload)
				}
				// The edge's pins are the versions THIS merge wrote for the
				// two endpoints — so the landed edge resolves inside the
				// accepted state and not into the fork's rows.
				if rel.Version.SourceObjectVersionID != landedSupporting.id {
					t.Errorf("external fork merge read: A's read %s pins the edge's source at %s, want the landed version %s",
						pinned.name, rel.Version.SourceObjectVersionID, landedSupporting.id)
				}
				if rel.Version.TargetObjectVersionID != landed.id {
					t.Errorf("external fork merge read: A's read %s pins the edge's target at %s, want the landed version %s",
						pinned.name, rel.Version.TargetObjectVersionID, landed.id)
				}
			}

			// The upstream outline — the other caller of the same read —
			// carries the contributor's object too, under the contributor's
			// title, and counts it as a claim of the project.
			outline := p.readOutline(t, p.alice, l.parent.ID)
			named, title := outlineNamesObject(outline, l.object)
			if !named {
				t.Errorf("external fork merge read: A's outline does not name the landed object %s at all (claims counted: %d)",
					l.object, outline.Counts.Claims)
			}
			if named && title != source.title {
				t.Errorf("external fork merge read: A's outline names the landed object with title %q, want the contributor's %q",
					title, source.title)
			}
			if outline.Counts.Claims != 2 {
				t.Errorf("external fork merge read: A's outline counts %d claims, want the two the merge landed",
					outline.Counts.Claims)
			}

			// ==================== (2) THE FORK DIRECTION ====================
			// B's own read of the SAME object must not render the landed
			// version as its latest: the accepted state is not in B's lineage,
			// so the version the merge wrote upstream is not B's to see.
			for _, pinned := range []struct {
				name string
				in   rsg.QueryInput
			}{
				{"project-wide (no pin)", rsg.QueryInput{}},
				{"pinned at the fork branch head", rsg.QueryInput{StateID: l.forkHead}},
			} {
				res := p.querySlice(t, p.bob, l.fork.Project.ID, pinned.in)
				got, ok := queryObject(res, l.object)
				if !ok {
					t.Fatalf("external fork merge read: B's read %s no longer carries its own object %s; objects=%v",
						pinned.name, l.object, objectIDs(res))
				}
				if got.Version.ID == landed.id {
					t.Errorf("external fork merge read: B's read %s renders the LANDED version %s as its object's latest — "+
						"the accepted state is not in B's lineage", pinned.name, landed.id)
				}
				if got.Version.ID != l.version {
					t.Errorf("external fork merge read: B's read %s renders its object at version %s, want the contributor's own %s",
						pinned.name, got.Version.ID, l.version)
				}
				if got.Version.StateID == l.mainAfter {
					t.Errorf("external fork merge read: B's read %s renders its object in the accepted state %s",
						pinned.name, l.mainAfter)
				}
				for _, r := range res.Relations {
					if r.Version.ID == landedRelation.ID {
						t.Errorf("external fork merge read: B's read %s carries the LANDED relation version %s",
							pinned.name, landedRelation.ID)
					}
				}
			}

			// ===================== (3) NO NEW IDENTITY ======================
			// Neither project gained a container: the landed content is the
			// contributor's container carrying an upstream state, never a
			// second identity created upstream (ADR-027 Decision 1).
			if after := p.countObjects(t, l.parent.ID); after != l.objectsBeforeParent {
				t.Errorf("external fork merge read: the upstream project holds %d objects after the merge, %d before — "+
					"the merge created an identity in the upstream project", after, l.objectsBeforeParent)
			}
			if after := p.countObjects(t, l.fork.Project.ID); after != l.objectsBeforeFork {
				t.Errorf("external fork merge read: the fork project holds %d objects after the merge, %d before",
					after, l.objectsBeforeFork)
			}
			// The landed content is a version of the CONTRIBUTOR's container,
			// which is what makes "no new identity" the right expectation.
			var containers int
			if err := p.pool.QueryRow(ctx,
				`SELECT count(*) FROM scientific_object_versions WHERE state_id = $1 AND object_id = $2`,
				l.mainAfter, l.object).Scan(&containers); err != nil {
				t.Fatalf("external fork merge read: count the accepted state's versions of the contributor's object: %v", err)
			}
			if containers != 1 {
				t.Errorf("external fork merge read: the accepted state carries %d version(s) of the contributor's object, want exactly the landed one",
					containers)
			}

			// ================= (4) THE ENUMERATION DID NOT WIDEN ============
			// A version B wrote in ITS OWN state after the merge — never
			// proposed, never accepted — must be invisible to A's readers,
			// wherever A reads from: the project-domain query at every pin,
			// and the outline.
			followUp := p.createBranch(t, p.bob, l.fork.Project.ID, "follow-up", "")
			const withheld = "the contributor's private follow-up measurement, never proposed"
			withheldObject, withheldVersion := p.writeClaim(t, p.bob, l.fork.Project.ID, followUp.ID, withheld)
			withheldState := p.branchHead(t, followUp.ID)
			var withheldStateProject string
			if err := p.pool.QueryRow(ctx,
				`SELECT project_id::text FROM project_states WHERE id = $1`, withheldState).Scan(&withheldStateProject); err != nil {
				t.Fatalf("external fork merge read: read the withheld version's state: %v", err)
			}
			if withheldStateProject != l.fork.Project.ID {
				t.Fatalf("external fork merge read: the withheld version's state belongs to %s, want the fork's %s — "+
					"the case is not the one this assertion is about", withheldStateProject, l.fork.Project.ID)
			}
			if withheldState == l.mainAfter {
				t.Fatal("external fork merge read: the withheld version sits in the accepted state — the case is not the negative one")
			}
			for _, pinned := range []struct {
				name string
				in   rsg.QueryInput
			}{
				{"project-wide (no pin)", rsg.QueryInput{}},
				{"pinned at the accepted state", rsg.QueryInput{StateID: l.mainAfter}},
				{"pinned at main", rsg.QueryInput{BranchID: l.main.ID}},
			} {
				res := p.querySlice(t, p.alice, l.parent.ID, pinned.in)
				if _, ok := queryObject(res, withheldObject); ok {
					t.Errorf("external fork merge read: A's read %s carries %s, a version B kept in its own state %s "+
						"and never merged into A", pinned.name, withheldObject, withheldState)
				}
				for _, o := range res.Objects {
					if o.Version.ID == withheldVersion {
						t.Errorf("external fork merge read: A's read %s renders the unmerged version %s",
							pinned.name, withheldVersion)
					}
				}
			}
			// The control that keeps the assertion above from being vacuous:
			// B's own read DOES carry it, so "invisible to A" is about the
			// reader, not about the row being unreadable by everyone.
			bobRes := p.querySlice(t, p.bob, l.fork.Project.ID, rsg.QueryInput{})
			if _, ok := queryObject(bobRes, withheldObject); !ok {
				t.Fatalf("external fork merge read: B's own read does not carry its own unmerged object %s — "+
					"the invisibility asserted above would be vacuous; objects=%v", withheldObject, objectIDs(bobRes))
			}
		})
	}
}
