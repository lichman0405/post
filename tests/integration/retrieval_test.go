// Task T0904 — required test "retrieval integration".
//
// The unit suite (internal/search/retrieval/retrieval_test.go) drives a fake
// store, so everything it proves is about the pipeline's own behaviour. What
// only a real PostgreSQL can settle is what this file is for:
//
//  1. THE SEED QUERY RECALLS CLAIMS, MATERIALS AND FINDINGS, from a corpus
//     the REAL projection produced (events → outbox → dispatcher →
//     projector, internal/search), not from rows this test typed by hand.
//     The facet keys a candidate reads its version pin from are therefore the
//     projection's own (internal/search/sources.go), which is the claim
//     internal/search/pin.go makes and can only make here.
//
//  2. THE GRAPH EXPANSION WORKS OVER REAL relations / relation_versions, and
//     the candidate it produces is pinned to an object VERSION, reached
//     through a hop that says which relation type and which direction.
//
//  3. NO PRIVATE LEAK, in both directions and at every surface: the actor's
//     own rows ARE returned (without which "no leak" is satisfied by
//     returning nothing), and another project's private rows are NOT — not
//     as a document candidate, not as a graph node, not as a hop endpoint,
//     and not as an id anywhere in the result. The same question is used for
//     both halves, so authorization is the only thing that separates them.
//
//  4. THE READ QUERIES FAIL CLOSED ON THEIR OWN. Each of the three document
//     queries is run with an EMPTY project scope and must return exactly the
//     public rows — never the table. And SearchDocumentsFullText, with no
//     narrowing, must return the SAME rows in the same order as the
//     canonical SearchDocuments: the access predicate was copied verbatim,
//     and a copy that drifts is the failure mode the copy invites.
//
//  5. THE AUTHORIZATION IS IN THE SQL, and this file proves it by trying to
//     break it. Two counter-proofs run unwrapped versions of the traversal's
//     two queries — the same SQL without its project predicates — and show
//     that another project's version id DOES come back: the predicates are
//     what stops it, and the leak they would cause is observed rather than
//     argued.
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/sqlc"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/search"
	"github.com/lichman0405/post/internal/search/embedding"
	"github.com/lichman0405/post/internal/search/planner"
	"github.com/lichman0405/post/internal/search/retrieval"
)

// retrievalTaskID namespaces this file's test databases (docs/66 §3).
const retrievalTaskID = "T0904"

// --------------------------------------------------------------------------
// Fixture

// retrievalFixture is one real database with the production projection
// pipeline wired over it and the production retrieval store over that. It
// holds three projects: alice's (private), bob's (private) and a public one
// alice is not a member of — the three cases authorization has to tell apart.
type retrievalFixture struct {
	ctx        context.Context
	pool       *pgxpool.Pool
	q          *sqlc.Queries
	dispatcher *events.Dispatcher
	projector  *search.Projector

	aliceID string
	bobID   string
	orgID   string

	aliceProject  string
	bobProject    string
	publicProject string
}

func newRetrievalFixture(t *testing.T, ctx context.Context) *retrievalFixture {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), retrievalTaskID)
	f := &retrievalFixture{
		ctx:        ctx,
		pool:       pool,
		q:          sqlc.New(pool),
		dispatcher: events.NewDispatcher(pool, events.WithLogger(silentLogger())),
		projector:  search.NewProjector(pool, search.WithLogger(silentLogger())),
	}
	f.aliceID = mustQueryUUID(t, ctx, pool,
		`INSERT INTO users (handle, display_name) VALUES ('ret-alice','Alice') RETURNING id`)
	f.bobID = mustQueryUUID(t, ctx, pool,
		`INSERT INTO users (handle, display_name) VALUES ('ret-bob','Bob') RETURNING id`)
	f.orgID = mustQueryUUID(t, ctx, pool,
		`INSERT INTO organizations (slug, name) VALUES ('ret-lab','Retrieval Lab') RETURNING id`)
	f.aliceProject = f.seedProject(t, "ret-alice-lab", "private", f.aliceID)
	f.bobProject = f.seedProject(t, "ret-bob-lab", "private", f.bobID)
	f.publicProject = f.seedProject(t, "ret-open", "public", f.bobID)
	return f
}

func (f *retrievalFixture) seedProject(t *testing.T, slug, visibility string, members ...string) string {
	t.Helper()
	id := mustQueryUUID(t, f.ctx, f.pool,
		`INSERT INTO projects (organization_id, slug, name, purpose, visibility, created_by)
		 VALUES ($1, $2, $2, 'T0904 retrieval fixture', $3, $4) RETURNING id`,
		f.orgID, slug, visibility, f.aliceID)
	for _, m := range members {
		if _, err := f.pool.Exec(f.ctx,
			`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, 'owner')`,
			id, m); err != nil {
			t.Fatalf("seed membership on %s: %v", slug, err)
		}
	}
	return id
}

// seedObject writes one scientific object with one version on its own branch
// — branches is UNIQUE(project_id, name) — and returns the version's id.
func (f *retrievalFixture) seedObject(t *testing.T, projectID, owner, objectType, title, name string) string {
	t.Helper()
	objectID := mustQueryUUID(t, f.ctx, f.pool,
		`INSERT INTO scientific_objects (project_id, object_type, created_by)
		 VALUES ($1, $2, $3) RETURNING id`, projectID, objectType, owner)
	branchID := mustQueryUUID(t, f.ctx, f.pool,
		`INSERT INTO branches (project_id, name, visibility, git_ref, created_by)
		 VALUES ($1, $2, 'private', 'refs/heads/' || $2, $3) RETURNING id`,
		projectID, name, owner)
	stateID := mustQueryUUID(t, f.ctx, f.pool,
		`INSERT INTO project_states (project_id, branch_id, state_hash, git_commit_sha, manifest_version)
		 VALUES ($1, $2, 'h-' || gen_random_uuid()::text, 'sha-' || gen_random_uuid()::text, '1')
		 RETURNING id`, projectID, branchID)
	return mustQueryUUID(t, f.ctx, f.pool,
		`INSERT INTO scientific_object_versions
		   (object_id, version_no, state_id, schema_id, schema_version, title, lifecycle_state,
		    payload, integrity_hash, created_by)
		 VALUES ($1, 1, $2, $3, '1', $4, 'active', '{}'::jsonb, 'sha256:seed', $5)
		 RETURNING id`, objectID, stateID, objectType, title, owner)
}

