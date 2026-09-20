// Task T0905 — required test "ranking integration".
//
// The unit suite (internal/search/ranking) and the golden fixtures
// (tests/ranking) drive a fake FactorStore, so everything they prove is about
// the ranking's own rules: which level a fact sheet maps to, what the order
// does with a tie, what is refused before a read is issued. What only a real
// PostgreSQL can settle is what this file is for:
//
//  1. THE FACT SHEET IS READ FROM THE ROWS THE PLATFORM ACTUALLY KEEPS.
//     Every count is checked against evidence_assertions, reviews,
//     relation_versions and scientific_object_versions this file seeded, in
//     the shapes the write paths produce: an INDEPENDENT reproduction is one
//     asserted by another project, a review is recorded against the STATE a
//     version was created in, and a contradiction is counted by RELATION and
//     not by version.
//
//  2. THE RANKING IS NOT ORDERED BY WHO WROTE THE CANDIDATE. This is the
//     task's acceptance criterion ("不按 star/prestige 主排序", docs/14 §3) and
//     it is proven by counter-proof rather than by inspection: two versions
//     with identical fact sheets, in projects whose MEMBERSHIP, ORGANIZATION,
//     FOLLOWER and ACTIVITY counts differ by orders of magnitude, must rank
//     as a tie — and a tie keeps the retrieval's order, so ranking the pair
//     in both orders must mirror the result. A ranking that read any of those
//     numbers would separate them, in a fixed direction, whichever way the
//     pair arrived.
//
//  3. THE SCOPE IS ENFORCED IN SQL AND FAILS CLOSED. The fact read is run
//     with a narrowed scope and with an EMPTY one: another project's version
//     is ABSENT from the result in both cases — never present with zero
//     counts — because "we may not read this" and "there is nothing there"
//     are the two answers the ranking renders as `unknown` and `none`, and a
//     store that returned zeros for the first would make them the same. The
//     unwrapped query is then run to show the row DOES come back without its
//     predicates: the scope is what stops it, and the leak is observed rather
//     than argued.
//
//  4. THE TWO LAYERS COMPOSE. The retrieval's own output (T0904) is fed to
//     the ranker, so the boundary between them is exercised end to end: the
//     version resolution the ranking redoes for every candidate must agree
//     with the one the traversal's seed step did, and the ranking must return
//     exactly the candidates it was given, in an order the facts explain.
package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/search"
	"github.com/lichman0405/post/internal/search/ranking"
	"github.com/lichman0405/post/internal/search/retrieval"
)

// rankingTaskID namespaces this file's test databases (docs/66 §3).
const rankingTaskID = "T0905"

// --------------------------------------------------------------------------
// Fixture

// rankingFixture is one real database holding four projects:
//
//   - `small`: one member, no organization, no followers, two objects. The
//     obscure lab.
//   - `famous`: the same objects and an order of magnitude more of everything
//     about the people and the project — twelve members, an organization,
//     twenty-five subscribers to the project and twenty-five to its author,
//     forty extra objects. The prestigious lab.
//   - `open`: PUBLIC and owned by bob, who alice has nothing to do with. Her
//     retrieval returns its published documents (a document is read by its own
//     visibility) while its object versions are outside her scope — the one
//     situation where a candidate the caller may see has facts the caller may
//     not.
//   - `other`: private, bob's. Nothing of it reaches alice at all.
type rankingFixture struct {
	ctx   context.Context
	pool  *pgxpool.Pool
	store *ranking.SQLStore

	aliceID string
	bobID   string
	carolID string

	small  string
	famous string
	open   string
	other  string

	// prNumber is the pull_requests.number counter (UNIQUE per project).
	prNumber int
}

func newRankingFixture(t *testing.T, ctx context.Context) *rankingFixture {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), rankingTaskID)
	f := &rankingFixture{ctx: ctx, pool: pool}
	store, err := ranking.NewSQLStore(pool)
	if err != nil {
		t.Fatalf("ranking.NewSQLStore: %v", err)
	}
	f.store = store

	f.aliceID = f.user(t, "rank-alice", "Alice")
	f.bobID = f.user(t, "rank-bob", "Bob")
	f.carolID = f.user(t, "rank-carol", "Carol")

	orgID := mustQueryUUID(t, ctx, pool,
		`INSERT INTO organizations (slug, name) VALUES ('rank-lab','Ranking Lab') RETURNING id`)
	f.small = f.seedProject(t, "rank-small", "private", nil, f.aliceID)
	f.famous = f.seedProject(t, "rank-famous", "private", &orgID, f.aliceID)
	f.open = f.seedProject(t, "rank-open", "public", &orgID, f.bobID)
	f.other = f.seedProject(t, "rank-other", "private", &orgID, f.bobID)

	// The prestige bundle: real columns the platform keeps, and none of them
	// anything the six factors read. More members, more followers (of the
	// project AND of the author), and more objects.
	for i := 0; i < 11; i++ {
		extra := f.user(t, fmt.Sprintf("rank-member-%02d", i), "Member")
		if _, err := pool.Exec(ctx,
			`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, 'contributor')`,
			f.famous, extra); err != nil {
			t.Fatalf("seed famous membership: %v", err)
		}
	}
	for i := 0; i < 25; i++ {
		extra := f.user(t, fmt.Sprintf("rank-sub-%02d", i), "Subscriber")
		for _, target := range []struct{ kind, id string }{{"project", f.famous}, {"user", f.aliceID}} {
			if _, err := pool.Exec(ctx,
				`INSERT INTO subscriptions (user_id, target_type, target_id) VALUES ($1, $2, $3)`,
				extra, target.kind, target.id); err != nil {
				t.Fatalf("seed %s subscription: %v", target.kind, err)
			}
		}
	}
	for i := 0; i < 40; i++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO scientific_objects (project_id, object_type, created_by) VALUES ($1, 'claim', $2)`,
			f.famous, f.aliceID); err != nil {
			t.Fatalf("seed famous object: %v", err)
		}
	}
	return f
}

func (f *rankingFixture) user(t *testing.T, handle, display string) string {
	t.Helper()
	return mustQueryUUID(t, f.ctx, f.pool,
		`INSERT INTO users (handle, display_name) VALUES ($1, $2) RETURNING id`, handle, display)
}

func (f *rankingFixture) seedProject(t *testing.T, slug, visibility string, orgID *string, owner string) string {
	t.Helper()
	id := mustQueryUUID(t, f.ctx, f.pool,
		`INSERT INTO projects (organization_id, slug, name, purpose, visibility, created_by)
		 VALUES ($1, $2, $2, 'T0905 ranking fixture', $3, $4) RETURNING id`,
		orgID, slug, visibility, owner)
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, 'owner')`, id, owner); err != nil {
		t.Fatalf("seed membership on %s: %v", slug, err)
	}
	return id
}

