package rsg

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
)

// Outline unit tests (T0211): the aggregation rules over fakes — question
// tree building (parent_question_id), addresses_question linkage, claim
// resolution, the unassigned remainder, auxiliary counts, and the
// visibility gate ordering. The SQL shapes are the query tests' business
// (T0209); the e2e over real PostgreSQL lives in
// tests/integration/research_page_test.go.

// outlineObj builds one canned object row with a payload.
func outlineObj(id, objectType, versionID, title string, payload map[string]any) ObjectQueryRow {
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return ObjectQueryRow{
		Object: domain.ScientificObject{ID: id, ProjectID: qP, ObjectType: objectType, CurrentVersionNo: 1},
		Version: domain.ScientificObjectVersion{
			ID: versionID, ObjectID: id, VersionNo: 1, StateID: "state-x",
			Title: title, LifecycleState: domain.LifecycleActive, Payload: raw,
		},
	}
}

// outlineRel builds one canned relation row with the given endpoints.
func outlineRel(id, relationType string, src, tgt EndpointContext) RelationQueryRow {
	return RelationQueryRow{
		Relation: domain.Relation{ID: id, ProjectID: qP},
		Version: domain.RelationVersion{
			ID: id + "-v1", RelationID: id, VersionNo: 1, StateID: "state-x",
			RelationType: relationType, SourceObjectVersionID: src.VersionID, TargetObjectVersionID: tgt.VersionID,
		},
		Source: src,
		Target: tgt,
	}
}

// outlineFixture is the standard outline graph: a root question, one
// sub-question, hypotheses/findings addressing them, a claim referenced
// by a finding, and leftover materials that address nothing.
func outlineFixture() ([]ObjectQueryRow, []RelationQueryRow) {
	objects := []ObjectQueryRow{
		outlineObj("q-root", "research_question", "q-root-v1", "Does X adsorb?", map[string]any{
			"statement": "Does the material adsorb CO2?", "question_state": "open",
		}),
		outlineObj("q-child", "research_question", "q-child-v1", "At what pressure?", map[string]any{
			"statement": "At which pressure does uptake peak?", "question_state": "partially_answered",
			"parent_question_id": "q-root",
		}),
		outlineObj("h1", "hypothesis", "h1-v1", "Uptake scales with pressure", map[string]any{
			"statement": "Uptake scales with pressure", "question_id": "q-root",
		}),
		outlineObj("f1", "finding", "f1-v1", "Uptake peaks at 30 bar", map[string]any{
			"statement": "Uptake peaks at 30 bar", "finding_type": "trend", "assessment": "accepted",
			"claim_version_refs": []any{"c1-v1", "c-missing-v9"},
		}),
		outlineObj("c1", "claim", "c1-v1", "Capacity is 2 mmol/g", map[string]any{
			"statement": "Capacity is 2 mmol/g", "claim_type": "quantitative",
		}),
		outlineObj("m1", "material", "m1-v1", "MOF-5", map[string]any{"name": "MOF-5"}),
		outlineObj("m2", "dataset", "m2-v1", "isotherm series", map[string]any{"name": "isotherms"}),
	}
	relations := []RelationQueryRow{
		// h1 and f1 address the root question; m1 addresses the sub-question.
		outlineRel("r1", "addresses_question",
			queryEndpoint("h1-v1", "h1", "hypothesis", qP), queryEndpoint("q-root-v1", "q-root", "research_question", qP)),
		outlineRel("r2", "addresses_question",
			queryEndpoint("f1-v1", "f1", "finding", qP), queryEndpoint("q-root-v1", "q-root", "research_question", qP)),
		outlineRel("r3", "addresses_question",
			queryEndpoint("m1-v1", "m1", "material", qP), queryEndpoint("q-child-v1", "q-child", "research_question", qP)),
		// One edge that must be ignored: not an addresses_question type.
		outlineRel("r4", "derived_from",
			queryEndpoint("f1-v1", "f1", "finding", qP), queryEndpoint("c1-v1", "c1", "claim", qP)),
	}
	return objects, relations
}

