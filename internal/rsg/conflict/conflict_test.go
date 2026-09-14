package conflict

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/rsg/diff"
	"github.com/lichman0405/post/internal/rsg/manifest"
)

// fixtureClock is the fixed timestamp every fixture row carries, so the
// canonical bytes never depend on the wall clock.
var fixtureClock = time.Date(2026, 2, 10, 9, 30, 0, 0, time.UTC)

// objRow builds one object version row for fixtures.
func objRow(id, objectID, objectType string, versionNo int, stateID, lifecycle, title, payload string, schema manifest.SchemaRef, policy *string) manifest.ObjectVersion {
	return manifest.ObjectVersion{
		ID:                 id,
		ObjectID:           objectID,
		ObjectType:         objectType,
		VersionNo:          versionNo,
		StateID:            stateID,
		SchemaRef:          schema,
		Title:              title,
		LifecycleState:     lifecycle,
		Payload:            json.RawMessage(payload),
		VisibilityPolicyID: policy,
		IntegrityHash:      "sha256:fixture",
		CreatedBy:          "user-00000001",
		CreatedAt:          fixtureClock,
	}
}

// relRow builds one relation version row for fixtures.
func relRow(id, relationID string, versionNo int, stateID, relationType, sourceID, targetID, payload string) manifest.RelationVersion {
	return manifest.RelationVersion{
		ID:                    id,
		RelationID:            relationID,
		VersionNo:             versionNo,
		StateID:               stateID,
		RelationType:          relationType,
		SourceObjectVersionID: sourceID,
		TargetObjectVersionID: targetID,
		Payload:               json.RawMessage(payload),
		IntegrityHash:         "sha256:fixture",
		CreatedBy:             "user-00000001",
		CreatedAt:             fixtureClock,
	}
}

func strptr(s string) *string { return &s }

// schemaV is the default fixture schema ref.
func schemaV(version string) manifest.SchemaRef {
	return manifest.SchemaRef{ID: "https://open-rd.example/schemas/claim.schema.json", Version: version}
}

// inputs builds a three-way input around one base object with the two
// heads given as (row, present) pairs. Extra base/source/target rows are
// appended for multi-object scenarios.
type fixture struct {
	in diff.Inputs
}

func newFixture() *fixture {
	return &fixture{in: diff.Inputs{
		ProjectID: "proj-00000001",
		Base:      diff.StateRef{ID: "state-base-0001"},
		Source:    diff.StateRef{ID: "state-src-000001", GitRef: strptr("cccccccccccccccccccccccccccccccccccccccc")},
		Target:    diff.StateRef{ID: "state-tgt-000001"},
	}}
}

func (f *fixture) base(rows ...manifest.ObjectVersion) *fixture {
	f.in.BaseSnapshot.ObjectVersions = append(f.in.BaseSnapshot.ObjectVersions, rows...)
	return f
}

func (f *fixture) source(rows ...manifest.ObjectVersion) *fixture {
	f.in.SourceSnapshot.ObjectVersions = append(f.in.SourceSnapshot.ObjectVersions, rows...)
	return f
}

func (f *fixture) target(rows ...manifest.ObjectVersion) *fixture {
	f.in.TargetSnapshot.ObjectVersions = append(f.in.TargetSnapshot.ObjectVersions, rows...)
	return f
}

func (f *fixture) baseRel(rows ...manifest.RelationVersion) *fixture {
	f.in.BaseSnapshot.RelationVersions = append(f.in.BaseSnapshot.RelationVersions, rows...)
	return f
}

func (f *fixture) sourceRel(rows ...manifest.RelationVersion) *fixture {
	f.in.SourceSnapshot.RelationVersions = append(f.in.SourceSnapshot.RelationVersions, rows...)
	return f
}

func (f *fixture) targetRel(rows ...manifest.RelationVersion) *fixture {
	f.in.TargetSnapshot.RelationVersions = append(f.in.TargetSnapshot.RelationVersions, rows...)
	return f
}

func (f *fixture) detect(t *testing.T) *Report {
	t.Helper()
	r, err := Detect(f.in)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	return r
}

// verdict returns the object verdict with the given id, or fails.
func objectVerdict(t *testing.T, r *Report, objectID string) ObjectVerdict {
	t.Helper()
	for _, v := range r.ObjectVerdicts {
		if v.ObjectID == objectID {
			return v
		}
	}
	t.Fatalf("no object verdict for %s in %+v", objectID, r.ObjectVerdicts)
	return ObjectVerdict{}
}

func relationVerdict(t *testing.T, r *Report, relationID string) RelationVerdict {
	t.Helper()
	for _, v := range r.RelationVerdicts {
		if v.RelationID == relationID {
			return v
		}
	}
	t.Fatalf("no relation verdict for %s in %+v", relationID, r.RelationVerdicts)
	return RelationVerdict{}
}

// oneConflict asserts the verdict carries exactly one conflict and returns it.
func oneConflict(t *testing.T, v ObjectVerdict) Conflict {
	t.Helper()
	if len(v.Conflicts) != 1 {
		t.Fatalf("conflicts = %+v, want exactly one", v.Conflicts)
	}
	return v.Conflicts[0]
}

