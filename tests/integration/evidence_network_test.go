// Task T0806 required test "external evidence tests".
//
// docs/10 §7 and Master Acceptance Gate D: the Published Knowledge Object
// page distinguishes Origin Evidence, Reviewed External Evidence and
// Unreviewed External Evidence, and the origin maintainer may not delete the
// network's counter-evidence (docs/24 §2: external evidence 不可被 origin
// maintainer 静默删除).
//
// The unit suites pin the decisions (internal/domain: the classification over
// the two axes; internal/application/evidencenetwork: the section's buckets
// and its fail-closed re-check; internal/application/rsg: the write command's
// derived axes and its one existence-hiding refusal; cmd/api/knowledgehttp:
// the read's scope and its 503). What only a real PostgreSQL and the whole
// composed path can settle is pinned here, over real rows, through the real
// product path — two projects, a real publish, an evidence assertion posted
// on the contract's route:
//
//  1. CROSS-PROJECT EVIDENCE LANDS AND IS CLASSIFIED. The asserting project
//     is another project than the one that owns the published version, and
//     the page presents the three classes in three separate buckets. Nothing
//     here is hand-built state: the projects, the versions, the reviews, the
//     publication and the assertions all come from the product's own
//     surfaces (see the fixture below for the two exceptions, both of them
//     things V1 has NO surface for, and for why a raw write is the only way
//     to express them).
//
//  2. DELETE PROTECTION IS STRUCTURAL. The origin project's OWNER (and the
//     project owner, which is the same actor here) asks for a deletion: there
//     is no route to ask on, and the database refuses a raw DELETE that
//     bypasses the application entirely. The guard is the DELETE half only —
//     reviewing an assertion stays legal (criterion 3 needs it).
//
//  3. CONFLICTING STANCES COEXIST. Two assertions on the SAME (target,
//     evidence) pair, one supporting and one contesting, each with its own
//     independently changing review_state — and the schema has no unique
//     constraint on that pair to prevent it.
//
//  4. NO TRUTH SCORE. The read response carries no score of any kind, at any
//     depth (CLAUDE.md §9.13, docs/10 §4).
//
//  5. THE EVENT IS REAL. knowledge.external_evidence_added (the YAML's
//     spelling, specs/events/event-types.yaml:28) is written to the outbox
//     inside the assertion's own state commit, the dispatcher turns it into a
//     research_events row, and its visibility never exceeds the assertion's
//     own.
//
//  6. NOTHING PRIVATE LEAKS. A private project's assertion does not reach the
//     document even when a row is planted below the application (proved by
//     the row EXISTING), a members-only publication's evidence is not
//     reachable at all by the network, and an assertion the write derived as
//     private is not rendered.
//
// # The two raw writes in the fixture, and why they are not "hand-built state"
//
// The publication review and the evidence assertion are written through the
// product's own surfaces. Two things have no surface in V1:
//
//   - the REVIEW transition (unreviewed → reviewed). reviews of assertions
//     are a read of the class model, not a route: no task in the DAG owns one
//     yet, and this one does not invent it. The fixture therefore moves a row
//     with the UPDATE the database explicitly still allows (migration 00091
//     takes only the DELETE half of the append-only pair), which is the
//     reviewer's write expressed directly.
//   - the PRIVATE PROJECT's assertion against another project's published
//     version. The write path REFUSES it — that is exactly rule 5 of the
//     command (a cross-project assertion needs a network-visible
//     publication) — so the only way to obtain the row is to plant it. It is
//     planted so the READ can be shown to refuse it too, which is the
//     assertion this criterion is about; the row's existence is asserted
//     first, so a vacuous "it is not in the document" cannot pass.
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
)

// evidenceNetworkTaskID namespaces this task's test databases
// (test_T0806_<run_id>).
const evidenceNetworkTaskID = "T0806"

// externalEvidenceAddedEvent is the machine-readable event name from
// specs/events/event-types.yaml — the knowledge family, not docs/18's older
// `external_evidence.added` prose spelling.
const externalEvidenceAddedEvent = "knowledge.external_evidence_added"

// --------------------------------------------------------------------------
// The wire shapes this suite reads (declared here rather than reused from the
// transport, so a silent JSON tag change fails here)

// evidenceAssertionWire is one assertion as the write route renders it.
type evidenceAssertionWire struct {
	ID                      string          `json:"id"`
	ProjectID               string          `json:"project_id"`
	StateID                 string          `json:"state_id"`
	TargetObjectVersionID   string          `json:"target_object_version_id"`
	EvidenceObjectVersionID string          `json:"evidence_object_version_id"`
	Relation                string          `json:"relation"`
	EvidenceType            string          `json:"evidence_type"`
	Scope                   json.RawMessage `json:"scope"`
	Directness              string          `json:"directness"`
	InferenceNature         string          `json:"inference_nature"`
	ReviewState             string          `json:"review_state"`
	EvidenceOrigin          string          `json:"evidence_origin"`
	Visibility              string          `json:"visibility"`
	CreatedAt               string          `json:"created_at"`
}

// evidenceRowWire is one assertion as the READ renders it — the same facts
// plus the stance label, and (deliberately) no class field: the class is the
// bucket the row sits in.
type evidenceRowWire struct {
	ID                      string          `json:"id"`
	Relation                string          `json:"relation"`
	Stance                  string          `json:"stance"`
	EvidenceType            string          `json:"evidence_type"`
	Directness              string          `json:"directness"`
	InferenceNature         string          `json:"inference_nature"`
	Scope                   json.RawMessage `json:"scope"`
	ReasoningNote           string          `json:"reasoning_note"`
	ReviewState             string          `json:"review_state"`
	AssertingProjectID      string          `json:"asserting_project_id"`
	TargetObjectVersionID   string          `json:"target_object_version_id"`
	EvidenceObjectVersionID string          `json:"evidence_object_version_id"`
	CreatedAt               string          `json:"created_at"`
}

// evidenceSectionWire is the document's evidence part: the three classes,
// each an array, plus the truncation flag.
type evidenceSectionWire struct {
	Origin             []evidenceRowWire `json:"origin"`
	ReviewedExternal   []evidenceRowWire `json:"reviewed_external"`
	UnreviewedExternal []evidenceRowWire `json:"unreviewed_external"`
	Truncated          bool              `json:"truncated"`
}