func outlineServiceWith(objects []ObjectQueryRow, relations []RelationQueryRow, gate *queryFakeProjects) *Service {
	port := &queryFakePort{objects: objects, relations: relations}
	return &Service{projects: gate, queries: port}
}

func TestResearchOutlineAggregatesByQuestion(t *testing.T) {
	objects, relations := outlineFixture()
	svc := outlineServiceWith(objects, relations, &queryFakeProjects{outcomes: map[string]error{}})
	out, err := svc.ResearchOutline(context.Background(), projects.Reader{}, qP)
	if err != nil {
		t.Fatalf("ResearchOutline: %v", err)
	}
	if out.ProjectID != qP {
		t.Errorf("ProjectID = %q, want %q", out.ProjectID, qP)
	}

	// The tree: one root with the sub-question nested.
	if len(out.Questions) != 1 {
		t.Fatalf("roots = %d, want 1 (%+v)", len(out.Questions), out.Questions)
	}
	root := out.Questions[0]
	if root.ObjectID != "q-root" || root.Title != "Does X adsorb?" || root.Statement != "Does the material adsorb CO2?" {
		t.Errorf("root = %+v", root)
	}
	if root.QuestionState != "open" {
		t.Errorf("root state = %q, want open", root.QuestionState)
	}
	if len(root.Children) != 1 || root.Children[0].ObjectID != "q-child" {
		t.Fatalf("root children = %+v, want [q-child]", root.Children)
	}

	// The addressed-by lists are role-grouped and sorted by title.
	if len(root.Hypotheses) != 1 || root.Hypotheses[0].ObjectID != "h1" {
		t.Errorf("root hypotheses = %+v, want [h1]", root.Hypotheses)
	}
	if len(root.Findings) != 1 || root.Findings[0].ObjectID != "f1" {
		t.Errorf("root findings = %+v, want [f1]", root.Findings)
	}
	if len(root.OtherObjects) != 0 {
		t.Errorf("root other = %+v, want none", root.OtherObjects)
	}
	child := root.Children[0]
	if len(child.OtherObjects) != 1 || child.OtherObjects[0].ObjectID != "m1" || child.OtherObjects[0].ObjectType != "material" {
		t.Errorf("child other = %+v, want [m1]", child.OtherObjects)
	}

	// The finding carries its claim refs: one resolved, one pinned-only.
	if len(out.Findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(out.Findings))
	}
	f := out.Findings[0]
	if f.FindingType != "trend" || f.Assessment != "accepted" || f.Statement != "Uptake peaks at 30 bar" {
		t.Errorf("finding = %+v", f)
	}
	if len(f.Claims) != 2 {
		t.Fatalf("claims = %+v, want 2", f.Claims)
	}
	if !f.Claims[0].Resolved || f.Claims[0].ObjectID != "c1" || f.Claims[0].Title != "Capacity is 2 mmol/g" {
		t.Errorf("claims[0] = %+v, want resolved c1", f.Claims[0])
	}
	if f.Claims[1].Resolved || f.Claims[1].VersionID != "c-missing-v9" {
		t.Errorf("claims[1] = %+v, want unresolved pinned ref", f.Claims[1])
	}
	if len(f.Questions) != 1 || f.Questions[0].ObjectID != "q-root" {
		t.Errorf("finding questions = %+v, want [q-root]", f.Questions)
	}

	// The unassigned remainder: the dataset only. The material addresses a
	// question; the claim is referenced by the finding (its home is the
	// findings axis); the rest are questions.
	if len(out.Unassigned) != 1 || out.Unassigned[0].ObjectID != "m2" {
		t.Errorf("unassigned = %+v, want [m2]", out.Unassigned)
	}

	// Counts are the auxiliary summary.
	want := OutlineCounts{Questions: 2, Findings: 1, Hypotheses: 1, Claims: 1, OtherObjects: 2}
	if out.Counts != want {
		t.Errorf("counts = %+v, want %+v", out.Counts, want)
	}
}