// wantAuto asserts the verdict is auto-mergeable with no conflicts.
func wantAuto(t *testing.T, v ObjectVerdict) {
	t.Helper()
	if !v.AutoMergeable {
		t.Fatalf("verdict auto_mergeable = false, conflicts = %+v", v.Conflicts)
	}
	if len(v.Conflicts) != 0 {
		t.Fatalf("verdict conflicts = %+v, want none", v.Conflicts)
	}
}

// TestProtocolSameFieldDivergesIsScientific is the first acceptance
// criterion: the same field changed to different values on the two
// branches of a protocol is a scientific conflict (docs/09 §7: Protocol
// 冲突数值折中禁止自动), never an auto-mergeable attribute conflict.
func TestProtocolSameFieldDivergesIsScientific(t *testing.T) {
	// Base protocol with one step; each branch moves the step's
	// temperature to a different value.
	base := objRow("pv-00000001", "obj-p-0000001", "protocol", 1, "state-base-0001", "active",
		"P1", `{"purpose":"p","steps":[{"id":"s1","temperature":300}]}`, schemaV("1"), nil)
	src := objRow("pv-00000002", "obj-p-0000001", "protocol", 2, "state-src-000001", "active",
		"P1", `{"purpose":"p","steps":[{"id":"s1","temperature":350}]}`, schemaV("1"), nil)
	tgt := objRow("pv-00000003", "obj-p-0000001", "protocol", 2, "state-tgt-000001", "active",
		"P1", `{"purpose":"p","steps":[{"id":"s1","temperature":400}]}`, schemaV("1"), nil)
	r := newFixture().base(base).source(base, src).target(base, tgt).detect(t)

	v := objectVerdict(t, r, "obj-p-0000001")
	if v.AutoMergeable {
		t.Fatalf("protocol verdict auto_mergeable = true, conflicts = %+v", v.Conflicts)
	}
	c := oneConflict(t, v)
	if c.Category != CategoryScientific {
		t.Fatalf("category = %q, want scientific", c.Category)
	}
	if c.Code != CodeScientificFieldDiverges {
		t.Fatalf("code = %q, want %q", c.Code, CodeScientificFieldDiverges)
	}
	if len(c.Fields) != 1 || c.Fields[0] != "payload" {
		t.Fatalf("fields = %v, want [payload]", c.Fields)
	}
	if len(c.PayloadKeys) != 1 || c.PayloadKeys[0] != "steps" {
		t.Fatalf("payload_keys = %v, want [steps]", c.PayloadKeys)
	}
	if r.AutoMergeable {
		t.Fatalf("report auto_mergeable = true, want false (one scientific conflict)")
	}
	if r.Summary.ObjectsConflicted != 1 || r.Summary.ObjectsAutoMergeable != 0 {
		t.Fatalf("summary = %+v, want one conflicted object", r.Summary)
	}
}

// TestProtocolTitleDivergesIsScientific pins the literal reading of the
// acceptance criterion: ANY same-field-different-values pair on a
// protocol — title included — is a scientific conflict.
func TestProtocolTitleDivergesIsScientific(t *testing.T) {
	base := objRow("pv-00000001", "obj-p-0000001", "protocol", 1, "state-base-0001", "active",
		"P1", `{"purpose":"p"}`, schemaV("1"), nil)
	src := objRow("pv-00000002", "obj-p-0000001", "protocol", 2, "state-src-000001", "active",
		"P2", `{"purpose":"p"}`, schemaV("1"), nil)
	tgt := objRow("pv-00000003", "obj-p-0000001", "protocol", 2, "state-tgt-000001", "active",
		"P3", `{"purpose":"p"}`, schemaV("1"), nil)
	r := newFixture().base(base).source(base, src).target(base, tgt).detect(t)

	c := oneConflict(t, objectVerdict(t, r, "obj-p-0000001"))
	if c.Category != CategoryScientific || c.Fields[0] != "title" {
		t.Fatalf("conflict = %+v, want scientific on [title]", c)
	}
}

// TestProtocolAppendBothSidesStillScientific: even mergeable-looking
// appends on a protocol are scientific conflicts — no branch validated
// the combined steps (docs/09 §7).
func TestProtocolAppendBothSidesStillScientific(t *testing.T) {
	base := objRow("pv-00000001", "obj-p-0000001", "protocol", 1, "state-base-0001", "active",
		"P1", `{"purpose":"p","steps":[{"id":"s1"}]}`, schemaV("1"), nil)
	src := objRow("pv-00000002", "obj-p-0000001", "protocol", 2, "state-src-000001", "active",
		"P1", `{"purpose":"p","steps":[{"id":"s1"},{"id":"s2"}]}`, schemaV("1"), nil)
	tgt := objRow("pv-00000003", "obj-p-0000001", "protocol", 2, "state-tgt-000001", "active",
		"P1", `{"purpose":"p","steps":[{"id":"s1"},{"id":"s3"}]}`, schemaV("1"), nil)
	r := newFixture().base(base).source(base, src).target(base, tgt).detect(t)

	c := oneConflict(t, objectVerdict(t, r, "obj-p-0000001"))
	if c.Category != CategoryScientific {
		t.Fatalf("category = %q, want scientific", c.Category)
	}
}

