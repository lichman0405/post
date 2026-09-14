package integrity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/manifest"
	"github.com/lichman0405/post/internal/rsg/schemareg"
)

// testRegistry loads the embedded canonical schema set the checks run
// against.
func testRegistry(t *testing.T) *schemareg.Registry {
	t.Helper()
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	return reg
}

// datasetObject builds one stored-form dataset version.
func datasetObject(id, objectID string, versionNo int, pin *string, payload string) manifest.ObjectVersion {
	return manifest.ObjectVersion{
		ID: id, ObjectID: objectID, ObjectType: "dataset", VersionNo: versionNo,
		StateID: "state-any", BranchID: strPtr("branch-src"),
		SchemaRef: manifest.SchemaRef{ID: "https://open-rd.example/schemas/dataset.schema.json", Version: "1"},
		Title:     "Measurements", LifecycleState: "active",
		Payload:            json.RawMessage(payload),
		VisibilityPolicyID: pin,
		IntegrityHash:      hashOf(payload),
		CreatedBy:          "u-alice",
		CreatedAt:          time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC),
	}
}

// materialObject builds one stored-form material version (no blob fields
// in the payload — material's required fields all come from the row).
func materialObject(id, objectID string, versionNo int, pin *string) manifest.ObjectVersion {
	payload := `{}`
	return manifest.ObjectVersion{
		ID: id, ObjectID: objectID, ObjectType: "material", VersionNo: versionNo,
		StateID: "state-any", BranchID: strPtr("branch-src"),
		SchemaRef: manifest.SchemaRef{ID: "https://open-rd.example/schemas/material.schema.json", Version: "1"},
		Title:     "Alloy", LifecycleState: "active",
		Payload:            json.RawMessage(payload),
		VisibilityPolicyID: pin,
		IntegrityHash:      hashOf(payload),
		CreatedBy:          "u-alice",
		CreatedAt:          time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC),
	}
}

func strPtr(s string) *string { return &s }

// hashOf is hashPayload without the testing.T (fixture builders are not
// tests).
func hashOf(payload string) string {
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

// happySnapshot builds the well-formed PR snapshot every corruption test
// derives from: target chain m1→m2 (rooted at the genesis boundary), PR
// base m2; source chain s1→s2 forked from m2, PR proposed s2; dataset v1
// unchanged across the lineages, material v1 new in the proposal, one
// "uses" relation, one blob ref, both pins resolved.
func happySnapshot() Snapshot {
	genesis := "state-genesis"
	m1, m2 := "state-m1", "state-m2"
	s1, s2 := "state-s1", "state-s2"
	policyID := "policy-1"
	datasetV1 := datasetObject("ov-d1", "obj-dataset", 1, &policyID,
		`{"purpose":"store measurements","blob_ids":["blob-1"]}`)
	materialV1 := materialObject("ov-m1", "obj-material", 1, &policyID)
	usesRelation := manifest.RelationVersion{
		ID: "rv-1", RelationID: "rel-1", VersionNo: 1, StateID: s2,
		RelationType:          "uses",
		SourceObjectVersionID: datasetV1.ID, TargetObjectVersionID: materialV1.ID,
		Payload:       json.RawMessage(`{}`),
		IntegrityHash: hashOf(`{}`),
		CreatedBy:     "u-alice",
		CreatedAt:     time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC),
	}
	return Snapshot{
		ProjectID: "proj-1", PRNumber: 7,
		Base: StateRef{ID: m2}, Proposed: StateRef{ID: s2},
		TargetStates: []domain.ProjectState{
			{ID: m1, ProjectID: "proj-1", BranchID: strPtr("branch-main"), ParentStateID: strPtr(genesis)},
			{ID: m2, ProjectID: "proj-1", BranchID: strPtr("branch-main"), ParentStateID: strPtr(m1)},
		},
		SourceStates: []domain.ProjectState{
			{ID: s1, ProjectID: "proj-1", BranchID: strPtr("branch-src"), ParentStateID: strPtr(m2)},
			{ID: s2, ProjectID: "proj-1", BranchID: strPtr("branch-src"), ParentStateID: strPtr(s1)},
		},
		SourceCommits: []domain.StateCommit{
			{ID: "c1", ProjectID: "proj-1", BranchID: "branch-src", BaseStateID: strPtr(m2), ResultStateID: s1, ActorID: "u-alice", Via: domain.ViaAPI, Message: "first"},
			{ID: "c2", ProjectID: "proj-1", BranchID: "branch-src", BaseStateID: strPtr(s1), ResultStateID: s2, ActorID: "u-alice", Via: domain.ViaAPI, Message: "second"},
		},
		BaseObjects:       []manifest.ObjectVersion{datasetV1},
		ProposedObjects:   []manifest.ObjectVersion{datasetV1, materialV1},
		BaseRelations:     nil,
		ProposedRelations: []manifest.RelationVersion{usesRelation},
		BaseBlobRefs:      nil,
		ProposedBlobRefs:  []manifest.BlobRef{{ID: "blob-1", Hash: "hash-1"}},
		PolicyVersions: []domain.PolicyVersion{
			{ID: policyID, Scope: domain.PolicyScope{ProjectID: "proj-1"}, Version: "1"},
		},
		// The two chains' boundaries, as the application layer resolves
		// them, kept as separate per-side sets: the target chain roots at
		// the genesis state, the source chain roots at the main state it
		// forked from (the PR base).
		TargetBoundaries: map[string]bool{genesis: true},
		SourceBoundaries: map[string]bool{m2: true},
	}
}

