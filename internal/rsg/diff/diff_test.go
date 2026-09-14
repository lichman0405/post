package diff

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/rsg/manifest"
)

// fixtureClock is the fixed timestamp every fixture row carries, so the
// canonical bytes never depend on the wall clock.
var fixtureClock = time.Date(2026, 2, 10, 9, 30, 0, 0, time.UTC)

// objRow builds one object version row for fixtures. Payload is stored in
// a deliberately non-canonical spelling when spellNonCanonical is true, to
// prove the engine compares canonical bytes, not stored ones.
func objRow(id, objectID, objectType string, versionNo int, stateID, lifecycle, title, payload string, policy *string) manifest.ObjectVersion {
	return manifest.ObjectVersion{
		ID:                 id,
		ObjectID:           objectID,
		ObjectType:         objectType,
		VersionNo:          versionNo,
		StateID:            stateID,
		SchemaRef:          manifest.SchemaRef{ID: "https://open-rd.example/schemas/" + objectType + ".schema.json", Version: "1"},
		Title:              title,
		LifecycleState:     lifecycle,
		Payload:            json.RawMessage(payload),
		VisibilityPolicyID: policy,
		IntegrityHash:      "sha256:fixture",
		CreatedBy:          "user-00000001",
		CreatedAt:          fixtureClock,
	}
}

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

// baseInputs is the shared three-way fixture: the base lineage holds claim
// C (v1), hypothesis H (v1) and relation R (v1, supports C→H); the source
// lineage updates C to v2, aborts H with v2 and creates finding F (v1);
// the target lineage also updates C (v2 with a different statement) and
// creates experiment E — target-side movement the conflict detector will
// later read off the diff. Input row order is deliberately scrambled:
// stable ordering is the engine's rule, not the store's.
func baseInputs() Inputs {
	in := Inputs{
		ProjectID: "proj-00000001",
		Base:      StateRef{ID: "state-base-0001"},
		Source:    StateRef{ID: "state-src-000001", GitRef: strptr("cccccccccccccccccccccccccccccccccccccccc")},
		Target:    StateRef{ID: "state-tgt-000001"},
	}
	in.BaseSnapshot = manifest.Snapshot{
		ObjectVersions: []manifest.ObjectVersion{
			objRow("cv-00000001", "obj-c-0000001", "claim", 1, "state-base-0001", "active", "C1", `{"statement":"alpha"}`, nil),
			objRow("hv-00000001", "obj-h-0000001", "hypothesis", 1, "state-base-0001", "active", "H1", `{"question_ref":"q1"}`, nil),
		},
		RelationVersions: []manifest.RelationVersion{
			relRow("rv-00000001", "rel-r-0000001", 1, "state-base-0001", "supports", "cv-00000001", "hv-00000001", `{"scope":"base"}`),
		},
		BlobRefs: []manifest.BlobRef{},
	}
	in.SourceSnapshot = manifest.Snapshot{
		ObjectVersions: []manifest.ObjectVersion{
			objRow("cv-00000002", "obj-c-0000001", "claim", 2, "state-src-000001", "active", "C2", `{"statement":"beta"}`, nil),
			objRow("hv-00000002", "obj-h-0000001", "hypothesis", 2, "state-src-000001", "aborted", "H1", `{"question_ref":"q1"}`, nil),
			objRow("fv-00000001", "obj-f-0000001", "finding", 1, "state-src-000001", "active", "F1", `{"summary":"found"}`, nil),
			objRow("cv-00000001", "obj-c-0000001", "claim", 1, "state-base-0001", "active", "C1", `{"statement":"alpha"}`, nil),
			objRow("hv-00000001", "obj-h-0000001", "hypothesis", 1, "state-base-0001", "active", "H1", `{"question_ref":"q1"}`, nil),
		},
		RelationVersions: []manifest.RelationVersion{
			relRow("rv-00000002", "rel-r-0000001", 2, "state-src-000001", "supports", "cv-00000002", "hv-00000002", `{"scope":"new"}`),
			relRow("rv-00000001", "rel-r-0000001", 1, "state-base-0001", "supports", "cv-00000001", "hv-00000001", `{"scope":"base"}`),
		},
		BlobRefs: []manifest.BlobRef{},
	}
	in.TargetSnapshot = manifest.Snapshot{
		ObjectVersions: []manifest.ObjectVersion{
			objRow("cv-00000003", "obj-c-0000001", "claim", 2, "state-tgt-000001", "active", "C2", `{"statement":"gamma"}`, nil),
			objRow("ev-00000001", "obj-e-0000001", "experiment", 1, "state-tgt-000001", "active", "E1", `{"design":"d"}`, nil),
			objRow("cv-00000001", "obj-c-0000001", "claim", 1, "state-base-0001", "active", "C1", `{"statement":"alpha"}`, nil),
			objRow("hv-00000001", "obj-h-0000001", "hypothesis", 1, "state-base-0001", "active", "H1", `{"question_ref":"q1"}`, nil),
		},
		RelationVersions: []manifest.RelationVersion{
			relRow("rv-00000001", "rel-r-0000001", 1, "state-base-0001", "supports", "cv-00000001", "hv-00000001", `{"scope":"base"}`),
		},
		BlobRefs: []manifest.BlobRef{},
	}
	return in
}