// publish writes the publication row for a version. The pid is the identity
// every later step addresses it by: the projection's entity_ref, the graph
// seed, the citation.
func (f *retrievalFixture) publish(t *testing.T, versionID, pid, owner string) {
	t.Helper()
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO knowledge_publications (object_version_id, public_version, rights_json, published_by, pid)
		 VALUES ($1, 'v1', $2::jsonb, $3, $4)`,
		versionID, seedRightsJSON(t), owner, pid); err != nil {
		t.Fatalf("publish %s: %v", pid, err)
	}
}

// project drives the production projection pipeline for one publication: the
// event goes into the outbox, the real dispatcher publishes it, the real
// projector consumes it. Nothing here writes search_documents directly — the
// rows this test then retrieves over are the projection's own output.
func (f *retrievalFixture) project(t *testing.T, projectID, visibility, pid string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"publication_id": pid})
	if err != nil {
		t.Fatalf("render payload: %v", err)
	}
	if err := events.Record(f.ctx, f.pool, events.Event{
		EventType:     search.EventTypeKnowledgeVersionPublished,
		ActorID:       f.aliceID,
		ProjectID:     projectID,
		Visibility:    visibility,
		CorrelationID: "T0904-" + pid,
		Payload:       body,
	}); err != nil {
		t.Fatalf("record knowledge.version_published for %s: %v", pid, err)
	}
	if _, err := f.dispatcher.RunOnce(f.ctx); err != nil {
		t.Fatalf("dispatcher RunOnce: %v", err)
	}
	if _, err := f.projector.RunOnce(f.ctx); err != nil {
		t.Fatalf("projector RunOnce: %v", err)
	}
}

// relate writes one relation and its version 1 between two object versions,
// OWNED BY projectID. The relation's project is what the hop query filters
// on, and it is deliberately not always the endpoints' project — see the
// cross-project edge below.
func (f *retrievalFixture) relate(t *testing.T, projectID, owner, relationType, sourceVersionID, targetVersionID string) {
	t.Helper()
	var stateID string
	if err := f.pool.QueryRow(f.ctx,
		`SELECT state_id::text FROM scientific_object_versions WHERE id = $1::uuid`,
		sourceVersionID).Scan(&stateID); err != nil {
		t.Fatalf("read the state of %s: %v", sourceVersionID, err)
	}
	relationID := mustQueryUUID(t, f.ctx, f.pool,
		`INSERT INTO relations (project_id) VALUES ($1) RETURNING id`, projectID)
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO relation_versions
		   (relation_id, version_no, state_id, relation_type,
		    source_object_version_id, target_object_version_id, payload, integrity_hash, created_by)
		 VALUES ($1, 1, $2, $3, $4, $5, '{}'::jsonb, 'sha256:rel', $6)`,
		relationID, stateID, relationType, sourceVersionID, targetVersionID, owner); err != nil {
		t.Fatalf("seed relation %s: %v", relationType, err)
	}
}

// retrievalCorpus is what the fixture seeded, by name, so the assertions can
// talk about "the claim" rather than about uuids.
type retrievalCorpus struct {
	claimPID, materialPID, findingPID, experimentPID string
	claimVersion, materialVersion                    string
	findingVersion, experimentVersion                string
	secretPID, secretVersion                         string
	bobMaterialVersion                               string
	publicPID                                        string
}

// seedRetrievalCorpus builds the corpus every assertion in this file is
// written against.
//
// Alice's project: a claim, the material it is about, the finding that
// supports it, and the experiment that produced the finding on that material.
// The three relation types are the catalog's own words for exactly those
// connections (internal/rsg/relationcatalog): "supports the target claim",
// "produces the target object version as an output", "was performed on the
// target material or sample".
//
// Bob's project: a claim whose title carries the SAME distinctive term as
// alice's, so one query recalls both and only authorization separates them —
// plus a material, plus a relation OWNED BY BOB'S PROJECT between bob's claim
// and ALICE's claim. That last edge is the sharp case: it touches an object
// alice may read, so a hop query that filtered only the relation's project
// (or only the near endpoint) would hand alice bob's version id.
//
// The public project: a publication alice may READ (the projection indexes it
// public) but is not a member of, so it must not be expanded.
func seedRetrievalCorpus(t *testing.T, f *retrievalFixture) retrievalCorpus {
	t.Helper()
	// 26 Crockford base32 characters, the shape 00083's
	// knowledge_publications_pid_format requires; the leading digit is what
	// tells them apart.
	var c retrievalCorpus
	c.claimPID = "0123456789abcdefghjkmnpqrs"
	c.materialPID = "1123456789abcdefghjkmnpqrs"
	c.findingPID = "2123456789abcdefghjkmnpqrs"
	c.experimentPID = "3123456789abcdefghjkmnpqrs"
	c.secretPID = "4123456789abcdefghjkmnpqrs"
	c.publicPID = "5123456789abcdefghjkmnpqrs"

	c.claimVersion = f.seedObject(t, f.aliceProject, f.aliceID, "claim", "Mg-MOF-74 CO2 uptake claim", "claim-branch")
	c.materialVersion = f.seedObject(t, f.aliceProject, f.aliceID, "material", "Mg-MOF-74 framework sample", "material-branch")
	c.findingVersion = f.seedObject(t, f.aliceProject, f.aliceID, "finding", "Mg-MOF-74 CO2 uptake finding at 298 K", "finding-branch")
	c.experimentVersion = f.seedObject(t, f.aliceProject, f.aliceID, "experiment", "Mg-MOF-74 uptake experiment", "experiment-branch")
	c.secretVersion = f.seedObject(t, f.bobProject, f.bobID, "claim", "Mg-MOF-74 secret claim", "secret-branch")
	c.bobMaterialVersion = f.seedObject(t, f.bobProject, f.bobID, "material", "Mg-MOF-74 secret material", "secret-material-branch")

	for pid, version := range map[string]string{
		c.claimPID: c.claimVersion, c.materialPID: c.materialVersion,
		c.findingPID: c.findingVersion, c.experimentPID: c.experimentVersion,
	} {
		f.publish(t, version, pid, f.aliceID)
	}
	f.publish(t, c.secretVersion, c.secretPID, f.bobID)
	// The public publication: a public project, the rights model's own
	// fail-closed default and no pinned visibility policy, which is the one
	// combination knowledgepublish.AudienceFor resolves as AudienceNetwork —
	// the same rule the read path asks, so the projection and the read path
	// cannot disagree about whether this row is public.
	publicVersion := f.seedObject(t, f.publicProject, f.bobID, "claim", "Mg-MOF-74 public claim", "public-branch")
	f.publish(t, publicVersion, c.publicPID, f.bobID)

	f.relate(t, f.aliceProject, f.aliceID, "supports", c.findingVersion, c.claimVersion)
	f.relate(t, f.aliceProject, f.aliceID, "produces", c.experimentVersion, c.findingVersion)
	f.relate(t, f.aliceProject, f.aliceID, "performed_on", c.experimentVersion, c.materialVersion)
	// The cross-project edge, owned by BOB's project: bob's claim contradicts
	// alice's. Alice may read her own endpoint and must not learn bob's.
	f.relate(t, f.bobProject, f.bobID, "contradicts", c.secretVersion, c.claimVersion)

	for _, one := range []struct {
		pid        string
		projectID  string
		visibility string
	}{
		{c.claimPID, f.aliceProject, "private"},
		{c.materialPID, f.aliceProject, "private"},
		{c.findingPID, f.aliceProject, "private"},
		{c.experimentPID, f.aliceProject, "private"},
		{c.secretPID, f.bobProject, "private"},
		{c.publicPID, f.publicProject, "public"},
	} {
		f.project(t, one.projectID, one.visibility, one.pid)
	}

	// The fixture verifies its own output before any assertion is made about
	// the retrieval's use of it: a retrieval test that cannot tell "the
	// projection wrote nothing" from "the recall dropped it" is a test of
	// neither. The rows are read directly, from the table.
	landed := map[string]string{}
	rows, err := f.pool.Query(f.ctx,
		`SELECT entity_ref, visibility || ' ' || COALESCE(project_id::text, '') FROM search_documents`)
	if err != nil {
		t.Fatalf("read the projected rows: %v", err)
	}
	for rows.Next() {
		var ref, where string
		if err := rows.Scan(&ref, &where); err != nil {
			t.Fatalf("scan projected row: %v", err)
		}
		landed[ref] = where
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate projected rows: %v", err)
	}
	for _, one := range []struct {
		pid, projectID, visibility string
	}{
		{c.claimPID, f.aliceProject, "private"},
		{c.materialPID, f.aliceProject, "private"},
		{c.findingPID, f.aliceProject, "private"},
		{c.experimentPID, f.aliceProject, "private"},
		{c.secretPID, f.bobProject, "private"},
		{c.publicPID, f.publicProject, "public"},
	} {
		ref := search.EntityRef(search.EntityKnowledge, one.pid)
		if got, ok := landed[ref]; !ok {
			t.Errorf("the projection did not land %s; the table holds %v", ref, landed)
		} else if got != one.visibility+" "+one.projectID {
			t.Errorf("%s projected as %q, want %q", ref, got, one.visibility+" "+one.projectID)
		}
	}
	if len(landed) != 6 {
		t.Errorf("the projection landed %d rows, want the corpus's 6: %v", len(landed), landed)
	}
	return c
}

