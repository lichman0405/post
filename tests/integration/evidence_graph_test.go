// Task T0506 required test "evidence graph integration".
//
// The task's acceptance criteria, over real rows and the whole composed path:
//
//  1. WHY BELIEVE / WHY DOUBT. One object version carries a supporting AND a
//     contesting assertion — on the SAME (target, evidence) pair, which
//     infra/migrations/00058 declares NOT unique BY DESIGN — and the read
//     returns both, each in its own group.
//  2. THE ANTI-SHAPE IS PINNED TOO. Both rows must still be there as two
//     rows (the same id appears exactly once in the answer: not merged, not
//     duplicated across buckets), and no count, ratio or net field may
//     appear at any depth. The scan that forbids them carries its own
//     self-check, because a check that forbade nothing reads green while
//     measuring nothing (tests/integration/asset_page_test.go's own note).
//  3. THE HYPOTHESIS PAGE'S TWO SECTIONS STAY APART. The hypothesis H
//     carries an assertion directly; its subordinate claim C carries its
//     own. H's section must not contain C's assertion, and no aggregate
//     appears anywhere.
//  4. STANCE LABELS ARE THE DOMAIN'S. The storage CHECK's relation set and
//     domain.CanonicalEvidenceRelations() are the same nine, every one of
//     them maps to a stance, and a relation outside the nine cannot be
//     stored at all — so the fail-closed branch (an unmapped relation
//     rendered with no label) is unreachable through storage. That branch
//     itself is pinned where it can be constructed:
//     internal/evidence/projection_test.go's
//     TestUnmappedRelationKeepsNoStanceLabel.
//  5. THE PROJECT GATE RUNS FIRST: a private project's evidence answers the
//     existence-hiding PROJECT_NOT_FOUND to an anonymous reader and to a
//     non-member, whatever object id and version_no are asked for.
//
// Everything is seeded through the product's own surfaces (the RSG service,
// the contract's evidence-assertion route, the knowledge publish route) — no
// hand-built rows.

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/domain"
)

// evidenceGraphTaskID namespaces this task's test databases (test_T0506_<run_id>).
const evidenceGraphTaskID = "T0506"

// --------------------------------------------------------------------------
// The wire shapes this suite reads (declared here rather than reused from the
// transport, so a silent JSON tag change fails here)

type evidenceGraphAssertionWire struct {
	ID                      string          `json:"id"`
	Relation                string          `json:"relation"`
	Stance                  string          `json:"stance"`
	EvidenceType            string          `json:"evidence_type"`
	Directness              string          `json:"directness"`
	InferenceNature         string          `json:"inference_nature"`
	Scope                   json.RawMessage `json:"scope"`
	ReasoningNote           string          `json:"reasoning_note"`
	ReviewState             string          `json:"review_state"`
	TargetObjectVersionID   string          `json:"target_object_version_id"`
	EvidenceObjectVersionID string          `json:"evidence_object_version_id"`
	CreatedAt               string          `json:"created_at"`
}

type evidenceGraphTargetWire struct {
	ObjectVersionID string `json:"object_version_id"`
	VersionNo       *int   `json:"version_no"`
	Title           string `json:"title"`
}

type evidenceGraphGroupWire struct {
	Target     evidenceGraphTargetWire      `json:"target"`
	Supporting []evidenceGraphAssertionWire `json:"supporting"`
	Contesting []evidenceGraphAssertionWire `json:"contesting"`
	Neutral    []evidenceGraphAssertionWire `json:"neutral"`
	Unlabeled  []evidenceGraphAssertionWire `json:"unlabeled"`
}

type evidenceGraphObjectRefWire struct {
	ObjectID   string `json:"object_id"`
	ObjectType string `json:"object_type"`
	Title      string `json:"title"`
}

type evidenceGraphObjectReadWire struct {
	ProjectID string                     `json:"project_id"`
	Object    evidenceGraphObjectRefWire `json:"object"`
	Groups    []evidenceGraphGroupWire   `json:"groups"`
}

