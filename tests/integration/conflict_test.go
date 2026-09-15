package integration

import (
	"context"
	"testing"

	"github.com/lichman0405/post/internal/application/diffs"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/conflict"
)

// Package integration holds the G1/G3 database gate for the Semantic
// Conflict Detector (task T0405): the classifier runs over real
// PostgreSQL through the same service composition cmd/api/main.go uses —
// the rsg write path builds the three branches, the diffs service reads
// them, mocks are not used anywhere in this package.
//
// Required tests (task T0405):
//
//   - conflict unit: internal/rsg/conflict's unit suite (the pure
//     classifier over fixture snapshots);
//
// and these real-stack checks of the two acceptance criteria:
//
//   - Protocol 同字段不同值识别 scientific conflict: TestProtocolSameFieldDivergesIsScientificConflict
//     — both branches move one protocol step to different values; the
//     verdict is a scientific conflict, never auto-mergeable;
//   - append evidence 不误判文本 conflict: TestAppendEvidenceNotTextConflict
//     — both branches append a different claim version ref to the same
//     finding; the verdict stays auto_mergeable with no conflicts.

// computeConflicts runs the diffs service's Conflicts method over the
// three named states.
func (f *diffFixture) computeConflicts(t *testing.T, ctx context.Context, base, source, target string) *conflict.Report {
	t.Helper()
	r, err := f.diffs.Conflicts(ctx, diffs.Params{
		ProjectID:     f.project.ID,
		BaseStateID:   base,
		SourceStateID: source,
		TargetStateID: target,
	})
	if err != nil {
		t.Fatalf("Conflicts: %v", err)
	}
	return r
}