// seedVersion writes one object with one version on its own branch and
// returns the version id and the state it was created in — the state is what
// a review is recorded against.
func (f *rankingFixture) seedVersion(t *testing.T, projectID, owner, objectType, title, branch string) (versionID, stateID, objectID string) {
	t.Helper()
	objectID = mustQueryUUID(t, f.ctx, f.pool,
		`INSERT INTO scientific_objects (project_id, object_type, created_by)
		 VALUES ($1, $2, $3) RETURNING id`, projectID, objectType, owner)
	branchID := mustQueryUUID(t, f.ctx, f.pool,
		`INSERT INTO branches (project_id, name, visibility, git_ref, created_by)
		 VALUES ($1, $2, 'private', 'refs/heads/' || $2, $3) RETURNING id`,
		projectID, branch, owner)
	stateID = mustQueryUUID(t, f.ctx, f.pool,
		`INSERT INTO project_states (project_id, branch_id, state_hash, git_commit_sha, manifest_version)
		 VALUES ($1, $2, 'h-' || gen_random_uuid()::text, 'sha-' || gen_random_uuid()::text, '1')
		 RETURNING id`, projectID, branchID)
	versionID = mustQueryUUID(t, f.ctx, f.pool,
		`INSERT INTO scientific_object_versions
		   (object_id, version_no, state_id, schema_id, schema_version, title, lifecycle_state,
		    payload, integrity_hash, created_by)
		 VALUES ($1, 1, $2, $3, '1', $4, 'active', '{}'::jsonb, 'sha256:seed', $5)
		 RETURNING id`, objectID, stateID, objectType, title, owner)
	return versionID, stateID, objectID
}

// assertion is one evidence_assertions row, spelled out. Nine positional
// strings in a row would be unreadable at the call sites, and half of them
// would be indistinguishable from each other.
type assertion struct {
	project    string // the project that asserts it
	owner      string // created_by
	state      string // the state the assertion is committed to
	target     string // the object version the assertion is ABOUT
	evidence   string // the object version that carries the evidence
	relation   string // supports | contradicts | reproduces | ...
	review     string // unreviewed | reviewed | rejected
	visibility string // public | private (00091)
	directness string // direct | indirect | unknown
}

// assert writes one evidence assertion. evidence_origin is derived the way
// the write path derives it (00091): an assertion whose project is the
// target's own project is internal, anything else is external.
func (f *rankingFixture) assert(t *testing.T, a assertion) {
	t.Helper()
	if a.evidence == a.target {
		t.Fatalf("an assertion may not carry its own target as evidence (00058's CHECK)")
	}
	origin := "external"
	if a.project == f.ownerOf(t, a.target) {
		origin = "internal"
	}
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO evidence_assertions
		   (project_id, state_id, target_object_version_id, evidence_object_version_id,
		    relation_type, evidence_type, scope, directness, inference_nature, reasoning_note,
		    review_state, visibility, evidence_origin, created_by)
		 VALUES ($1, $2, $3, $4, $5, 'experimental', '{}'::jsonb, $6, 'observational', 'n',
		         $7, $8, $9, $10)`,
		a.project, a.state, a.target, a.evidence, a.relation, a.directness,
		a.review, a.visibility, origin, a.owner); err != nil {
		t.Fatalf("seed %s evidence on %s: %v", a.relation, a.target, err)
	}
}

// ownerOf reads the project a version belongs to. The write path derives an
// assertion's origin from exactly this comparison (00091), so the fixture
// makes the same one instead of hard-coding the direction.
func (f *rankingFixture) ownerOf(t *testing.T, versionID string) string {
	t.Helper()
	var project string
	if err := f.pool.QueryRow(f.ctx,
		`SELECT so.project_id::text FROM scientific_object_versions sov
		 JOIN scientific_objects so ON so.id = sov.object_id WHERE sov.id = $1::uuid`,
		versionID).Scan(&project); err != nil {
		t.Fatalf("read the project of %s: %v", versionID, err)
	}
	return project
}

// review records one scientific review of a version's STATE.
func (f *rankingFixture) review(t *testing.T, projectID, prOwner, stateID, reviewer, decision string) {
	t.Helper()
	branchID := mustQueryUUID(t, f.ctx, f.pool,
		`SELECT branch_id::text FROM project_states WHERE id = $1::uuid`, stateID)
	f.prNumber++
	prID := mustQueryUUID(t, f.ctx, f.pool,
		`INSERT INTO pull_requests (project_id, number, source_branch_id, target_branch_id,
		    base_state_id, proposed_state_id, title, body, state, created_by)
		 VALUES ($1, $2, $3, $3, $4, $4, 'review me', '', 'open', $5) RETURNING id`,
		projectID, f.prNumber, branchID, stateID, prOwner)
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO reviews (pull_request_id, reviewer_id, review_kind, decision, reviewed_state_id)
		 VALUES ($1, $2, 'scientific', $3, $4)`, prID, reviewer, decision, stateID); err != nil {
		t.Fatalf("seed review on state %s: %v", stateID, err)
	}
}