type evidenceGraphClaimRefWire struct {
	ObjectID   string `json:"object_id"`
	ObjectType string `json:"object_type"`
	Title      string `json:"title"`
}

type evidenceGraphClaimWire struct {
	Claim    evidenceGraphClaimRefWire `json:"claim"`
	Evidence []evidenceGraphGroupWire  `json:"evidence"`
}

type evidenceGraphHypothesisReadWire struct {
	ProjectID  string                     `json:"project_id"`
	Hypothesis evidenceGraphObjectRefWire `json:"hypothesis"`
	Direct     []evidenceGraphGroupWire   `json:"direct"`
	Claims     []evidenceGraphClaimWire   `json:"claims"`
}

// --------------------------------------------------------------------------
// The fixture

// evidenceGraphFixture is the world the cases share: one PUBLIC project that
// owns a published hypothesis and a published claim subordinate to it, plus
// a PRIVATE project that exists only to be hidden.
type evidenceGraphFixture struct {
	w     *knowledgeWorld
	alpha *knowledgeProject
	gamma *knowledgeProject

	// pub is the public branch the assertions and the subordination edge are
	// committed to.
	pub string

	questionID string

	// The published hypothesis and its version.
	hypothesisID      string
	hypothesisVersion string

	// The published claim (subordinate to the hypothesis) and its version.
	claimID      string
	claimVersion string

	// The two object versions cited as evidence. They are one version each:
	// the pair below deliberately reuses ONE of them for both stances.
	evidenceA string
	evidenceB string
}

func newEvidenceGraphFixture(t *testing.T, ctx context.Context) *evidenceGraphFixture {
	t.Helper()
	w := newKnowledgeWorldFor(t, ctx, evidenceGraphTaskID)
	w.signups(t)

	f := &evidenceGraphFixture{w: w}
	f.alpha = w.newProject(t, ctx, "eg-alpha", "public")
	f.gamma = w.newProject(t, ctx, "eg-gamma", "private")
	f.pub = w.createPublicBranch(t, ctx, f.alpha, "pub")

	// The question the hypothesis answers (migration 00040's guard resolves
	// hypothesis.question_id against an existing research_question of the
	// same project, so it has to exist first).
	f.questionID, _, _ = w.seedVersion(t, ctx, f.alpha.id, f.alpha.probe, "What is the CO2 uptake of MOF-5 at 298 K")

	// The hypothesis, published: an evidence assertion's target must be
	// published knowledge, which is the write path's own rule (T0806).
	f.hypothesisID, f.hypothesisVersion, _ = w.seedPublishedVersion(t, ctx, f.alpha, "hypothesis",
		fmt.Sprintf(`{"statement":"MOF-5 maximizes CO2 uptake at 298 K","question_id":%q,`+
			`"hypothesis_type":"mechanistic","scope":{"conditions":"298 K, 1 bar"}}`, f.questionID), 1)

	// Its subordinate claim, published too (it carries its own assertions).
	f.claimID, f.claimVersion, _ = w.seedPublishedVersion(t, ctx, f.alpha, "claim",
		fmt.Sprintf(`{"statement":"MOF-5 takes up 5.1 mmol/g CO2 at 298 K and 1 bar",`+
			`"claim_type":"descriptive","subject_ref":%q,"property":"CO2 uptake",`+
			`"scope":{"conditions":"298 K, 1 bar"},"assessment":"preliminary"}`, f.questionID), 2)

	// The evidence units the assertions cite (any version of the same
	// project; the write path resolves both ends and never leaves it).
	_, f.evidenceA, _ = w.seedVersion(t, ctx, f.alpha.id, f.alpha.probe, "uptake replication, same lab")
	_, f.evidenceB, _ = w.seedVersion(t, ctx, f.alpha.id, f.alpha.probe, "uptake counter-experiment, different activation")

	// The subordination edge: a claim TESTS a hypothesis (docs/44's catalog,
	// target pinned to a hypothesis by migration 00040). Created through the
	// production command, like every other write here.
	if _, err := w.svc.CreateRelation(ctx, domain.User{ID: w.aliceID}, f.alpha.id, f.pub, rsg.CreateRelationInput{
		RelationType:          "tests_hypothesis",
		SourceObjectVersionID: f.claimVersion,
		TargetObjectVersionID: f.hypothesisVersion,
		Payload:               json.RawMessage(`{"note":"the claim is one of the hypothesis's own"}`),
	}); err != nil {
		t.Fatalf("create tests_hypothesis relation: %v", err)
	}
	return f
}