func TestCheckHappyPathPasses(t *testing.T) {
	e := New(testRegistry(t))
	report := e.Check(happySnapshot())
	if report.Blocked() {
		t.Fatalf("happy path blocked:\n%s", report.Explanation)
	}
	if report.Verdict != VerdictPass {
		t.Fatalf("verdict = %q, want %q (warnings: %d)", report.Verdict, VerdictPass, len(report.Warnings()))
	}
	if len(report.Results) == 0 {
		t.Fatal("no results produced")
	}
	// Every spec produced at least one result, and the payload_integrity
	// check covers the proposal's objects and relations.
	byCheck := map[CheckID]int{}
	for _, res := range report.Results {
		byCheck[res.Check]++
	}
	for _, spec := range specs {
		if byCheck[spec.Check] == 0 {
			t.Errorf("check %s produced no results", spec.Check)
		}
	}
	if got := byCheck[CheckPayloadIntegrity]; got != 3 {
		t.Errorf("payload_integrity results = %d, want 3 (2 objects + 1 relation)", got)
	}
}

func TestBlockingAndWarningSeverity(t *testing.T) {
	e := New(testRegistry(t))
	t.Run("blocking failure blocks", func(t *testing.T) {
		snap := happySnapshot()
		snap.Base = StateRef{ID: "state-elsewhere"}
		report := e.Check(snap)
		if !report.Blocked() || report.Verdict != VerdictBlocked {
			t.Fatalf("verdict = %q, want %q", report.Verdict, VerdictBlocked)
		}
		failures := report.BlockingFailures()
		if len(failures) != 1 || failures[0].Check != CheckBaseOnTargetChain || failures[0].Severity != SeverityBlocking {
			t.Fatalf("blocking failures = %+v, want exactly the base_on_target_chain failure", failures)
		}
		if len(report.Warnings()) != 0 {
			t.Fatalf("unexpected warnings: %+v", report.Warnings())
		}
	})
	t.Run("warning does not block", func(t *testing.T) {
		snap := happySnapshot()
		// An unpinned version fires the inheritance warning only.
		snap.ProposedObjects = append(snap.ProposedObjects, materialObject("ov-m2", "obj-material2", 1, nil))
		report := e.Check(snap)
		if report.Blocked() || report.Verdict != VerdictPassWithWarn {
			t.Fatalf("verdict = %q, want %q", report.Verdict, VerdictPassWithWarn)
		}
		warnings := report.Warnings()
		if len(warnings) != 1 || warnings[0].Check != CheckRightsPinInheritsDefault || warnings[0].Severity != SeverityWarning {
			t.Fatalf("warnings = %+v, want exactly the inheritance warning", warnings)
		}
	})
	t.Run("payload integrity is a warning", func(t *testing.T) {
		snap := happySnapshot()
		snap.ProposedObjects[0].IntegrityHash = strings.Repeat("0", 64)
		report := e.Check(snap)
		if report.Blocked() {
			t.Fatalf("corrupt payload blocked: %s", report.Explanation)
		}
		if report.Verdict != VerdictPassWithWarn {
			t.Fatalf("verdict = %q, want %q", report.Verdict, VerdictPassWithWarn)
		}
		var found bool
		for _, w := range report.Warnings() {
			if w.Check == CheckPayloadIntegrity {
				found = true
			}
		}
		if !found {
			t.Fatalf("no payload_integrity warning in %+v", report.Warnings())
		}
	})
}