// TestAppendEvidenceNotTextConflict is the second acceptance criterion:
// appending evidence on both branches is append-only (docs/09 §7: in the
// auto-allowed set) and must not be misjudged as a text/attribute
// conflict — the change stays auto_mergeable.
func TestAppendEvidenceNotTextConflict(t *testing.T) {
	base := objRow("cv-00000001", "obj-c-0000001", "claim", 1, "state-base-0001", "active",
		"C1", `{"statement":"s","evidence_refs":["e1"]}`, schemaV("1"), nil)
	src := objRow("cv-00000002", "obj-c-0000001", "claim", 2, "state-src-000001", "active",
		"C1", `{"statement":"s","evidence_refs":["e1","e2"]}`, schemaV("1"), nil)
	tgt := objRow("cv-00000003", "obj-c-0000001", "claim", 2, "state-tgt-000001", "active",
		"C1", `{"statement":"s","evidence_refs":["e1","e3"]}`, schemaV("1"), nil)
	r := newFixture().base(base).source(base, src).target(base, tgt).detect(t)

	wantAuto(t, objectVerdict(t, r, "obj-c-0000001"))
	if !r.AutoMergeable {
		t.Fatalf("report auto_mergeable = false, verdicts = %+v", r.ObjectVerdicts)
	}
}

// TestAppendOnlyNullIsNotAnAnchor pins the appendOnly boundary: a JSON
// null is not an empty array, so it never counts as a list to extend or
// as an extension. (json.Unmarshal decodes null into a slice with no
// error and leaves it nil — the exact trap this test guards.)
func TestAppendOnlyNullIsNotAnAnchor(t *testing.T) {
	null := json.RawMessage(`null`)
	empty := json.RawMessage(`[]`)
	if appendOnly(true, null, true, json.RawMessage(`["e2"]`)) {
		t.Fatalf("appendOnly(null base, array head) = true, want false: null is no anchor list")
	}
	if appendOnly(true, null, true, empty) {
		t.Fatalf("appendOnly(null base, [] head) = true, want false: null != []")
	}
	if appendOnly(true, json.RawMessage(`["e1"]`), true, null) {
		t.Fatalf("appendOnly(array base, null head) = true, want false: null is no extension")
	}
	if !appendOnly(true, empty, true, json.RawMessage(`["e2"]`)) {
		t.Fatalf("appendOnly([] base, [e2] head) = false, want true: an empty array IS an anchor")
	}
	if !appendOnly(true, json.RawMessage(`["e1"]`), true, json.RawMessage(`["e1","e2"]`)) {
		t.Fatalf("appendOnly([e1] base, [e1,e2] head) = false, want true")
	}
}

// TestNullBaseBothSidesSetDifferentArraysIsConflict is the regression for
// the review finding: base carries evidence_refs:null and both branches
// replace it with DIFFERENT arrays. That is the same field changed to
// different values (docs/09 §6), a conflict a human must resolve — under
// the old null-as-empty-anchor behavior it was silently auto-merged.
func TestNullBaseBothSidesSetDifferentArraysIsConflict(t *testing.T) {
	base := objRow("cv-00000001", "obj-c-0000001", "claim", 1, "state-base-0001", "active",
		"C1", `{"statement":"s","evidence_refs":null}`, schemaV("1"), nil)
	src := objRow("cv-00000002", "obj-c-0000001", "claim", 2, "state-src-000001", "active",
		"C1", `{"statement":"s","evidence_refs":["e2"]}`, schemaV("1"), nil)
	tgt := objRow("cv-00000003", "obj-c-0000001", "claim", 2, "state-tgt-000001", "active",
		"C1", `{"statement":"s","evidence_refs":["e3"]}`, schemaV("1"), nil)
	r := newFixture().base(base).source(base, src).target(base, tgt).detect(t)

	v := objectVerdict(t, r, "obj-c-0000001")
	if v.AutoMergeable {
		t.Fatalf("verdict auto_mergeable = true, conflicts = %+v", v.Conflicts)
	}
	c := oneConflict(t, v)
	if c.Category != CategoryKnowledge {
		t.Fatalf("category = %q, want knowledge", c.Category)
	}
	if len(c.PayloadKeys) != 1 || c.PayloadKeys[0] != "evidence_refs" {
		t.Fatalf("payload_keys = %v, want [evidence_refs]", c.PayloadKeys)
	}
	if r.AutoMergeable {
		t.Fatalf("report auto_mergeable = true, verdicts = %+v", r.ObjectVerdicts)
	}
}

// TestBothSidesRemoveSameKeyAuto: both branches deleting the same payload
// key is a converged change (they agree on absence), so it stays
// auto-mergeable rather than surfacing as a conflict.
func TestBothSidesRemoveSameKeyAuto(t *testing.T) {
	base := objRow("cv-00000001", "obj-c-0000001", "claim", 1, "state-base-0001", "active",
		"C1", `{"statement":"s","note":"n"}`, schemaV("1"), nil)
	src := objRow("cv-00000002", "obj-c-0000001", "claim", 2, "state-src-000001", "active",
		"C1", `{"statement":"s"}`, schemaV("1"), nil)
	tgt := objRow("cv-00000003", "obj-c-0000001", "claim", 2, "state-tgt-000001", "active",
		"C1", `{"statement":"s"}`, schemaV("1"), nil)
	r := newFixture().base(base).source(base, src).target(base, tgt).detect(t)

	wantAuto(t, objectVerdict(t, r, "obj-c-0000001"))
	if !r.AutoMergeable {
		t.Fatalf("report auto_mergeable = false, verdicts = %+v", r.ObjectVerdicts)
	}
}