// TestProtocolSameFieldDivergesIsScientificConflict is the first
// acceptance criterion over the real stack: the same protocol field
// changed to different values on the two branches is a scientific
// conflict (docs/09 §7: Protocol 冲突数值折中禁止自动), never
// auto-mergeable.
func TestProtocolSameFieldDivergesIsScientificConflict(t *testing.T) {
	ctx := testCtx(t)
	f := newDiffFixture(t, ctx)

	// main: protocol P v1 with one step.
	pObject, _ := f.createObject(t, ctx, f.main, "protocol",
		`{"purpose":"synthesize","steps":[{"id":"s1","temperature":300}]}`)

	// feature forks main's head — that state is the merge base.
	feature, err := f.svc.CreateBranch(ctx, f.alice, f.project.ID, rsg.CreateBranchInput{
		Name:       "feature",
		BaseRef:    *f.head(t, ctx, f.main),
		Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create feature branch: %v", err)
	}
	base := *feature.BaseStateID

	// feature moves the step's temperature to 350; main moves it to 400.
	f.updateObject(t, ctx, feature.ID, pObject, 1,
		`{"purpose":"synthesize","steps":[{"id":"s1","temperature":350}]}`)
	f.updateObject(t, ctx, f.main, pObject, 2,
		`{"purpose":"synthesize","steps":[{"id":"s1","temperature":400}]}`)

	sourceHead := *f.head(t, ctx, feature.ID)
	targetHead := *f.head(t, ctx, f.main)
	r := f.computeConflicts(t, ctx, base, sourceHead, targetHead)

	if r.AutoMergeable {
		t.Fatalf("report auto_mergeable = true, verdicts = %+v", r.ObjectVerdicts)
	}
	var verdict conflict.ObjectVerdict
	for _, v := range r.ObjectVerdicts {
		if v.ObjectID == pObject {
			verdict = v
		}
	}
	if verdict.ObjectID == "" {
		t.Fatalf("no verdict for protocol %s in %+v", pObject, r.ObjectVerdicts)
	}
	if verdict.AutoMergeable {
		t.Fatalf("protocol verdict auto_mergeable = true, conflicts = %+v", verdict.Conflicts)
	}
	if len(verdict.Conflicts) != 1 || verdict.Conflicts[0].Category != conflict.CategoryScientific {
		t.Fatalf("conflicts = %+v, want exactly one scientific conflict", verdict.Conflicts)
	}
	if verdict.Conflicts[0].Code != conflict.CodeScientificFieldDiverges {
		t.Fatalf("code = %q, want %q", verdict.Conflicts[0].Code, conflict.CodeScientificFieldDiverges)
	}
	if len(verdict.Conflicts[0].PayloadKeys) != 1 || verdict.Conflicts[0].PayloadKeys[0] != "steps" {
		t.Fatalf("payload_keys = %v, want [steps]", verdict.Conflicts[0].PayloadKeys)
	}
}

// TestAppendEvidenceNotTextConflict is the second acceptance criterion
// over the real stack: both branches appending a different claim version
// ref to the same finding is append-only (docs/09 §7: append-only
// evidence is auto-allowed) — the verdict stays auto_mergeable, never a
// text conflict.
func TestAppendEvidenceNotTextConflict(t *testing.T) {
	ctx := testCtx(t)
	f := newDiffFixture(t, ctx)

	// main: claim C and finding F referencing C's first version.
	_, cV1 := f.createObject(t, ctx, f.main, "claim", `{"statement":"alpha"}`)
	fObject, _ := f.createObject(t, ctx, f.main, "finding",
		`{"statement":"found","claim_version_refs":["`+cV1+`"]}`)

	feature, err := f.svc.CreateBranch(ctx, f.alice, f.project.ID, rsg.CreateBranchInput{
		Name:       "feature",
		BaseRef:    *f.head(t, ctx, f.main),
		Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create feature branch: %v", err)
	}
	base := *feature.BaseStateID

	// feature appends a new claim version ref; main appends a different
	// one. Both are pure appends of the same base list. The appended refs
	// are well-formed version ids: the T0503 semantics check requires
	// every pinned ref to be a canonical uuid (existence is not checked
	// on this path).
	f.updateObject(t, ctx, feature.ID, fObject, 1,
		`{"claim_version_refs":["`+cV1+`","aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"]}`)
	f.updateObject(t, ctx, f.main, fObject, 2,
		`{"claim_version_refs":["`+cV1+`","bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"]}`)

	sourceHead := *f.head(t, ctx, feature.ID)
	targetHead := *f.head(t, ctx, f.main)
	r := f.computeConflicts(t, ctx, base, sourceHead, targetHead)

	for _, v := range r.ObjectVerdicts {
		if v.ObjectID != fObject {
			continue
		}
		if !v.AutoMergeable {
			t.Fatalf("finding verdict auto_mergeable = false, conflicts = %+v", v.Conflicts)
		}
		if len(v.Conflicts) != 0 {
			t.Fatalf("finding conflicts = %+v, want none (append-only evidence)", v.Conflicts)
		}
	}
	if !r.AutoMergeable {
		t.Fatalf("report auto_mergeable = false, verdicts = %+v", r.ObjectVerdicts)
	}
}

// TestNullBaseEvidenceRefsDivergesIsConflict is the real-stack regression
// for the review finding: the base claim carries evidence_refs:null and
// the two branches replace it with different arrays. null is not an empty
// anchor list, so this is the same field changed to different values —
// a knowledge conflict a human must resolve, never auto-merged.
func TestNullBaseEvidenceRefsDivergesIsConflict(t *testing.T) {
	ctx := testCtx(t)
	f := newDiffFixture(t, ctx)

	// main: claim C with a null evidence refs anchor.
	cObject, _ := f.createObject(t, ctx, f.main, "claim",
		`{"statement":"s","evidence_refs":null}`)

	feature, err := f.svc.CreateBranch(ctx, f.alice, f.project.ID, rsg.CreateBranchInput{
		Name:       "feature",
		BaseRef:    *f.head(t, ctx, f.main),
		Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create feature branch: %v", err)
	}
	base := *feature.BaseStateID

	// feature and main each replace the null anchor with a different list.
	f.updateObject(t, ctx, feature.ID, cObject, 1,
		`{"statement":"s","evidence_refs":["e2"]}`)
	f.updateObject(t, ctx, f.main, cObject, 2,
		`{"statement":"s","evidence_refs":["e3"]}`)

	sourceHead := *f.head(t, ctx, feature.ID)
	targetHead := *f.head(t, ctx, f.main)
	r := f.computeConflicts(t, ctx, base, sourceHead, targetHead)

	if r.AutoMergeable {
		t.Fatalf("report auto_mergeable = true, verdicts = %+v", r.ObjectVerdicts)
	}
	var verdict conflict.ObjectVerdict
	for _, v := range r.ObjectVerdicts {
		if v.ObjectID == cObject {
			verdict = v
		}
	}
	if verdict.ObjectID == "" {
		t.Fatalf("no verdict for claim %s in %+v", cObject, r.ObjectVerdicts)
	}
	if verdict.AutoMergeable {
		t.Fatalf("claim verdict auto_mergeable = true, conflicts = %+v", verdict.Conflicts)
	}
	if len(verdict.Conflicts) != 1 || verdict.Conflicts[0].Category != conflict.CategoryKnowledge {
		t.Fatalf("conflicts = %+v, want exactly one knowledge conflict", verdict.Conflicts)
	}
	if len(verdict.Conflicts[0].PayloadKeys) != 1 || verdict.Conflicts[0].PayloadKeys[0] != "evidence_refs" {
		t.Fatalf("payload_keys = %v, want [evidence_refs]", verdict.Conflicts[0].PayloadKeys)
	}
}