// TestBoundariesArePerSide is the regression for the shared-boundaries
// defect: with one boundary set for both chains, a pin naming the OTHER
// side's legitimate boundary passed its provenance check — a false PASS
// over a corrupt row, exactly what this engine exists to catch (a raw
// INSERT satisfies every foreign key, so the pins are the only tell).
// Each check must accept only its own chain's boundaries: the target's
// base check consults TargetBoundaries, the source's proposed check and
// chain walk consult SourceBoundaries.
func TestBoundariesArePerSide(t *testing.T) {
	e := New(testRegistry(t))

	t.Run("a proposed pin on the target chain's boundary is not on the source chain", func(t *testing.T) {
		snap := happySnapshot()
		// The genesis state is the TARGET chain's boundary (its fork
		// point). The source chain does not contain it and does not root
		// from it, so the pin must block.
		snap.Proposed = StateRef{ID: "state-genesis"}
		report := e.Check(snap)
		failures := report.BlockingFailures()
		if len(failures) != 1 || failures[0].Check != CheckProposedOnSourceChain {
			t.Fatalf("blocking failures = %+v, want exactly the proposed_on_source_chain failure (verdict %s)", failures, report.Verdict)
		}
	})

	t.Run("a base pin on the source chain's fork point is not on the target chain", func(t *testing.T) {
		snap := happySnapshot()
		// Re-root the source chain on a third branch's state x1 (a child
		// of the target chain): x1 is the SOURCE chain's legitimate fork
		// point and is not on the target chain.
		snap.SourceStates[0].ParentStateID = strPtr("state-x1")
		snap.SourceCommits[0].BaseStateID = strPtr("state-x1")
		snap.SourceBoundaries = map[string]bool{"state-x1": true}
		snap.Base = StateRef{ID: "state-x1"}
		report := e.Check(snap)
		failures := report.BlockingFailures()
		if len(failures) != 1 || failures[0].Check != CheckBaseOnTargetChain {
			t.Fatalf("blocking failures = %+v, want exactly the base_on_target_chain failure (verdict %s)", failures, report.Verdict)
		}
	})

	t.Run("each chain's own boundary still passes", func(t *testing.T) {
		// The legitimate shapes the split must not break: a source branch
		// with no states proposes its fork point (the source boundary),
		// and the target's genesis boundary stays acceptable as a base.
		snap := happySnapshot()
		snap.Proposed = StateRef{ID: "state-m2"} // the source's fork point
		report := e.Check(snap)
		if report.Blocked() {
			t.Fatalf("source boundary as the proposed pin blocked:\n%s", report.Explanation)
		}
		snap = happySnapshot()
		snap.Base = StateRef{ID: "state-genesis"} // the target's fork point
		report = e.Check(snap)
		if report.Blocked() {
			t.Fatalf("target boundary as the base pin blocked:\n%s", report.Explanation)
		}
	})
}