// evidenceReadWire is the part of the published page this suite reads.
type evidenceReadWire struct {
	PID      string `json:"pid"`
	Audience string `json:"audience"`
	Version  struct {
		ID string `json:"id"`
	} `json:"version"`
	Project struct {
		ID         string `json:"id"`
		Visibility string `json:"visibility"`
	} `json:"project"`
	Evidence evidenceSectionWire `json:"evidence"`
}

// --------------------------------------------------------------------------
// The fixture

// externalEvidenceFixture is the world the cases share: two PUBLIC projects (alpha
// owns the published object, beta asserts against it) and one PRIVATE project
// (gamma, the one whose work must not leak), all created through the real
// API over the world's own composition.
type externalEvidenceFixture struct {
	w *knowledgeWorld

	alpha, beta, gamma *knowledgeProject

	// alpha's published object version and its pid.
	alphaVersion string
	alphaPID     string

	// The version each project cites as evidence (its own, as the write
	// command requires).
	alphaEvidence, betaEvidence, gammaEvidence string

	// One PUBLIC branch per asserting project (the axis that makes an
	// assertion renderable) and one private branch in beta (the axis that
	// keeps one out).
	alphaPub, betaPub, betaHidden string

	// gamma's published (members-only) version and its pid.
	gammaVersion string
	gammaPID     string
}

// newExternalEvidenceFixture builds the world through the product path.
func newExternalEvidenceFixture(t *testing.T, ctx context.Context) *externalEvidenceFixture {
	t.Helper()
	w := newKnowledgeWorldFor(t, ctx, evidenceNetworkTaskID)
	w.signups(t)

	f := &externalEvidenceFixture{w: w}
	f.alpha = w.newProject(t, ctx, "ev-alpha", "public")
	f.beta = w.newProject(t, ctx, "ev-beta", "public")
	f.gamma = w.newProject(t, ctx, "ev-gamma", "private")

	// alpha's published object: a reviewed version, published on the
	// contract's route — the ordinary product path for both.
	_, f.alphaVersion, _ = w.seedReviewedVersion(t, ctx, f.alpha, "MOF-5 CO2 uptake at 298 K", 1, true)
	f.alphaPID = w.mustKnowledgePublish(t, w.alice, f.alpha.id,
		knowledgePublishBody(t, f.alphaVersion, "v1.0", ""), "").PID

	// The versions each project cites as its own evidence, and the branches
	// the assertions are committed to.
	_, f.alphaEvidence, _ = w.seedVersion(t, ctx, f.alpha.id, f.alpha.probe, "alpha benchmark")
	_, f.betaEvidence, _ = w.seedVersion(t, ctx, f.beta.id, f.beta.probe, "beta replication")
	f.alphaPub = w.createPublicBranch(t, ctx, f.alpha, "pub")
	f.betaPub = w.createPublicBranch(t, ctx, f.beta, "pub")
	f.betaHidden = w.createBranch(t, ctx, f.beta.id, f.beta.genesis, "hidden")

	// gamma's own published object: legal from a private project
	// (docs/12 §2) and members-only afterwards, which is the publication
	// whose page the network must never reach.
	_, f.gammaVersion, _ = w.seedReviewedVersion(t, ctx, f.gamma, "gamma internal finding", 1, true)
	f.gammaPID = w.mustKnowledgePublish(t, w.alice, f.gamma.id,
		knowledgePublishBody(t, f.gammaVersion, "v1.0", ""), "").PID
	_, f.gammaEvidence, _ = w.seedVersion(t, ctx, f.gamma.id, f.gamma.probe, "gamma dataset")
	return f
}

// createPublicBranch seeds one PUBLIC branch (the shared createBranch helper
// makes private ones, which is what betaHidden wants).
func (w *knowledgeWorld) createPublicBranch(t *testing.T, ctx context.Context, p *knowledgeProject, name string) string {
	t.Helper()
	b, err := w.svc.CreateBranch(ctx, domain.User{ID: w.aliceID}, p.id, rsg.CreateBranchInput{
		Name:       name,
		BaseRef:    p.genesis,
		Visibility: domain.BranchVisibilityPublic,
	})
	if err != nil {
		t.Fatalf("create public branch %q in %s: %v", name, p.id, err)
	}
	return b.ID
}

// --------------------------------------------------------------------------
// Requests

// evidenceAssertionBody is the contract's route body: the evidence-assertion
// schema's own field names. There is no evidence_origin and no visibility
// field — the two axes are derived, and a request cannot claim either.
func evidenceAssertionBody(targetVersion, evidenceVersion, relation string) string {
	return fmt.Sprintf(`{"target_version_ref":"object_version:%s","evidence_version_ref":"object_version:%s",`+
		`"relation":%q,"evidence_type":"experimental","scope":{"conditions":"298 K, 1 bar"},`+
		`"directness":"direct","inference_nature":"observational","reasoning_note":"replication of the uptake isotherm"}`,
		targetVersion, evidenceVersion, relation)
}

// postEvidenceAssertion posts one assertion and returns the response.
func (f *externalEvidenceFixture) postEvidenceAssertion(t *testing.T, uc *testUserClient, projectID, branchID, body string) *http.Response {
	t.Helper()
	return uc.do(t, http.MethodPost,
		"/api/v1/projects/"+projectID+"/branches/"+branchID+"/evidence-assertions", body)
}

// mustAssertEvidence requires the 201 and returns the stored assertion.
func (f *externalEvidenceFixture) mustAssertEvidence(t *testing.T, uc *testUserClient, projectID, branchID, body string) evidenceAssertionWire {
	t.Helper()
	resp := f.postEvidenceAssertion(t, uc, projectID, branchID, body)
	mustStatus(t, resp, http.StatusCreated)
	raw := readAll(t, resp)
	var got evidenceAssertionWire
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("evidence assertion payload: %v: %s", err, raw)
	}
	return got
}