// TestAppendEvidenceOneSideTextOtherSideStillAuto: the evidence append
// and a text change of a DIFFERENT payload key are non-overlapping
// fields — auto-mergeable (docs/09 §7: 同对象不同非冲突字段).
func TestAppendEvidenceOneSideTextOtherSideStillAuto(t *testing.T) {
	base := objRow("cv-00000001", "obj-c-0000001", "claim", 1, "state-base-0001", "active",
		"C1", `{"statement":"s","evidence_refs":["e1"]}`, schemaV("1"), nil)
	src := objRow("cv-00000002", "obj-c-0000001", "claim", 2, "state-src-000001", "active",
		"C1", `{"statement":"s","evidence_refs":["e1","e2"]}`, schemaV("1"), nil)
	tgt := objRow("cv-00000003", "obj-c-0000001", "claim", 2, "state-tgt-000001", "active",
		"C1", `{"statement":"t","evidence_refs":["e1"]}`, schemaV("1"), nil)
	r := newFixture().base(base).source(base, src).target(base, tgt).detect(t)

	wantAuto(t, objectVerdict(t, r, "obj-c-0000001"))
}

// TestAppendVersusRewriteIsKnowledgeConflict: an append on one side and a
// rewrite of the SAME list on the other genuinely conflict — and on a
// claim that conflict is knowledge, not a text conflict.
func TestAppendVersusRewriteIsKnowledgeConflict(t *testing.T) {
	base := objRow("cv-00000001", "obj-c-0000001", "claim", 1, "state-base-0001", "active",
		"C1", `{"statement":"s","evidence_refs":["e1"]}`, schemaV("1"), nil)
	src := objRow("cv-00000002", "obj-c-0000001", "claim", 2, "state-src-000001", "active",
		"C1", `{"statement":"s","evidence_refs":["e1","e2"]}`, schemaV("1"), nil)
	tgt := objRow("cv-00000003", "obj-c-0000001", "claim", 2, "state-tgt-000001", "active",
		"C1", `{"statement":"s","evidence_refs":["e9"]}`, schemaV("1"), nil)
	r := newFixture().base(base).source(base, src).target(base, tgt).detect(t)

	c := oneConflict(t, objectVerdict(t, r, "obj-c-0000001"))
	if c.Category != CategoryKnowledge {
		t.Fatalf("category = %q, want knowledge", c.Category)
	}
	if len(c.PayloadKeys) != 1 || c.PayloadKeys[0] != "evidence_refs" {
		t.Fatalf("payload_keys = %v, want [evidence_refs]", c.PayloadKeys)
	}
}

// TestAttributeConflict: title divergence on a non-knowledge object is an
// attribute conflict (docs/09 §6).
func TestAttributeConflict(t *testing.T) {
	base := objRow("ev-00000001", "obj-e-0000001", "experiment", 1, "state-base-0001", "active",
		"E1", `{"design":"d"}`, schemaV("1"), nil)
	src := objRow("ev-00000002", "obj-e-0000001", "experiment", 2, "state-src-000001", "active",
		"E2", `{"design":"d"}`, schemaV("1"), nil)
	tgt := objRow("ev-00000003", "obj-e-0000001", "experiment", 2, "state-tgt-000001", "active",
		"E3", `{"design":"d"}`, schemaV("1"), nil)
	r := newFixture().base(base).source(base, src).target(base, tgt).detect(t)

	c := oneConflict(t, objectVerdict(t, r, "obj-e-0000001"))
	if c.Category != CategoryAttribute || c.Code != CodeAttributeFieldDiverges {
		t.Fatalf("conflict = %+v, want attribute", c)
	}
	if len(c.Fields) != 1 || c.Fields[0] != "title" {
		t.Fatalf("fields = %v, want [title]", c.Fields)
	}
}

// TestLifecycleDivergesIsAttribute: abort on one side and reopen on the
// other is an attribute conflict on lifecycle_state.
func TestLifecycleDivergesIsAttribute(t *testing.T) {
	base := objRow("hv-00000001", "obj-h-0000001", "hypothesis", 1, "state-base-0001", "active",
		"H1", `{"question_id":"q1"}`, schemaV("1"), nil)
	src := objRow("hv-00000002", "obj-h-0000001", "hypothesis", 2, "state-src-000001", "aborted",
		"H1", `{"question_id":"q1"}`, schemaV("1"), nil)
	tgt := objRow("hv-00000003", "obj-h-0000001", "hypothesis", 2, "state-tgt-000001", "reopened",
		"H1", `{"question_id":"q1"}`, schemaV("1"), nil)
	r := newFixture().base(base).source(base, src).target(base, tgt).detect(t)

	c := oneConflict(t, objectVerdict(t, r, "obj-h-0000001"))
	if c.Category != CategoryAttribute || len(c.Fields) != 1 || c.Fields[0] != "lifecycle_state" {
		t.Fatalf("conflict = %+v, want attribute on [lifecycle_state]", c)
	}
}