// TestSourceChainWalkConsultsOnlySourceBoundaries covers the third site
// that reads a boundary set: the source chain's parent walk. It terminates
// the walk when it exits through a SOURCE boundary, and the target chain's
// boundaries are not the source's to accept — with one shared or unioned
// set, a source chain rooted on a state that is merely the target's fork
// point would pass a walk it cannot legitimately terminate, and a corrupt
// parent link would read as a clean chain.
func TestSourceChainWalkConsultsOnlySourceBoundaries(t *testing.T) {
	e := New(testRegistry(t))

	t.Run("a parent that is only the target chain's boundary breaks the walk", func(t *testing.T) {
		snap := happySnapshot()
		// state-p is a boundary of the TARGET chain (an extra fork point
		// of main) and nothing else: it is not a source state and not a
		// source boundary.
		snap.TargetBoundaries = map[string]bool{"state-genesis": true, "state-p": true}
		snap.SourceStates[0].ParentStateID = strPtr("state-p")
		snap.SourceCommits[0].BaseStateID = strPtr("state-p")

		report := e.Check(snap)
		failures := report.BlockingFailures()
		if len(failures) != 1 || failures[0].Check != CheckSourceChainUnbroken {
			t.Fatalf("blocking failures = %+v, want exactly the source_chain_unbroken failure (verdict %s)", failures, report.Verdict)
		}
		if !strings.Contains(failures[0].Detail, "broken parent link") {
			t.Fatalf("detail = %q, want the broken parent link path", failures[0].Detail)
		}
		if !strings.Contains(failures[0].Detail, "state-p") {
			t.Fatalf("detail = %q, want it to name the offending parent state-p", failures[0].Detail)
		}
	})

	t.Run("the same parent as a source boundary still terminates the walk", func(t *testing.T) {
		snap := happySnapshot()
		snap.TargetBoundaries = map[string]bool{"state-genesis": true, "state-p": true}
		snap.SourceBoundaries = map[string]bool{"state-p": true}
		snap.SourceStates[0].ParentStateID = strPtr("state-p")
		snap.SourceCommits[0].BaseStateID = strPtr("state-p")

		report := e.Check(snap)
		if report.Blocked() {
			t.Fatalf("a source boundary as the chain's fork point blocked:\n%s", report.Explanation)
		}
	})
}