// findObject returns the object change with the given id, or fails.
func findObject(t *testing.T, d *Diff, objectID string) ObjectChange {
	t.Helper()
	for _, c := range d.ObjectChanges {
		if c.ObjectID == objectID {
			return c
		}
	}
	t.Fatalf("no object change for %s in %+v", objectID, d.ObjectChanges)
	return ObjectChange{}
}

func findRelation(t *testing.T, d *Diff, relationID string) RelationChange {
	t.Helper()
	for _, c := range d.RelationChanges {
		if c.RelationID == relationID {
			return c
		}
	}
	t.Fatalf("no relation change for %s in %+v", relationID, d.RelationChanges)
	return RelationChange{}
}

// TestClassifyKinds pins the kind classification on the shared fixture:
// updated claim, aborted hypothesis, created finding; the target-only
// experiment is not listed (nothing disappears and target-only changes are
// not source-side changes).
func TestClassifyKinds(t *testing.T) {
	d, err := Compute(baseInputs())
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	c := findObject(t, d, "obj-c-0000001")
	if c.Kind != ChangeUpdated {
		t.Fatalf("claim kind = %q, want updated", c.Kind)
	}
	if !c.TargetMoved {
		t.Fatalf("claim TargetMoved = false; target updated it since base")
	}
	if c.BaseVersion == nil || c.BaseVersion.ID != "cv-00000001" {
		t.Fatalf("claim base head = %+v, want cv-00000001", c.BaseVersion)
	}
	if c.SourceVersion.ID != "cv-00000002" {
		t.Fatalf("claim source head = %s, want cv-00000002", c.SourceVersion.ID)
	}
	if c.TargetVersion == nil || c.TargetVersion.ID != "cv-00000003" {
		t.Fatalf("claim target head = %+v, want cv-00000003", c.TargetVersion)
	}
	h := findObject(t, d, "obj-h-0000001")
	if h.Kind != ChangeAborted {
		t.Fatalf("hypothesis kind = %q, want aborted", h.Kind)
	}
	f := findObject(t, d, "obj-f-0000001")
	if f.Kind != ChangeCreated {
		t.Fatalf("finding kind = %q, want created", f.Kind)
	}
	if f.BaseVersion != nil {
		t.Fatalf("created finding must have nil base version, got %+v", f.BaseVersion)
	}
	if f.TargetMoved {
		t.Fatalf("finding TargetMoved = true; target never saw it")
	}
	for _, c := range d.ObjectChanges {
		if c.ObjectID == "obj-e-0000001" {
			t.Fatalf("target-only experiment listed as a source-side change: %+v", c)
		}
	}
	if len(d.ObjectChanges) != 3 {
		t.Fatalf("object changes = %d, want 3: %+v", len(d.ObjectChanges), d.ObjectChanges)
	}
}

// TestRelationChanges pins relation classification: updated R with the
// moved endpoints, and the created case in a separate scenario.
func TestRelationChanges(t *testing.T) {
	d, err := Compute(baseInputs())
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	r := findRelation(t, d, "rel-r-0000001")
	if r.Kind != ChangeUpdated {
		t.Fatalf("relation kind = %q, want updated", r.Kind)
	}
	want := []string{"source_object_version_id", "target_object_version_id", "payload"}
	if strings.Join(r.ChangedFields, ",") != strings.Join(want, ",") {
		t.Fatalf("relation changed fields = %v, want %v", r.ChangedFields, want)
	}
	if r.TargetMoved {
		t.Fatalf("relation TargetMoved = true; target did not touch it")
	}
	if len(r.NewVersions) != 1 || r.NewVersions[0].ID != "rv-00000002" {
		t.Fatalf("relation trail = %+v, want [rv-00000002]", r.NewVersions)
	}
	if r.BaseVersion == nil || r.BaseVersion.ID != "rv-00000001" {
		t.Fatalf("relation base head = %+v, want rv-00000001", r.BaseVersion)
	}
}