// TestSchemaConflict: schema_ref divergence is a schema compatibility
// conflict whatever the object type (docs/09 §6).
func TestSchemaConflict(t *testing.T) {
	base := objRow("cv-00000001", "obj-c-0000001", "claim", 1, "state-base-0001", "active",
		"C1", `{"statement":"s"}`, schemaV("1"), nil)
	src := objRow("cv-00000002", "obj-c-0000001", "claim", 2, "state-src-000001", "active",
		"C1", `{"statement":"s"}`, schemaV("2"), nil)
	tgt := objRow("cv-00000003", "obj-c-0000001", "claim", 2, "state-tgt-000001", "active",
		"C1", `{"statement":"s"}`, schemaV("3"), nil)
	r := newFixture().base(base).source(base, src).target(base, tgt).detect(t)

	c := oneConflict(t, objectVerdict(t, r, "obj-c-0000001"))
	if c.Category != CategorySchema || c.Code != CodeSchemaFieldDiverges {
		t.Fatalf("conflict = %+v, want schema", c)
	}
}

// TestRightsConflictDiverged: visibility divergence is a rights conflict
// and never auto (docs/09 §7: rights conflict 禁止自动).
func TestRightsConflictDiverged(t *testing.T) {
	base := objRow("cv-00000001", "obj-c-0000001", "claim", 1, "state-base-0001", "active",
		"C1", `{"statement":"s"}`, schemaV("1"), nil)
	src := objRow("cv-00000002", "obj-c-0000001", "claim", 2, "state-src-000001", "active",
		"C1", `{"statement":"s"}`, schemaV("1"), strptr("policy-a"))
	tgt := objRow("cv-00000003", "obj-c-0000001", "claim", 2, "state-tgt-000001", "active",
		"C1", `{"statement":"s"}`, schemaV("1"), strptr("policy-b"))
	r := newFixture().base(base).source(base, src).target(base, tgt).detect(t)

	v := objectVerdict(t, r, "obj-c-0000001")
	if v.AutoMergeable {
		t.Fatalf("rights conflict verdict auto_mergeable = true")
	}
	c := oneConflict(t, v)
	if c.Category != CategoryRights || c.Code != CodeRightsFieldDiverges {
		t.Fatalf("conflict = %+v, want rights divergence", c)
	}
}

// TestRightsVisibilityChangeNeverAuto: a unilateral visibility move (the
// target did not touch the policy) is never auto-merged — expansion
// requires explicit authorized confirmation (docs/12 §3).
func TestRightsVisibilityChangeNeverAuto(t *testing.T) {
	base := objRow("cv-00000001", "obj-c-0000001", "claim", 1, "state-base-0001", "active",
		"C1", `{"statement":"s"}`, schemaV("1"), nil)
	src := objRow("cv-00000002", "obj-c-0000001", "claim", 2, "state-src-000001", "active",
		"C1", `{"statement":"s"}`, schemaV("1"), strptr("policy-a"))
	tgt := objRow("cv-00000003", "obj-c-0000001", "claim", 2, "state-tgt-000001", "active",
		"C2", `{"statement":"s"}`, schemaV("1"), nil)
	r := newFixture().base(base).source(base, src).target(base, tgt).detect(t)

	v := objectVerdict(t, r, "obj-c-0000001")
	if v.AutoMergeable {
		t.Fatalf("visibility move verdict auto_mergeable = true")
	}
	c := oneConflict(t, v)
	if c.Category != CategoryRights || c.Code != CodeRightsVisibilityChange {
		t.Fatalf("conflict = %+v, want rights visibility change", c)
	}
}

// TestRightsVisibilityChangeTargetUntouched: same rule when the target
// did not move the object at all.
func TestRightsVisibilityChangeTargetUntouched(t *testing.T) {
	base := objRow("cv-00000001", "obj-c-0000001", "claim", 1, "state-base-0001", "active",
		"C1", `{"statement":"s"}`, schemaV("1"), nil)
	src := objRow("cv-00000002", "obj-c-0000001", "claim", 2, "state-src-000001", "active",
		"C1", `{"statement":"s"}`, schemaV("1"), strptr("policy-a"))
	r := newFixture().base(base).source(base, src).target(base).detect(t)

	c := oneConflict(t, objectVerdict(t, r, "obj-c-0000001"))
	if c.Category != CategoryRights || c.Code != CodeRightsVisibilityChange {
		t.Fatalf("conflict = %+v, want rights visibility change", c)
	}
}