// readEvidencePage reads the published page and decodes the evidence part.
func (f *externalEvidenceFixture) readEvidencePage(t *testing.T, uc *testUserClient, pid string) (int, string, evidenceReadWire) {
	t.Helper()
	status, raw := f.w.readKnowledge(t, uc, pid)
	var got evidenceReadWire
	if status == http.StatusOK {
		if err := json.Unmarshal([]byte(raw), &got); err != nil {
			t.Fatalf("knowledge read payload: %v: %s", err, raw)
		}
	}
	return status, raw, got
}

// assertionIDs is the ids of one bucket, in order.
func assertionIDs(rows []evidenceRowWire) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

// assertionRowsIn counts the evidence_assertions rows a project owns — the
// table-level fact the deletion cases are about (the shared countRows helper
// answers the query).
func assertionRowsIn(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID string) int {
	t.Helper()
	return countRows(t, ctx, pool,
		`SELECT count(*) FROM evidence_assertions WHERE project_id = $1`, parseUUIDOrDie(projectID))
}

// --------------------------------------------------------------------------
// 1, 3, 4, 6: the read side

// TestExternalEvidenceNetworkIsClassifiedAndDoesNotLeak is the task's centre:
// cross-project evidence lands through the product path and the published
// page presents it in the three classes docs/10 §7 names, dropping what the
// network may not see.
func TestExternalEvidenceNetworkIsClassifiedAndDoesNotLeak(t *testing.T) {
	ctx := testCtx(t)
	f := newExternalEvidenceFixture(t, ctx)

	// --- alpha asserts its own evidence (the ORIGIN side) ----------------
	origin := f.mustAssertEvidence(t, f.w.alice, f.alpha.id, f.alphaPub,
		evidenceAssertionBody(f.alphaVersion, f.alphaEvidence, "supports"))
	if origin.EvidenceOrigin != string(domain.EvidenceOriginInternal) {
		t.Errorf("an assertion from the published object's own project = %q, want internal", origin.EvidenceOrigin)
	}
	if origin.Visibility != string(domain.EvidenceVisibilityPublic) {
		t.Errorf("visibility = %q, want public (public project, public branch, no policy pin)", origin.Visibility)
	}
	if origin.ReviewState != string(domain.EvidenceReviewUnreviewed) {
		t.Errorf("review_state = %q, want the row's own default", origin.ReviewState)
	}

	// --- beta asserts against alpha's published version (EXTERNAL) ------
	supporting := f.mustAssertEvidence(t, f.w.alice, f.beta.id, f.betaPub,
		evidenceAssertionBody(f.alphaVersion, f.betaEvidence, "supports"))
	if supporting.EvidenceOrigin != string(domain.EvidenceOriginExternal) {
		t.Errorf("a cross-project assertion = %q, want external", supporting.EvidenceOrigin)
	}
	if supporting.Visibility != string(domain.EvidenceVisibilityPublic) {
		t.Errorf("cross-project visibility = %q, want public", supporting.Visibility)
	}

	// The SAME (target, evidence) pair, the opposite stance — criterion 3's
	// coexistence, and the reason 00058 keeps that pair non-unique.
	contesting := f.mustAssertEvidence(t, f.w.alice, f.beta.id, f.betaPub,
		evidenceAssertionBody(f.alphaVersion, f.betaEvidence, "contradicts"))
	if contesting.EvidenceObjectVersionID != supporting.EvidenceObjectVersionID ||
		contesting.TargetObjectVersionID != supporting.TargetObjectVersionID {
		t.Fatalf("the two stances did not land on the same pair: %+v / %+v", supporting, contesting)
	}
	if contesting.EvidenceOrigin != string(domain.EvidenceOriginExternal) {
		t.Errorf("the contesting assertion = %q, want external", contesting.EvidenceOrigin)
	}

	// An assertion beta commits to a PRIVATE branch: same pair, same
	// stance vocabulary, but its own visibility axis says "not for the
	// network" — so it must not be rendered, and its event must not be
	// public.
	hidden := f.mustAssertEvidence(t, f.w.alice, f.beta.id, f.betaHidden,
		evidenceAssertionBody(f.alphaVersion, f.betaEvidence, "contextualizes"))
	if hidden.Visibility != string(domain.EvidenceVisibilityPrivate) {
		t.Errorf("an assertion on a private branch = %q, want private", hidden.Visibility)
	}

	// --- the network reads the page -------------------------------------
	status, raw, page := f.readEvidencePage(t, nil, f.alphaPID)
	if status != http.StatusOK {
		t.Fatalf("anonymous read of the published page = %d, want 200: %s", status, raw)
	}
	if got := assertionIDs(page.Evidence.Origin); len(got) != 1 || got[0] != origin.ID {
		t.Errorf("origin bucket = %v, want the published project's own assertion %s", got, origin.ID)
	}
	if got := assertionIDs(page.Evidence.UnreviewedExternal); len(got) != 2 {
		t.Errorf("unreviewed_external = %v, want the two public-branch assertions from the other project", got)
	}
	if got := assertionIDs(page.Evidence.ReviewedExternal); len(got) != 0 {
		t.Errorf("reviewed_external = %v, want empty (nothing has been reviewed yet)", got)
	}
	if page.Evidence.Truncated {
		t.Errorf("the section reports truncation with 3 of at most 200 assertions")
	}

	// The stances are the rows' own labels, side by side: nothing was
	// adjudicated, merged or counted (criterion 4's shape).
	stances := map[string]string{}
	for _, row := range page.Evidence.UnreviewedExternal {
		stances[row.ID] = row.Stance
	}
	if stances[supporting.ID] != "supporting" || stances[contesting.ID] != "contesting" {
		t.Errorf("stances = %v, want the two opposing labels side by side", stances)
	}
	for _, row := range page.Evidence.UnreviewedExternal {
		if row.AssertingProjectID != f.beta.id {
			t.Errorf("asserting_project_id = %q, want the other project (%s) — the origin axis is a comparison of the two", row.AssertingProjectID, f.beta.id)
		}
		if row.TargetObjectVersionID != f.alphaVersion {
			t.Errorf("target_object_version_id = %q, want the PUBLISHED version", row.TargetObjectVersionID)
		}
	}

	// --- nothing private is in the bytes --------------------------------
	for _, secret := range []string{hidden.ID, f.betaHidden, f.gamma.id, f.gammaVersion, f.gammaPID, f.gammaEvidence} {
		if strings.Contains(raw, secret) {
			t.Errorf("the network page discloses %q", secret)
		}
	}

	t.Run("a private project's assertion does not reach the page", func(t *testing.T) {
		// The write path refuses this assertion (a cross-project assertion
		// needs a network-visible publication), so the row is planted — and
		// PLANTED LOUDLY: it claims visibility 'public', which is the only
		// form that could be rendered if the read trusted its own row.
		var plantedID string
		if err := f.w.pool.QueryRow(ctx, `
			INSERT INTO evidence_assertions
				(project_id, state_id, target_object_version_id, evidence_object_version_id,
				 relation_type, evidence_type, scope, directness, inference_nature,
				 reasoning_note, created_by, evidence_origin, visibility)
			SELECT $1, sov.state_id, $2, $3, 'contradicts', 'experimental', '{}'::jsonb,
			       'direct', 'observational', 'planted', $4, 'external', 'public'
			FROM scientific_object_versions sov WHERE sov.id = $3
			RETURNING id::text`,
			parseUUIDOrDie(f.gamma.id), parseUUIDOrDie(f.alphaVersion),
			parseUUIDOrDie(f.gammaEvidence), parseUUIDOrDie(f.w.aliceID)).Scan(&plantedID); err != nil {
			t.Fatalf("plant a private project's assertion: %v", err)
		}

		// The row EXISTS (so the negative below is about the read, not about
		// a write that never happened)...
		var rows int
		if err := f.w.pool.QueryRow(ctx,
			`SELECT count(*) FROM evidence_assertions WHERE id = $1`, parseUUIDOrDie(plantedID)).Scan(&rows); err != nil {
			t.Fatalf("count the planted row: %v", err)
		}
		if rows != 1 {
			t.Fatalf("the planted row is not in the table (%d rows): the negative assertion would be vacuous", rows)
		}

		// ...and it is not in the document, nor in any of the three buckets.
		status, raw, page := f.readEvidencePage(t, nil, f.alphaPID)
		if status != http.StatusOK {
			t.Fatalf("anonymous read = %d, want 200: %s", status, raw)
		}
		if strings.Contains(raw, plantedID) {
			t.Errorf("a private project's assertion reached the page: %s", raw)
		}
		for _, bucket := range [][]evidenceRowWire{page.Evidence.Origin, page.Evidence.ReviewedExternal, page.Evidence.UnreviewedExternal} {
			for _, row := range bucket {
				if row.ID == plantedID || row.AssertingProjectID == f.gamma.id {
					t.Errorf("the planted row was rendered in %+v", row)
				}
			}
		}
	})

	t.Run("a members-only publication's evidence is unreachable", func(t *testing.T) {
		// gamma's own published object, with its own (origin) assertion: the
		// write path allows both, and the network may reach neither.
		gammaAssertion := f.mustAssertEvidence(t, f.w.alice, f.gamma.id, f.gamma.probe,
			evidenceAssertionBody(f.gammaVersion, f.gammaEvidence, "supports"))

		status, raw, page := f.readEvidencePage(t, nil, f.gammaPID)
		if status != http.StatusNotFound {
			t.Fatalf("anonymous read of a members-only publication = %d, want 404: %s", status, raw)
		}
		for _, secret := range []string{gammaAssertion.ID, f.gammaVersion, f.gamma.id} {
			if strings.Contains(raw, secret) {
				t.Errorf("the refusal discloses %q: %s", secret, raw)
			}
		}
		if page.Evidence.Origin != nil || page.Evidence.UnreviewedExternal != nil {
			t.Errorf("a 404 carried an evidence section: %+v", page.Evidence)
		}

		// The project's OWN member reads the page — and the section is still
		// empty, because the assertion's own axis is private (a private
		// project's preset) and the section renders only assertions the
		// network could be shown. Fail-closed: a member sees less, never
		// more, and never another project's rows.
		status, raw, page = f.readEvidencePage(t, f.w.alice, f.gammaPID)
		if status != http.StatusOK {
			t.Fatalf("member read = %d, want 200: %s", status, raw)
		}
		if page.Audience != "members" {
			t.Errorf("audience = %q, want members", page.Audience)
		}
		if len(page.Evidence.Origin)+len(page.Evidence.ReviewedExternal)+len(page.Evidence.UnreviewedExternal) != 0 {
			t.Errorf("a members-only page rendered evidence: %+v", page.Evidence)
		}
	})
}