// aliceScope resolves alice's scope the way the search path resolves it:
// from the actor, through the real project store, with no other input.
func (f *retrievalFixture) aliceScope(t *testing.T) search.Scope {
	t.Helper()
	scope, err := search.ResolveScope(f.ctx, persistence.NewProjectStore(f.pool), f.aliceID)
	if err != nil {
		t.Fatalf("ResolveScope: %v", err)
	}
	if !scope.Authenticated() {
		t.Fatal("the resolved scope is not attributable to an actor")
	}
	if got := scope.AllowedProjectIDs(); len(got) != 1 || got[0] != f.aliceProject {
		t.Fatalf("resolved scope = %v, want exactly alice's membership [%s]: "+
			"naming the public project here would admit its members-only rows", got, f.aliceProject)
	}
	return scope
}

// embedTheCorpus fills the vectors through the production batch job, so the
// vector signal has something to compare against and the provenance the read
// filters on is the worker's own.
func (f *retrievalFixture) embedTheCorpus(t *testing.T) {
	t.Helper()
	report, err := mustEmbeddingWorker(t, f.pool, mustEmbedder(t, embedding.LocalModel)).RunOnce(f.ctx)
	if err != nil {
		t.Fatalf("embedding worker RunOnce: %v", err)
	}
	// The fixture's corpus is six publications and the default batch is 32,
	// so a single pass covers it — and the count is asserted because every
	// assertion below that reads a vector depends on it.
	if report.Embedded != 6 {
		t.Fatalf("the embedding pass wrote %d vectors, want the corpus's 6: %+v", report.Embedded, report)
	}
}

// --------------------------------------------------------------------------
// Reading a result

// retrievalRefs lists a result's candidate refs, in order.
func retrievalRefs(res retrieval.Result) []string {
	out := make([]string, 0, len(res.Candidates))
	for _, c := range res.Candidates {
		out = append(out, c.Ref)
	}
	return out
}

// hasRef reports whether any candidate ref starts with prefix. Candidate refs
// carry a version suffix ("kind:identity@version"), so the comparison is by
// prefix on the identity.
func hasRef(res retrieval.Result, prefix string) bool {
	for _, c := range res.Candidates {
		if strings.HasPrefix(c.Ref, prefix) {
			return true
		}
	}
	return false
}

// findCandidate returns the first candidate whose ref has the given prefix, or
// fails the test.
func findCandidate(t *testing.T, res retrieval.Result, prefix string) retrieval.Candidate {
	t.Helper()
	for _, c := range res.Candidates {
		if strings.HasPrefix(c.Ref, prefix) {
			return c
		}
	}
	t.Fatalf("no candidate with ref prefix %q; candidates = %v", prefix, retrievalRefs(res))
	return retrieval.Candidate{}
}

// candidateOfType returns the candidate of the given object type in the given
// project, or fails.
func candidateOfType(t *testing.T, res retrieval.Result, objectType, projectID string) retrieval.Candidate {
	t.Helper()
	for _, c := range res.Candidates {
		if c.ObjectType == objectType && c.ProjectID == projectID {
			return c
		}
	}
	t.Fatalf("no %s candidate in project %s; candidates = %+v", objectType, projectID, res.Candidates)
	return retrieval.Candidate{}
}

// --------------------------------------------------------------------------
// The required test