func TestResearchOutlineQuestionTree(t *testing.T) {
	// A chain q-a -> q-b -> q-c plus an orphaned parent reference.
	objects := []ObjectQueryRow{
		outlineObj("q-a", "research_question", "q-a-v1", "A", map[string]any{"parent_question_id": nil}),
		outlineObj("q-b", "research_question", "q-b-v1", "B", map[string]any{"parent_question_id": "q-a"}),
		outlineObj("q-c", "research_question", "q-c-v1", "C", map[string]any{"parent_question_id": "q-b"}),
		outlineObj("q-d", "research_question", "q-d-v1", "D", map[string]any{"parent_question_id": "q-gone"}),
	}
	svc := outlineServiceWith(objects, nil, &queryFakeProjects{outcomes: map[string]error{}})
	out, err := svc.ResearchOutline(context.Background(), projects.Reader{}, qP)
	if err != nil {
		t.Fatalf("ResearchOutline: %v", err)
	}
	// q-a is the only root; its chain nests B then C. q-d is a root too
	// (its parent is outside the project) — sorted by title: A first.
	if len(out.Questions) != 2 {
		t.Fatalf("roots = %+v, want 2", out.Questions)
	}
	if out.Questions[0].ObjectID != "q-a" || out.Questions[1].ObjectID != "q-d" {
		t.Errorf("roots = [%s %s], want [q-a q-d]", out.Questions[0].ObjectID, out.Questions[1].ObjectID)
	}
	if len(out.Questions[0].Children) != 1 || out.Questions[0].Children[0].ObjectID != "q-b" {
		t.Fatalf("q-a children = %+v", out.Questions[0].Children)
	}
	if len(out.Questions[0].Children[0].Children) != 1 || out.Questions[0].Children[0].Children[0].ObjectID != "q-c" {
		t.Fatalf("q-b children = %+v", out.Questions[0].Children[0].Children)
	}
}

func TestResearchOutlineParentCycleBreaksIntoRoots(t *testing.T) {
	// A <-> B mutual parent references: no loop, both questions present.
	objects := []ObjectQueryRow{
		outlineObj("q-a", "research_question", "q-a-v1", "A", map[string]any{"parent_question_id": "q-b"}),
		outlineObj("q-b", "research_question", "q-b-v1", "B", map[string]any{"parent_question_id": "q-a"}),
	}
	svc := outlineServiceWith(objects, nil, &queryFakeProjects{outcomes: map[string]error{}})
	out, err := svc.ResearchOutline(context.Background(), projects.Reader{}, qP)
	if err != nil {
		t.Fatalf("ResearchOutline: %v", err)
	}
	// Exactly one root and one nested child — the cycle edge is dropped,
	// both questions stay visible, and the walk terminates.
	if len(out.Questions) != 1 {
		t.Fatalf("roots = %+v, want exactly 1", out.Questions)
	}
	seen := map[string]bool{out.Questions[0].ObjectID: true}
	for _, c := range out.Questions[0].Children {
		seen[c.ObjectID] = true
		if len(c.Children) != 0 {
			t.Errorf("cycle child %s has children: %+v", c.ObjectID, c.Children)
		}
	}
	if !seen["q-a"] || !seen["q-b"] {
		t.Errorf("cycle lost a question: seen = %v", seen)
	}
}

func TestResearchOutlineEmptyProject(t *testing.T) {
	svc := outlineServiceWith(nil, nil, &queryFakeProjects{outcomes: map[string]error{}})
	out, err := svc.ResearchOutline(context.Background(), projects.Reader{}, qP)
	if err != nil {
		t.Fatalf("ResearchOutline: %v", err)
	}
	if len(out.Questions) != 0 || len(out.Findings) != 0 || len(out.Unassigned) != 0 {
		t.Errorf("empty project outline = %+v, want all empty", out)
	}
	if out.Counts != (OutlineCounts{}) {
		t.Errorf("counts = %+v, want zero", out.Counts)
	}
}