// TestEvidenceReviewTransitionsMoveOneAssertionAtATime is criterion 3's
// second half: review_state changes INDEPENDENTLY per row on one (target,
// evidence) pair, and the pair is not unique — so a review of one stance
// cannot move, hide or merge the other.
func TestEvidenceReviewTransitionsMoveOneAssertionAtATime(t *testing.T) {
	ctx := testCtx(t)
	f := newExternalEvidenceFixture(t, ctx)

	supporting := f.mustAssertEvidence(t, f.w.alice, f.beta.id, f.betaPub,
		evidenceAssertionBody(f.alphaVersion, f.betaEvidence, "supports"))
	contesting := f.mustAssertEvidence(t, f.w.alice, f.beta.id, f.betaPub,
		evidenceAssertionBody(f.alphaVersion, f.betaEvidence, "contradicts"))

	// No unique constraint was introduced on the pair (criterion 3): the
	// only unique index on the table is the primary key, so two assertions on
	// one (target, evidence) pair are a legal state and not an accident of
	// this build's ordering.
	rows, err := f.w.pool.Query(ctx, `
		SELECT i.relname, ix.indisprimary, ix.indisunique, pg_get_indexdef(ix.indexrelid)
		FROM pg_index ix
		JOIN pg_class i ON i.oid = ix.indexrelid
		JOIN pg_class t ON t.oid = ix.indrelid
		WHERE t.relname = 'evidence_assertions' AND ix.indisunique`)
	if err != nil {
		t.Fatalf("read the unique indexes: %v", err)
	}
	defer rows.Close()
	uniques := 0
	for rows.Next() {
		var name, def string
		var primary, unique bool
		if err := rows.Scan(&name, &primary, &unique, &def); err != nil {
			t.Fatalf("scan the index row: %v", err)
		}
		uniques++
		if !primary {
			t.Errorf("evidence_assertions carries the unique index %s (%s): two stances on one pair must be able to coexist", name, def)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the unique indexes: %v", err)
	}
	if uniques != 1 {
		t.Errorf("unique indexes on evidence_assertions = %d, want exactly the primary key", uniques)
	}

	// The reviewer's write, expressed directly: V1 has no review route (see
	// the file header), and the database deliberately still allows this
	// UPDATE — 00091 takes only the DELETE half of the append-only pair.
	if _, err := f.w.pool.Exec(ctx,
		`UPDATE evidence_assertions SET review_state = 'reviewed' WHERE id = $1`,
		parseUUIDOrDie(supporting.ID)); err != nil {
		t.Fatalf("review an assertion: %v", err)
	}

	_, _, page := f.readEvidencePage(t, nil, f.alphaPID)
	if got := assertionIDs(page.Evidence.ReviewedExternal); len(got) != 1 || got[0] != supporting.ID {
		t.Errorf("reviewed_external = %v, want exactly the reviewed assertion %s", got, supporting.ID)
	}
	if got := assertionIDs(page.Evidence.UnreviewedExternal); len(got) != 1 || got[0] != contesting.ID {
		t.Errorf("unreviewed_external = %v, want the CONTESTING assertion still unreviewed — review moves one row, never the pair", got)
	}

	// A rejection is the third state and it does not delete anything: the
	// assertion stays in the picture, on the origin side of the review axis.
	if _, err := f.w.pool.Exec(ctx,
		`UPDATE evidence_assertions SET review_state = 'rejected' WHERE id = $1`,
		parseUUIDOrDie(supporting.ID)); err != nil {
		t.Fatalf("reject an assertion: %v", err)
	}
	_, _, page = f.readEvidencePage(t, nil, f.alphaPID)
	if got := assertionIDs(page.Evidence.ReviewedExternal); len(got) != 0 {
		t.Errorf("reviewed_external = %v, want empty after the rejection", got)
	}
	if got := assertionIDs(page.Evidence.UnreviewedExternal); len(got) != 2 {
		t.Errorf("unreviewed_external = %v, want both assertions: a rejected one is not removed (invariant 8)", got)
	}
}

// TestPublishedKnowledgeCarriesNoScore is criterion 4 as an assertion pinning
// ABSENCE: no response key, at any depth, is a score of any kind.
func TestPublishedKnowledgeCarriesNoScore(t *testing.T) {
	ctx := testCtx(t)
	f := newExternalEvidenceFixture(t, ctx)

	f.mustAssertEvidence(t, f.w.alice, f.alpha.id, f.alphaPub,
		evidenceAssertionBody(f.alphaVersion, f.alphaEvidence, "supports"))
	f.mustAssertEvidence(t, f.w.alice, f.beta.id, f.betaPub,
		evidenceAssertionBody(f.alphaVersion, f.betaEvidence, "contradicts"))

	status, raw, _ := f.readEvidencePage(t, nil, f.alphaPID)
	if status != http.StatusOK {
		t.Fatalf("anonymous read = %d: %s", status, raw)
	}
	var document map[string]any
	if err := json.Unmarshal([]byte(raw), &document); err != nil {
		t.Fatalf("decode the page: %v", err)
	}
	for _, banned := range []string{
		"truth_score", "research_score", "score", "scores", "confidence",
		"weight", "weights", "strength", "consensus", "vote", "count",
	} {
		if keyAnywhereIn(document, banned) {
			t.Errorf("the page carries a %q key: %s", banned, raw)
		}
	}
	// The words themselves must not appear anywhere in the bytes either: a
	// score smuggled into a message would be as bad as one in a field.
	for _, banned := range []string{"truth_score", "research_score", "truth score"} {
		if strings.Contains(strings.ToLower(raw), banned) {
			t.Errorf("the page mentions %q: %s", banned, raw)
		}
	}
}

// keyAnywhereIn reports whether any object in a decoded document has the key,
// at any depth.
func keyAnywhereIn(v any, key string) bool {
	switch node := v.(type) {
	case map[string]any:
		if _, present := node[key]; present {
			return true
		}
		for _, child := range node {
			if keyAnywhereIn(child, key) {
				return true
			}
		}
	case []any:
		for _, child := range node {
			if keyAnywhereIn(child, key) {
				return true
			}
		}
	}
	return false
}

// --------------------------------------------------------------------------
// 2: deletion

// TestExternalEvidenceCannotBeDeletedByAnyone is criterion 2, at the storage
// layer and at the surface:
//
//   - there is NO route to delete an assertion (the contract declares one
//     write path and this build adds no other), so the origin project's owner
//     — the actor the requirement names — has nothing to call;
//   - a raw DELETE that bypasses the application entirely is refused by the
//     DATABASE, which knows no actor and refuses it for everyone;
//   - and the guard is DELETE-only: the review UPDATE the criterion above
//     needs still lands, so this is protection of the history, not a freeze
//     of the row.
func TestExternalEvidenceCannotBeDeletedByAnyone(t *testing.T) {
	ctx := testCtx(t)
	f := newExternalEvidenceFixture(t, ctx)

	// The external counter-evidence, asserted by the OTHER project against
	// alpha's published version — the row the origin maintainer must not be
	// able to remove.
	external := f.mustAssertEvidence(t, f.w.alice, f.beta.id, f.betaPub,
		evidenceAssertionBody(f.alphaVersion, f.betaEvidence, "contradicts"))

	// The origin project's OWNER asks. (alice owns both projects in this
	// world, so the actor here IS the origin maintainer and the project
	// owner; a lesser role is refused by the same absence of a route.)
	t.Run("no route deletes an assertion", func(t *testing.T) {
		for _, path := range []string{
			"/api/v1/projects/" + f.alpha.id + "/branches/" + f.alphaPub + "/evidence-assertions/" + external.ID,
			"/api/v1/projects/" + f.alpha.id + "/branches/" + f.alphaPub + "/evidence-assertions",
			"/api/v1/projects/" + f.beta.id + "/branches/" + f.betaPub + "/evidence-assertions/" + external.ID,
		} {
			resp := f.w.alice.do(t, http.MethodDelete, path, "")
			status := resp.StatusCode
			readAll(t, resp)
			if status < 400 {
				t.Errorf("DELETE %s = %d, want a refusal: there is no delete path", path, status)
			}
		}
		if n := assertionRowsIn(t, ctx, f.w.pool, f.beta.id); n != 1 {
			t.Errorf("evidence assertions in the asserting project = %d, want 1 (the HTTP attempts must not have removed anything)", n)
		}
	})

	t.Run("the database refuses a raw DELETE", func(t *testing.T) {
		mustGuardRefusal(t, ctx, f.w.pool,
			`DELETE FROM evidence_assertions WHERE id = $1`, []any{parseUUIDOrDie(external.ID)},
			"evidence_assertions", "DELETE")

		// The wholesale path is closed too (00015's half).
		mustGuardRefusal(t, ctx, f.w.pool, `TRUNCATE evidence_assertions`, nil, "evidence_assertions", "TRUNCATE")

		// Both rows are still there: the refusal is not a silent zero-row
		// DELETE.
		if n := assertionRowsIn(t, ctx, f.w.pool, f.beta.id); n != 1 {
			t.Errorf("evidence assertions after the raw DELETE = %d, want the row intact", n)
		}
	})

	t.Run("the review transition is still legal", func(t *testing.T) {
		// The counterpart of the guard: 00091 takes only the DELETE half, so
		// the reviewer's write lands. A guard that froze the row would pass
		// every assertion above and break the class model.
		tag, err := f.w.pool.Exec(ctx,
			`UPDATE evidence_assertions SET review_state = 'reviewed' WHERE id = $1`,
			parseUUIDOrDie(external.ID))
		if err != nil {
			t.Fatalf("the review UPDATE was refused: %v", err)
		}
		if tag.RowsAffected() != 1 {
			t.Errorf("reviewed %d rows, want 1", tag.RowsAffected())
		}
		var state string
		if err := f.w.pool.QueryRow(ctx,
			`SELECT review_state FROM evidence_assertions WHERE id = $1`, parseUUIDOrDie(external.ID)).Scan(&state); err != nil {
			t.Fatalf("read the review state: %v", err)
		}
		if state != "reviewed" {
			t.Errorf("review_state = %q, want reviewed", state)
		}
	})
}

// mustGuardRefusal runs one statement and requires the append-only guard's
// refusal: SQLSTATE P0001 and a message naming the table and the operation.
func mustGuardRefusal(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args []any, table, op string) {
	t.Helper()
	_, err := pool.Exec(ctx, sql, args...)
	if err == nil {
		t.Fatalf("%s was accepted: the guard did not fire", sql)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("%s: expected a PostgreSQL error, got %v", sql, err)
	}
	if pgErr.Code != "P0001" {
		t.Errorf("%s: SQLSTATE = %s, want P0001 (raise_exception)", sql, pgErr.Code)
	}
	if !strings.Contains(pgErr.Message, table) {
		t.Errorf("%s: error %q must name the table %s", sql, pgErr.Message, table)
	}
	if !strings.Contains(pgErr.Message, op) {
		t.Errorf("%s: error %q must name the forbidden operation %s", sql, pgErr.Message, op)
	}
}

// --------------------------------------------------------------------------
// 5: the event

// TestExternalEvidenceAddedEventIsRealAndNeverMoreVisible is criterion 5: the
// event declared at specs/events/event-types.yaml:28 has a producer, the
// producer's row reaches research_events through the transactional outbox,
// and the visibility it carries is the assertion's own — never wider.
func TestExternalEvidenceAddedEventIsRealAndNeverMoreVisible(t *testing.T) {
	ctx := testCtx(t)
	f := newExternalEvidenceFixture(t, ctx)

	// An origin assertion: NOT "external evidence added", so no such event.
	origin := f.mustAssertEvidence(t, f.w.alice, f.alpha.id, f.alphaPub,
		evidenceAssertionBody(f.alphaVersion, f.alphaEvidence, "supports"))
	// An external assertion on a public branch: public evidence, public event.
	external := f.mustAssertEvidence(t, f.w.alice, f.beta.id, f.betaPub,
		evidenceAssertionBody(f.alphaVersion, f.betaEvidence, "contradicts"))
	// An external assertion on a private branch: an event, and a private one.
	hidden := f.mustAssertEvidence(t, f.w.alice, f.beta.id, f.betaHidden,
		evidenceAssertionBody(f.alphaVersion, f.betaEvidence, "contextualizes"))

	// The outbox holds the events inside the assertion's own state commit:
	// two external assertions, two rows (the origin one records only its
	// state.committed — the write is not silent, it is just not this news).
	if n := f.w.outboxRowsOf(t, ctx, externalEvidenceAddedEvent); n != 2 {
		t.Fatalf("outbox rows for %s = %d, want 2 (one per cross-project assertion)", externalEvidenceAddedEvent, n)
	}

	// The dispatcher publishes them into research_events (the chain
	// cmd/worker runs).
	dispatcher := events.NewDispatcher(f.w.pool, events.WithLogger(outboxTestLogger()))
	if _, err := dispatcher.RunOnce(ctx); err != nil {
		t.Fatalf("dispatcher pass: %v", err)
	}

	byAssertion := map[string]eventRow{}
	for _, e := range f.w.eventsOf(t, ctx, externalEvidenceAddedEvent) {
		id, _ := e.Payload["assertion_id"].(string)
		byAssertion[id] = e
	}
	if len(byAssertion) != 2 {
		t.Fatalf("research events for %s = %d, want one per cross-project assertion: %v", externalEvidenceAddedEvent, len(byAssertion), byAssertion)
	}
	if _, ok := byAssertion[origin.ID]; ok {
		t.Errorf("the origin assertion emitted %s: the event is about EXTERNAL evidence", externalEvidenceAddedEvent)
	}

	public, ok := byAssertion[external.ID]
	if !ok {
		t.Fatalf("no event for the cross-project assertion %s", external.ID)
	}
	if public.Visibility != "public" {
		t.Errorf("event visibility = %q, want public: the assertion and the publication it lands on are both public", public.Visibility)
	}
	if public.ProjectID != f.beta.id {
		t.Errorf("event project = %q, want the ASSERTING project %s", public.ProjectID, f.beta.id)
	}
	if public.ActorID != f.w.aliceID {
		t.Errorf("event actor = %q, want the session's user", public.ActorID)
	}
	for key, want := range map[string]string{
		"target_object_version_id":   f.alphaVersion,
		"evidence_object_version_id": f.betaEvidence,
		"relation":                   "contradicts",
		"evidence_type":              "experimental",
		"evidence_origin":            string(domain.EvidenceOriginExternal),
		"knowledge_pid":              f.alphaPID,
		"project_id":                 f.beta.id,
	} {
		if got, _ := public.Payload[key].(string); got != want {
			t.Errorf("payload[%q] = %q, want %q", key, got, want)
		}
	}

	private, ok := byAssertion[hidden.ID]
	if !ok {
		t.Fatalf("no event for the assertion on the private branch %s: the write is not silently skipped", hidden.ID)
	}
	if private.Visibility != "private" {
		t.Errorf("event visibility = %q for an assertion on a private branch, want private: an event is never more visible than its subject", private.Visibility)
	}

	// And the private assertion's event is not readable news either: the
	// fan-out's public view of the log holds exactly the public one.
	var publicRows int
	if err := f.w.pool.QueryRow(ctx,
		`SELECT count(*) FROM research_events WHERE event_type = $1 AND visibility = 'public'`,
		externalEvidenceAddedEvent).Scan(&publicRows); err != nil {
		t.Fatalf("count the public events: %v", err)
	}
	if publicRows != 1 {
		t.Errorf("public %s events = %d, want exactly the public assertion's", externalEvidenceAddedEvent, publicRows)
	}
}

// --------------------------------------------------------------------------
// The write path's refusals, over real rows

// TestEvidenceAssertionWriteFailsClosedOverRealRows drives the three
// refusals that need real storage to mean anything: an unpublished target, a
// members-only target asserted on from another project, and a cited version
// that belongs to someone else. All three answer the SAME outcome, and none
// of them leaves a row.
func TestEvidenceAssertionWriteFailsClosedOverRealRows(t *testing.T) {
	ctx := testCtx(t)
	f := newExternalEvidenceFixture(t, ctx)

	// A version that exists but is not published: no knowledge_publications
	// row, so it is not a published knowledge object (docs/10 §7).
	_, unpublished, _ := f.w.seedVersion(t, ctx, f.alpha.id, f.alpha.probe, "alpha unpublished")

	cases := []struct {
		name string
		// project and branch the assertion is posted in.
		project, branch string
		target          string
		evidence        string
	}{
		{
			name:    "the target is not published",
			project: f.alpha.id, branch: f.alphaPub,
			target: unpublished, evidence: f.alphaEvidence,
		},
		{
			name:    "another project asserts against a members-only publication",
			project: f.beta.id, branch: f.betaPub,
			target: f.gammaVersion, evidence: f.betaEvidence,
		},
		{
			name:    "the cited version belongs to another project",
			project: f.beta.id, branch: f.betaPub,
			target: f.alphaVersion, evidence: f.alphaEvidence,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := assertionRowsIn(t, ctx, f.w.pool, c.project)
			resp := f.postEvidenceAssertion(t, f.w.alice, c.project, c.branch,
				evidenceAssertionBody(c.target, c.evidence, "supports"))
			status := resp.StatusCode
			raw := readAll(t, resp)
			if status != http.StatusNotFound {
				t.Fatalf("status = %d, want the one existence-hiding 404: %s", status, raw)
			}
			var env struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal([]byte(raw), &env); err != nil {
				t.Fatalf("refusal envelope: %v: %s", err, raw)
			}
			if env.Code != "EVIDENCE_REF_UNAVAILABLE" {
				t.Errorf("code = %q, want EVIDENCE_REF_UNAVAILABLE", env.Code)
			}
			// The refusal says the same thing in all three cases: the caller
			// learns that the write did not happen, never which of the
			// reasons applied.
			for _, leak := range []string{"publication", "publish", "external", "audience"} {
				if strings.Contains(strings.ToLower(raw), leak) {
					t.Errorf("the refusal discloses %q: %s", leak, raw)
				}
			}
			if after := assertionRowsIn(t, ctx, f.w.pool, c.project); after != before {
				t.Errorf("rows after the refusal = %d, want %d: a refused write leaves nothing", after, before)
			}
		})
	}

	// The refusals above are not a broken route: the SAME request with a
	// published, network-visible target and the project's own cited version
	// lands.
	ok := f.mustAssertEvidence(t, f.w.alice, f.beta.id, f.betaPub,
		evidenceAssertionBody(f.alphaVersion, f.betaEvidence, "reproduces"))
	if ok.ReviewState != "unreviewed" || ok.EvidenceOrigin != "external" {
		t.Errorf("the control write = %+v, want an unreviewed external assertion", ok)
	}
}