func TestRetrievalRecallsSeedsAndExpandsWithoutPrivateLeak(t *testing.T) {
	ctx := testCtx(t)
	f := newRetrievalFixture(t, ctx)
	corpus := seedRetrievalCorpus(t, f)
	f.embedTheCorpus(t)

	store, err := retrieval.NewSQLStore(f.pool)
	if err != nil {
		t.Fatalf("NewSQLStore: %v", err)
	}
	// The same embedder the batch job used: a query vector from another model
	// is not a weaker match, it is not a match at all (embedding.Model, and
	// SearchDocumentsByVector's provenance predicate).
	r, err := retrieval.NewRetriever(store, mustEmbedder(t, embedding.LocalModel),
		retrieval.WithLogger(silentLogger()))
	if err != nil {
		t.Fatalf("NewRetriever: %v", err)
	}
	scope := f.aliceScope(t)

	// ------------------------------------------------------------------
	// 1. The seed query recalls the actor's Claims, Materials and Findings.
	//
	// One question, asked once: "Mg-MOF-74" occurs in every title in the
	// corpus — including bob's and the public one — so this single result is
	// where both the recall claim and the leak claim are decided.
	res, err := r.Retrieve(ctx, scope, retrieval.Request{Query: "Mg-MOF-74"})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	refs := retrievalRefs(res)
	// The signals are carried in every failure below: "nothing was recalled"
	// and "the recall ran and matched nothing" are different faults, and a
	// message that cannot tell them apart sends its reader to the wrong
	// half of the pipeline.
	why := fmt.Sprintf("candidates = %v, signals = %+v", refs, res.Signals)
	for _, want := range []struct{ name, pid string }{
		{"claim", corpus.claimPID},
		{"material", corpus.materialPID},
		{"finding", corpus.findingPID},
		{"experiment", corpus.experimentPID},
	} {
		ref := search.EntityRef(search.EntityKnowledge, want.pid)
		if !hasRef(res, ref) {
			t.Errorf("the seed query did not recall the %s (%s); %s", want.name, ref, why)
			continue
		}
		cand := findCandidate(t, res, ref)
		if cand.ObjectType == "" {
			t.Errorf("the %s candidate carries no object_type: the projection's facet was not read back", want.name)
		}
		if !cand.Pinned() {
			t.Errorf("the %s candidate is not version-pinned: %+v", want.name, cand)
		}
	}
	// The version label is the projection's own — the publication's
	// public_version — read back through search.PinnedVersion.
	claim := findCandidate(t, res, search.EntityRef(search.EntityKnowledge, corpus.claimPID))
	if claim.Version != "v1" {
		t.Errorf("the claim's version = %q, want the public_version the projection wrote", claim.Version)
	}

	// Every signal that has something to run on ran, and the one that does
	// not says so rather than being absent.
	signals := map[string]retrieval.SignalReport{}
	for _, s := range res.Signals {
		signals[s.Signal] = s
	}
	for _, name := range []string{retrieval.SignalFullText, retrieval.SignalVector, retrieval.SignalGraph} {
		if !signals[name].Ran {
			t.Errorf("signal %q did not run: %+v", name, signals[name])
		}
	}
	if signals[retrieval.SignalFacets].Ran {
		t.Error("the facet signal ran without a facet filter; it would have been 'the first N rows of the index'")
	}
	if got := signals[retrieval.SignalFacets].Skipped; got != retrieval.SkippedNoFacets {
		t.Errorf("the facet signal's skip reason = %q, want %q", got, retrieval.SkippedNoFacets)
	}

	// ------------------------------------------------------------------
	// 2. The graph half: the recalled claim is connected to the finding that
	// supports it, and the material is reached through the experiment that
	// was performed on it — reaches the text signal alone would not make,
	// because none of those titles uses another's wording.
	if claim.ObjectVersionID != corpus.claimVersion {
		t.Errorf("the claim candidate is not pinned to the version its publication published: %+v", claim)
	}
	if len(claim.Hops) == 0 {
		t.Errorf("the claim carries no hop: alice's own relation to the finding should have reached it: %+v", claim)
	}
	// The hop's direction is the WALK's, not the candidate's: an "out" hop is
	// one the traversal followed from its source to its target, out of the
	// frontier node FromRef names. The supports edge runs finding → claim, and
	// both endpoints are seeds here, so the claim is reached by following it
	// out of the finding while the finding is reached by the same edge
	// arriving at it from the claim. Asserting both halves is what pins the
	// meaning of the field rather than one reading of it.
	findingRef := docRef(corpus.findingPID)
	assertHop(t, claim, "supports", retrieval.DirectionOut, findingRef)
	for _, h := range claim.Hops {
		if h.RelationType == "contradicts" {
			t.Errorf("alice's expansion crossed a relation in bob's project: %+v", h)
		}
	}
	assertHop(t, findCandidate(t, res, findingRef), "supports", retrieval.DirectionIn,
		docRef(corpus.claimPID))

	material := candidateOfType(t, res, "material", f.aliceProject)
	if material.ObjectVersionID != corpus.materialVersion {
		t.Errorf("the material candidate is not pinned to its object version: %+v", material)
	}
	assertHop(t, material, "performed_on", retrieval.DirectionOut, docRef(corpus.experimentPID))

	// ------------------------------------------------------------------
	// 3. No private leak, in both directions.
	//
	// Direction 1 — the actor's own rows came back. Without this, every
	// assertion below would also pass against a scope that resolved to
	// nothing.
	if !hasRef(res, search.EntityRef(search.EntityKnowledge, corpus.claimPID)) {
		t.Fatalf("the actor's own claim is missing from %v", refs)
	}
	// The public row is readable by anyone: it is the floor that a
	// failed-open reading would have produced for EVERYTHING, which is why
	// its presence is asserted separately from the private rows'.
	if !hasRef(res, search.EntityRef(search.EntityKnowledge, corpus.publicPID)) {
		t.Errorf("the public publication was not recalled: %v", refs)
	}

	// Direction 2 — nothing the actor may not read, anywhere in the result.
	// The check is over the ENCODING of the whole Result, not over the refs
	// alone: a leaked version id would travel in a hop or in an
	// object_version_id, and a test that compared only refs would not see it.
	encoded := mustJSON(t, res)
	for _, secret := range []struct{ name, id string }{
		{"bob's claim pid", corpus.secretPID},
		{"bob's claim object version", corpus.secretVersion},
		{"bob's material object version", corpus.bobMaterialVersion},
		{"bob's project", f.bobProject},
	} {
		if strings.Contains(encoded, secret.id) {
			t.Errorf("the result leaks %s (%s): %s", secret.name, secret.id, encoded)
		}
	}
	for _, cand := range res.Candidates {
		if cand.ProjectID == f.bobProject {
			t.Errorf("a candidate belongs to a project the actor is not in: %+v", cand)
		}
	}

	// The public row is recalled but NOT expanded: the graph half is
	// members-only (CLAUDE.md §9.6, Publish controls visibility), so an
	// object version in a project the actor is not in never becomes a
	// traversal node or a seed.
	public := findCandidate(t, res, search.EntityRef(search.EntityKnowledge, corpus.publicPID))
	if len(public.Hops) != 0 {
		t.Errorf("the public publication was expanded from: %+v", public.Hops)
	}
	if public.ObjectVersionID != "" {
		t.Errorf("a publication outside the actor's scope was pinned to its object version: %+v", public)
	}

	// ------------------------------------------------------------------
	// 4. Determinism: the same question, scope and corpus give the same
	// order. T0905's ranking is measured against this.
	again, err := r.Retrieve(ctx, scope, retrieval.Request{Query: "Mg-MOF-74"})
	if err != nil {
		t.Fatalf("Retrieve (second run): %v", err)
	}
	if fmt.Sprint(retrievalRefs(again)) != fmt.Sprint(refs) {
		t.Errorf("the same query returned a different order:\n first = %v\nsecond = %v", refs, retrievalRefs(again))
	}

	// ------------------------------------------------------------------
	// 5. A facet-only query recalls by kind. This is how "the materials
	// about X" is answered when the question's wording does not occur in the
	// documents: the plan's target object becomes a containment filter.
	facets, err := r.Retrieve(ctx, scope, retrieval.Request{
		Facets: []byte(`{"object_type":"material"}`),
	})
	if err != nil {
		t.Fatalf("Retrieve (facets): %v", err)
	}
	if len(facets.Candidates) == 0 {
		t.Fatal("the facet-only query recalled nothing")
	}
	// The filter narrows the DOCUMENT signal, and a document is exactly what
	// it can narrow: a candidate reached by the traversal is a scientific
	// object version, which no search_documents facet describes. So the
	// assertion is on the documents, and the graph half is asserted to still
	// run — a filter that switched the traversal off would be a filter
	// silently changing the pipeline's shape.
	sawMaterial := false
	for _, cand := range facets.Candidates {
		if cand.ProjectID == f.bobProject {
			t.Errorf("the facet filter recalled another project's row: %+v", cand)
		}
		if cand.Kind != retrieval.KindDocument {
			continue
		}
		if cand.ObjectType != "material" {
			t.Errorf("the facet filter admitted a %s document: %+v", cand.ObjectType, cand)
		}
		if strings.HasPrefix(cand.Ref, docRef(corpus.materialPID)) {
			sawMaterial = true
		}
	}
	if !sawMaterial {
		t.Errorf("the facet-only query did not recall the material: %v", retrievalRefs(facets))
	}
	experiment := candidateOfType(t, facets, "experiment", f.aliceProject)
	if experiment.Kind != retrieval.KindObjectVersion {
		t.Errorf("the experiment reached from a facet-recalled seed is %q, want a graph node: %+v",
			experiment.Kind, experiment)
	}
	if experiment.ObjectVersionID != corpus.experimentVersion {
		t.Errorf("the experiment node is not pinned to its object version: %+v", experiment)
	}
	// The experiment is NOT itself recalled here — the filter admitted only
	// materials — so it is discovered by the traversal, and the hop that
	// found it is the one that records the walk: performed_on runs
	// experiment → material, so it was followed from the material to the
	// experiment, i.e. in.
	assertHop(t, experiment, "performed_on", retrieval.DirectionIn, docRef(corpus.materialPID))

	// ------------------------------------------------------------------
	// 6. The retrieval refuses an unresolved scope without reading.
	if _, err := r.Retrieve(ctx, search.Scope{}, retrieval.Request{Query: "Mg-MOF-74"}); !errors.Is(err, retrieval.ErrNoScope) {
		t.Errorf("Retrieve with a zero scope: err = %v, want ErrNoScope", err)
	}
	if _, err := store.FullText(ctx, retrieval.DocumentQuery{Query: "Mg-MOF-74"}); !errors.Is(err, retrieval.ErrNoScope) {
		t.Errorf("SQLStore.FullText with a zero scope: err = %v, want ErrNoScope", err)
	}
}