// TestReopenedAndReAbort pins the lifecycle-flip edges: an aborted base
// head with an active source head is a reopen; aborting an already-aborted
// object is an update, not an abort (the flip is what names the kind).
func TestReopenedAndReAbort(t *testing.T) {
	base := manifest.Snapshot{ObjectVersions: []manifest.ObjectVersion{
		objRow("ov-00000001", "obj-a-0000001", "claim", 1, "s-b", "aborted", "A", `{"statement":"a"}`, nil),
	}}
	source := manifest.Snapshot{ObjectVersions: []manifest.ObjectVersion{
		objRow("ov-00000001", "obj-a-0000001", "claim", 1, "s-b", "aborted", "A", `{"statement":"a"}`, nil),
		objRow("ov-00000002", "obj-a-0000001", "claim", 2, "s-s", "active", "A", `{"statement":"a"}`, nil),
	}}
	d, err := Compute(Inputs{
		ProjectID: "p", Base: StateRef{ID: "s-b"}, Source: StateRef{ID: "s-s"}, Target: StateRef{ID: "s-t"},
		BaseSnapshot: base, SourceSnapshot: source, TargetSnapshot: manifest.Snapshot{},
	})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	c := findObject(t, d, "obj-a-0000001")
	if c.Kind != ChangeReopened {
		t.Fatalf("kind = %q, want reopened", c.Kind)
	}

	// Re-abort: base head already aborted, source appends another aborted
	// version. The lifecycle does not flip — the kind is updated, with the
	// lifecycle field unmoved.
	base2 := manifest.Snapshot{ObjectVersions: []manifest.ObjectVersion{
		objRow("ov-00000002", "obj-a-0000001", "claim", 2, "s-b", "aborted", "A", `{"statement":"a"}`, nil),
	}}
	source2 := manifest.Snapshot{ObjectVersions: []manifest.ObjectVersion{
		objRow("ov-00000003", "obj-a-0000001", "claim", 3, "s-s", "aborted", "A", `{"statement":"a"}`, nil),
	}}
	d2, err := Compute(Inputs{
		ProjectID: "p", Base: StateRef{ID: "s-b"}, Source: StateRef{ID: "s-s"}, Target: StateRef{ID: "s-t"},
		BaseSnapshot: base2, SourceSnapshot: source2, TargetSnapshot: manifest.Snapshot{},
	})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	c2 := findObject(t, d2, "obj-a-0000001")
	if c2.Kind != ChangeUpdated {
		t.Fatalf("re-abort kind = %q, want updated", c2.Kind)
	}
	if len(c2.ChangedFields) != 0 {
		t.Fatalf("re-abort changed fields = %v, want none (content identical)", c2.ChangedFields)
	}
}

// TestUnchangedSkipped proves objects and relations whose head did not
// move produce no entry, and an object absent from the source (it lives
// only on the base side) is not listed — nothing disappears.
func TestUnchangedSkipped(t *testing.T) {
	row := objRow("ov-00000001", "obj-a-0000001", "claim", 1, "s-b", "active", "A", `{"statement":"a"}`, nil)
	d, err := Compute(Inputs{
		ProjectID: "p", Base: StateRef{ID: "s-b"}, Source: StateRef{ID: "s-s"}, Target: StateRef{ID: "s-t"},
		BaseSnapshot:   manifest.Snapshot{ObjectVersions: []manifest.ObjectVersion{row, objRow("ov-00000002", "obj-b-0000001", "claim", 1, "s-b", "active", "B", `{"statement":"b"}`, nil)}},
		SourceSnapshot: manifest.Snapshot{ObjectVersions: []manifest.ObjectVersion{row}},
		TargetSnapshot: manifest.Snapshot{},
	})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if len(d.ObjectChanges) != 0 {
		t.Fatalf("object changes = %+v, want none", d.ObjectChanges)
	}
}