// TestEvidenceAssertionAcceptsTheDeclaredParameterSet: a body built from the
// platform's OWN declaration for this operation must be enough to create an
// assertion. specs/mcp/tools.json's evidence.create_assertion lists
// project_id, branch_id, target_version_ref, evidence_version_ref, relation,
// scope, reasoning_note and idempotency_key — it lists neither directness nor
// inference_nature, so a caller that follows the declaration sends neither.
//
// Both of those columns are named explicitly by the INSERT, so the table's
// DEFAULT never applies to them and 00058's CHECK admits only the canonical
// vocabulary: an empty string is a REJECTED row. Left unnormalized, that
// rejection reaches the caller as a store failure — a permanently wrong
// request answered with a retryable 503, which is the one answer that must
// never describe a caller's own input (docs/45). The command therefore stores
// the schema's own 'unknown' for an axis the caller did not declare.
//
// evidence_type is also absent from tools.json's list but IS required by the
// transport, so the body below carries it; that contract drift is recorded in
// RESULT rather than papered over here.
func TestEvidenceAssertionAcceptsTheDeclaredParameterSet(t *testing.T) {
	ctx := testCtx(t)
	f := newExternalEvidenceFixture(t, ctx)

	// The declared set, and nothing else: no directness, no inference_nature.
	body := fmt.Sprintf(`{"target_version_ref":"object_version:%s","evidence_version_ref":"object_version:%s",`+
		`"relation":"supports","evidence_type":"experimental","scope":{"conditions":"298 K, 1 bar"},`+
		`"reasoning_note":"declared parameter set only"}`,
		f.alphaVersion, f.alphaEvidence)

	resp := f.postEvidenceAssertion(t, f.w.alice, f.alpha.id, f.alphaPub, body)
	raw := readAll(t, resp)
	if resp.StatusCode == http.StatusServiceUnavailable {
		t.Fatalf("503 for a body built from specs/mcp/tools.json's own declared parameter set — a caller that omits the two undeclared axes has made a permanent input mistake, and a retryable service failure is the one answer that must never describe it (docs/45): %s", raw)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body: %s)", resp.StatusCode, raw)
	}
	var got evidenceAssertionWire
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("evidence assertion payload: %v: %s", err, raw)
	}
	if got.Directness != string(domain.EvidenceDirectnessUnknown) {
		t.Errorf("directness = %q, want %q: the caller declared nothing, and 'unknown' is what the schema calls that",
			got.Directness, domain.EvidenceDirectnessUnknown)
	}
	if got.InferenceNature != string(domain.EvidenceInferenceUnknown) {
		t.Errorf("inference_nature = %q, want %q for an undeclared axis", got.InferenceNature, domain.EvidenceInferenceUnknown)
	}

	// The row is a real one, not a 201 over a half-written assertion: it is
	// readable back on the page it was asserted against, carrying the same
	// two values.
	status, _, page := f.readEvidencePage(t, nil, f.alphaPID)
	if status != http.StatusOK {
		t.Fatalf("the published page = %d, want 200", status)
	}
	found := false
	for _, row := range page.Evidence.Origin {
		if row.ID != got.ID {
			continue
		}
		found = true
		if row.Directness != string(domain.EvidenceDirectnessUnknown) || row.InferenceNature != string(domain.EvidenceInferenceUnknown) {
			t.Errorf("the stored row reads back as (%q, %q), want (%q, %q)",
				row.Directness, row.InferenceNature, domain.EvidenceDirectnessUnknown, domain.EvidenceInferenceUnknown)
		}
	}
	if !found {
		t.Errorf("the created assertion %s is not in the origin bucket of %s — the 201 did not land a readable row", got.ID, f.alphaPID)
	}
}