// --------------------------------------------------------------------------
// The parameter encoding, which is the one thing that is not a rule

// TestStoreSpellsNoNarrowingTheOneWaySQLUnderstands is a regression test for a
// failure that no unit test and no compiler could see.
//
// The read queries guard their narrowing parameters with `IS NULL`, and Go has
// two ways to write an empty list. Nil arrives as NULL and the guard is true; a
// NON-NIL EMPTY slice arrives as the empty array, which is not NULL, so the
// guard is false and `entity_type = ANY('{}')` matches nothing. A request that
// passed an empty slice meaning "no restriction" therefore got the empty set
// back — indistinguishable from an empty corpus, and reproduced here end to end
// because that is exactly how it was found.
func TestStoreSpellsNoNarrowingTheOneWaySQLUnderstands(t *testing.T) {
	ctx := testCtx(t)
	f := newRetrievalFixture(t, ctx)
	corpus := seedRetrievalCorpus(t, f)
	scope := f.aliceScope(t)

	store, err := retrieval.NewSQLStore(f.pool)
	if err != nil {
		t.Fatalf("NewSQLStore: %v", err)
	}
	// Both spellings of "no entity narrowing", against the same corpus.
	nilTypes, err := store.FullText(ctx, retrieval.DocumentQuery{
		Scope: scope, Query: "Mg-MOF-74", PageSize: 20,
	})
	if err != nil {
		t.Fatalf("FullText (nil entity types): %v", err)
	}
	emptyTypes, err := store.FullText(ctx, retrieval.DocumentQuery{
		Scope: scope, Query: "Mg-MOF-74", EntityTypes: []string{}, PageSize: 20,
	})
	if err != nil {
		t.Fatalf("FullText (empty entity types): %v", err)
	}
	if len(nilTypes) == 0 {
		t.Fatal("the fixture recalled nothing; the comparison would be vacuous")
	}
	if len(emptyTypes) != len(nilTypes) {
		t.Errorf("an empty entity-type list returned %d rows against nil's %d: an empty list means NO NARROWING, "+
			"and the SQL guard cannot tell an empty array from a filter", len(emptyTypes), len(nilTypes))
	}

	// The same for the facet filter, where the empty form is not merely empty
	// but malformed: ''::jsonb is a parse error, so a caller passing an empty
	// non-nil slice would get a failed read rather than a wide one.
	emptyFilter, err := store.FullText(ctx, retrieval.DocumentQuery{
		Scope: scope, Query: "Mg-MOF-74", StructuredFilter: []byte{}, PageSize: 20,
	})
	if err != nil {
		t.Fatalf("FullText (empty filter): %v", err)
	}
	if len(emptyFilter) != len(nilTypes) {
		t.Errorf("an empty facet filter returned %d rows against nil's %d", len(emptyFilter), len(nilTypes))
	}

	// And the real narrowing still narrows, so the above is not "every empty
	// value is ignored".
	oneType, err := store.FullText(ctx, retrieval.DocumentQuery{
		Scope: scope, Query: "Mg-MOF-74", EntityTypes: []string{search.EntityKnowledge}, PageSize: 20,
	})
	if err != nil {
		t.Fatalf("FullText (one entity type): %v", err)
	}
	if len(oneType) != len(nilTypes) {
		t.Errorf("narrowing to %q returned %d rows against an unnarrowed %d, but the corpus is all knowledge",
			search.EntityKnowledge, len(oneType), len(nilTypes))
	}
	none, err := store.FullText(ctx, retrieval.DocumentQuery{
		Scope: scope, Query: "Mg-MOF-74", EntityTypes: []string{search.EntityRelease}, PageSize: 20,
	})
	if err != nil {
		t.Fatalf("FullText (absent entity type): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("narrowing to %q returned %d rows of a type the corpus does not have", search.EntityRelease, len(none))
	}

	// The retriever's own request path spells it the same way, and that is
	// the half that produced the empty result: the fallback plan — a question
	// the planner could not structure, which is the COMMON case — carries no
	// entity types, and the Request built from it must reach the store as nil
	// rather than as an empty list.
	r, err := retrieval.NewRetriever(store, nil, retrieval.WithLogger(silentLogger()))
	if err != nil {
		t.Fatalf("NewRetriever: %v", err)
	}
	fallback := retrieval.RequestFromPlan(planner.Plan{
		Status: planner.StatusFallback,
		Reason: planner.ReasonInvalidPlan,
		Query:  "Mg-MOF-74",
	}, nil, retrieval.Limits{})
	res, err := r.Retrieve(ctx, scope, fallback)
	if err != nil {
		t.Fatalf("Retrieve (fallback plan): %v", err)
	}
	if !hasRef(res, docRef(corpus.claimPID)) {
		t.Errorf("the fallback plan recalled none of the corpus: %+v", res.Signals)
	}
	// And a plan that DOES name an entity type still narrows: the fix must not
	// have turned every plan into an unnarrowed read.
	planned := retrieval.RequestFromPlan(planner.Plan{
		Status: planner.StatusPlanned,
		Query:  "Mg-MOF-74",
		Document: &planner.Document{
			PlanVersion:  planner.PlanVersion,
			Intent:       planner.IntentAnswer,
			TargetObject: planner.TargetObject{EntityTypes: []string{search.EntityKnowledge}},
		},
	}, nil, retrieval.Limits{})
	res, err = r.Retrieve(ctx, scope, planned)
	if err != nil {
		t.Fatalf("Retrieve (planned): %v", err)
	}
	if !hasRef(res, docRef(corpus.claimPID)) {
		t.Errorf("the plan naming the projection's own entity type recalled nothing: %+v", res.Signals)
	}
}

// --------------------------------------------------------------------------
// The queries' own fail-closed property, and the copy's fidelity

// TestRetrievalQueriesFailClosedOnTheirOwn runs each of the three document
// queries with an EMPTY project scope and asserts the result is exactly the
// public rows.
//
// This is asserted per query rather than once, because the access predicate
// was COPIED into each of them (search.sql's comment says why: the canonical
// query is the access regression test's subject and its shape is the search
// surface's). A copy is where a rule drifts, so each copy has to hold the
// line by itself — and the direction that fails silently is the empty scope,
// which returns the whole table if the predicate is dropped and the public
// rows only if it is intact.
func TestRetrievalQueriesFailClosedOnTheirOwn(t *testing.T) {
	ctx := testCtx(t)
	f := newRetrievalFixture(t, ctx)
	corpus := seedRetrievalCorpus(t, f)
	// Every row carries a vector, so the vector query is genuinely comparing
	// the whole corpus and only the access predicate decides.
	f.embedTheCorpus(t)

	// What an empty scope must return: the rows whose own visibility is
	// public, and nothing else. Read directly, so the expectation does not
	// come from any code under test.
	var want []string
	rows, err := f.pool.Query(ctx,
		`SELECT entity_ref FROM search_documents WHERE visibility = 'public' ORDER BY entity_ref`)
	if err != nil {
		t.Fatalf("read the public rows: %v", err)
	}
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			t.Fatalf("scan public row: %v", err)
		}
		want = append(want, ref)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate public rows: %v", err)
	}
	if len(want) != 1 || want[0] != search.EntityRef(search.EntityKnowledge, corpus.publicPID) {
		t.Fatalf("the fixture's public corpus = %v, want exactly the public publication", want)
	}

	// The same question the leak assertions use, under no scope at all.
	const question = "Mg-MOF-74"
	got := map[string][]string{}

	ft, err := f.q.SearchDocumentsFullText(ctx, sqlc.SearchDocumentsFullTextParams{
		Query: question, PageSize: 50,
	})
	if err != nil {
		t.Fatalf("SearchDocumentsFullText with an empty scope: %v", err)
	}
	got[retrieval.SignalFullText] = sortedRefs(ft)

	vec, err := f.q.SearchDocumentsByVector(ctx, sqlc.SearchDocumentsByVectorParams{
		Embedding:         vectorText(t, f, corpus.claimPID),
		EmbeddingProvider: embedding.LocalModel.Provider,
		EmbeddingModel:    embedding.LocalModel.Name,
		EmbeddingVersion:  embedding.LocalModel.Version,
		PageSize:          50,
	})
	if err != nil {
		t.Fatalf("SearchDocumentsByVector with an empty scope: %v", err)
	}
	got[retrieval.SignalVector] = vecRefs(vec)

	fac, err := f.q.SearchDocumentsByFacets(ctx, sqlc.SearchDocumentsByFacetsParams{
		StructuredFilter: []byte(`{"entity_type":"knowledge"}`),
		PageSize:         50,
	})
	if err != nil {
		t.Fatalf("SearchDocumentsByFacets with an empty scope: %v", err)
	}
	got[retrieval.SignalFacets] = facRefs(fac)

	for _, signal := range []string{retrieval.SignalFullText, retrieval.SignalVector, retrieval.SignalFacets} {
		if fmt.Sprint(got[signal]) != fmt.Sprint(want) {
			t.Errorf("the %s signal under an empty scope returned %v, want the public rows only (%v)\n"+
				"an empty scope is not 'no filter': it is 'the rows anybody may read'", signal, got[signal], want)
		}
	}

	// The discriminating half: a scope that NAMES the project DOES return its
	// rows. Without it, "the predicate refused everything" and "the predicate
	// is right" would look identical.
	scoped, err := f.q.SearchDocumentsFullText(ctx, sqlc.SearchDocumentsFullTextParams{
		Query:             question,
		AllowedProjectIds: []pgtype.UUID{pgUUID(t, f.aliceProject)},
		PageSize:          50,
	})
	if err != nil {
		t.Fatalf("SearchDocumentsFullText under a named scope: %v", err)
	}
	if len(scoped) <= len(want) {
		t.Errorf("naming alice's project returned %d rows, no more than the %d public ones: "+
			"the scope must ADMIT the project's rows or the empty-scope result proves nothing", len(scoped), len(want))
	}
	if !hasRowRef(scoped, search.EntityRef(search.EntityKnowledge, corpus.claimPID)) {
		t.Errorf("naming alice's project did not return her claim: %v", rowRefs(scoped))
	}
	for _, row := range scoped {
		if strings.HasSuffix(row.EntityRef, corpus.secretPID) {
			t.Errorf("naming one project admitted another's document: %s", row.EntityRef)
		}
	}
}