// TestKnowledgeConflict: payload divergence on a claim is a knowledge
// conflict (docs/09 §6: claim assessment/evidence).
func TestKnowledgeConflict(t *testing.T) {
	base := objRow("cv-00000001", "obj-c-0000001", "claim", 1, "state-base-0001", "active",
		"C1", `{"statement":"alpha"}`, schemaV("1"), nil)
	src := objRow("cv-00000002", "obj-c-0000001", "claim", 2, "state-src-000001", "active",
		"C1", `{"statement":"beta"}`, schemaV("1"), nil)
	tgt := objRow("cv-00000003", "obj-c-0000001", "claim", 2, "state-tgt-000001", "active",
		"C1", `{"statement":"gamma"}`, schemaV("1"), nil)
	r := newFixture().base(base).source(base, src).target(base, tgt).detect(t)

	c := oneConflict(t, objectVerdict(t, r, "obj-c-0000001"))
	if c.Category != CategoryKnowledge || c.Code != CodeKnowledgeFieldDiverges {
		t.Fatalf("conflict = %+v, want knowledge", c)
	}
	if len(c.PayloadKeys) != 1 || c.PayloadKeys[0] != "statement" {
		t.Fatalf("payload_keys = %v, want [statement]", c.PayloadKeys)
	}
}

// TestIdentityConflictSuspectedDuplicate: two same-type same-title
// creations on opposite sides of the fork are a suspected duplicate
// (docs/09 §6 identity conflict).
func TestIdentityConflictSuspectedDuplicate(t *testing.T) {
	src := objRow("xv-00000001", "obj-x-0000001", "experiment", 1, "state-src-000001", "active",
		"Trial A", `{"design":"d"}`, schemaV("1"), nil)
	tgtOther := objRow("yv-00000001", "obj-y-0000001", "experiment", 1, "state-tgt-000001", "active",
		"Trial A", `{"design":"d2"}`, schemaV("1"), nil)
	r := newFixture().source(src).target(tgtOther).detect(t)

	v := objectVerdict(t, r, "obj-x-0000001")
	if v.AutoMergeable {
		t.Fatalf("suspected duplicate verdict auto_mergeable = true")
	}
	c := oneConflict(t, v)
	if c.Category != CategoryIdentity || c.Code != CodeIdentitySuspectedDuplicate {
		t.Fatalf("conflict = %+v, want identity", c)
	}
	if c.OtherObjectID != "obj-y-0000001" {
		t.Fatalf("other_object_id = %q, want obj-y-0000001", c.OtherObjectID)
	}
}

// TestIdentityConflictNegativeCases: different titles or different types
// do not flag; a creation with nothing similar on the target side is
// auto-mergeable.
func TestIdentityConflictNegativeCases(t *testing.T) {
	src := objRow("xv-00000001", "obj-x-0000001", "experiment", 1, "state-src-000001", "active",
		"Trial A", `{"design":"d"}`, schemaV("1"), nil)

	// Different title: no identity conflict, auto.
	tgtDiffTitle := objRow("yv-00000001", "obj-y-0000001", "experiment", 1, "state-tgt-000001", "active",
		"Trial B", `{"design":"d2"}`, schemaV("1"), nil)
	r := newFixture().source(src).target(tgtDiffTitle).detect(t)
	wantAuto(t, objectVerdict(t, r, "obj-x-0000001"))

	// Same title, different type: no identity conflict.
	tgtDiffType := objRow("yv-00000002", "obj-y-0000002", "sample", 1, "state-tgt-000001", "active",
		"Trial A", `{"form":"f"}`, schemaV("1"), nil)
	r = newFixture().source(src).target(tgtDiffType).detect(t)
	wantAuto(t, objectVerdict(t, r, "obj-x-0000001"))

	// Target with nothing: auto.
	r = newFixture().source(src).detect(t)
	wantAuto(t, objectVerdict(t, r, "obj-x-0000001"))
}

// TestDependencyConflict: endpoint divergence on a relation is a
// dependency conflict (which upstream version the edge depends on).
func TestDependencyConflict(t *testing.T) {
	base := relRow("rv-00000001", "rel-r-0000001", 1, "state-base-0001", "depends_on", "ov-1", "ov-2", `{}`)
	src := relRow("rv-00000002", "rel-r-0000001", 2, "state-src-000001", "depends_on", "ov-3", "ov-2", `{}`)
	tgt := relRow("rv-00000003", "rel-r-0000001", 2, "state-tgt-000001", "depends_on", "ov-4", "ov-2", `{}`)
	r := newFixture().baseRel(base).sourceRel(base, src).targetRel(base, tgt).detect(t)

	v := relationVerdict(t, r, "rel-r-0000001")
	if v.AutoMergeable {
		t.Fatalf("dependency conflict verdict auto_mergeable = true")
	}
	c := oneRelConflict(t, v)
	if c.Category != CategoryDependency || c.Code != CodeDependencyFieldDiverges {
		t.Fatalf("conflict = %+v, want dependency", c)
	}
	if len(c.Fields) != 1 || c.Fields[0] != "source_object_version_id" {
		t.Fatalf("fields = %v, want [source_object_version_id]", c.Fields)
	}
}