// seedPublishedVersion creates one object of the given type through the RSG
// service, proposes its state to main with the two approved reviews
// docs/43 requires, and publishes it through the contract's route —
// the same ladder seedReviewedVersion climbs, for the two types whose
// evidence this suite reads.
func (w *knowledgeWorld) seedPublishedVersion(t *testing.T, ctx context.Context, p *knowledgeProject, objectType, payload string, prNumber int64) (objectID, versionID, stateID string) {
	t.Helper()
	res, err := w.svc.CreateObject(ctx, domain.User{ID: w.aliceID}, p.id, p.probe, rsg.CreateObjectInput{
		ObjectType: objectType,
		Payload:    json.RawMessage(payload),
	})
	if err != nil {
		t.Fatalf("CreateObject(%s) on %s: %v", objectType, p.probe, err)
	}
	objectID, versionID, stateID = res.Object.ID, res.Version.ID, res.Version.StateID
	pr := w.seedPR(t, ctx, p, prNumber, stateID)
	w.approve(t, ctx, pr, stateID, "scientific")
	w.approve(t, ctx, pr, stateID, "integrity")
	w.mustKnowledgePublish(t, w.alice, p.id, knowledgePublishBody(t, versionID, "v1.0", ""), "")
	return objectID, versionID, stateID
}

// --------------------------------------------------------------------------
// Requests

// evidenceGraphURL is the object read's path; version_no < 1 means "no pin".
func evidenceGraphURL(projectID, objectID string, versionNo int) string {
	u := "/api/v1/projects/" + projectID + "/objects/" + objectID + "/evidence"
	if versionNo > 0 {
		u += fmt.Sprintf("?version_no=%d", versionNo)
	}
	return u
}

func (f *evidenceGraphFixture) get(t *testing.T, uc *testUserClient, path string) (int, string) {
	t.Helper()
	if uc == nil {
		uc = newTestUserClient(f.w.ts.URL)
	}
	resp := uc.do(t, http.MethodGet, path, "")
	return resp.StatusCode, readAll(t, resp)
}

// mustObjectEvidence requires the 200 and decodes the object read.
func (f *evidenceGraphFixture) mustObjectEvidence(t *testing.T, uc *testUserClient, projectID, objectID string, versionNo int) evidenceGraphObjectReadWire {
	t.Helper()
	status, raw := f.get(t, uc, evidenceGraphURL(projectID, objectID, versionNo))
	if status != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200: %s", evidenceGraphURL(projectID, objectID, versionNo), status, raw)
	}
	var got evidenceGraphObjectReadWire
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("object evidence payload: %v: %s", err, raw)
	}
	return got
}

// mustHypothesisEvidence requires the 200 and decodes the hypothesis page.
func (f *evidenceGraphFixture) mustHypothesisEvidence(t *testing.T, uc *testUserClient, projectID, objectID string) (evidenceGraphHypothesisReadWire, string) {
	t.Helper()
	path := "/api/v1/projects/" + projectID + "/hypotheses/" + objectID + "/evidence"
	status, raw := f.get(t, uc, path)
	if status != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200: %s", path, status, raw)
	}
	var got evidenceGraphHypothesisReadWire
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("hypothesis evidence payload: %v: %s", err, raw)
	}
	return got, raw
}

// mustAssertEvidence posts one assertion through the contract's route and
// returns the stored id.
func (f *evidenceGraphFixture) mustAssertEvidence(t *testing.T, projectID, branchID, body string) string {
	t.Helper()
	resp := f.w.alice.do(t, http.MethodPost,
		"/api/v1/projects/"+projectID+"/branches/"+branchID+"/evidence-assertions", body)
	mustStatus(t, resp, http.StatusCreated)
	raw := readAll(t, resp)
	var got evidenceAssertionWire
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("evidence assertion payload: %v: %s", err, raw)
	}
	return got.ID
}