// TestResearchOutlineVisibilityGateFirst: the denial precedes every store
// read — a caller who may not read the project answers not-found and the
// query port is never consulted.
func TestResearchOutlineVisibilityGateFirst(t *testing.T) {
	gate := &queryFakeProjects{outcomes: map[string]error{qP: projects.ErrProjectNotFound}}
	port := &queryFakePort{err: errors.New("store must not be reached")}
	svc := &Service{projects: gate, queries: port}
	_, err := svc.ResearchOutline(context.Background(), projects.Reader{}, qP)
	if !errors.Is(err, projects.ErrProjectNotFound) {
		t.Fatalf("err = %v, want ErrProjectNotFound", err)
	}
	if port.objectCalls != 0 {
		t.Errorf("query port consulted %d times before the gate — the denial must precede every store read", port.objectCalls)
	}
	if len(gate.calls) != 1 || gate.calls[0] != qP {
		t.Errorf("gate calls = %v, want [%s]", gate.calls, qP)
	}
}

func TestResearchOutlineStoreFailureFailsClosed(t *testing.T) {
	port := &queryFakePort{err: errors.New("db down")}
	svc := &Service{projects: &queryFakeProjects{outcomes: map[string]error{}}, queries: port}
	_, err := svc.ResearchOutline(context.Background(), projects.Reader{}, qP)
	if err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("err = %v, want the store failure", err)
	}
}

func TestResearchOutlineRequiresProjectAndPort(t *testing.T) {
	svc := &Service{projects: &queryFakeProjects{outcomes: map[string]error{}}, queries: &queryFakePort{}}
	if _, err := svc.ResearchOutline(context.Background(), projects.Reader{}, ""); !errors.Is(err, ErrValidation) {
		t.Errorf("empty project id: err = %v, want ErrValidation", err)
	}
	svc = &Service{projects: &queryFakeProjects{outcomes: map[string]error{}}}
	if _, err := svc.ResearchOutline(context.Background(), projects.Reader{}, qP); !errors.Is(err, ErrStore) {
		t.Errorf("unwired query port: err = %v, want ErrStore", err)
	}
}

// TestResearchOutlineEdgesPinnedToSupersededVersionsStillCount: relation
// endpoints pin exact object versions, and an edge stays pinned to the
// version it was created against — nothing re-pins it when an endpoint
// object gains a version. The outline resolves endpoints by their container
// object (the endpoint context the query joins), so an edge pinned to a
// superseded version still counts on BOTH axes: the question's addressed-by
// lists and the finding's questions must not lose it, and the rendered
// title/branch come from the object's current (newest) row.
func TestResearchOutlineEdgesPinnedToSupersededVersionsStillCount(t *testing.T) {
	q := outlineObj("q", "research_question", "q-v2", "Does X adsorb? (v2)", map[string]any{
		"statement": "Does the material adsorb CO2?", "question_state": "open",
	})
	q.Version.VersionNo = 2
	h := outlineObj("h", "hypothesis", "h-v2", "Uptake scales (v2)", map[string]any{
		"statement": "Uptake scales with pressure",
	})
	h.Version.VersionNo = 2
	f := outlineObj("f", "finding", "f-v2", "Uptake peaks (v2)", map[string]any{
		"statement": "Uptake peaks at 30 bar", "finding_type": "trend", "assessment": "accepted",
	})
	f.Version.VersionNo = 2
	// The edges pin the objects' FIRST versions; the slice carries only the
	// objects' newest versions (nil lineage). Nothing re-pins the edges.
	relations := []RelationQueryRow{
		outlineRel("r1", "addresses_question",
			queryEndpoint("h-v1", "h", "hypothesis", qP), queryEndpoint("q-v1", "q", "research_question", qP)),
		outlineRel("r2", "addresses_question",
			queryEndpoint("f-v1", "f", "finding", qP), queryEndpoint("q-v1", "q", "research_question", qP)),
	}
	svc := outlineServiceWith([]ObjectQueryRow{q, h, f}, relations, &queryFakeProjects{outcomes: map[string]error{}})
	out, err := svc.ResearchOutline(context.Background(), projects.Reader{}, qP)
	if err != nil {
		t.Fatalf("ResearchOutline: %v", err)
	}

	// The question axis: both v1-pinned edges survive, rendered from the
	// current rows (v2 titles).
	if len(out.Questions) != 1 {
		t.Fatalf("questions = %+v, want [q]", out.Questions)
	}
	root := out.Questions[0]
	if root.VersionID != "q-v2" {
		t.Errorf("question as-of version = %q, want q-v2 (the newest)", root.VersionID)
	}
	if len(root.Hypotheses) != 1 || root.Hypotheses[0].ObjectID != "h" || root.Hypotheses[0].Title != "Uptake scales (v2)" {
		t.Errorf("question hypotheses = %+v, want [h at its current title]", root.Hypotheses)
	}
	if len(root.Findings) != 1 || root.Findings[0].ObjectID != "f" || root.Findings[0].Title != "Uptake peaks (v2)" {
		t.Errorf("question findings = %+v, want [f at its current title]", root.Findings)
	}

	// The finding axis: the addressed question survives, at its current
	// title.
	if len(out.Findings) != 1 {
		t.Fatalf("findings = %+v, want [f]", out.Findings)
	}
	if len(out.Findings[0].Questions) != 1 || out.Findings[0].Questions[0].ObjectID != "q" ||
		out.Findings[0].Questions[0].Title != "Does X adsorb? (v2)" {
		t.Errorf("finding questions = %+v, want [q at its current title]", out.Findings[0].Questions)
	}

	// And the outline tells no lies: a surviving edge never orphans its
	// source into the unassigned remainder.
	if len(out.Unassigned) != 0 {
		t.Errorf("unassigned = %+v, want none (pinned edges must not orphan their sources)", out.Unassigned)
	}
}