// TestFullTextQueryMatchesTheCanonicalRead pins the copy: with no narrowing
// at all, SearchDocumentsFullText must return the same rows, in the same
// order, as the canonical SearchDocuments.
//
// The FTS semantics were left character-for-character identical precisely so
// this holds, and this is the test that says whether they still are. A drift
// here would be silent in production — both queries would keep returning
// plausible rows — while the search surface and the retrieval disagreed about
// what "matches" means.
func TestFullTextQueryMatchesTheCanonicalRead(t *testing.T) {
	ctx := testCtx(t)
	f := newRetrievalFixture(t, ctx)
	seedRetrievalCorpus(t, f)
	scope := f.aliceScope(t)

	canonical, err := f.q.SearchDocuments(ctx, sqlc.SearchDocumentsParams{
		Query:             "Mg-MOF-74",
		AllowedProjectIds: scope.AllowedProjectUUIDs(),
		PageSize:          50,
	})
	if err != nil {
		t.Fatalf("SearchDocuments: %v", err)
	}
	if len(canonical) == 0 {
		t.Fatal("the canonical read matched nothing; the comparison would be vacuous")
	}
	wide, err := f.q.SearchDocumentsFullText(ctx, sqlc.SearchDocumentsFullTextParams{
		Query:             "Mg-MOF-74",
		AllowedProjectIds: scope.AllowedProjectUUIDs(),
		PageSize:          50,
	})
	if err != nil {
		t.Fatalf("SearchDocumentsFullText: %v", err)
	}
	canonicalRefs := make([]string, 0, len(canonical))
	for _, row := range canonical {
		canonicalRefs = append(canonicalRefs, row.EntityRef)
	}
	if got := rowRefs(wide); fmt.Sprint(got) != fmt.Sprint(canonicalRefs) {
		t.Errorf("SearchDocumentsFullText = %v\n SearchDocuments    = %v\n"+
			"the two must agree row for row while the retrieval adds no narrowing", got, canonicalRefs)
	}

	// And the narrowing really narrows: an entity type the corpus does not
	// have returns none of it.
	narrow, err := f.q.SearchDocumentsFullText(ctx, sqlc.SearchDocumentsFullTextParams{
		Query:             "Mg-MOF-74",
		EntityTypes:       []string{search.EntityRelease},
		AllowedProjectIds: scope.AllowedProjectUUIDs(),
		PageSize:          50,
	})
	if err != nil {
		t.Fatalf("SearchDocumentsFullText (narrowed): %v", err)
	}
	if len(narrow) != 0 {
		t.Errorf("narrowing to %q returned %d rows: %v", search.EntityRelease, len(narrow), rowRefs(narrow))
	}

	// The public_only flag can only ever REMOVE rows: 'public' narrows to the
	// rows anybody may read, and never adds one.
	publicOnly, err := f.q.SearchDocumentsFullText(ctx, sqlc.SearchDocumentsFullTextParams{
		Query:             "Mg-MOF-74",
		AllowedProjectIds: scope.AllowedProjectUUIDs(),
		PublicOnly:        true,
		PageSize:          50,
	})
	if err != nil {
		t.Fatalf("SearchDocumentsFullText (public only): %v", err)
	}
	for _, row := range publicOnly {
		if row.Visibility != "public" {
			t.Errorf("public_only returned a %s row: %s", row.Visibility, row.EntityRef)
		}
	}
	if len(publicOnly) > len(canonical) {
		t.Errorf("public_only WIDENED the read: %d rows against the canonical %d", len(publicOnly), len(canonical))
	}
}

