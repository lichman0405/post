package conflict

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lichman0405/post/internal/rsg/diff"
	"github.com/lichman0405/post/internal/rsg/manifest"
)

// goldenInputs is the comprehensive fixture the golden bytes pin: one
// scenario per conflict category plus the auto-mergeable cases, in the
// same three-way shape the diff engine's own golden fixture uses.
//
//   - protocol P: steps diverged on both branches → scientific conflict;
//   - claim C: evidence list appended on both branches → auto (append-only
//     evidence, never a text conflict);
//   - experiment E: title diverged → attribute conflict;
//   - dataset D: schema_ref diverged → schema conflict;
//   - sample S: visibility moved on the source only → rights change (never
//     auto);
//   - finding F: created on the source side with a same-title target-side
//     creation → identity conflict;
//   - hypothesis H: aborted on the source side, untouched target → auto;
//   - relation supports R1: endpoint and payload diverged → dependency +
//     knowledge conflicts (evidence-shaped edge);
//   - relation uses R2: endpoints and payload diverged → dependency +
//     relation conflicts;
//   - relation depends_on R3: endpoint diverged → dependency conflict.
func goldenInputs() diff.Inputs {
	in := diff.Inputs{
		ProjectID: "proj-00000001",
		Base:      diff.StateRef{ID: "state-base-0001"},
		Source:    diff.StateRef{ID: "state-src-000001", GitRef: strptr("cccccccccccccccccccccccccccccccccccccccc")},
		Target:    diff.StateRef{ID: "state-tgt-000001", GitRef: strptr("dddddddddddddddddddddddddddddddddddddddd")},
	}
	pbase := objRow("pv-00000001", "obj-p-0000001", "protocol", 1, "state-base-0001", "active",
		"P1", `{"purpose":"p","steps":[{"id":"s1","temperature":300}]}`, schemaV("1"), nil)
	psrc := objRow("pv-00000002", "obj-p-0000001", "protocol", 2, "state-src-000001", "active",
		"P1", `{"purpose":"p","steps":[{"id":"s1","temperature":350}]}`, schemaV("1"), nil)
	ptgt := objRow("pv-00000003", "obj-p-0000001", "protocol", 2, "state-tgt-000001", "active",
		"P1", `{"purpose":"p","steps":[{"id":"s1","temperature":400}]}`, schemaV("1"), nil)

	cbase := objRow("cv-00000001", "obj-c-0000001", "claim", 1, "state-base-0001", "active",
		"C1", `{"statement":"s","evidence_refs":["e1"]}`, schemaV("1"), nil)
	csrc := objRow("cv-00000002", "obj-c-0000001", "claim", 2, "state-src-000001", "active",
		"C1", `{"statement":"s","evidence_refs":["e1","e2"]}`, schemaV("1"), nil)
	ctgt := objRow("cv-00000003", "obj-c-0000001", "claim", 2, "state-tgt-000001", "active",
		"C1", `{"statement":"s","evidence_refs":["e1","e3"]}`, schemaV("1"), nil)

	ebase := objRow("ev-00000001", "obj-e-0000001", "experiment", 1, "state-base-0001", "active",
		"E1", `{"design":"d"}`, schemaV("1"), nil)
	esrc := objRow("ev-00000002", "obj-e-0000001", "experiment", 2, "state-src-000001", "active",
		"E2", `{"design":"d"}`, schemaV("1"), nil)
	etgt := objRow("ev-00000003", "obj-e-0000001", "experiment", 2, "state-tgt-000001", "active",
		"E3", `{"design":"d"}`, schemaV("1"), nil)

	// schemaV is claim-shaped; the dataset rows pin their own dataset
	// schema refs so the divergence is a ref move on both sides.
	dschema := func(v string) manifest.SchemaRef {
		return manifest.SchemaRef{ID: "https://open-rd.example/schemas/dataset.schema.json", Version: v}
	}
	dbase := objRow("dv-00000001", "obj-d-0000001", "dataset", 1, "state-base-0001", "active",
		"D1", `{"purpose":"p"}`, dschema("1"), nil)
	dsrc := objRow("dv-00000002", "obj-d-0000001", "dataset", 2, "state-src-000001", "active",
		"D1", `{"purpose":"p"}`, dschema("2"), nil)
	dtgt := objRow("dv-00000003", "obj-d-0000001", "dataset", 2, "state-tgt-000001", "active",
		"D1", `{"purpose":"p"}`, dschema("3"), nil)

	sbase := objRow("sv-00000001", "obj-s-0000001", "sample", 1, "state-base-0001", "active",
		"S1", `{"form":"f"}`, schemaV("1"), nil)
	ssrc := objRow("sv-00000002", "obj-s-0000001", "sample", 2, "state-src-000001", "active",
		"S1", `{"form":"f"}`, schemaV("1"), strptr("policy-a"))
	stgt := objRow("sv-00000003", "obj-s-0000001", "sample", 2, "state-tgt-000001", "active",
		"S2", `{"form":"f"}`, schemaV("1"), nil)

	fsrc := objRow("fv-00000001", "obj-f-0000001", "finding", 1, "state-src-000001", "active",
		"Finding A", `{"summary":"found"}`, schemaV("1"), nil)
	fdup := objRow("fv-00000002", "obj-f-0000002", "finding", 1, "state-tgt-000001", "active",
		"Finding A", `{"summary":"found again"}`, schemaV("1"), nil)

	hbase := objRow("hv-00000001", "obj-h-0000001", "hypothesis", 1, "state-base-0001", "active",
		"H1", `{"question_id":"q1"}`, schemaV("1"), nil)
	hsrc := objRow("hv-00000002", "obj-h-0000001", "hypothesis", 2, "state-src-000001", "aborted",
		"H1", `{"question_id":"q1"}`, schemaV("1"), nil)

	r1base := relRow("rv-00000001", "rel-r-0000001", 1, "state-base-0001", "supports", "cv-00000001", "hv-00000001", `{"note":"a"}`)
	r1src := relRow("rv-00000002", "rel-r-0000001", 2, "state-src-000001", "supports", "cv-00000002", "hv-00000001", `{"note":"b"}`)
	r1tgt := relRow("rv-00000003", "rel-r-0000001", 2, "state-tgt-000001", "supports", "cv-00000003", "hv-00000001", `{"note":"c"}`)

	r2base := relRow("rv-00000011", "rel-r-0000002", 1, "state-base-0001", "uses", "ev-00000001", "pv-00000001", `{"scope":"a"}`)
	r2src := relRow("rv-00000012", "rel-r-0000002", 2, "state-src-000001", "uses", "ev-00000002", "pv-00000002", `{"scope":"b"}`)
	r2tgt := relRow("rv-00000013", "rel-r-0000002", 2, "state-tgt-000001", "uses", "ev-00000003", "pv-00000003", `{"scope":"c"}`)

	r3base := relRow("rv-00000021", "rel-r-0000003", 1, "state-base-0001", "depends_on", "cv-00000001", "pv-00000001", `{}`)
	r3src := relRow("rv-00000022", "rel-r-0000003", 2, "state-src-000001", "depends_on", "cv-00000002", "pv-00000001", `{}`)
	r3tgt := relRow("rv-00000023", "rel-r-0000003", 2, "state-tgt-000001", "depends_on", "cv-00000003", "pv-00000001", `{}`)

	in.BaseSnapshot = manifest.Snapshot{
		ObjectVersions:   []manifest.ObjectVersion{pbase, cbase, ebase, dbase, sbase, hbase},
		RelationVersions: []manifest.RelationVersion{r1base, r2base, r3base},
	}
	in.SourceSnapshot = manifest.Snapshot{
		ObjectVersions: []manifest.ObjectVersion{
			pbase, psrc, cbase, csrc, ebase, esrc, dbase, dsrc, sbase, ssrc, hbase, hsrc, fsrc,
		},
		RelationVersions: []manifest.RelationVersion{
			r1base, r1src, r2base, r2src, r3base, r3src,
		},
	}
	in.TargetSnapshot = manifest.Snapshot{
		ObjectVersions: []manifest.ObjectVersion{
			pbase, ptgt, cbase, ctgt, ebase, etgt, dbase, dtgt, sbase, stgt, hbase, fdup,
		},
		RelationVersions: []manifest.RelationVersion{
			r1base, r1tgt, r2base, r2tgt, r3base, r3tgt,
		},
	}
	return in
}

// TestGolden pins the report's canonical bytes: the detector's output
// format is the contract the merge engine (T0406) and the resolution UI
// (T0407) build on, so any change to the classification or the field
// order must be a deliberate, reviewed change of the golden file.
func TestGolden(t *testing.T) {
	r, err := Detect(goldenInputs())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	got, err := r.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	got = append(got, '\n')

	path := filepath.Join("testdata", "v1-golden.json")
	if update := os.Getenv("UPDATE_GOLDEN"); update == "1" {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("golden updated: %s", path)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (re-run with UPDATE_GOLDEN=1 to create)", path, err)
	}
	if string(got) != string(want) {
		t.Fatalf("golden mismatch (re-run with UPDATE_GOLDEN=1 after reviewing the diff):\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