// TestRelationConflict: payload divergence on a provenance relation is a
// relation conflict (docs/09 §6 "Relation/provenance conflict").
func TestRelationConflict(t *testing.T) {
	base := relRow("rv-00000001", "rel-r-0000001", 1, "state-base-0001", "uses", "ov-1", "ov-2", `{"scope":"a"}`)
	src := relRow("rv-00000002", "rel-r-0000001", 2, "state-src-000001", "uses", "ov-1", "ov-2", `{"scope":"b"}`)
	tgt := relRow("rv-00000003", "rel-r-0000001", 2, "state-tgt-000001", "uses", "ov-1", "ov-2", `{"scope":"c"}`)
	r := newFixture().baseRel(base).sourceRel(base, src).targetRel(base, tgt).detect(t)

	c := oneRelConflict(t, relationVerdict(t, r, "rel-r-0000001"))
	if c.Category != CategoryRelation || c.Code != CodeRelationFieldDiverges {
		t.Fatalf("conflict = %+v, want relation", c)
	}
}

// TestKnowledgeRelationConflict: divergence on a knowledge-category edge
// (supports) is a knowledge/evidence conflict (docs/09 §6).
func TestKnowledgeRelationConflict(t *testing.T) {
	base := relRow("rv-00000001", "rel-r-0000001", 1, "state-base-0001", "supports", "ov-1", "ov-2", `{"note":"a"}`)
	src := relRow("rv-00000002", "rel-r-0000001", 2, "state-src-000001", "supports", "ov-1", "ov-2", `{"note":"b"}`)
	tgt := relRow("rv-00000003", "rel-r-0000001", 2, "state-tgt-000001", "supports", "ov-1", "ov-2", `{"note":"c"}`)
	r := newFixture().baseRel(base).sourceRel(base, src).targetRel(base, tgt).detect(t)

	c := oneRelConflict(t, relationVerdict(t, r, "rel-r-0000001"))
	if c.Category != CategoryKnowledge || c.Code != CodeKnowledgeFieldDiverges {
		t.Fatalf("conflict = %+v, want knowledge", c)
	}
}

// oneRelConflict asserts the relation verdict carries exactly one
// conflict and returns it.
func oneRelConflict(t *testing.T, v RelationVerdict) Conflict {
	t.Helper()
	if len(v.Conflicts) != 1 {
		t.Fatalf("conflicts = %+v, want exactly one", v.Conflicts)
	}
	return v.Conflicts[0]
}

// TestConvergedChangesAuto: both branches making the SAME change is not
// a conflict — the same field converged (docs/09 §7: non-conflicting).
func TestConvergedChangesAuto(t *testing.T) {
	// Same title on both sides.
	base := objRow("ev-00000001", "obj-e-0000001", "experiment", 1, "state-base-0001", "active",
		"E1", `{"design":"d"}`, schemaV("1"), nil)
	src := objRow("ev-00000002", "obj-e-0000001", "experiment", 2, "state-src-000001", "active",
		"E2", `{"design":"d"}`, schemaV("1"), nil)
	tgt := objRow("ev-00000003", "obj-e-0000001", "experiment", 2, "state-tgt-000001", "active",
		"E2", `{"design":"d"}`, schemaV("1"), nil)
	r := newFixture().base(base).source(base, src).target(base, tgt).detect(t)
	wantAuto(t, objectVerdict(t, r, "obj-e-0000001"))

	// Same payload bytes on both sides (converged at key level).
	src2 := objRow("ev-00000004", "obj-e-0000001", "experiment", 2, "state-src-000001", "active",
		"E1", `{"design":"d2"}`, schemaV("1"), nil)
	tgt2 := objRow("ev-00000005", "obj-e-0000001", "experiment", 2, "state-tgt-000001", "active",
		"E1", `{"design":"d2"}`, schemaV("1"), nil)
	r = newFixture().base(base).source(base, src2).target(base, tgt2).detect(t)
	wantAuto(t, objectVerdict(t, r, "obj-e-0000001"))

	// Both branches abort.
	src3 := objRow("hv-00000002", "obj-h-0000001", "hypothesis", 2, "state-src-000001", "aborted",
		"H1", `{"question_id":"q1"}`, schemaV("1"), nil)
	tgt3 := objRow("hv-00000003", "obj-h-0000001", "hypothesis", 2, "state-tgt-000001", "aborted",
		"H1", `{"question_id":"q1"}`, schemaV("1"), nil)
	hbase := objRow("hv-00000001", "obj-h-0000001", "hypothesis", 1, "state-base-0001", "active",
		"H1", `{"question_id":"q1"}`, schemaV("1"), nil)
	r = newFixture().base(hbase).source(hbase, src3).target(hbase, tgt3).detect(t)
	wantAuto(t, objectVerdict(t, r, "obj-h-0000001"))
}

// TestDisjointFieldsAuto: the source and target changed different fields
// of the same object — non-overlapping, auto (docs/09 §7: 同对象不同非冲突字段).
func TestDisjointFieldsAuto(t *testing.T) {
	base := objRow("ev-00000001", "obj-e-0000001", "experiment", 1, "state-base-0001", "active",
		"E1", `{"design":"d"}`, schemaV("1"), nil)
	src := objRow("ev-00000002", "obj-e-0000001", "experiment", 2, "state-src-000001", "active",
		"E2", `{"design":"d"}`, schemaV("1"), nil)
	tgt := objRow("ev-00000003", "obj-e-0000001", "experiment", 2, "state-tgt-000001", "active",
		"E1", `{"design":"d2"}`, schemaV("1"), nil)
	r := newFixture().base(base).source(base, src).target(base, tgt).detect(t)
	wantAuto(t, objectVerdict(t, r, "obj-e-0000001"))
}

