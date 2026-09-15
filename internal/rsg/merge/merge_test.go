package merge

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/conflict"
	"github.com/lichman0405/post/internal/rsg/diff"
	"github.com/lichman0405/post/internal/rsg/manifest"
)

// fixtureClock is the fixed timestamp every fixture row carries, so the
// canonical bytes never depend on the wall clock.
var fixtureClock = time.Date(2026, 2, 10, 9, 30, 0, 0, time.UTC)

func objRow(id, objectID, objectType string, versionNo int, stateID, lifecycle, title, payload string, policy *string) manifest.ObjectVersion {
	return manifest.ObjectVersion{
		ID:                 id,
		ObjectID:           objectID,
		ObjectType:         objectType,
		VersionNo:          versionNo,
		StateID:            stateID,
		SchemaRef:          manifest.SchemaRef{ID: "https://open-rd.example/schemas/claim.schema.json", Version: "1"},
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

// fixture builds a three-way input the way the persistence layer hands one
// to the engine: the three lineages' stored rows.
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

func (f *fixture) sourceRel(rows ...manifest.RelationVersion) *fixture {
	f.in.SourceSnapshot.RelationVersions = append(f.in.SourceSnapshot.RelationVersions, rows...)
	return f
}

// merge plans with the given decisions and both branches public (the
// visibility variants are set by visibility()).
func (f *fixture) merge(t *testing.T, decisions ...Decision) *Plan {
	t.Helper()
	return f.mergeAs(t, domain.BranchVisibilityPublic, domain.BranchVisibilityPublic, decisions...)
}

func (f *fixture) mergeAs(t *testing.T, sourceVis, targetVis domain.BranchVisibility, decisions ...Decision) *Plan {
	t.Helper()
	in := Inputs{
		ProjectID:        f.in.ProjectID,
		Diff:             f.in,
		Decisions:        decisions,
		SourceVisibility: sourceVis,
		TargetVisibility: targetVis,
	}
	p, err := Merge(in)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	return p
}

// change returns the planned change for one target id, or fails.
func change(t *testing.T, p *Plan, id string) Change {
	t.Helper()
	for _, c := range p.Changes {
		if c.TargetID == id {
			return c
		}
	}
	t.Fatalf("no planned change for %s in %+v", id, p.Changes)
	return Change{}
}

// decide builds the decision a human would record for one reported
// conflict of the fixture — the same transcription the resolution store
// does (T0407): the conflict's own classifier key, plus the chosen kind.
func decide(t *testing.T, p *Plan, targetKind domain.ConflictResolutionTargetKind, id, code string, kind domain.ResolutionKind) Decision {
	t.Helper()
	var conflicts []conflict.Conflict
	switch targetKind {
	case domain.ConflictResolutionTargetObject:
		for _, v := range p.Report.ObjectVerdicts {
			if v.ObjectID == id {
				conflicts = v.Conflicts
			}
		}
	case domain.ConflictResolutionTargetRelation:
		for _, v := range p.Report.RelationVerdicts {
			if v.RelationID == id {
				conflicts = v.Conflicts
			}
		}
	}
	for _, c := range conflicts {
		if c.Code != code {
			continue
		}
		return Decision{
			TargetKind:    targetKind,
			TargetID:      id,
			Code:          c.Code,
			Fields:        append([]string{}, c.Fields...),
			PayloadKeys:   append([]string{}, c.PayloadKeys...),
			OtherObjectID: c.OtherObjectID,
			Kind:          kind,
			DecidedBy:     "user-00000002",
			Note:          "decided for the merge test",
		}
	}
	t.Fatalf("no conflict %s on %s %s in the report", code, targetKind, id)
	return Decision{}
}

// blocked reports whether the plan carries the given blocker code.
func blocked(p *Plan, code string) bool {
	for _, b := range p.Blockers {
		if b.Code == code {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------
// docs/09 §7: the auto-merge boundary.

// TestAutoMergeDifferentObjectsNeedsNoDecision: the two branches changed
// different objects, so the source-side change lands with no human
// decision and the plan is executable.
func TestAutoMergeDifferentObjectsNeedsNoDecision(t *testing.T) {
	// One object created on the source side, one created on the target
	// side: different objects, nothing to decide.
	src := objRow("ov-00000001", "obj-src-0001", "claim", 1, "state-src-000001", "active",
		"Source claim", `{"statement":"s"}`, nil)
	tgt := objRow("ov-00000002", "obj-tgt-0001", "claim", 1, "state-tgt-000001", "active",
		"Target claim", `{"statement":"t"}`, nil)
	p := newFixture().source(src).target(tgt).merge(t)

	if !p.Executable {
		t.Fatalf("plan not executable: blockers %+v", p.Blockers)
	}
	c := change(t, p, "obj-src-0001")
	if c.Resolution != ResolutionAuto || c.Effect != EffectApply || !c.Materialize {
		t.Fatalf("change = %+v, want an auto-applied source change", c)
	}
	if len(p.Carried) != 0 {
		t.Fatalf("carried = %+v, want none", p.Carried)
	}
	if p.Summary.Applied != 1 {
		t.Fatalf("summary = %+v, want one applied change", p.Summary)
	}
}

// TestAutoMergeSameObjectDisjointFields: the same object changed on both
// branches in different, non-conflicting fields still auto-merges (docs/09
// §7's second auto case), so the plan needs no decision.
func TestAutoMergeSameObjectDisjointFields(t *testing.T) {
	base := objRow("ov-00000001", "obj-c-0000001", "claim", 1, "state-base-0001", "active",
		"C1", `{"statement":"s","confidence":"low"}`, nil)
	src := objRow("ov-00000002", "obj-c-0000001", "claim", 2, "state-src-000001", "active",
		"C1", `{"statement":"s2","confidence":"low"}`, nil)
	tgt := objRow("ov-00000003", "obj-c-0000001", "claim", 2, "state-tgt-000001", "active",
		"C1", `{"statement":"s","confidence":"high"}`, nil)
	p := newFixture().base(base).source(base, src).target(base, tgt).merge(t)

	c := change(t, p, "obj-c-0000001")
	if c.Resolution != ResolutionAuto || !c.Materialize {
		t.Fatalf("change = %+v, want auto-merge (disjoint payload keys)", c)
	}
	if !p.Executable {
		t.Fatalf("plan not executable: %+v", p.Blockers)
	}
}

// TestScientificConflictUndecidedBlocks: a protocol's content diverged on
// the two branches, so nothing lands until a human decides — and the
// blocker is the engine's refusal, not an inferred default.
func TestScientificConflictUndecidedBlocks(t *testing.T) {
	base := objRow("pv-00000001", "obj-p-0000001", "protocol", 1, "state-base-0001", "active",
		"P1", `{"purpose":"p","steps":[{"id":"s1","temperature":300}]}`, nil)
	src := objRow("pv-00000002", "obj-p-0000001", "protocol", 2, "state-src-000001", "active",
		"P1", `{"purpose":"p","steps":[{"id":"s1","temperature":350}]}`, nil)
	tgt := objRow("pv-00000003", "obj-p-0000001", "protocol", 2, "state-tgt-000001", "active",
		"P1", `{"purpose":"p","steps":[{"id":"s1","temperature":400}]}`, nil)
	p := newFixture().base(base).source(base, src).target(base, tgt).merge(t)

	c := change(t, p, "obj-p-0000001")
	if c.Materialize {
		t.Fatalf("change = %+v, want nothing materialized without a decision", c)
	}
	if c.Effect != EffectBlocked || c.Block != CodeConflictUndecided {
		t.Fatalf("change = %+v, want blocked by %s", c, CodeConflictUndecided)
	}
	if p.Executable {
		t.Fatalf("plan executable with an undecided scientific conflict")
	}
	if !blocked(p, CodeConflictUndecided) {
		t.Fatalf("blockers = %+v, want %s", p.Blockers, CodeConflictUndecided)
	}
}

// TestScientificConflictNeverResolvedToWinner is the acceptance criterion
// the task names: whatever the human decides, the plan never turns a
// scientific conflict into a silently chosen winner. Accept A applies the
// source content only because a human said so; Accept B applies nothing;
// keep_both / unresolved write no content at all and name both versions.
func TestScientificConflictNeverResolvedToWinner(t *testing.T) {
	build := func() *fixture {
		base := objRow("pv-00000001", "obj-p-0000001", "protocol", 1, "state-base-0001", "active",
			"P1", `{"purpose":"p","steps":[{"id":"s1","temperature":300}]}`, nil)
		src := objRow("pv-00000002", "obj-p-0000001", "protocol", 2, "state-src-000001", "active",
			"P1", `{"purpose":"p","steps":[{"id":"s1","temperature":350}]}`, nil)
		tgt := objRow("pv-00000003", "obj-p-0000001", "protocol", 2, "state-tgt-000001", "active",
			"P1", `{"purpose":"p","steps":[{"id":"s1","temperature":400}]}`, nil)
		return newFixture().base(base).source(base, src).target(base, tgt)
	}
	const code = "SCIENTIFIC_FIELD_DIVERGES"

	// No decision at all: blocked, and nothing written.
	if c := change(t, build().merge(t), "obj-p-0000001"); c.Materialize {
		t.Fatalf("undecided scientific conflict materialized: %+v", c)
	}

	// Accept B: the source content does not land.
	b := build().merge(t)
	bp := build().mergeAs(t, domain.BranchVisibilityPublic, domain.BranchVisibilityPublic,
		decide(t, b, domain.ConflictResolutionTargetObject, "obj-p-0000001", code, domain.ResolutionAcceptTarget))
	bc := change(t, bp, "obj-p-0000001")
	if bc.Materialize || bc.Effect != EffectKeepTarget {
		t.Fatalf("Accept B change = %+v, want keep_target with no write", bc)
	}

	// Accept A: only the explicit human decision lands the source content,
	// verbatim (never a computed mixture of the two values).
	a := build().merge(t)
	ap := build().mergeAs(t, domain.BranchVisibilityPublic, domain.BranchVisibilityPublic,
		decide(t, a, domain.ConflictResolutionTargetObject, "obj-p-0000001", code, domain.ResolutionAcceptSource))
	ac := change(t, ap, "obj-p-0000001")
	if ac.Effect != EffectApply || !ac.Materialize || ac.Resolution != Resolution(domain.ResolutionAcceptSource) {
		t.Fatalf("Accept A change = %+v, want the source content applied", ac)
	}
	if ac.TargetVersionID != "pv-00000003" || ac.SourceVersionID != "pv-00000002" {
		t.Fatalf("Accept A change versions = %q/%q, want the two side versions", ac.SourceVersionID, ac.TargetVersionID)
	}

	// Keep both / unresolved: no content is written and both versions are
	// named in the carried record — the conflict stays contested instead
	// of being collapsed into a winner.
	for _, kind := range []domain.ResolutionKind{domain.ResolutionKeepBoth, domain.ResolutionExplicitCoexistence, domain.ResolutionUnresolved} {
		f := build()
		plan := f.mergeAs(t, domain.BranchVisibilityPublic, domain.BranchVisibilityPublic,
			decide(t, f.merge(t), domain.ConflictResolutionTargetObject, "obj-p-0000001", code, kind))
		c := change(t, plan, "obj-p-0000001")
		if c.Materialize {
			t.Fatalf("%s materialized content: %+v", kind, c)
		}
		if c.Effect != EffectCarryBoth {
			t.Fatalf("%s effect = %q, want carry_both", kind, c.Effect)
		}
		if len(plan.Carried) != 1 {
			t.Fatalf("%s carried = %+v, want exactly one carried conflict", kind, plan.Carried)
		}
		carried := plan.Carried[0]
		if carried.SourceVersionID != "pv-00000002" || carried.TargetVersionID != "pv-00000003" {
			t.Fatalf("%s carried versions = %q/%q, want both sides named",
				kind, carried.SourceVersionID, carried.TargetVersionID)
		}
		if carried.Decision != kind || carried.DecidedBy != "user-00000002" {
			t.Fatalf("%s carried record = %+v, want the human decision recorded", kind, carried)
		}
		if plan.Executable {
			t.Fatalf("%s: a plan that writes nothing must not be executable", kind)
		}
		if !blocked(plan, CodeNothingToMerge) {
			t.Fatalf("%s blockers = %+v, want %s", kind, plan.Blockers, CodeNothingToMerge)
		}
	}
}

// TestEveryDocs09ActionIsExpressible walks the seven docs/09 §8 actions
// (plus the contested state) and pins each one's structural effect: all of
// them must be expressible through the merge, and only the two that defer
// or abandon stop the merge.
func TestEveryDocs09ActionIsExpressible(t *testing.T) {
	want := []struct {
		kind        domain.ResolutionKind
		action      string
		effect      Effect
		materialize bool
		blocker     string
	}{
		{domain.ResolutionAcceptSource, "Accept A", EffectApply, true, ""},
		{domain.ResolutionAcceptTarget, "Accept B", EffectKeepTarget, false, ""},
		{domain.ResolutionKeepBoth, "Keep both versions", EffectCarryBoth, false, ""},
		{domain.ResolutionExplicitCoexistence, "Explicit coexistence", EffectCarryBoth, false, ""},
		{domain.ResolutionValidationBranch, "Create validation branch", EffectBlocked, false, CodeValidationBranch},
		{domain.ResolutionRequestEvidence, "Request more evidence", EffectBlocked, false, CodeMoreEvidence},
		{domain.ResolutionAbortChange, "Abort proposed change", EffectAbortProposed, false, ""},
		{domain.ResolutionUnresolved, "contested/unresolved", EffectCarryBoth, false, ""},
	}
	if len(want) != 8 {
		t.Fatalf("docs/09 §8 has seven actions plus contested/unresolved; this table has %d", len(want))
	}
	for _, tc := range want {
		t.Run(tc.action, func(t *testing.T) {
			f := scientificFixture()
			probe := f.merge(t)
			d := decide(t, probe, domain.ConflictResolutionTargetObject, "obj-p-0000001", "SCIENTIFIC_FIELD_DIVERGES", tc.kind)
			// Give the merge something else to land, so a deferring
			// action's blocker is visible instead of MERGE_NOTHING_TO_MERGE.
			f.source(objRow("cv-00000009", "obj-c-0000009", "claim", 1, "state-src-000001", "active",
				"Other claim", `{"statement":"other"}`, nil))
			p := f.merge(t, d)

			c := change(t, p, "obj-p-0000001")
			if c.Effect != tc.effect {
				t.Fatalf("%s: effect = %q, want %q", tc.action, c.Effect, tc.effect)
			}
			if c.Materialize != tc.materialize {
				t.Fatalf("%s: materialize = %v, want %v", tc.action, c.Materialize, tc.materialize)
			}
			if tc.blocker != "" && !blocked(p, tc.blocker) {
				t.Fatalf("%s: blockers = %+v, want %s", tc.action, p.Blockers, tc.blocker)
			}
			// Whatever the action, the OTHER change (an ordinary claim
			// created on the source side) must still land: a decision
			// about one conflict never changes the treatment of another.
			if oc := change(t, p, "obj-c-0000009"); !oc.Materialize {
				t.Fatalf("%s: the unrelated change did not land: %+v", tc.action, oc)
			}
			if tc.blocker == "" && !p.Executable {
				t.Fatalf("%s: executable = false with 1 applied change; blockers %+v", tc.action, p.Blockers)
			}
		})
	}
}

// scientificFixture is a protocol whose content diverged on both branches:
// one scientific conflict, nothing auto-mergeable.
func scientificFixture() *fixture {
	base := objRow("pv-00000001", "obj-p-0000001", "protocol", 1, "state-base-0001", "active",
		"P1", `{"purpose":"p","steps":[{"id":"s1","temperature":300}]}`, nil)
	src := objRow("pv-00000002", "obj-p-0000001", "protocol", 2, "state-src-000001", "active",
		"P1", `{"purpose":"p","steps":[{"id":"s1","temperature":350}]}`, nil)
	tgt := objRow("pv-00000003", "obj-p-0000001", "protocol", 2, "state-tgt-000001", "active",
		"P1", `{"purpose":"p","steps":[{"id":"s1","temperature":400}]}`, nil)
	return newFixture().base(base).source(base, src).target(base, tgt)
}

// TestAbortChangeDoesNotLandAndIsNotCarried: the abort action drops the
// proposal — it is neither written nor carried as an open conflict.
func TestAbortChangeDoesNotLandAndIsNotCarried(t *testing.T) {
	f := scientificFixture()
	probe := f.merge(t)
	d := decide(t, probe, domain.ConflictResolutionTargetObject, "obj-p-0000001", "SCIENTIFIC_FIELD_DIVERGES", domain.ResolutionAbortChange)
	f.source(objRow("cv-00000009", "obj-c-0000009", "claim", 1, "state-src-000001", "active",
		"Other claim", `{"statement":"other"}`, nil))
	p := f.merge(t, d)

	c := change(t, p, "obj-p-0000001")
	if c.Effect != EffectAbortProposed || c.Materialize {
		t.Fatalf("aborted change = %+v, want abort_proposed with no write", c)
	}
	if len(p.Carried) != 0 {
		t.Fatalf("carried = %+v, want an aborted change not to be carried", p.Carried)
	}
	if p.Summary.Aborted != 1 {
		t.Fatalf("summary = %+v, want one aborted change", p.Summary)
	}
	if !p.Executable {
		t.Fatalf("plan not executable: %+v", p.Blockers)
	}
}

// ---------------------------------------------------------------------
// Decision hygiene.

// TestDecisionNotInReportRefused: a decision about a conflict this triple
// does not have is refused, never ignored.
func TestDecisionNotInReportRefused(t *testing.T) {
	f := scientificFixture()
	in := Inputs{
		ProjectID: f.in.ProjectID,
		Diff:      f.in,
		Decisions: []Decision{{
			TargetKind: domain.ConflictResolutionTargetObject,
			TargetID:   "obj-p-0000001",
			Code:       "SCIENTIFIC_FIELD_DIVERGES",
			Fields:     []string{"payload"},
			PayloadKeys: []string{
				"purpose", // the conflict is about "steps"
			},
			Kind:      domain.ResolutionAcceptSource,
			DecidedBy: "user-00000002",
		}},
	}
	if _, err := Merge(in); !errors.Is(err, ErrDecisionNotInReport) {
		t.Fatalf("Merge error = %v, want ErrDecisionNotInReport", err)
	}
}

// TestUnusableDecisionsRefused: every spelling the docs/09 §8 vocabulary
// does not name — an averaged value above all — and any decision without a
// human decider is refused.
func TestUnusableDecisionsRefused(t *testing.T) {
	cases := map[string]Decision{
		"averaged kind": {
			TargetKind: domain.ConflictResolutionTargetObject, TargetID: "obj-p-0000001",
			Code: "SCIENTIFIC_FIELD_DIVERGES", Fields: []string{"payload"}, PayloadKeys: []string{"steps"},
			Kind: domain.ResolutionKind("average"), DecidedBy: "user-00000002",
		},
		"no decider": {
			TargetKind: domain.ConflictResolutionTargetObject, TargetID: "obj-p-0000001",
			Code: "SCIENTIFIC_FIELD_DIVERGES", Fields: []string{"payload"}, PayloadKeys: []string{"steps"},
			Kind: domain.ResolutionAcceptSource,
		},
		"no target": {
			TargetKind: domain.ConflictResolutionTargetObject,
			Code:       "SCIENTIFIC_FIELD_DIVERGES", Fields: []string{"payload"}, PayloadKeys: []string{"steps"},
			Kind: domain.ResolutionAcceptSource, DecidedBy: "user-00000002",
		},
	}
	for name, d := range cases {
		t.Run(name, func(t *testing.T) {
			f := scientificFixture()
			in := Inputs{ProjectID: f.in.ProjectID, Diff: f.in, Decisions: []Decision{d}}
			if _, err := Merge(in); !errors.Is(err, ErrInvalidDecision) {
				t.Fatalf("Merge error = %v, want ErrInvalidDecision", err)
			}
		})
	}
}

// TestDuplicateDecisionRefused: two decisions for one conflict are
// ambiguous, so they are refused rather than resolved by order.
func TestDuplicateDecisionRefused(t *testing.T) {
	f := scientificFixture()
	probe := f.merge(t)
	d := decide(t, probe, domain.ConflictResolutionTargetObject, "obj-p-0000001", "SCIENTIFIC_FIELD_DIVERGES", domain.ResolutionAcceptSource)
	d2 := d
	d2.Kind = domain.ResolutionAcceptTarget
	in := Inputs{ProjectID: f.in.ProjectID, Diff: f.in, Decisions: []Decision{d, d2}}
	if _, err := Merge(in); !errors.Is(err, ErrDuplicateDecision) {
		t.Fatalf("Merge error = %v, want ErrDuplicateDecision", err)
	}
}

// ---------------------------------------------------------------------
// Publication (docs/09 §9).

// TestPrivateSourceIntoPublicTargetIsWithheld: the merge must not publish
// private state, so the change is withheld and named for the Publication
// Gate instead of landing in the public target.
func TestPrivateSourceIntoPublicTargetIsWithheld(t *testing.T) {
	src := objRow("ov-00000001", "obj-src-0001", "claim", 1, "state-src-000001", "active",
		"Source claim", `{"statement":"s"}`, nil)
	f := newFixture().source(src)
	p := f.mergeAs(t, domain.BranchVisibilityPrivate, domain.BranchVisibilityPublic)

	c := change(t, p, "obj-src-0001")
	if c.Materialize {
		t.Fatalf("private content materialized into a public target: %+v", c)
	}
	if !c.Withheld || c.WithholdReason != ReasonPublicationGate {
		t.Fatalf("change = %+v, want withheld for %q", c, ReasonPublicationGate)
	}
	if len(p.Withheld) != 1 || p.Withheld[0].TargetID != "obj-src-0001" {
		t.Fatalf("withheld = %+v, want the change named for the gate", p.Withheld)
	}
	if p.Executable {
		t.Fatalf("plan executable although nothing would land")
	}
	if !blocked(p, CodeNothingToMerge) {
		t.Fatalf("blockers = %+v, want %s", p.Blockers, CodeNothingToMerge)
	}
	if p.Summary.Applied != 0 || p.Summary.Withheld != 1 {
		t.Fatalf("summary = %+v, want one withheld and none applied", p.Summary)
	}
}

// TestSameVisibilityMergesNormally: only the private→public crossing is
// withheld — public→public and private→private both land.
func TestSameVisibilityMergesNormally(t *testing.T) {
	cases := map[string][2]domain.BranchVisibility{
		"public to public":   {domain.BranchVisibilityPublic, domain.BranchVisibilityPublic},
		"private to private": {domain.BranchVisibilityPrivate, domain.BranchVisibilityPrivate},
		"public to private":  {domain.BranchVisibilityPublic, domain.BranchVisibilityPrivate},
	}
	for name, vis := range cases {
		t.Run(name, func(t *testing.T) {
			src := objRow("ov-00000001", "obj-src-0001", "claim", 1, "state-src-000001", "active",
				"Source claim", `{"statement":"s"}`, nil)
			p := newFixture().source(src).mergeAs(t, vis[0], vis[1])
			c := change(t, p, "obj-src-0001")
			if !c.Materialize || c.Withheld {
				t.Fatalf("change = %+v, want it to land", c)
			}
			if !p.Executable {
				t.Fatalf("plan not executable: %+v", p.Blockers)
			}
		})
	}
}

// ---------------------------------------------------------------------
// Relations.

// TestRelationEndpointRepinnedOntoWrittenVersion: a relation whose source
// endpoint is an object version this merge writes is re-pinned to that
// object, so the merged state never points at a version it does not
// contain.
func TestRelationEndpointRepinnedOntoWrittenVersion(t *testing.T) {
	srcObj := objRow("ov-00000001", "obj-src-0001", "claim", 1, "state-src-000001", "active",
		"Source claim", `{"statement":"s"}`, nil)
	tgtObj := objRow("ov-00000002", "obj-tgt-0001", "claim", 1, "state-tgt-000001", "active",
		"Target claim", `{"statement":"t"}`, nil)
	srcRel := relRow("rv-00000001", "rel-src-0001", 1, "state-src-000001", "supports", "ov-00000001", "ov-00000002", `{}`)
	f := newFixture().source(srcObj).target(tgtObj).sourceRel(srcRel)
	p := f.merge(t)

	c := change(t, p, "rel-src-0001")
	if !c.Materialize {
		t.Fatalf("relation change = %+v, want it materialized", c)
	}
	if c.EndpointRewrites["ov-00000001"] != "obj-src-0001" {
		t.Fatalf("endpoint rewrites = %+v, want ov-00000001 -> obj-src-0001", c.EndpointRewrites)
	}
	if _, ok := c.EndpointRewrites["ov-00000002"]; ok {
		t.Fatalf("endpoint rewrites = %+v, want the target-lineage endpoint left alone", c.EndpointRewrites)
	}
}

// TestRelationWithHeldEndpointIsWithheld: when the endpoint's change does
// not land, the edge would dangle, so it is withheld rather than written.
func TestRelationWithHeldEndpointIsWithheld(t *testing.T) {
	base := objRow("pv-00000001", "obj-p-0000001", "protocol", 1, "state-base-0001", "active",
		"P1", `{"purpose":"p","steps":[{"id":"s1","temperature":300}]}`, nil)
	src := objRow("pv-00000002", "obj-p-0000001", "protocol", 2, "state-src-000001", "active",
		"P1", `{"purpose":"p","steps":[{"id":"s1","temperature":350}]}`, nil)
	tgt := objRow("pv-00000003", "obj-p-0000001", "protocol", 2, "state-tgt-000001", "active",
		"P1", `{"purpose":"p","steps":[{"id":"s1","temperature":400}]}`, nil)
	holder := objRow("cv-00000001", "obj-c-0000001", "claim", 1, "state-base-0001", "active",
		"C1", `{"statement":"c"}`, nil)
	// The relation depends on the new protocol version, which Accept B
	// leaves out of the accepted state.
	rel := relRow("rv-00000001", "rel-c-0001", 1, "state-src-000001", "supports", "cv-00000001", "pv-00000002", `{}`)
	f := newFixture().base(base, holder).source(base, src, holder).target(base, tgt, holder).sourceRel(rel)
	probe := newFixture().base(base, holder).source(base, src, holder).target(base, tgt, holder).sourceRel(rel).merge(t)
	d := decide(t, probe, domain.ConflictResolutionTargetObject, "obj-p-0000001", "SCIENTIFIC_FIELD_DIVERGES", domain.ResolutionAcceptTarget)

	p := f.mergeAs(t, domain.BranchVisibilityPublic, domain.BranchVisibilityPublic, d)
	c := change(t, p, "rel-c-0001")
	if c.Materialize {
		t.Fatalf("relation change = %+v, want no edge to a version the state lacks", c)
	}
	if c.WithholdReason != ReasonFoundationHeld {
		t.Fatalf("relation withhold reason = %q, want %q", c.WithholdReason, ReasonFoundationHeld)
	}
}

// TestRelationEndpointHeldByPublicationReportsTheGate: when the endpoint's
// change is withheld for the Publication Gate, the edge reports the same
// reason, so the gate sees the whole private cluster.
func TestRelationEndpointHeldByPublicationReportsTheGate(t *testing.T) {
	priv := objRow("ov-00000001", "obj-src-0001", "claim", 1, "state-src-000001", "active",
		"Private claim", `{"statement":"s"}`, nil)
	pub := objRow("ov-00000002", "obj-tgt-0001", "claim", 1, "state-tgt-000001", "active",
		"Public claim", `{"statement":"t"}`, nil)
	rel := relRow("rv-00000001", "rel-src-0001", 1, "state-src-000001", "supports", "ov-00000001", "ov-00000002", `{}`)
	p := newFixture().source(priv).target(pub).sourceRel(rel).
		mergeAs(t, domain.BranchVisibilityPrivate, domain.BranchVisibilityPublic)

	c := change(t, p, "rel-src-0001")
	if c.Materialize {
		t.Fatalf("relation change = %+v, want it withheld", c)
	}
	if c.WithholdReason != ReasonPublicationGate {
		t.Fatalf("relation withhold reason = %q, want %q", c.WithholdReason, ReasonPublicationGate)
	}
	for _, w := range p.Withheld {
		if w.Reason != ReasonPublicationGate {
			t.Fatalf("withheld = %+v, want every entry to name the gate", p.Withheld)
		}
	}
}

// ---------------------------------------------------------------------
// Determinism and shape.

// TestPlanIsDeterministic: the same inputs produce the same bytes, and the
// plan does not depend on the order the decisions were submitted in.
func TestPlanIsDeterministic(t *testing.T) {
	build := func(decisions ...Decision) (*Plan, []byte) {
		base := objRow("pv-00000001", "obj-p-0000001", "protocol", 1, "state-base-0001", "active",
			"P1", `{"purpose":"p","steps":[{"id":"s1","temperature":300}]}`, nil)
		src := objRow("pv-00000002", "obj-p-0000001", "protocol", 2, "state-src-000001", "active",
			"P1", `{"purpose":"p","steps":[{"id":"s1","temperature":350}]}`, nil)
		tgt := objRow("pv-00000003", "obj-p-0000001", "protocol", 2, "state-tgt-000001", "active",
			"P1", `{"purpose":"p","steps":[{"id":"s1","temperature":400}]}`, nil)
		other := objRow("cv-00000001", "obj-c-0000001", "claim", 1, "state-base-0001", "active",
			"C1", `{"statement":"c"}`, nil)
		otherSrc := objRow("cv-00000002", "obj-c-0000001", "claim", 2, "state-src-000001", "active",
			"C1", `{"statement":"c2"}`, nil)
		// A SECOND object both sides moved differently: with only one
		// conflicted object there is nothing to swap the decisions over,
		// and an order-independence check that feeds one decision twice
		// passes for any implementation, including one that reads the
		// decision list as an ordered log.
		qBase := objRow("qv-00000001", "obj-q-0000001", "protocol", 1, "state-base-0001", "active",
			"Q1", `{"purpose":"q","steps":[{"id":"s1","temperature":300}]}`, nil)
		qSrc := objRow("qv-00000002", "obj-q-0000001", "protocol", 2, "state-src-000001", "active",
			"Q1", `{"purpose":"q","steps":[{"id":"s1","temperature":350}]}`, nil)
		qTgt := objRow("qv-00000003", "obj-q-0000001", "protocol", 2, "state-tgt-000001", "active",
			"Q1", `{"purpose":"q","steps":[{"id":"s1","temperature":400}]}`, nil)
		f := newFixture().base(base, other, qBase).
			source(base, src, other, otherSrc, qSrc).
			target(base, tgt, other, qTgt)
		p := f.merge(t, decisions...)
		b, err := p.CanonicalJSON()
		if err != nil {
			t.Fatalf("CanonicalJSON: %v", err)
		}
		return p, b
	}
	_, first := build()
	if _, again := build(); !bytes.Equal(first, again) {
		t.Fatalf("two runs over the same inputs differ")
	}
	// The decisions are looked up by classifier key, so their order cannot
	// matter: build the same plan twice with the two decisions swapped.
	// The two decisions address two DIFFERENT targets, so the swap is a
	// real one — and the plan each build produces is checked to have
	// actually applied them, so the byte comparison is not two identical
	// refusals.
	probeP := scientificFixture().merge(t)
	d1 := decide(t, probeP, domain.ConflictResolutionTargetObject, "obj-p-0000001", "SCIENTIFIC_FIELD_DIVERGES", domain.ResolutionKeepBoth)
	qProbe := newFixture().
		base(objRow("qv-00000001", "obj-q-0000001", "protocol", 1, "state-base-0001", "active",
			"Q1", `{"purpose":"q","steps":[{"id":"s1","temperature":300}]}`, nil)).
		source(objRow("qv-00000002", "obj-q-0000001", "protocol", 2, "state-src-000001", "active",
			"Q1", `{"purpose":"q","steps":[{"id":"s1","temperature":350}]}`, nil)).
		target(objRow("qv-00000003", "obj-q-0000001", "protocol", 2, "state-tgt-000001", "active",
			"Q1", `{"purpose":"q","steps":[{"id":"s1","temperature":400}]}`, nil)).
		merge(t)
	d2 := decide(t, qProbe, domain.ConflictResolutionTargetObject, "obj-q-0000001", "SCIENTIFIC_FIELD_DIVERGES", domain.ResolutionAcceptTarget)
	forward, forwardBytes := build(d1, d2)
	_, swappedBytes := build(d2, d1)
	if !bytes.Equal(forwardBytes, swappedBytes) {
		t.Fatalf("plan bytes depend on the order the decisions were submitted in:\nforward: %s\nswapped: %s",
			forwardBytes, swappedBytes)
	}
	// Prove the compared plans are the DECIDED ones: both decisions took
	// effect, one as a carried conflict and one as keep-target, and the
	// third (auto-mergeable) change still lands, so the plan is executable.
	if !forward.Executable {
		t.Fatalf("plan not executable: blockers %+v", forward.Blockers)
	}
	if got := change(t, forward, "obj-p-0000001"); got.Effect != EffectCarryBoth {
		t.Fatalf("obj-p change = %+v, want the keep-both decision to have taken effect", got)
	}
	if got := change(t, forward, "obj-q-0000001"); got.Effect != EffectKeepTarget {
		t.Fatalf("obj-q change = %+v, want the accept-target decision to have taken effect", got)
	}
	// A decision the report contains but the plan does not need is still
	// an error; the same decision set must therefore produce identical
	// bytes each time it is accepted.
	var decoded map[string]any
	if err := json.Unmarshal(first, &decoded); err != nil {
		t.Fatalf("plan is not a JSON object: %v", err)
	}
	for _, key := range []string{"format_version", "project_id", "base", "source", "target", "report", "changes", "carried_conflicts", "withheld", "blockers", "executable", "summary"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("plan is missing %q", key)
		}
	}
}

// TestEmptyPlanRefused: a source branch with nothing to merge is refused
// rather than committed as an empty state.
func TestEmptyPlanRefused(t *testing.T) {
	base := objRow("cv-00000001", "obj-c-0000001", "claim", 1, "state-base-0001", "active",
		"C1", `{"statement":"c"}`, nil)
	p := newFixture().base(base).source(base).target(base).merge(t)
	if p.Executable {
		t.Fatalf("plan executable with no changes")
	}
	if !blocked(p, CodeNothingToMerge) {
		t.Fatalf("blockers = %+v, want %s", p.Blockers, CodeNothingToMerge)
	}
	if len(p.Materialized()) != 0 {
		t.Fatalf("materialized = %+v, want none", p.Materialized())
	}
}

// TestContradictingDecisionsBlock: two conflicts on one object decided
// opposite ways cannot both be written into one version row, so the plan
// refuses instead of choosing a precedence.
func TestContradictingDecisionsBlock(t *testing.T) {
	base := objRow("cv-00000001", "obj-c-0000001", "claim", 1, "state-base-0001", "active",
		"C1", `{"statement":"c","confidence":"low"}`, nil)
	src := objRow("cv-00000002", "obj-c-0000001", "claim", 2, "state-src-000001", "active",
		"C2", `{"statement":"c2","confidence":"high"}`, nil)
	src.VisibilityPolicyID = strptr("policy-src")
	tgt := objRow("cv-00000003", "obj-c-0000001", "claim", 2, "state-tgt-000001", "active",
		"C3", `{"statement":"c3","confidence":"low"}`, nil)
	tgt.VisibilityPolicyID = strptr("policy-tgt")
	f := newFixture().base(base).source(base, src).target(base, tgt)
	probe := f.merge(t)
	if len(probe.Report.ObjectVerdicts) != 1 || len(probe.Report.ObjectVerdicts[0].Conflicts) < 2 {
		t.Fatalf("fixture does not produce several conflicts on one object: %+v", probe.Report.ObjectVerdicts)
	}
	decisions := make([]Decision, 0, len(probe.Report.ObjectVerdicts[0].Conflicts))
	for i, c := range probe.Report.ObjectVerdicts[0].Conflicts {
		kind := domain.ResolutionAcceptSource
		if i > 0 {
			kind = domain.ResolutionAcceptTarget
		}
		decisions = append(decisions, decide(t, probe, domain.ConflictResolutionTargetObject, "obj-c-0000001", c.Code, kind))
	}
	p := f.merge(t, decisions...)
	c := change(t, p, "obj-c-0000001")
	if c.Materialize {
		t.Fatalf("change = %+v, want no write from contradicting decisions", c)
	}
	if !blocked(p, CodeDecisionsDiverge) {
		t.Fatalf("blockers = %+v, want %s", p.Blockers, CodeDecisionsDiverge)
	}
}

// TestEveryBlockingDecisionIsReported: a change whose conflicts carry two
// DIFFERENT blocking decisions (one sent to a validation branch, one held
// for more evidence) stops the plan for both reasons. Reporting only the
// first would tell an operator to lift one decision when two have to be
// lifted, and every blocker entry carries the line explaining it — a code
// repeated as its own detail explains nothing.
func TestEveryBlockingDecisionIsReported(t *testing.T) {
	base := objRow("cv-00000001", "obj-c-0000001", "claim", 1, "state-base-0001", "active",
		"C1", `{"statement":"c","confidence":"low"}`, nil)
	src := objRow("cv-00000002", "obj-c-0000001", "claim", 2, "state-src-000001", "active",
		"C2", `{"statement":"c2","confidence":"high"}`, nil)
	src.VisibilityPolicyID = strptr("policy-src")
	tgt := objRow("cv-00000003", "obj-c-0000001", "claim", 2, "state-tgt-000001", "active",
		"C3", `{"statement":"c3","confidence":"low"}`, nil)
	tgt.VisibilityPolicyID = strptr("policy-tgt")
	f := newFixture().base(base).source(base, src).target(base, tgt)
	probe := f.merge(t)
	conflicts := probe.Report.ObjectVerdicts[0].Conflicts
	if len(conflicts) < 2 {
		t.Fatalf("fixture does not produce several conflicts on one object: %+v", probe.Report.ObjectVerdicts)
	}
	// Alternate the two blocking kinds over the conflicts: whichever way the
	// detector orders them, both kinds are decided on this one change.
	kinds := []domain.ResolutionKind{domain.ResolutionValidationBranch, domain.ResolutionRequestEvidence}
	decisions := make([]Decision, 0, len(conflicts))
	for i, c := range conflicts {
		decisions = append(decisions, decide(t, probe, domain.ConflictResolutionTargetObject,
			"obj-c-0000001", c.Code, kinds[i%len(kinds)]))
	}
	p := f.merge(t, decisions...)

	c := change(t, p, "obj-c-0000001")
	if c.Effect != EffectBlocked || !c.Blocked {
		t.Fatalf("change = %+v, want a blocked change", c)
	}
	got := map[string]string{}
	for _, b := range p.Blockers {
		got[b.Code] = b.Detail
	}
	for _, want := range []string{CodeValidationBranch, CodeMoreEvidence} {
		detail, ok := got[want]
		if !ok {
			t.Fatalf("blockers = %+v, want %s among them (every blocking decision is reported)", p.Blockers, want)
		}
		if detail == want || detail == "" {
			t.Fatalf("blocker %s detail = %q, want the line explaining the decision, not the code", want, detail)
		}
	}
	if len(p.Blockers) != 2 {
		t.Fatalf("blockers = %+v, want exactly the two distinct blocking decisions", p.Blockers)
	}
}