// --------------------------------------------------------------------------
// The authorization is in the SQL: proved by trying to break it

// TestTraversalQueriesAreWhatRefuseTheOtherProject runs UNWRAPPED versions of
// the traversal's two queries — the same SQL without its project predicates —
// and observes the leak they would cause.
//
// This is the counter-proof the design needs. "Another project's version ids
// never come back" is a claim about an output; the claim worth testing is that
// the PREDICATES are what refuse them. Without this, a corpus that simply
// never crossed projects would make the filters look load-bearing when
// nothing had tried them.
func TestTraversalQueriesAreWhatRefuseTheOtherProject(t *testing.T) {
	ctx := testCtx(t)
	f := newRetrievalFixture(t, ctx)
	corpus := seedRetrievalCorpus(t, f)

	alice := []pgtype.UUID{pgUUID(t, f.aliceProject)}

	// The hop query, filtered the way the checked-in one is: the relation's
	// project AND both endpoints' projects must be in the scope.
	guarded, err := f.q.ListScopeAdjacentRelationVersions(ctx, sqlc.ListScopeAdjacentRelationVersionsParams{
		VersionIds: []pgtype.UUID{pgUUID(t, corpus.claimVersion)},
		ProjectIds: alice,
	})
	if err != nil {
		t.Fatalf("ListScopeAdjacentRelationVersions: %v", err)
	}
	foundAliceEdge := false
	for _, row := range guarded {
		if row.RelationType == "supports" {
			foundAliceEdge = true
		}
		if row.RelationType == "contradicts" {
			t.Errorf("the guarded hop query returned bob's edge: %+v", row)
		}
	}
	if !foundAliceEdge {
		t.Fatalf("the guarded hop query returned %+v, which does not include alice's own supports edge: "+
			"a query that returns nothing proves nothing", guarded)
	}

	// The same shape WITHOUT the predicates — raw SQL, deliberately not a
	// checked-in query: an edge touching alice's claim, whoever owns it.
	var (
		leakedRelation string
		leakedVersion  string
	)
	if err := f.pool.QueryRow(ctx,
		`SELECT rv.relation_type, rv.source_object_version_id::text
		 FROM relation_versions rv
		 WHERE rv.target_object_version_id = $1::uuid AND rv.relation_type = 'contradicts'`,
		corpus.claimVersion).Scan(&leakedRelation, &leakedVersion); err != nil {
		t.Fatalf("the unwrapped hop query found no edge: %v — the cross-project edge is not in the corpus, "+
			"so this test cannot prove the predicate refuses it", err)
	}
	if leakedVersion != corpus.secretVersion {
		t.Fatalf("the unwrapped hop query returned %s, want bob's claim version %s", leakedVersion, corpus.secretVersion)
	}

	// The node query, filtered the way the checked-in one is.
	guardedNodes, err := f.q.ListScopeObjectVersions(ctx, sqlc.ListScopeObjectVersionsParams{
		VersionIds: []pgtype.UUID{pgUUID(t, leakedVersion)},
		ProjectIds: alice,
	})
	if err != nil {
		t.Fatalf("ListScopeObjectVersions: %v", err)
	}
	if len(guardedNodes) != 0 {
		t.Errorf("the guarded node query returned %d rows outside the scope: %+v", len(guardedNodes), guardedNodes)
	}
	// The discriminating half: the same node read with ITS OWN project in the
	// scope comes back, so the empty result above is the filter and not a
	// broken id.
	ownNodes, err := f.q.ListScopeObjectVersions(ctx, sqlc.ListScopeObjectVersionsParams{
		VersionIds: []pgtype.UUID{pgUUID(t, leakedVersion)},
		ProjectIds: []pgtype.UUID{pgUUID(t, f.bobProject)},
	})
	if err != nil {
		t.Fatalf("ListScopeObjectVersions (bob's scope): %v", err)
	}
	if len(ownNodes) != 1 {
		t.Fatalf("bob's own scope returned %d rows for his version, want 1: the empty result under alice's "+
			"scope would otherwise be indistinguishable from a bad id", len(ownNodes))
	}

	// The same node read WITHOUT the project predicate: bob's title comes
	// back, which is the leak in the form a reader would see it.
	var leakedTitle string
	if err := f.pool.QueryRow(ctx,
		`SELECT sov.title FROM scientific_object_versions sov WHERE sov.id = $1::uuid`,
		leakedVersion).Scan(&leakedTitle); err != nil {
		t.Fatalf("the unwrapped node query found no row: %v", err)
	}
	if !strings.Contains(leakedTitle, "secret") {
		t.Fatalf("the unwrapped node query returned %q, want bob's unpublished title", leakedTitle)
	}

	// The seed read is the one graph read that CANNOT carry a scope predicate
	// (its input is a list of already-authorized pids, and the mapping it
	// performs has no project of its own to filter on), so its output is
	// authorized in Go instead. This shows the row it would return for a
	// foreign publication, which is why that check exists.
	var pid string
	if err := f.pool.QueryRow(ctx,
		`SELECT kp.pid FROM knowledge_publications kp WHERE kp.object_version_id = $1::uuid`,
		corpus.secretVersion).Scan(&pid); err != nil {
		t.Fatalf("read bob's publication: %v", err)
	}
	if pid != corpus.secretPID {
		t.Fatalf("bob's publication pid = %q, want %q", pid, corpus.secretPID)
	}
}