// relate writes one relation of relationType from source to target, owned by
// projectID and committed to stateID.
func (f *rankingFixture) relate(t *testing.T, projectID, owner, stateID, relationType, source, target string) {
	t.Helper()
	relationID := mustQueryUUID(t, f.ctx, f.pool,
		`INSERT INTO relations (project_id) VALUES ($1) RETURNING id`, projectID)
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO relation_versions
		   (relation_id, version_no, state_id, relation_type,
		    source_object_version_id, target_object_version_id, payload, integrity_hash, created_by)
		 VALUES ($1, 1, $2, $3, $4, $5, '{}'::jsonb, 'sha256:rel', $6)`,
		relationID, stateID, relationType, source, target, owner); err != nil {
		t.Fatalf("seed relation %s: %v", relationType, err)
	}
}

// scopeFor resolves the scope of a user the way every production caller does.
func (f *rankingFixture) scopeFor(t *testing.T, userID string) search.Scope {
	t.Helper()
	scope, err := search.ResolveScope(f.ctx, persistence.NewProjectStore(f.pool), userID)
	if err != nil {
		t.Fatalf("ResolveScope: %v", err)
	}
	return scope
}

// --------------------------------------------------------------------------
// The fact sheet, read from real rows

// TestRankingFactsReadFromRealRows seeds the shapes the six factors are about
// — reviewed evidence, rejected evidence, a reproduction from another project,
// a reproduction from the version's own, a contradicting relation, a restated
// relation — and checks every count the store returns against them.
//
// The four assertions about the subject are chosen so that no two counts can
// be confused with each other: 4 assertions, 3 reviewed, 1 rejected, 3 direct
// (one is indirect), 3 supporting, 1 contradicting, 2 reproductions, of which
// 1 is from another project. A query that returned a wrong column under a
// right name would have to agree with all eight.
func TestRankingFactsReadFromRealRows(t *testing.T) {
	ctx := testCtx(t)
	f := newRankingFixture(t, ctx)

	subject, subjectState, _ := f.seedVersion(t, f.small, f.aliceID, "claim", "CO2 uptake claim", "subject")
	ownEvidence, _, _ := f.seedVersion(t, f.small, f.aliceID, "experiment", "the experiment behind it", "own-evidence")

	// Alice's own project asserts three things about the subject:
	//   - a reviewed, DIRECT support;
	//   - a rejected, INDIRECT contradiction — indirect on purpose, so that
	//     "direct" is a count of a column rather than a second copy of
	//     "assertions";
	//   - a reviewed reproduction of its OWN work: counted as a reproduction,
	//     and NOT as an independent one. This is the half of the independence
	//     rule that a query comparing nothing would still get right, which is
	//     why the other half is seeded below.
	f.assert(t, assertion{project: f.small, owner: f.aliceID, state: subjectState, target: subject,
		evidence: ownEvidence, relation: "supports", review: "reviewed", visibility: "private", directness: "direct"})
	f.assert(t, assertion{project: f.small, owner: f.aliceID, state: subjectState, target: subject,
		evidence: ownEvidence, relation: "contradicts", review: "rejected", visibility: "private", directness: "indirect"})
	f.assert(t, assertion{project: f.small, owner: f.aliceID, state: subjectState, target: subject,
		evidence: ownEvidence, relation: "reproduces", review: "reviewed", visibility: "private", directness: "direct"})

	// Bob's PRIVATE reproduction: 'private' is the fail-closed default
	// (00091) and alice is not in his project, so it must contribute nothing
	// — not to the count, and not to the independence.
	bobEvidence, bobState, _ := f.seedVersion(t, f.other, f.bobID, "experiment", "bob's replication", "bob-evidence")
	f.assert(t, assertion{project: f.other, owner: f.bobID, state: bobState, target: subject,
		evidence: bobEvidence, relation: "reproduces", review: "reviewed", visibility: "private", directness: "direct"})

	// Bob's PUBLIC reproduction of the same version: visibility is the
	// assertion's own axis, and this one the platform made readable by
	// anyone. It is the only assertion here whose project differs from the
	// subject's, so it is the only one that may be called independent.
	publicEvidence, publicState, _ := f.seedVersion(t, f.other, f.bobID, "experiment", "bob's public attestation", "bob-public")
	f.assert(t, assertion{project: f.other, owner: f.bobID, state: publicState, target: subject,
		evidence: publicEvidence, relation: "reproduces", review: "reviewed", visibility: "public", directness: "direct"})

	// One contradicting relation touching the subject.
	contradicting, _, _ := f.seedVersion(t, f.small, f.aliceID, "finding", "a finding that contradicts it", "contradicting")
	f.relate(t, f.small, f.aliceID, subjectState, "contradicts", contradicting, subject)

	alice := f.scopeFor(t, f.aliceID)
	got := f.facts(t, alice, subject)

	for _, tc := range []struct {
		name string
		got  int
		want int
	}{
		{"assertions", got.EvidenceAssertions, 4}, // three of alice's, one public from bob's
		{"reviewed", got.EvidenceReviewed, 3},     // support, self-reproduction, bob's public one
		{"rejected", got.EvidenceRejected, 1},     // the indirect contradiction
		{"direct", got.EvidenceDirect, 3},         // everything but the contradiction
		{"supporting", got.EvidenceSupporting, 3}, // the support and both reproductions
		{"contradicting", got.EvidenceContradicting, 1},
		{"reproduces", got.Reproduces, 2},             // alice's own and bob's public one
		{"independent", got.ReproducesIndependent, 1}, // only bob's public one
		{"failed", got.FailsToReproduce, 0},
		{"contradicting relations", got.ContradictingRelations, 1},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d\nfull sheet: %+v", tc.name, tc.got, tc.want, got)
		}
	}
	if !got.IsNewest || got.VersionNo != 1 || got.NewestVersionNo != 1 || got.LifecycleState != "active" {
		t.Errorf("version = %d of %d (%s, newest %v), want 1 of 1 (active, true)",
			got.VersionNo, got.NewestVersionNo, got.LifecycleState, got.IsNewest)
	}

	// A restated edge: one relation with two versions is ONE contradiction
	// (the count is over relation_id), which is why a re-issued edge cannot
	// weigh more than a fresh one.
	restated := mustQueryUUID(t, ctx, f.pool,
		`INSERT INTO relations (project_id) VALUES ($1) RETURNING id`, f.small)
	for v := 1; v <= 2; v++ {
		if _, err := f.pool.Exec(ctx,
			`INSERT INTO relation_versions
			   (relation_id, version_no, state_id, relation_type,
			    source_object_version_id, target_object_version_id, payload, integrity_hash, created_by)
			 VALUES ($1, $2, $3, 'contradicts', $4, $5, '{}'::jsonb, 'sha256:rel', $6)`,
			restated, v, subjectState, contradicting, subject, f.aliceID); err != nil {
			t.Fatalf("seed the second relation version: %v", err)
		}
	}
	if got := f.facts(t, alice, subject); got.ContradictingRelations != 2 {
		t.Errorf("contradicting relations after a restated edge = %d, want 2 (two relations, three versions)",
			got.ContradictingRelations)
	}

	// A second version of the SAME object: freshness is a fact about the
	// object's own version lineage, so the older version stops being newest
	// while its evidence is untouched. Without this the `version` factor's
	// two columns would be indistinguishable from constants.
	newest := mustQueryUUID(t, ctx, f.pool,
		`INSERT INTO scientific_object_versions
		   (object_id, version_no, state_id, schema_id, schema_version, title, lifecycle_state,
		    payload, integrity_hash, created_by)
		 SELECT object_id, 2, $1, 'claim', '1', 'CO2 uptake claim, revised', 'active',
		        '{}'::jsonb, 'sha256:seed2', $2
		 FROM scientific_object_versions WHERE id = $3::uuid
		 RETURNING id`, subjectState, f.aliceID, subject)
	if got := f.facts(t, alice, subject); got.IsNewest || got.NewestVersionNo != 2 {
		t.Errorf("the older version reads version %d of %d (newest %v), want 1 of 2 (false)",
			got.VersionNo, got.NewestVersionNo, got.IsNewest)
	}
	if got := f.facts(t, alice, newest); !got.IsNewest || got.NewestVersionNo != 2 || got.EvidenceAssertions != 0 {
		t.Errorf("the revised version = %+v, want version 2 of 2, newest, with no evidence of its own", got)
	}
}

// TestRankingReviewStateFollowsTheVersionsState pins the review read: a review
// is recorded against the STATE a version was created in (00061), and the
// factor must not attribute a review of one state to a version created in
// another — even when the two are in the same project.
func TestRankingReviewStateFollowsTheVersionsState(t *testing.T) {
	ctx := testCtx(t)
	f := newRankingFixture(t, ctx)
	scope := f.scopeFor(t, f.aliceID)

	reviewed, reviewedState, _ := f.seedVersion(t, f.small, f.aliceID, "claim", "reviewed claim", "reviewed")
	sibling, siblingState, _ := f.seedVersion(t, f.small, f.aliceID, "claim", "sibling claim", "sibling")
	f.review(t, f.small, f.aliceID, reviewedState, f.bobID, "approved")

	if got := f.facts(t, scope, reviewed); got.ReviewsScientific != 1 || got.ReviewsApproved != 1 {
		t.Errorf("reviewed version: scientific/approved = %d/%d, want 1/1", got.ReviewsScientific, got.ReviewsApproved)
	}
	if got := f.facts(t, scope, sibling); got.ReviewsScientific != 0 {
		t.Errorf("the sibling version's state has no review, but it reports %d", got.ReviewsScientific)
	}
	// The change-request path is the other half of the decision vocabulary:
	// a review that is not an approval is still a review.
	requested, requestedState, _ := f.seedVersion(t, f.small, f.aliceID, "claim", "claim with changes requested", "requested")
	f.review(t, f.small, f.aliceID, requestedState, f.carolID, "changes_requested")
	if got := f.facts(t, scope, requested); got.ReviewsScientific != 1 || got.ReviewsApproved != 0 || got.ReviewsChangesRequested != 1 {
		t.Errorf("change-requested version: scientific/approved/changes = %d/%d/%d, want 1/0/1",
			got.ReviewsScientific, got.ReviewsApproved, got.ReviewsChangesRequested)
	}
	_ = siblingState
}

// --------------------------------------------------------------------------
// The acceptance criterion: not ordered by prestige

// TestRankingDoesNotOrderByPrestige is the task's acceptance criterion as a
// counter-proof over real rows.
//
// Two versions with fact sheets that are equal field for field live in
// projects that differ only in the things a popularity ranking would read:
// memberships, an organization, subscribers to the project, subscribers to
// the author, and the number of objects the project has produced.
//
// The result must be a TIE, and a tie keeps the retrieval's order, so ranking
// the pair in both orders must produce mirrored results. Both halves matter:
//
//   - if the ranking read any of the five numbers, the pair would stop tying
//     and one would win in a FIXED direction, which the second ordering
//     catches;
//   - if the factors disagreed at all, one ordering would not mirror the
//     other.
func TestRankingDoesNotOrderByPrestige(t *testing.T) {
	ctx := testCtx(t)
	f := newRankingFixture(t, ctx)
	scope := f.scopeFor(t, f.aliceID)

	obscure, obscureObject, famous, famousObject := f.seedPrestigePair(t)

	// The fixture asserts its own premise before any conclusion is drawn from
	// it: the two fact sheets are equal except for their identities. Compared
	// field by field with the ids blanked, so a field added to VersionFacts
	// later is compared by default rather than silently excluded.
	left, right := f.facts(t, scope, obscure), f.facts(t, scope, famous)
	left.ObjectVersionID, left.ObjectID = "", ""
	right.ObjectVersionID, right.ObjectID = "", ""
	if left != right {
		t.Fatalf("the fixture did not build two equal fact sheets, so this test would not be about prestige:\n%+v\nvs\n%+v",
			left, right)
	}

	obscureCand := rankingCandidate(obscure, obscureObject, "knowledge:0000000000000000000obscure", "CO2 uptake in an obscure lab")
	famousCand := rankingCandidate(famous, famousObject, "knowledge:00000000000000000000famous", "CO2 uptake in a famous lab")

	ranker, err := ranking.NewRanker(f.store)
	if err != nil {
		t.Fatalf("ranking.NewRanker: %v", err)
	}
	req := retrieval.Request{Query: "CO2 uptake"}

	for _, tc := range []struct {
		name  string
		order []retrieval.Candidate
		want  string
	}{
		{"obscure listed first", []retrieval.Candidate{obscureCand, famousCand}, obscure},
		{"famous listed first", []retrieval.Candidate{famousCand, obscureCand}, famous},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ranker.Rank(ctx, scope, req, retrieval.Result{Query: req.Query, Candidates: tc.order})
			if err != nil {
				t.Fatalf("Rank: %v", err)
			}
			if len(got.Ranked) != 2 {
				t.Fatalf("%d ranked, want 2", len(got.Ranked))
			}
			if got.Ranked[0].ObjectVersionID != tc.want {
				t.Fatalf("rank 1 is %s, want %s — the candidate the retrieval listed first. "+
					"The two fact sheets are equal and nothing else may separate them.\n%s",
					got.Ranked[0].ObjectVersionID, tc.want, describeFactors(got))
			}
			if got.Ranked[0].Rank != 1 || got.Ranked[1].Rank != 2 {
				t.Errorf("ranks are %d,%d, want 1,2", got.Ranked[0].Rank, got.Ranked[1].Rank)
			}
			if !equalAssessments(got.Ranked[0], got.Ranked[1]) {
				t.Errorf("the two candidates were separated by a factor although their fact sheets are equal:\n%s",
					describeFactors(got))
			}
		})
	}
}

// seedPrestigePair writes the counter-proof's corpus — one claim in each lab,
// each with one reviewed supporting assertion from its own project — and
// returns the two versions and their objects.
func (f *rankingFixture) seedPrestigePair(t *testing.T) (obscure, obscureObject, famous, famousObject string) {
	t.Helper()
	obscure, obscureState, obscureObject := f.seedVersion(t, f.small, f.aliceID, "claim", "CO2 uptake in an obscure lab", "obscure")
	famous, famousState, famousObject := f.seedVersion(t, f.famous, f.aliceID, "claim", "CO2 uptake in a famous lab", "famous")
	smallEvidence, _, _ := f.seedVersion(t, f.small, f.aliceID, "experiment", "small lab experiment", "ev-small")
	famousEvidence, _, _ := f.seedVersion(t, f.famous, f.aliceID, "experiment", "famous lab experiment", "ev-famous")
	f.assert(t, assertion{project: f.small, owner: f.aliceID, state: obscureState, target: obscure,
		evidence: smallEvidence, relation: "supports", review: "reviewed", visibility: "private", directness: "direct"})
	f.assert(t, assertion{project: f.famous, owner: f.aliceID, state: famousState, target: famous,
		evidence: famousEvidence, relation: "supports", review: "reviewed", visibility: "private", directness: "direct"})
	return obscure, obscureObject, famous, famousObject
}

// TestRankingPrestigeBundleIsReal keeps the counter-proof above from passing
// vacuously: if the prestige columns had failed to land (a trigger, a
// constraint, a rename), the two projects would be equally humble and the
// test above would prove nothing. It builds the counter-proof's corpus, then
// reads the numbers back.
func TestRankingPrestigeBundleIsReal(t *testing.T) {
	ctx := testCtx(t)
	f := newRankingFixture(t, ctx)
	f.seedPrestigePair(t)

	read := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatalf("read %s: %v", sql, err)
		}
		return n
	}
	for _, tc := range []struct {
		name        string
		project     string
		members     int
		subscribers int
		objects     int
		org         int
	}{
		{"obscure lab", f.small, 1, 0, 2, 0},
		{"famous lab", f.famous, 12, 25, 42, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := read(`SELECT count(*) FROM project_memberships WHERE project_id = $1`, tc.project); got != tc.members {
				t.Errorf("members = %d, want %d", got, tc.members)
			}
			if got := read(`SELECT count(*) FROM subscriptions WHERE target_type = 'project' AND target_id = $1`,
				tc.project); got != tc.subscribers {
				t.Errorf("project subscribers = %d, want %d", got, tc.subscribers)
			}
			// The counter-proof's subject and the version carrying its
			// evidence — the two versions it seeds in each of these projects.
			if got := read(`SELECT count(*) FROM scientific_objects WHERE project_id = $1`, tc.project); got != tc.objects {
				t.Errorf("objects = %d, want %d", got, tc.objects)
			}
			if got := read(`SELECT count(*) FROM projects WHERE id = $1 AND organization_id IS NOT NULL`,
				tc.project); got != tc.org {
				t.Errorf("organization attached = %d, want %d", got, tc.org)
			}
		})
	}
	if got := read(`SELECT count(*) FROM subscriptions WHERE target_type = 'user' AND target_id = $1`, f.aliceID); got != 25 {
		t.Errorf("subscribers to the author = %d, want 25", got)
	}
	// The counter-proof's corpus is exactly four versions: one subject and
	// one evidence version per project. Fewer would mean seedPrestigePair
	// silently wrote nothing, and the numbers above would be measuring an
	// empty fixture.
	if got := read(`SELECT count(*) FROM scientific_object_versions sov
	                 JOIN scientific_objects so ON so.id = sov.object_id
	                 WHERE so.project_id IN ($1, $2)`, f.small, f.famous); got != 4 {
		t.Errorf("the pair's four versions = %d, want 4", got)
	}
}

// --------------------------------------------------------------------------
// The scope, enforced in SQL

// TestRankingFactsAreScopedAndFailClosed runs the fact read against a version
// outside the caller's scope, under two scopes, and then shows the row is
// genuinely there.
//
// The three answers have to be different, and they are the whole reason the
// port distinguishes them:
//
//   - the version's own project in scope → a row, with the counts;
//   - a scope that does not include it → NO row (not a row of zeros), which
//     the ranking renders as `unknown`;
//   - an EMPTY, authenticated scope → also no row: the read fails closed on
//     its own, so a caller who resolves a scope for a user in no project gets
//     nothing rather than everything.
//
// The unwrapped query is then run, to show the row IS there and the predicate
// is what withholds it. Without that half, "no row" could be a fixture that
// seeded nothing.
func TestRankingFactsAreScopedAndFailClosed(t *testing.T) {
	ctx := testCtx(t)
	f := newRankingFixture(t, ctx)

	bobVersion, bobState, _ := f.seedVersion(t, f.other, f.bobID, "claim", "bob's CO2 claim", "bob")
	bobEvidence, _, _ := f.seedVersion(t, f.other, f.bobID, "experiment", "bob's experiment", "bob-ev")
	f.assert(t, assertion{project: f.other, owner: f.bobID, state: bobState, target: bobVersion,
		evidence: bobEvidence, relation: "supports", review: "reviewed", visibility: "private", directness: "direct"})
	f.review(t, f.other, f.bobID, bobState, f.carolID, "approved")

	// Alice's scope: her own two projects, not bob's private one.
	if got := len(f.factsFor(t, f.scopeFor(t, f.aliceID), bobVersion)); got != 0 {
		t.Errorf("alice's scope returned %d rows for bob's private version, want 0", got)
	}
	// Bob's scope: the row, with the facts.
	rows := f.factsFor(t, f.scopeFor(t, f.bobID), bobVersion)
	if len(rows) != 1 {
		t.Fatalf("bob's scope returned %d rows for his own version, want 1", len(rows))
	}
	if rows[0].EvidenceAssertions != 1 || rows[0].ReviewsApproved != 1 || !rows[0].IsNewest {
		t.Errorf("bob's own fact sheet = %+v, want 1 assertion, 1 approval, newest", rows[0])
	}

	// An authenticated actor in NO project: a real scope, the public floor,
	// and still nothing of bob's — whose rows are private.
	empty := f.scopeFor(t, f.carolID)
	if !empty.Authenticated() || len(empty.AllowedProjectIDs()) != 0 {
		t.Fatalf("the empty scope is not the state this test needs: %v", empty.AllowedProjectIDs())
	}
	if got := len(f.factsFor(t, empty, bobVersion)); got != 0 {
		t.Errorf("an empty scope returned %d rows, want 0: the read must fail closed", got)
	}

	// And the row is genuinely there: the unwrapped read — the same joins with
	// the project predicates removed — finds it. The predicate is what stopped
	// it, and that is observed here rather than argued.
	var unwrapped int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*)
		FROM scientific_object_versions sov
		JOIN scientific_objects so ON so.id = sov.object_id
		JOIN LATERAL (
		  SELECT count(*) AS n FROM evidence_assertions ea WHERE ea.target_object_version_id = sov.id
		) ev ON true
		WHERE sov.id = $1::uuid`, bobVersion).Scan(&unwrapped); err != nil {
		t.Fatalf("run the unwrapped fact read: %v", err)
	}
	if unwrapped != 1 {
		t.Fatalf("the unwrapped read returned %d rows for bob's version, want 1: this test's premise does not hold", unwrapped)
	}
}