func TestEveryCheckFires(t *testing.T) {
	e := New(testRegistry(t))
	corruptions := []struct {
		name     string
		check    CheckID
		severity Severity
		mut      func(*Snapshot)
	}{
		{"schema not registered", CheckSchemaRegistered, SeverityBlocking, func(s *Snapshot) {
			s.ProposedObjects[0].SchemaRef = manifest.SchemaRef{ID: "urn:unknown:schema", Version: "1"}
		}},
		{"payload violates schema", CheckSchemaPayloadConforms, SeverityBlocking, func(s *Snapshot) {
			// dataset requires purpose in the assembled document; an empty
			// payload carries none.
			s.ProposedObjects[0].Payload = json.RawMessage(`{}`)
		}},
		{"base off the target chain", CheckBaseOnTargetChain, SeverityBlocking, func(s *Snapshot) {
			s.Base = StateRef{ID: "state-elsewhere"}
		}},
		{"proposed off the source chain", CheckProposedOnSourceChain, SeverityBlocking, func(s *Snapshot) {
			s.Proposed = StateRef{ID: "state-elsewhere"}
		}},
		{"source chain broken parent link", CheckSourceChainUnbroken, SeverityBlocking, func(s *Snapshot) {
			s.SourceStates[1].ParentStateID = strPtr("state-missing")
		}},
		{"source chain forked", CheckSourceChainUnbroken, SeverityBlocking, func(s *Snapshot) {
			s.SourceStates = append(s.SourceStates, domain.ProjectState{
				ID: "state-fork", ProjectID: "proj-1", BranchID: strPtr("branch-src"), ParentStateID: strPtr("state-s1"),
			})
		}},
		{"commit missing for a state", CheckCommitLinkage, SeverityBlocking, func(s *Snapshot) {
			s.SourceCommits = s.SourceCommits[:1]
		}},
		{"commit base edge mismatch", CheckCommitLinkage, SeverityBlocking, func(s *Snapshot) {
			s.SourceCommits[1].BaseStateID = strPtr("state-m1")
		}},
		{"payload hash mismatch", CheckPayloadIntegrity, SeverityWarning, func(s *Snapshot) {
			s.ProposedObjects[0].IntegrityHash = strings.Repeat("0", 64)
		}},
		{"relation type unknown", CheckRelationTypeKnown, SeverityBlocking, func(s *Snapshot) {
			s.ProposedRelations[0].RelationType = "materials:not_declared"
		}},
		{"relation endpoint missing", CheckRelationEndpointsInProposal, SeverityBlocking, func(s *Snapshot) {
			s.ProposedRelations[0].TargetObjectVersionID = "ov-missing"
		}},
		{"relation endpoint type invalid", CheckRelationEndpointTypesValid, SeverityBlocking, func(s *Snapshot) {
			s.ProposedRelations[0].RelationType = "addresses_question" // pins target to research_question
		}},
		{"policy pin does not resolve", CheckRightsPolicyResolves, SeverityBlocking, func(s *Snapshot) {
			s.ProposedObjects[0].VisibilityPolicyID = strPtr("policy-ghost")
		}},
		{"visibility change flagged", CheckVisibilityChangeFlagged, SeverityBlocking, func(s *Snapshot) {
			s.ProposedObjects[0].VisibilityPolicyID = strPtr("policy-2")
			s.PolicyVersions = append(s.PolicyVersions, domain.PolicyVersion{
				ID: "policy-2", Scope: domain.PolicyScope{ProjectID: "proj-1"}, Version: "2",
			})
		}},
		{"blob ref does not resolve", CheckBlobRefsResolve, SeverityBlocking, func(s *Snapshot) {
			s.ProposedObjects[0].Payload = json.RawMessage(`{"purpose":"store measurements","blob_ids":["blob-ghost"]}`)
		}},
	}
	for _, tc := range corruptions {
		t.Run(tc.name, func(t *testing.T) {
			snap := happySnapshot()
			tc.mut(&snap)
			report := e.Check(snap)
			failures := report.BlockingFailures()
			if tc.severity == SeverityWarning {
				if report.Blocked() {
					t.Fatalf("warning-severity corruption blocked:\n%s", report.Explanation)
				}
				failures = report.Warnings()
			} else if !report.Blocked() {
				t.Fatalf("corruption did not block:\n%s", report.Explanation)
			}
			var found bool
			for _, res := range failures {
				if res.Check == tc.check {
					found = true
					if res.Detail == "" {
						t.Errorf("failure of %s carries no detail", tc.check)
					}
					if res.Dimension == "" || res.Severity == "" || res.Why == "" {
						t.Errorf("failure of %s is not self-describing: %+v", tc.check, res)
					}
					if res.Severity != tc.severity {
						t.Errorf("failure of %s has severity %q, want %q", tc.check, res.Severity, tc.severity)
					}
				}
			}
			if !found {
				t.Fatalf("no %s failure; got: %+v", tc.check, failures)
			}
		})
	}
}

func TestCheckDeterministic(t *testing.T) {
	e := New(testRegistry(t))
	snap := happySnapshot()
	first := e.Check(snap)

	// Re-running the same snapshot derives the same report.
	second := e.Check(happySnapshot())
	if !reflect.DeepEqual(first, second) {
		t.Fatal("two runs over the same snapshot derived different reports")
	}

	// Input order never moves the report.
	shuffled := happySnapshot()
	shuffled.ProposedObjects = []manifest.ObjectVersion{shuffled.ProposedObjects[1], shuffled.ProposedObjects[0]}
	shuffled.SourceStates = []domain.ProjectState{shuffled.SourceStates[1], shuffled.SourceStates[0]}
	shuffled.SourceCommits = []domain.StateCommit{shuffled.SourceCommits[1], shuffled.SourceCommits[0]}
	shuffled.TargetStates = []domain.ProjectState{shuffled.TargetStates[1], shuffled.TargetStates[0]}
	third := e.Check(shuffled)
	if !reflect.DeepEqual(first, third) {
		t.Fatal("reordered inputs derived a different report")
	}

	// A report with failures renders every failure in the explanation.
	snap2 := happySnapshot()
	snap2.Base = StateRef{ID: "state-elsewhere"}
	blocked := e.Check(snap2)
	if !strings.Contains(blocked.Explanation, string(CheckBaseOnTargetChain)) {
		t.Fatalf("explanation does not name the failing check:\n%s", blocked.Explanation)
	}
}