// --------------------------------------------------------------------------
// The anti-shape scan, and the proof that it can fail

// evidenceGraphAggregateFragments are the key shapes the domain forbids
// (docs/10 §4 「V1 不自动赋数值权重」; CLAUDE.md §9.13). They are matched as
// substrings of a JSON KEY, case-insensitively, so a field named
// supporting_count, total_evidence, net_stance or truth_score cannot slip
// through whatever it is spelled.
var evidenceGraphAggregateFragments = []string{
	"count", "total", "score", "ratio", "weight", "net", "sum", "average",
	"percent", "confidence", "vote", "consensus", "strength", "summary", "stancestrength",
}

// aggregateKeysIn returns every JSON key of the document (at any depth) that
// names an aggregate.
func aggregateKeysIn(t *testing.T, raw string) []string {
	t.Helper()
	var doc any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("decode for the aggregate scan: %v: %s", err, raw)
	}
	var found []string
	var walk func(node any)
	walk = func(node any) {
		switch v := node.(type) {
		case map[string]any:
			for key, child := range v {
				low := strings.ToLower(key)
				for _, frag := range evidenceGraphAggregateFragments {
					if strings.Contains(low, frag) {
						found = append(found, key)
					}
				}
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(doc)
	return found
}

// TestEvidenceGraphAggregateScanCanFail: the scan is measured before its
// silence is trusted. An empty result means "the read carries no aggregate"
// only if the same scan DOES report one when it is there.
func TestEvidenceGraphAggregateScanCanFail(t *testing.T) {
	planted := `{"groups":[{"supporting_count":2,"net_stance":"supports","rows":[{"truth_score":0.9}]}]}`
	got := aggregateKeysIn(t, planted)
	if len(got) != 3 {
		t.Fatalf("the scan flagged %v, want the three planted keys: a scan that forbids nothing reads green while measuring nothing", got)
	}
	// And it must not fire on the shapes this read legitimately carries.
	clean := `{"groups":[{"target":{"object_version_id":"x","version_no":1},"supporting":[],"contesting":[]}]}`
	if got := aggregateKeysIn(t, clean); len(got) != 0 {
		t.Fatalf("the scan flagged %v on a legitimate document", got)
	}
}

// --------------------------------------------------------------------------
// 1 and 2: why believe / why doubt, and the anti-shape

// TestEvidenceGraphAnswersWhyBelieveAndWhyDoubt is the task's centre: one
// target version carries BOTH stances and the read returns both.
func TestEvidenceGraphAnswersWhyBelieveAndWhyDoubt(t *testing.T) {
	ctx := testCtx(t)
	f := newEvidenceGraphFixture(t, ctx)

	// The SAME evidence version supports and contradicts the SAME target
	// version — 00058's own shape ("one evidence version may carry a supports
	// AND a contradicts assertion on the same target version, side by side,
	// each reviewed on its own").
	supporting := f.mustAssertEvidence(t, f.alpha.id, f.pub,
		evidenceAssertionBody(f.hypothesisVersion, f.evidenceA, "supports"))
	contesting := f.mustAssertEvidence(t, f.alpha.id, f.pub,
		evidenceAssertionBody(f.hypothesisVersion, f.evidenceA, "contradicts"))
	if supporting == contesting {
		t.Fatalf("the two writes produced one assertion id (%s): the pair is not unique by design", supporting)
	}

	read := f.mustObjectEvidence(t, f.w.alice, f.alpha.id, f.hypothesisID, 0)
	if len(read.Groups) != 1 {
		t.Fatalf("groups = %d, want one per pinned target version: %+v", len(read.Groups), read.Groups)
	}
	group := read.Groups[0]
	if group.Target.ObjectVersionID != f.hypothesisVersion {
		t.Errorf("group target = %s, want the hypothesis version %s", group.Target.ObjectVersionID, f.hypothesisVersion)
	}
	if group.Target.VersionNo == nil || *group.Target.VersionNo != 1 {
		t.Errorf("group version_no = %v, want the version's own number (1)", group.Target.VersionNo)
	}
	if got := assertionIDsOf(group.Supporting); len(got) != 1 || got[0] != supporting {
		t.Errorf("supporting = %v, want exactly [%s]", got, supporting)
	}
	if got := assertionIDsOf(group.Contesting); len(got) != 1 || got[0] != contesting {
		t.Errorf("contesting = %v, want exactly [%s]", got, contesting)
	}
	if len(group.Neutral)+len(group.Unlabeled) != 0 {
		t.Errorf("the pair leaked into another bucket: neutral=%v unlabeled=%v",
			assertionIDsOf(group.Neutral), assertionIDsOf(group.Unlabeled))
	}
	// The labels are the domain rule's, not a second copy of it. Guarded by
	// length so a bucketing mutant reports the whole picture instead of
	// panicking on the first index.
	if len(group.Supporting) == 1 && group.Supporting[0].Stance != string(domain.EvidenceStanceSupporting) {
		t.Errorf("supporting stance = %q, want %q", group.Supporting[0].Stance, domain.EvidenceStanceSupporting)
	}
	if len(group.Contesting) == 1 && group.Contesting[0].Stance != string(domain.EvidenceStanceContesting) {
		t.Errorf("contesting stance = %q, want %q", group.Contesting[0].Stance, domain.EvidenceStanceContesting)
	}
	// The real source axis is carried; see the fixture note on the axis that
	// deliberately is not.
	for _, row := range append(append([]evidenceGraphAssertionWire{}, group.Supporting...), group.Contesting...) {
		if row.EvidenceType != "experimental" {
			t.Errorf("evidence_type = %q, want the stored axis (%q)", row.EvidenceType, "experimental")
		}
		if !strings.Contains(string(row.Scope), "298 K") {
			t.Errorf("scope = %s, want the author's scope passed through", row.Scope)
		}
		if row.TargetObjectVersionID != f.hypothesisVersion || row.EvidenceObjectVersionID != f.evidenceA {
			t.Errorf("the two version pins are not carried: %+v", row)
		}
		if row.ReviewState != string(domain.EvidenceReviewUnreviewed) {
			t.Errorf("review_state = %q, want the row's stored state", row.ReviewState)
		}
	}

	// The anti-shape, on the raw bytes: two rows, not one merged row.
	_, raw := f.get(t, f.w.alice, evidenceGraphURL(f.alpha.id, f.hypothesisID, 0))
	if n := strings.Count(raw, supporting); n != 1 {
		t.Errorf("the supporting assertion appears %d times in the answer, want exactly 1 (merged or duplicated?)", n)
	}
	if n := strings.Count(raw, contesting); n != 1 {
		t.Errorf("the contesting assertion appears %d times in the answer, want exactly 1 (merged or duplicated?)", n)
	}
	if keys := aggregateKeysIn(t, raw); len(keys) != 0 {
		t.Errorf("the answer carries aggregate key(s) %v: %s", keys, raw)
	}
	// The pinned read answers the same one version's group.
	pinned := f.mustObjectEvidence(t, f.w.alice, f.alpha.id, f.hypothesisID, 1)
	if len(pinned.Groups) != 1 || pinned.Groups[0].Target.ObjectVersionID != f.hypothesisVersion {
		t.Errorf("pinned read groups = %+v, want the one version asked for", pinned.Groups)
	}
	if got := assertionIDsOf(pinned.Groups[0].Contesting); len(got) != 1 || got[0] != contesting {
		t.Errorf("pinned read contesting = %v, want [%s]", got, contesting)
	}
	// A second version of the SAME object, carrying nothing: asked for by
	// number it answers with its own empty group, and the unpinned read stays
	// an inventory of the versions that carry evidence.
	second, err := f.w.svc.CreateObjectVersion(ctx, domain.User{ID: f.w.aliceID}, f.alpha.id, f.alpha.probe,
		f.hypothesisID, rsg.CreateObjectVersionInput{
			ExpectedVersion: 1,
			Patch:           json.RawMessage(`{"statement":"MOF-5 maximizes CO2 uptake at 298 K (wording clarified)"}`),
		})
	if err != nil {
		t.Fatalf("create the hypothesis's second version: %v", err)
	}
	empty := f.mustObjectEvidence(t, f.w.alice, f.alpha.id, f.hypothesisID, 2)
	if len(empty.Groups) != 1 || empty.Groups[0].Target.ObjectVersionID != second.Version.ID {
		t.Errorf("a pinned version with no assertions answered %+v, want its own empty group", empty.Groups)
	}
	if empty.Groups[0].Supporting == nil || empty.Groups[0].Contesting == nil {
		t.Errorf("the empty group rendered null buckets: %+v", empty.Groups[0])
	}
	if empty.Object.Title != "MOF-5 maximizes CO2 uptake at 298 K (wording clarified)" {
		t.Errorf("object title = %q, want the newest version's title", empty.Object.Title)
	}
	inventory := f.mustObjectEvidence(t, f.w.alice, f.alpha.id, f.hypothesisID, 0)
	if len(inventory.Groups) != 1 || inventory.Groups[0].Target.ObjectVersionID != f.hypothesisVersion {
		t.Errorf("the unpinned read lists %+v, want only the version that carries evidence", inventory.Groups)
	}
}

func assertionIDsOf(rows []evidenceGraphAssertionWire) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

// --------------------------------------------------------------------------
// 4: the hypothesis page's two sections

// TestHypothesisEvidenceKeepsItsTwoSectionsApart: the hypothesis's own
// assertion and its subordinate claim's assertion are in TWO sections, and
// each section holds only its own.
func TestHypothesisEvidenceKeepsItsTwoSectionsApart(t *testing.T) {
	ctx := testCtx(t)
	f := newEvidenceGraphFixture(t, ctx)

	onHypothesis := f.mustAssertEvidence(t, f.alpha.id, f.pub,
		evidenceAssertionBody(f.hypothesisVersion, f.evidenceA, "supports"))
	onClaim := f.mustAssertEvidence(t, f.alpha.id, f.pub,
		evidenceAssertionBody(f.claimVersion, f.evidenceB, "contradicts"))

	page, raw := f.mustHypothesisEvidence(t, f.w.alice, f.alpha.id, f.hypothesisID)

	if page.Hypothesis.ObjectID != f.hypothesisID || page.Hypothesis.ObjectType != "hypothesis" {
		t.Errorf("hypothesis ref = %+v, want the object asked for", page.Hypothesis)
	}
	// Section 1: the hypothesis's own versions.
	if len(page.Direct) != 1 {
		t.Fatalf("direct sections = %d, want one group for the hypothesis's evidence-bearing version", len(page.Direct))
	}
	if page.Direct[0].Target.ObjectVersionID != f.hypothesisVersion {
		t.Errorf("direct target = %s, want the hypothesis version", page.Direct[0].Target.ObjectVersionID)
	}
	if got := assertionIDsOf(page.Direct[0].Supporting); len(got) != 1 || got[0] != onHypothesis {
		t.Errorf("direct supporting = %v, want [%s]", got, onHypothesis)
	}
	// Section 2: the subordinate claim, with its own.
	if len(page.Claims) != 1 {
		t.Fatalf("claims = %d, want the one subordinate claim", len(page.Claims))
	}
	if page.Claims[0].Claim.ObjectID != f.claimID || page.Claims[0].Claim.ObjectType != "claim" {
		t.Errorf("claim ref = %+v, want the claim that tests the hypothesis", page.Claims[0].Claim)
	}
	if len(page.Claims[0].Evidence) != 1 {
		t.Fatalf("claim evidence = %+v, want one group", page.Claims[0].Evidence)
	}
	if page.Claims[0].Evidence[0].Target.ObjectVersionID != f.claimVersion {
		t.Errorf("claim evidence target = %s, want the claim version", page.Claims[0].Evidence[0].Target.ObjectVersionID)
	}
	if got := assertionIDsOf(page.Claims[0].Evidence[0].Contesting); len(got) != 1 || got[0] != onClaim {
		t.Errorf("claim contesting = %v, want [%s]", got, onClaim)
	}

	// The separation itself: H's section must not contain C's assertion.
	for _, bucket := range [][]evidenceGraphAssertionWire{
		page.Direct[0].Supporting, page.Direct[0].Contesting, page.Direct[0].Neutral, page.Direct[0].Unlabeled,
	} {
		for _, row := range bucket {
			if strings.Contains(row.ID, onClaim) {
				t.Errorf("the claim's assertion %s appears in the hypothesis's own section", row.ID)
			}
		}
	}
	for _, g := range page.Claims[0].Evidence {
		for _, bucket := range [][]evidenceGraphAssertionWire{g.Supporting, g.Contesting, g.Neutral, g.Unlabeled} {
			for _, row := range bucket {
				if strings.Contains(row.ID, onHypothesis) {
					t.Errorf("the hypothesis's own assertion %s appears in a claim's section", row.ID)
				}
			}
		}
	}
	// Each assertion is rendered exactly once across the whole document, and
	// no aggregate appears anywhere.
	if n := strings.Count(raw, onHypothesis); n != 1 {
		t.Errorf("the hypothesis's assertion appears %d times in the page, want exactly 1", n)
	}
	if n := strings.Count(raw, onClaim); n != 1 {
		t.Errorf("the claim's assertion appears %d times in the page, want exactly 1 (two sections must not both list it)", n)
	}
	if keys := aggregateKeysIn(t, raw); len(keys) != 0 {
		t.Errorf("the hypothesis page carries aggregate key(s) %v: %s", keys, raw)
	}
	// The route's name is its contract: a claim asked for as a hypothesis is
	// not found (the same answer an unknown object gets).
	status, body := f.get(t, f.w.alice, "/api/v1/projects/"+f.alpha.id+"/hypotheses/"+f.claimID+"/evidence")
	if status != http.StatusNotFound {
		t.Errorf("GET a claim's hypothesis page = %d, want 404: %s", status, body)
	}
}

// --------------------------------------------------------------------------
// 3: the stance vocabulary, at the storage boundary

// TestStoredRelationsAreWithinTheDomainStanceVocabulary: the fail-closed
// branch (a relation the rule cannot place, rendered with no label) is
// unreachable THROUGH STORAGE — the table's CHECK admits exactly the domain's
// nine — and all nine map to a stance. Both halves are read off the live
// database, not off a constant in this file.
func TestStoredRelationsAreWithinTheDomainStanceVocabulary(t *testing.T) {
	ctx := testCtx(t)
	w := newKnowledgeWorldFor(t, ctx, evidenceGraphTaskID)

	rows, err := w.pool.Query(ctx, `
		SELECT pg_get_constraintdef(oid)
		FROM pg_constraint
		WHERE conrelid = 'evidence_assertions'::regclass AND contype = 'c'`)
	if err != nil {
		t.Fatalf("read evidence_assertions' CHECK constraints: %v", err)
	}
	defer rows.Close()
	var defs []string
	for rows.Next() {
		var def string
		if err := rows.Scan(&def); err != nil {
			t.Fatalf("scan constraint: %v", err)
		}
		defs = append(defs, def)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read constraints: %v", err)
	}
	joined := strings.Join(defs, "\n")
	if strings.TrimSpace(joined) == "" {
		t.Fatalf("evidence_assertions has no CHECK constraints: this read is not reading the table's own guards")
	}
	if !strings.Contains(joined, "relation_type") {
		t.Fatalf("no relation_type guard among the table's CHECKs: %s", joined)
	}
	for _, rel := range domain.CanonicalEvidenceRelations() {
		if !strings.Contains(joined, "'"+rel+"'") {
			t.Errorf("the stored vocabulary does not admit %q, which domain.CanonicalEvidenceRelations() names: %s", rel, joined)
		}
		if _, ok := domain.RelationStance(domain.EvidenceRelation(rel)); !ok {
			t.Errorf("domain.RelationStance(%q) has no answer, so a stored row could not be placed", rel)
		}
	}
	// And the vocabulary has no ninth-plus-one: nothing the domain does not
	// name may be stored. 'supersedes' is a canonical RSG relation and NOT an
	// evidence relation, which makes it the sharpest probe available.
	if strings.Contains(joined, "'supersedes'") {
		t.Errorf("the stored vocabulary admits a relation the domain does not map: %s", joined)
	}
}

// --------------------------------------------------------------------------
// 5: the gate runs first

// TestEvidenceGraphIsGatedBeforeAnyRead: a project this caller may not read
// answers the existence-hiding 404 — for every object id and every
// version_no, so the route cannot be used to learn that a private project or
// an object inside it exists.
func TestEvidenceGraphIsGatedBeforeAnyRead(t *testing.T) {
	ctx := testCtx(t)
	f := newEvidenceGraphFixture(t, ctx)

	// The private project's own object, seeded through the product path so it
	// really exists (the point is that its existence is not observable).
	hiddenID, hiddenVersion, _ := f.w.seedVersion(t, ctx, f.gamma.id, f.gamma.probe, "private work")

	paths := []string{
		"/api/v1/projects/" + f.gamma.id + "/objects/" + hiddenID + "/evidence",
		"/api/v1/projects/" + f.gamma.id + "/objects/" + hiddenID + "/evidence?version_no=1",
		"/api/v1/projects/" + f.gamma.id + "/hypotheses/" + hiddenID + "/evidence",
		// The path project is alpha but the object is gamma's: a foreign
		// object answers what a nonexistent one answers.
		"/api/v1/projects/" + f.alpha.id + "/objects/" + hiddenID + "/evidence",
		"/api/v1/projects/" + f.alpha.id + "/objects/" + hiddenVersion + "/evidence",
	}
	for _, uc := range []*testUserClient{nil, f.w.bob} {
		who := "anonymous"
		if uc != nil {
			who = "non-member"
		}
		for _, path := range paths {
			status, body := f.get(t, uc, path)
			if status != http.StatusNotFound {
				t.Errorf("%s GET %s = %d, want the existence-hiding 404: %s", who, path, status, body)
				continue
			}
			if !strings.Contains(body, "PROJECT_NOT_FOUND") && !strings.Contains(body, "OBJECT_NOT_FOUND") {
				t.Errorf("%s GET %s answered %s, want a not-found code with no detail", who, path, body)
			}
			if strings.Contains(body, hiddenID) || strings.Contains(body, f.gamma.id) {
				t.Errorf("%s GET %s echoed an id back: %s", who, path, body)
			}
		}
	}
	// A member reads the same private project's object (the gate is a gate,
	// not a wall), and an anonymous reader does reach a PUBLIC project.
	if status, body := f.get(t, f.w.alice, "/api/v1/projects/"+f.gamma.id+"/objects/"+hiddenID+"/evidence"); status != http.StatusOK {
		t.Errorf("the project's own member got %d on a public read, want 200: %s", status, body)
	}
	if status, body := f.get(t, nil, evidenceGraphURL(f.alpha.id, f.hypothesisID, 0)); status != http.StatusOK {
		t.Errorf("anonymous read of a public project = %d, want 200: %s", status, body)
	}
	// A client-shaped version_no is refused before any read.
	status, body := f.get(t, f.w.alice, evidenceGraphURL(f.alpha.id, f.hypothesisID, 0)+"?version_no=0")
	if status != http.StatusBadRequest || !strings.Contains(body, "EVIDENCE_VALIDATION_FAILED") {
		t.Errorf("version_no=0 = %d %s, want 400 EVIDENCE_VALIDATION_FAILED", status, body)
	}
	// A version the log does not hold is its own not-found.
	status, body = f.get(t, f.w.alice, evidenceGraphURL(f.alpha.id, f.hypothesisID, 99))
	if status != http.StatusNotFound || !strings.Contains(body, "OBJECT_VERSION_NOT_FOUND") {
		t.Errorf("version_no=99 = %d %s, want 404 for the version", status, body)
	}
}