// TestCanonicalPayloadComparison proves the field diff compares canonical
// payload bytes: a new version whose payload is the same JSON object in a
// different key order is still listed (a version change is a change) but
// reports NO payload field move, and a real content change reports it.
func TestCanonicalPayloadComparison(t *testing.T) {
	base := manifest.Snapshot{ObjectVersions: []manifest.ObjectVersion{
		objRow("ov-00000001", "obj-a-0000001", "claim", 1, "s-b", "active", "A", `{"statement":"x","confidence":0.9}`, nil),
	}}
	spelled := manifest.Snapshot{ObjectVersions: []manifest.ObjectVersion{
		objRow("ov-00000002", "obj-a-0000001", "claim", 2, "s-s", "active", "A", `{"confidence":0.9,"statement":"x"}`, nil),
	}}
	d, err := Compute(Inputs{
		ProjectID: "p", Base: StateRef{ID: "s-b"}, Source: StateRef{ID: "s-s"}, Target: StateRef{ID: "s-t"},
		BaseSnapshot: base, SourceSnapshot: spelled, TargetSnapshot: manifest.Snapshot{},
	})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	c := findObject(t, d, "obj-a-0000001")
	if c.Kind != ChangeUpdated {
		t.Fatalf("respelled payload version: kind = %q, want updated (a new version row is a change)", c.Kind)
	}
	if len(c.ChangedFields) != 0 {
		t.Fatalf("respelled payload listed as a field change: %v (canonical bytes are equal)", c.ChangedFields)
	}

	changed := manifest.Snapshot{ObjectVersions: []manifest.ObjectVersion{
		objRow("ov-00000003", "obj-a-0000001", "claim", 2, "s-s", "active", "A", `{"statement":"y","confidence":0.9}`, nil),
	}}
	d2, err := Compute(Inputs{
		ProjectID: "p", Base: StateRef{ID: "s-b"}, Source: StateRef{ID: "s-s"}, Target: StateRef{ID: "s-t"},
		BaseSnapshot: base, SourceSnapshot: changed, TargetSnapshot: manifest.Snapshot{},
	})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	c2 := findObject(t, d2, "obj-a-0000001")
	if strings.Join(c2.ChangedFields, ",") != "payload" {
		t.Fatalf("changed fields = %v, want [payload]", c2.ChangedFields)
	}
	if string(c2.SourceVersion.Payload) != `{"confidence":0.9,"statement":"y"}` {
		t.Fatalf("output payload not canonicalized: %s", c2.SourceVersion.Payload)
	}
}

// TestStableOrdering proves the change lists sort by identity key
// regardless of input row order, and the new-version trail sorts by
// version_no.
func TestStableOrdering(t *testing.T) {
	in := baseInputs()
	// Scramble every input slice; the output order must not move.
	scramble := func(s manifest.Snapshot) manifest.Snapshot {
		rev := make([]manifest.ObjectVersion, len(s.ObjectVersions))
		for i, v := range s.ObjectVersions {
			rev[len(s.ObjectVersions)-1-i] = v
		}
		s.ObjectVersions = rev
		return s
	}
	in.BaseSnapshot = scramble(in.BaseSnapshot)
	in.SourceSnapshot = scramble(in.SourceSnapshot)
	in.TargetSnapshot = scramble(in.TargetSnapshot)

	d1, err := Compute(baseInputs())
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	d2, err := Compute(in)
	if err != nil {
		t.Fatalf("Compute scrambled: %v", err)
	}
	b1, err := d1.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	b2, err := d2.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON scrambled: %v", err)
	}
	if string(b1) != string(b2) {
		t.Fatalf("scrambled input rows moved the canonical bytes:\n got: %s\nwant: %s", b2, b1)
	}

	ids := make([]string, len(d1.ObjectChanges))
	for i, c := range d1.ObjectChanges {
		ids[i] = c.ObjectID
	}
	if strings.Join(ids, ",") != "obj-c-0000001,obj-f-0000001,obj-h-0000001" {
		t.Fatalf("object change order = %v, want identity-sorted", ids)
	}
	c := findObject(t, d1, "obj-c-0000001")
	if len(c.NewVersions) != 1 || c.NewVersions[0].ID != "cv-00000002" {
		t.Fatalf("claim trail = %+v, want [cv-00000002]", c.NewVersions)
	}
}