// TestResearchOutlineClaimRefPinnedToSupersededVersionResolves: a finding
// referencing a claim version that is no longer the claim's newest version
// still resolves — Resolved means "a claim object of THIS project", and
// the rendered title/branch are the PINNED version's own. The claim must
// not land in the unassigned remainder (the page must not say "not in this
// project" about an object it also lists as a project object).
func TestResearchOutlineClaimRefPinnedToSupersededVersionResolves(t *testing.T) {
	claimV2 := outlineObj("c", "claim", "c-v2", "Capacity (v2)", map[string]any{
		"statement": "Capacity is 2 mmol/g", "claim_type": "quantitative",
	})
	claimV2.Version.VersionNo = 2
	finding := outlineObj("f", "finding", "f-v1", "Uptake peaks", map[string]any{
		"statement": "Uptake peaks at 30 bar", "finding_type": "trend", "assessment": "accepted",
		"claim_version_refs": []any{"c-v1"},
	})
	// The pinned v1 row, fetched on demand — the project slice carries only
	// each object's newest version.
	pinned := outlineObj("c", "claim", "c-v1", "Capacity (v1)", map[string]any{
		"statement": "Capacity is 2 mmol/g", "claim_type": "quantitative",
	})
	port := &queryFakePort{
		objects: []ObjectQueryRow{claimV2, finding},
		byIDs:   map[string]ObjectQueryRow{"c-v1": pinned},
	}
	svc := &Service{projects: &queryFakeProjects{outcomes: map[string]error{}}, queries: port}
	out, err := svc.ResearchOutline(context.Background(), projects.Reader{}, qP)
	if err != nil {
		t.Fatalf("ResearchOutline: %v", err)
	}
	if len(out.Findings) != 1 || len(out.Findings[0].Claims) != 1 {
		t.Fatalf("findings = %+v, want [f with one claim ref]", out.Findings)
	}
	ref := out.Findings[0].Claims[0]
	if !ref.Resolved || ref.ObjectID != "c" || ref.VersionID != "c-v1" || ref.Title != "Capacity (v1)" {
		t.Errorf("claim ref = %+v, want resolved c@c-v1 with the pinned version's title", ref)
	}
	if len(out.Unassigned) != 0 {
		t.Errorf("unassigned = %+v, want none (a resolved claim belongs to the findings axis)", out.Unassigned)
	}
}