// TestRankingReportsOutOfScopeAsUnknown closes the loop between the store's
// refusal and the ranking's level, over the one situation where it really
// happens, driven the way it really happens: a PUBLIC document, published by
// a project alice is not a member of, is recalled by her real retrieval (a
// document is read by its own visibility) while the object version behind it
// is outside her scope — so the traversal's seed step refuses it and the
// candidate arrives at the ranker with NO version pin.
//
// Two things are pinned, both on the real path. The retrieval's own output
// must carry the candidate with an EMPTY ObjectVersionID (the seed refusal,
// observed rather than assumed), and the ranking must then resolve the pid
// through the publication map to the version that EXISTS and report every
// database-backed factor as `unknown` with the scope as the reason — never
// `unversioned`, which would claim there is no version, and never the benign
// level, which would claim the other project asserted nothing.
func TestRankingReportsOutOfScopeAsUnknown(t *testing.T) {
	ctx := testCtx(t)
	f := newRankingFixture(t, ctx)

	version, state, _ := f.seedVersion(t, f.open, f.bobID, "claim", "bob's published CO2 claim", "open-claim")
	ownEvidence, _, _ := f.seedVersion(t, f.open, f.bobID, "experiment", "bob's experiment", "open-ev")
	f.assert(t, assertion{project: f.open, owner: f.bobID, state: state, target: version,
		evidence: ownEvidence, relation: "supports", review: "reviewed", visibility: "public", directness: "direct"})

	const pid = "8123456789abcdefghjkmnpqrs"
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO knowledge_publications (object_version_id, public_version, rights_json, published_by, pid)
		 VALUES ($1, 'v1', $2::jsonb, $3, $4)`, version, seedRightsJSON(t), f.bobID, pid); err != nil {
		t.Fatalf("publish: %v", err)
	}
	// The projected document, written with the columns T0901's projection
	// writes: a public project published it, so the row is public and alice
	// may read it although she is in no project of bob's.
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO search_documents
		   (entity_ref, entity_type, visibility, project_id, title, content, structured)
		 VALUES ('knowledge:' || $1, 'knowledge', 'public', $2, 'bob''s published CO2 claim',
		         'bob''s published CO2 claim', '{}'::jsonb)`, pid, f.open); err != nil {
		t.Fatalf("seed the projected document: %v", err)
	}
	// The premise: alice's scope does NOT cover the public project (scope is
	// membership, and a public row's readability is a different axis), while
	// the document itself is one she may read.
	alice := f.scopeFor(t, f.aliceID)
	if len(f.factsFor(t, alice, version)) != 0 {
		t.Fatal("alice's scope read the public project's version: this test's premise does not hold")
	}
	var readable int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM search_documents WHERE entity_ref = $1 AND (visibility = 'public' OR project_id = ANY($2::uuid[]))`,
		"knowledge:"+pid, alice.AllowedProjectIDs()).Scan(&readable); err != nil {
		t.Fatalf("check the document's visibility: %v", err)
	}
	if readable != 1 {
		t.Fatalf("alice cannot read the document either, so this test's premise does not hold: "+
			"the ranking would never be handed this candidate (%d rows)", readable)
	}

	// The REAL retriever, not a scripted candidate: the candidate the ranking
	// sees is whatever the retrieval actually hands over.
	store, err := retrieval.NewSQLStore(f.pool)
	if err != nil {
		t.Fatalf("retrieval.NewSQLStore: %v", err)
	}
	retriever, err := retrieval.NewRetriever(store, nil, retrieval.WithLogger(silentLogger()))
	if err != nil {
		t.Fatalf("retrieval.NewRetriever: %v", err)
	}
	req := retrieval.Request{Query: "published CO2 claim"}
	found, err := retriever.Retrieve(ctx, alice, req)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(found.Candidates) != 1 {
		t.Fatalf("the retrieval recalled %d candidates, want exactly the one public document", len(found.Candidates))
	}
	cand := found.Candidates[0]
	if cand.Ref != "knowledge:"+pid {
		t.Fatalf("the retrieval recalled %s, want knowledge:%s", cand.Ref, pid)
	}
	// The seed refusal, observed: the document is alice's to read, the version
	// behind it is not, so the traversal pinned nothing.
	if cand.ObjectVersionID != "" {
		t.Fatalf("the retrieval pinned the out-of-scope version %s on the candidate, want the seed refusal (\"\")",
			cand.ObjectVersionID)
	}

	ranker, err := ranking.NewRanker(f.store)
	if err != nil {
		t.Fatalf("ranking.NewRanker: %v", err)
	}
	got, err := ranker.Rank(ctx, alice, req, found)
	if err != nil {
		t.Fatalf("Rank: %v", err)
	}
	if len(got.Ranked) != 1 {
		t.Fatalf("%d ranked, want the one candidate", len(got.Ranked))
	}
	rc := got.Ranked[0]
	// The ranking resolves the pid through the publication map, so the output
	// names the version that EXISTS — and says it could not be read.
	if rc.ObjectVersionID != version {
		t.Errorf("the ranked candidate resolves to %q, want the version its publication pins (%q)",
			rc.ObjectVersionID, version)
	}
	for _, a := range rc.Factors {
		if a.Factor == ranking.FactorQueryScopeMatch {
			continue
		}
		if a.Level != ranking.LevelUnknown {
			t.Errorf("factor %s reported %q for a version outside the caller's scope, want %q — "+
				"'none' would claim the other project asserted nothing, which this read cannot know",
				a.Factor, a.Level, ranking.LevelUnknown)
		}
		if a.Factor == ranking.FactorVersion && a.Level == ranking.LevelUnversioned {
			t.Error("the version factor reports the candidate as `unversioned`: " +
				"the version EXISTS, and `unversioned` even outranks `unknown` on the freshness ladder")
		}
		if !strings.Contains(a.Reason, "not in the caller's project scope") {
			t.Errorf("factor %s's reason does not say the version was unread: %q", a.Factor, a.Reason)
		}
	}
	if len(rc.Labels) != 0 {
		t.Errorf("an unread version carries labels: %v", rc.Labels)
	}
}

// --------------------------------------------------------------------------
// The two layers compose

// TestRankingOverRetrievalOutput drives the real retriever and then the real
// ranker over one corpus: the boundary between T0904 and T0905, with the
// version resolution done twice (once by the traversal's seed step, bounded by
// its seed budget, once by the ranking for every candidate).
func TestRankingOverRetrievalOutput(t *testing.T) {
	ctx := testCtx(t)
	f := newRankingFixture(t, ctx)
	scope := f.scopeFor(t, f.aliceID)

	claim, claimState, _ := f.seedVersion(t, f.small, f.aliceID, "claim", "Mg-MOF-74 CO2 uptake claim", "det-claim")
	material, materialState, _ := f.seedVersion(t, f.small, f.aliceID, "material", "Mg-MOF-74 framework sample", "det-material")
	finding, _, _ := f.seedVersion(t, f.small, f.aliceID, "finding", "the finding behind the claim", "det-finding")
	f.assert(t, assertion{project: f.small, owner: f.aliceID, state: claimState, target: claim,
		evidence: finding, relation: "supports", review: "reviewed", visibility: "private", directness: "direct"})
	f.relate(t, f.small, f.aliceID, materialState, "performed_on", finding, material)

	const claimPID = "9123456789abcdefghjkmnpqrs"
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO knowledge_publications (object_version_id, public_version, rights_json, published_by, pid)
		 VALUES ($1, 'v1', $2::jsonb, $3, $4)`, claim, seedRightsJSON(t), f.aliceID, claimPID); err != nil {
		t.Fatalf("publish: %v", err)
	}
	// The projection is T0901's, not this task's, and re-running it is not
	// what this test is about: the document row is written directly, with the
	// columns the projection writes (search_documents' own list). The
	// document is public because the publication it indexes is; alice is in
	// the project either way.
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO search_documents
		   (entity_ref, entity_type, visibility, project_id, title, content, structured)
		 VALUES ('knowledge:' || $1, 'knowledge', 'public', $2,
		         'Mg-MOF-74 CO2 uptake claim', 'Mg-MOF-74 CO2 uptake claim', '{}'::jsonb)`,
		claimPID, f.small); err != nil {
		t.Fatalf("seed the projected document: %v", err)
	}

	store, err := retrieval.NewSQLStore(f.pool)
	if err != nil {
		t.Fatalf("retrieval.NewSQLStore: %v", err)
	}
	retriever, err := retrieval.NewRetriever(store, nil, retrieval.WithLogger(silentLogger()))
	if err != nil {
		t.Fatalf("retrieval.NewRetriever: %v", err)
	}
	req := retrieval.Request{Query: "Mg-MOF-74 CO2 uptake"}
	found, err := retriever.Retrieve(ctx, scope, req)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(found.Candidates) == 0 {
		t.Fatal("the retrieval recalled nothing: this test would then be about an empty ranking")
	}

	ranker, err := ranking.NewRanker(f.store)
	if err != nil {
		t.Fatalf("ranking.NewRanker: %v", err)
	}
	ranked, err := ranker.Rank(ctx, scope, req, found)
	if err != nil {
		t.Fatalf("Rank: %v", err)
	}

	// Exactly the retrieval's candidates, no more and no fewer, each ranked
	// once and each keeping the version the retrieval pinned.
	if len(ranked.Ranked) != len(found.Candidates) {
		t.Fatalf("%d ranked, want the %d the retrieval returned", len(ranked.Ranked), len(found.Candidates))
	}
	seen := map[string]bool{}
	for i, rc := range ranked.Ranked {
		if seen[rc.Ref] {
			t.Errorf("%s appears twice in the ranking", rc.Ref)
		}
		seen[rc.Ref] = true
		if rc.Rank != i+1 {
			t.Errorf("rank %d of %d reports Rank = %d", i+1, len(ranked.Ranked), rc.Rank)
		}
	}
	for _, c := range found.Candidates {
		if !seen[c.Ref] {
			t.Errorf("the retrieval returned %s and the ranking dropped it", c.Ref)
		}
		if !strings.Contains(c.Ref, claimPID) {
			continue
		}
		for _, rc := range ranked.Ranked {
			if rc.Ref == c.Ref && rc.ObjectVersionID != claim {
				t.Errorf("the ranking resolved the published claim to %s, want the version the retrieval pinned (%s)",
					rc.ObjectVersionID, claim)
			}
		}
	}

	// The published claim carries reviewed evidence and the material carries
	// none, so the claim must lead — over a real retrieve, not a scripted
	// candidate list.
	var claimRank, materialRank int
	for _, rc := range ranked.Ranked {
		switch rc.ObjectVersionID {
		case claim:
			claimRank = rc.Rank
		case material:
			materialRank = rc.Rank
		}
	}
	if claimRank == 0 {
		t.Fatalf("the published claim is not in the ranking at all:\n%s", describeFactors(ranked))
	}
	if materialRank != 0 && claimRank > materialRank {
		t.Errorf("the claim (rank %d) is below the material (rank %d), although the claim is the one with reviewed evidence:\n%s",
			claimRank, materialRank, describeFactors(ranked))
	}
}

// --------------------------------------------------------------------------
// Helpers

// facts reads one version's fact sheet through the store, failing if it does
// not come back: every caller here seeds a version it expects to be readable.
func (f *rankingFixture) facts(t *testing.T, scope search.Scope, versionID string) ranking.VersionFacts {
	t.Helper()
	rows := f.factsFor(t, scope, versionID)
	if len(rows) != 1 {
		t.Fatalf("the store returned %d fact sheets for %s, want 1", len(rows), versionID)
	}
	return rows[0]
}

func (f *rankingFixture) factsFor(t *testing.T, scope search.Scope, versionID string) []ranking.VersionFacts {
	t.Helper()
	rows, err := f.store.Factors(f.ctx, scope, []string{versionID})
	if err != nil {
		t.Fatalf("Factors(%s): %v", versionID, err)
	}
	return rows
}

// rankingCandidate builds the candidate shape the traversal produces for a
// scientific object version: a graph candidate pinned to the version.
func rankingCandidate(versionID, objectID, ref, title string) retrieval.Candidate {
	return retrieval.Candidate{
		Ref:             ref,
		Kind:            retrieval.KindObjectVersion,
		Identity:        objectID,
		Title:           title,
		ObjectID:        objectID,
		ObjectVersionID: versionID,
		Signals:         []retrieval.SignalHit{{Signal: retrieval.SignalGraph, Rank: 1}},
	}
}

// equalAssessments reports whether two ranked candidates carry the same level
// on all six factors — the tie the prestige counter-proof depends on.
func equalAssessments(a, b ranking.Ranked) bool {
	if len(a.Factors) != len(b.Factors) {
		return false
	}
	for i := range a.Factors {
		if a.Factors[i].LevelRank != b.Factors[i].LevelRank {
			return false
		}
	}
	return true
}

// describeFactors renders a ranking as one line per candidate, so a failure
// says which levels differed rather than only that something did.
func describeFactors(res ranking.Result) string {
	var b strings.Builder
	for _, rc := range res.Ranked {
		fmt.Fprintf(&b, "%d %s\n", rc.Rank, rc.Ref)
		for _, a := range rc.Factors {
			fmt.Fprintf(&b, "    %-18s %s\n", a.Factor, a.Level)
		}
	}
	return b.String()
}