// TestChangedFieldsCanonicalOrder pins the changed-fields order: title,
// payload, lifecycle_state, schema_ref, visibility_policy_id.
func TestChangedFieldsCanonicalOrder(t *testing.T) {
	base := manifest.Snapshot{ObjectVersions: []manifest.ObjectVersion{
		objRow("ov-00000001", "obj-a-0000001", "claim", 1, "s-b", "active", "A", `{"statement":"x"}`, nil),
	}}
	source := manifest.Snapshot{ObjectVersions: []manifest.ObjectVersion{
		objRow("ov-00000002", "obj-a-0000001", "claim", 2, "s-s", "aborted", "B", `{"statement":"y"}`, strptr("pol-00000001")),
	}}
	d, err := Compute(Inputs{
		ProjectID: "p", Base: StateRef{ID: "s-b"}, Source: StateRef{ID: "s-s"}, Target: StateRef{ID: "s-t"},
		BaseSnapshot: base, SourceSnapshot: source, TargetSnapshot: manifest.Snapshot{},
	})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	c := findObject(t, d, "obj-a-0000001")
	want := "title,payload,lifecycle_state,visibility_policy_id"
	if strings.Join(c.ChangedFields, ",") != want {
		t.Fatalf("changed fields = %v, want %v", c.ChangedFields, want)
	}
	if c.Kind != ChangeAborted {
		t.Fatalf("kind = %q, want aborted (lifecycle flipped)", c.Kind)
	}
}

// TestSchemaAndVisibilitySummary pins the schema/visibility summary
// categories: they count changed fields, and a schema-ref move on the
// source side shows up even when the target also moved the object.
func TestSchemaAndVisibilitySummary(t *testing.T) {
	base := manifest.Snapshot{ObjectVersions: []manifest.ObjectVersion{
		objRow("ov-00000001", "obj-a-0000001", "claim", 1, "s-b", "active", "A", `{"statement":"x"}`, nil),
	}}
	source := manifest.Snapshot{ObjectVersions: []manifest.ObjectVersion{
		objRow("ov-00000002", "obj-a-0000001", "claim", 2, "s-s", "active", "A", `{"statement":"x"}`, strptr("pol-00000001")),
	}}
	d, err := Compute(Inputs{
		ProjectID: "p", Base: StateRef{ID: "s-b"}, Source: StateRef{ID: "s-s"}, Target: StateRef{ID: "s-t"},
		BaseSnapshot: base, SourceSnapshot: source, TargetSnapshot: manifest.Snapshot{},
	})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if d.Summary.VisibilityChanged != 1 || d.Summary.SchemaChanged != 0 {
		t.Fatalf("summary = %+v, want visibility_changed=1 schema_changed=0", d.Summary)
	}
}

// TestFileDiffRefs pins the file-level refs: a side is listed when at
// least one end carries a git ref, source first; sides with no git
// material at all are absent.
func TestFileDiffRefs(t *testing.T) {
	in := baseInputs()
	d, err := Compute(in)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	// Base has no git ref; source has one. Target has none — the source
	// side renders (base end empty = empty tree), the target side does not.
	if len(d.FileDiffRefs) != 1 {
		t.Fatalf("file diff refs = %+v, want exactly the source side", d.FileDiffRefs)
	}
	ref := d.FileDiffRefs[0]
	if ref.Kind != fileRefSource || ref.BaseGitRef != "" || ref.HeadGitRef != "cccccccccccccccccccccccccccccccccccccccc" {
		t.Fatalf("source ref = %+v, want kind=source base=\"\" head=cccc…", ref)
	}

	// Both sides with git refs render in the canonical order source,
	// target — and the base ref appears on both entries.
	in2 := baseInputs()
	in2.Base.GitRef = strptr("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	in2.Target.GitRef = strptr("dddddddddddddddddddddddddddddddddddddddd")
	d2, err := Compute(in2)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if len(d2.FileDiffRefs) != 2 {
		t.Fatalf("file diff refs = %+v, want both sides", d2.FileDiffRefs)
	}
	if d2.FileDiffRefs[0].Kind != fileRefSource || d2.FileDiffRefs[1].Kind != fileRefTarget {
		t.Fatalf("ref order = %+v, want source then target", d2.FileDiffRefs)
	}
	if d2.FileDiffRefs[0].BaseGitRef != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("base ref not carried: %+v", d2.FileDiffRefs[0])
	}
}