// TestAbortVersusEditAuto: an abort on one side and a content edit on the
// other touch different fields — both facts land, nothing is chosen
// between (docs/09 §7 never picks a winner; the abort is preserved).
func TestAbortVersusEditAuto(t *testing.T) {
	base := objRow("hv-00000001", "obj-h-0000001", "hypothesis", 1, "state-base-0001", "active",
		"H1", `{"question_id":"q1"}`, schemaV("1"), nil)
	src := objRow("hv-00000002", "obj-h-0000001", "hypothesis", 2, "state-src-000001", "aborted",
		"H1", `{"question_id":"q1"}`, schemaV("1"), nil)
	tgt := objRow("hv-00000003", "obj-h-0000001", "hypothesis", 2, "state-tgt-000001", "active",
		"H1", `{"question_id":"q2"}`, schemaV("1"), nil)
	r := newFixture().base(base).source(base, src).target(base, tgt).detect(t)
	wantAuto(t, objectVerdict(t, r, "obj-h-0000001"))
}

// TestDetectErrorPropagation: shape errors from the diff engine surface
// through Detect.
func TestDetectErrorPropagation(t *testing.T) {
	_, err := Detect(diff.Inputs{ProjectID: "p"})
	if err == nil {
		t.Fatalf("Detect() = nil error, want missing-state error")
	}
}

// TestReportSummaryCounts: the roll-up counts verdicts and conflict
// categories.
func TestReportSummaryCounts(t *testing.T) {
	// One scientific conflict (protocol) + one auto change (untouched
	// claim) + one identity conflict.
	pbase := objRow("pv-00000001", "obj-p-0000001", "protocol", 1, "state-base-0001", "active",
		"P1", `{"purpose":"p","steps":[{"id":"s1","temperature":300}]}`, schemaV("1"), nil)
	psrc := objRow("pv-00000002", "obj-p-0000001", "protocol", 2, "state-src-000001", "active",
		"P1", `{"purpose":"p","steps":[{"id":"s1","temperature":350}]}`, schemaV("1"), nil)
	ptgt := objRow("pv-00000003", "obj-p-0000001", "protocol", 2, "state-tgt-000001", "active",
		"P1", `{"purpose":"p","steps":[{"id":"s1","temperature":400}]}`, schemaV("1"), nil)
	claim := objRow("cv-00000001", "obj-c-0000001", "claim", 1, "state-base-0001", "active",
		"C1", `{"statement":"s"}`, schemaV("1"), nil)
	claimSrc := objRow("cv-00000002", "obj-c-0000001", "claim", 2, "state-src-000001", "active",
		"C1", `{"statement":"s2"}`, schemaV("1"), nil)
	xsrc := objRow("xv-00000001", "obj-x-0000001", "experiment", 1, "state-src-000001", "active",
		"Trial A", `{"design":"d"}`, schemaV("1"), nil)
	ytgt := objRow("yv-00000001", "obj-y-0000001", "experiment", 1, "state-tgt-000001", "active",
		"Trial A", `{"design":"d2"}`, schemaV("1"), nil)

	r := newFixture().
		base(pbase, claim).
		source(pbase, psrc, claim, claimSrc, xsrc).
		target(pbase, ptgt, claim, ytgt).
		detect(t)

	if r.AutoMergeable {
		t.Fatalf("report auto_mergeable = true, verdicts = %+v", r.ObjectVerdicts)
	}
	s := r.Summary
	if s.ObjectsConflicted != 2 || s.ObjectsAutoMergeable != 1 {
		t.Fatalf("summary = %+v, want 2 conflicted / 1 auto object", s)
	}
	if len(s.ConflictsByCategory) != 2 {
		t.Fatalf("categories = %+v, want scientific + identity", s.ConflictsByCategory)
	}
	got := map[ConflictCategory]int{}
	for _, cc := range s.ConflictsByCategory {
		got[cc.Category] = cc.Count
	}
	if got[CategoryScientific] != 1 || got[CategoryIdentity] != 1 {
		t.Fatalf("category counts = %v, want {scientific:1, identity:1}", got)
	}
}

// TestDetectDeterministic: the same inputs yield the same canonical
// bytes on every run.
func TestDetectDeterministic(t *testing.T) {
	base := objRow("cv-00000001", "obj-c-0000001", "claim", 1, "state-base-0001", "active",
		"C1", `{"statement":"alpha"}`, schemaV("1"), nil)
	src := objRow("cv-00000002", "obj-c-0000001", "claim", 2, "state-src-000001", "active",
		"C1", `{"statement":"beta"}`, schemaV("1"), nil)
	tgt := objRow("cv-00000003", "obj-c-0000001", "claim", 2, "state-tgt-000001", "active",
		"C1", `{"statement":"gamma"}`, schemaV("1"), nil)
	f := newFixture().base(base).source(base, src).target(base, tgt)

	r1 := f.detect(t)
	r2 := f.detect(t)
	b1, err := r1.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	b2, err := r2.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if string(b1) != string(b2) {
		t.Fatalf("canonical bytes differ between runs:\n%s\n%s", b1, b2)
	}
}