// TestResearchOutlineClaimRefToForeignProjectStaysUnresolved: a ref to
// another project's claim version answers "not in this project" — the
// fetched row's title and identity must never render (docs/45: the
// outline must not become a cross-project existence oracle).
func TestResearchOutlineClaimRefToForeignProjectStaysUnresolved(t *testing.T) {
	finding := outlineObj("f", "finding", "f-v1", "Uptake peaks", map[string]any{
		"claim_version_refs": []any{"foreign-c-v1"},
	})
	foreign := outlineObj("c-foreign", "claim", "foreign-c-v1", "SECRET foreign claim", map[string]any{})
	foreign.Object.ProjectID = "other-project"
	port := &queryFakePort{
		objects: []ObjectQueryRow{finding},
		byIDs:   map[string]ObjectQueryRow{"foreign-c-v1": foreign},
	}
	svc := &Service{projects: &queryFakeProjects{outcomes: map[string]error{}}, queries: port}
	out, err := svc.ResearchOutline(context.Background(), projects.Reader{}, qP)
	if err != nil {
		t.Fatalf("ResearchOutline: %v", err)
	}
	if len(out.Findings) != 1 || len(out.Findings[0].Claims) != 1 {
		t.Fatalf("findings = %+v, want [f with one claim ref]", out.Findings)
	}
	ref := out.Findings[0].Claims[0]
	if ref.Resolved || ref.ObjectID != "" || ref.Title != "" || ref.VersionID != "foreign-c-v1" {
		t.Errorf("foreign claim ref = %+v, want unresolved with the pinned id only", ref)
	}
}

// TestResearchOutlineClaimFetchFailureFailsClosed: the pinned-claim fetch
// is part of the read — its failure aborts the outline (never a silently
// partial outline with claims left unresolved).
func TestResearchOutlineClaimFetchFailureFailsClosed(t *testing.T) {
	finding := outlineObj("f", "finding", "f-v1", "Uptake peaks", map[string]any{
		"claim_version_refs": []any{"c-v1"},
	})
	port := &queryFakePort{objects: []ObjectQueryRow{finding}, byIDsErr: errors.New("claim fetch down")}
	svc := &Service{projects: &queryFakeProjects{outcomes: map[string]error{}}, queries: port}
	_, err := svc.ResearchOutline(context.Background(), projects.Reader{}, qP)
	if err == nil || !strings.Contains(err.Error(), "claim fetch down") {
		t.Fatalf("err = %v, want the claim fetch failure", err)
	}
}

// TestResearchOutlineToleratesMalformedPayloads: payloads outside the
// canonical shapes (missing statement, mistyped fields, broken JSON)
// degrade to empty fields — the outline never fails on data it does not
// understand.
func TestResearchOutlineToleratesMalformedPayloads(t *testing.T) {
	objects := []ObjectQueryRow{
		outlineObj("q-raw", "research_question", "q-raw-v1", "Broken question", map[string]any{
			"statement": 42, "question_state": 7, "parent_question_id": []any{"x"},
		}),
		outlineObj("f-raw", "finding", "f-raw-v1", "Broken finding", map[string]any{
			"claim_version_refs": "not-a-list", "finding_type": 3,
		}),
	}
	objects[0].Version.Payload = json.RawMessage(`{"statement":`) // invalid JSON
	svc := outlineServiceWith(objects, nil, &queryFakeProjects{outcomes: map[string]error{}})
	out, err := svc.ResearchOutline(context.Background(), projects.Reader{}, qP)
	if err != nil {
		t.Fatalf("ResearchOutline: %v", err)
	}
	if len(out.Questions) != 1 || out.Questions[0].Statement != "" {
		t.Errorf("question = %+v, want empty statement on broken payload", out.Questions)
	}
	if len(out.Findings) != 1 || len(out.Findings[0].Claims) != 0 {
		t.Errorf("finding = %+v, want zero claims on mistyped refs", out.Findings)
	}
}