// TestEvidenceAssertionInputMistakesAreNeverServiceFailures: the other half of
// the same rule. A caller's own mistake is a PERMANENT condition — resending
// it changes nothing — so it must come back as a 4xx that names the problem,
// never as a retryable service failure: a caller that believes
// "retryable: true" retries a request that can never succeed (docs/45). That
// is what made the omission above expensive rather than merely wrong.
//
// Each case is a body a caller can really send: every field the declared
// parameter set leaves out, and every malformed declaration of the fields it
// carries.
func TestEvidenceAssertionInputMistakesAreNeverServiceFailures(t *testing.T) {
	ctx := testCtx(t)
	f := newExternalEvidenceFixture(t, ctx)

	// declared() is the parameter set specs/mcp/tools.json declares for
	// evidence.create_assertion, and nothing else: no directness, no
	// inference_nature (the transport additionally requires evidence_type,
	// which tools.json does not list — that drift is in RESULT).
	declared := func() map[string]any {
		return map[string]any{
			"target_version_ref":   "object_version:" + f.alphaVersion,
			"evidence_version_ref": "object_version:" + f.alphaEvidence,
			"relation":             "supports",
			"evidence_type":        "experimental",
			"scope":                map[string]any{"conditions": "298 K, 1 bar"},
			"reasoning_note":       "declared parameter set",
		}
	}
	body := func() string {
		raw, err := json.Marshal(declared())
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		return string(raw)
	}
	without := func(key string) string {
		fields := declared()
		delete(fields, key)
		raw, err := json.Marshal(fields)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		return string(raw)
	}
	withValue := func(key string, value any) string {
		fields := declared()
		fields[key] = value
		raw, err := json.Marshal(fields)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		return string(raw)
	}

	cases := []struct {
		name   string
		body   string
		status int
	}{
		{"the declared set and nothing else", body(), http.StatusCreated},
		{"without scope", without("scope"), http.StatusCreated},
		{"without reasoning_note", without("reasoning_note"), http.StatusCreated},
		{"without relation", without("relation"), http.StatusBadRequest},
		{"without evidence_type", without("evidence_type"), http.StatusBadRequest},
		{"without target_version_ref", without("target_version_ref"), http.StatusBadRequest},
		{"without evidence_version_ref", without("evidence_version_ref"), http.StatusBadRequest},
		{"a relation outside the catalog", withValue("relation", "proves"), http.StatusBadRequest},
		{"a directness outside the catalog", withValue("directness", "sort of"), http.StatusBadRequest},
		{"an inference_nature outside the catalog", withValue("inference_nature", "because"), http.StatusBadRequest},
		{"a scope that is not an object", withValue("scope", []string{"298 K"}), http.StatusBadRequest},
		{"the same version on both ends", withValue("evidence_version_ref", "object_version:"+f.alphaVersion), http.StatusBadRequest},
		{"a target ref that could never name a version", withValue("target_version_ref", "not-a-version"), http.StatusBadRequest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := f.postEvidenceAssertion(t, f.w.alice, f.alpha.id, f.alphaPub, tc.body)
			raw := readAll(t, resp)
			if resp.StatusCode >= 500 {
				t.Fatalf("status = %d for a body built from the caller's own input (%s): a permanent input condition must never be reported as retryable", resp.StatusCode, raw)
			}
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tc.status, raw)
			}
			if tc.status != http.StatusBadRequest {
				return
			}
			var e struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal([]byte(raw), &e); err != nil {
				t.Fatalf("the refusal is not the platform's error shape: %v: %s", err, raw)
			}
			if e.Code != rsg.CodeValidation {
				t.Errorf("code = %q, want %q", e.Code, rsg.CodeValidation)
			}
		})
	}
}