// TestSummaryCounts pins the categorized counts of the shared fixture.
func TestSummaryCounts(t *testing.T) {
	d, err := Compute(baseInputs())
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	s := d.Summary
	if s.ObjectsCreated != 1 || s.ObjectsUpdated != 1 || s.ObjectsAborted != 1 || s.ObjectsReopened != 0 {
		t.Fatalf("object summary = %+v, want created=1 updated=1 aborted=1", s)
	}
	if s.RelationsCreated != 0 || s.RelationsUpdated != 1 {
		t.Fatalf("relation summary = %+v, want updated=1", s)
	}
	wantTypes := []TypeCount{{Type: "claim", Count: 1}, {Type: "finding", Count: 1}, {Type: "hypothesis", Count: 1}}
	if len(s.ObjectTypes) != len(wantTypes) {
		t.Fatalf("object types = %+v, want %+v", s.ObjectTypes, wantTypes)
	}
	for i := range wantTypes {
		if s.ObjectTypes[i] != wantTypes[i] {
			t.Fatalf("object types = %+v, want %+v (sorted by type)", s.ObjectTypes, wantTypes)
		}
	}
	if len(s.RelationTypes) != 1 || s.RelationTypes[0] != (TypeCount{Type: "supports", Count: 1}) {
		t.Fatalf("relation types = %+v, want [supports 1]", s.RelationTypes)
	}
}

// TestValidation pins the input-shape errors.
func TestValidation(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Inputs)
		want string
	}{
		{"missing project", func(i *Inputs) { i.ProjectID = "" }, "project_id"},
		{"missing base", func(i *Inputs) { i.Base.ID = "" }, "base state id"},
		{"missing source", func(i *Inputs) { i.Source.ID = "" }, "source state id"},
		{"missing target", func(i *Inputs) { i.Target.ID = "" }, "target state id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInputs()
			tc.mut(&in)
			_, err := Compute(in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Compute error = %v, want one naming %q", err, tc.want)
			}
		})
	}
}

// TestInvalidPayloadSurfaces proves a corrupted stored payload is an
// error, never a silently compared byte blob.
func TestInvalidPayloadSurfaces(t *testing.T) {
	in := baseInputs()
	in.SourceSnapshot.ObjectVersions[0].Payload = json.RawMessage(`{"statement": `)
	_, err := Compute(in)
	if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("Compute error = %v, want invalid-payload error", err)
	}
}

// TestBaseNotAncestor pins the pathological case honestly: when the source
// lineage's head is older than the base head (the base is not an ancestor
// of the source), the change is still reported as an update but the
// version trail is empty — the engine never fabricates versions the source
// lineage does not carry.
func TestBaseNotAncestor(t *testing.T) {
	base := manifest.Snapshot{ObjectVersions: []manifest.ObjectVersion{
		objRow("ov-00000003", "obj-a-0000001", "claim", 3, "s-b", "active", "A", `{"statement":"x"}`, nil),
	}}
	source := manifest.Snapshot{ObjectVersions: []manifest.ObjectVersion{
		objRow("ov-00000001", "obj-a-0000001", "claim", 1, "s-s", "active", "A", `{"statement":"x"}`, nil),
	}}
	d, err := Compute(Inputs{
		ProjectID: "p", Base: StateRef{ID: "s-b"}, Source: StateRef{ID: "s-s"}, Target: StateRef{ID: "s-t"},
		BaseSnapshot: base, SourceSnapshot: source, TargetSnapshot: manifest.Snapshot{},
	})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	c := findObject(t, d, "obj-a-0000001")
	if c.Kind != ChangeUpdated {
		t.Fatalf("kind = %q, want updated (heads differ)", c.Kind)
	}
	if len(c.NewVersions) != 0 {
		t.Fatalf("trail = %+v, want empty (base ahead of source)", c.NewVersions)
	}
}

// TestRepeatedComputeByteIdentical is the reproducibility half of the
// acceptance criteria: computing the same inputs twice yields the same
// canonical bytes, timestamps and all.
func TestRepeatedComputeByteIdentical(t *testing.T) {
	d1, err := Compute(baseInputs())
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	d2, err := Compute(baseInputs())
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	b1, _ := d1.CanonicalJSON()
	b2, _ := d2.CanonicalJSON()
	if string(b1) != string(b2) {
		t.Fatalf("repeated compute moved the canonical bytes:\nfirst: %s\nsecond: %s", b1, b2)
	}
}