// TestRetrieverRefusesASeedFromAnotherProject drives the seed check directly:
// a store that answers the seed read with a version row from another project
// must not turn it into a traversal origin.
//
// It is the one graph read whose scope is applied in Go rather than in SQL —
// its input is a list of already-authorized pids, and the mapping it performs
// has no project of its own to filter on — so it needs a test that the rule is
// there. The store lies on purpose: "the reader is trusted to be right" is
// exactly the assumption the check exists not to make.
//
// What the check does and does not do is worth stating, because the test must
// not claim more than the code does. It verifies the PROJECT of the version row
// it is handed; it does not re-derive pid → version, which is the seed query's
// own join (knowledge_publications.object_version_id, 1:1 by 00083). A store
// that returned a same-project version for the wrong pid would pass this check
// and mis-pin one document — a real fault, and one no single read can detect.
func TestRetrieverRefusesASeedFromAnotherProject(t *testing.T) {
	ctx := testCtx(t)
	f := newRetrievalFixture(t, ctx)
	corpus := seedRetrievalCorpus(t, f)

	store, err := retrieval.NewSQLStore(f.pool)
	if err != nil {
		t.Fatalf("NewSQLStore: %v", err)
	}
	// Bob's version, reported as the answer for whichever pid comes first:
	// the whole row is his, title and project included, because a store that
	// resolved a pid to another project's version would report that version's
	// own columns.
	liar := &lyingSeedStore{Store: store, foreign: retrieval.GraphObject{
		ObjectVersionID: corpus.secretVersion,
		Title:           "Mg-MOF-74 secret claim",
		ProjectID:       f.bobProject,
	}}
	r, err := retrieval.NewRetriever(liar, nil, retrieval.WithLogger(silentLogger()))
	if err != nil {
		t.Fatalf("NewRetriever: %v", err)
	}

	res, err := r.Retrieve(ctx, f.aliceScope(t), retrieval.Request{Query: "Mg-MOF-74"})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if !liar.lied {
		t.Fatal("the store's lie was never exercised: the seed read returned nothing to corrupt")
	}
	encoded := mustJSON(t, res)
	if strings.Contains(encoded, corpus.secretVersion) {
		t.Errorf("a seed from another project was pinned onto a candidate: %s", encoded)
	}
	if strings.Contains(encoded, "secret") {
		t.Errorf("a foreign title reached the result through the graph: %s", encoded)
	}
	for _, cand := range res.Candidates {
		if cand.ObjectVersionID == corpus.secretVersion {
			t.Errorf("a candidate was pinned to a version outside the scope: %+v", cand)
		}
	}
	// And the refusal did not take the legitimate half with it: alice's own
	// publications are still recalled, and the traversal still runs from them.
	if !hasRef(res, docRef(corpus.claimPID)) {
		t.Errorf("the seed refusal removed alice's own documents too: %v", retrievalRefs(res))
	}
}

// lyingSeedStore wraps the real store and answers the seed read with one
// foreign node. It is the "the store is wrong" case.
type lyingSeedStore struct {
	retrieval.Store
	foreign retrieval.GraphObject
	lied    bool
}

func (l *lyingSeedStore) SeedObjectVersions(ctx context.Context, scope search.Scope, pids []string) ([]retrieval.SeedVersion, error) {
	out, err := l.Store.SeedObjectVersions(ctx, scope, pids)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}
	// Re-point the first seed at another project's version, keeping the pid
	// the caller asked about so the row still looks like an answer.
	node := l.foreign
	node.ObjectID = out[0].GraphObject.ObjectID
	node.VersionNo = out[0].GraphObject.VersionNo
	out[0].GraphObject = node
	l.lied = true
	return out, nil
}

// --------------------------------------------------------------------------
// Small helpers

// docRef is the ref a projected knowledge document's candidate carries: the
// entity ref with the version the publication pinned appended to it.
func docRef(pid string) string {
	return search.EntityRef(search.EntityKnowledge, pid) + "@v1"
}

// assertHop requires the candidate to carry exactly the hop described: that
// relation type, walked in that direction, out of that candidate. A hop the
// result cannot state is a hop nobody can check, and the graph half of a
// retrieval is only explicable if the path is in the answer.
func assertHop(t *testing.T, cand retrieval.Candidate, relationType, direction, fromRef string) {
	t.Helper()
	for _, h := range cand.Hops {
		if h.RelationType != relationType || h.Direction != direction {
			continue
		}
		if h.FromRef != fromRef {
			t.Errorf("%s: the %s/%s hop left from %q, want %q",
				cand.Ref, relationType, direction, h.FromRef, fromRef)
		}
		if h.Depth != 1 {
			t.Errorf("%s: the %s hop has depth %d, want 1 (both ends are seeds at this depth)",
				cand.Ref, relationType, h.Depth)
		}
		return
	}
	t.Errorf("%s: no %s/%s hop out of %q; hops = %+v", cand.Ref, relationType, direction, fromRef, cand.Hops)
}

func rowRefs(rows []sqlc.SearchDocumentsFullTextRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.EntityRef)
	}
	return out
}

func hasRowRef(rows []sqlc.SearchDocumentsFullTextRow, ref string) bool {
	for _, r := range rows {
		if r.EntityRef == ref {
			return true
		}
	}
	return false
}

func sortedRefs(rows []sqlc.SearchDocumentsFullTextRow) []string {
	out := rowRefs(rows)
	sort.Strings(out)
	return out
}

func vecRefs(rows []sqlc.SearchDocumentsByVectorRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.EntityRef)
	}
	sort.Strings(out)
	return out
}

func facRefs(rows []sqlc.SearchDocumentsByFacetsRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.EntityRef)
	}
	sort.Strings(out)
	return out
}

// vectorText reads one row's stored vector back as the text form the read
// query takes, so the vector signal is driven with a real vector rather than
// a literal this test typed.
func vectorText(t *testing.T, f *retrievalFixture, pid string) string {
	t.Helper()
	var text string
	if err := f.pool.QueryRow(f.ctx,
		`SELECT embedding::text FROM search_documents WHERE entity_ref = $1`,
		search.EntityRef(search.EntityKnowledge, pid)).Scan(&text); err != nil {
		t.Fatalf("read the stored vector for %s: %v", pid, err)
	}
	return text
}

func pgUUID(t *testing.T, text string) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := id.Scan(text); err != nil {
		t.Fatalf("parse uuid %q: %v", text, err)
	}
	return id
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}
