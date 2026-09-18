package knowledgepublish

import (
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/application/releases"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rights"
)

// AudienceFor is the ONE definition of who may read a published knowledge
// object, and it is the rule both the public read (GET /knowledge/{id},
// through PublishedKnowledge.Audience) and the explore index apply. A
// table is the only honest way to pin a fail-closed function: every row
// that is not explicitly admitted must answer members, so a future edit
// that widens one axis lights up here.

// policyPinned is a version that carries a visibility policy of its own
// (scientific_object_versions.visibility_policy_id is non-NULL).
func policyPinned() *string { s := "9f0f0e0d-0000-4000-8000-000000000001"; return &s }

func TestAudienceForIsFailClosed(t *testing.T) {
	projectPolicy := rights.New() // metadata = project_policy, data_access = restricted
	declared := rights.New()
	declared.Visibility.Metadata = "public"

	cases := []struct {
		name    string
		project string
		policy  *string
		doc     rights.Document
		want    Audience
		because string
	}{
		{
			name:    "a public project, an inherited visibility, and the project-policy rights default",
			project: "public",
			policy:  nil,
			doc:     projectPolicy,
			want:    AudienceNetwork,
			because: "the one combination the rule admits",
		},
		{
			name:    "a version that pins its own visibility policy, in a PUBLIC project",
			project: "public",
			policy:  policyPinned(),
			doc:     projectPolicy,
			want:    AudienceMembers,
			because: "the version's own axis governs it, and this build resolves no policy token to a public grant — T0805's headline case: 发布 ≠ 公开",
		},
		{
			name:    "an inherited visibility in a PRIVATE project",
			project: "private",
			policy:  nil,
			doc:     projectPolicy,
			want:    AudienceMembers,
			because: "the project preset is the second required axis (docs/12 §2)",
		},
		{
			name:    "a public project and an inherited visibility, but the rights document pins a metadata policy",
			project: "public",
			policy:  nil,
			doc:     declared,
			want:    AudienceMembers,
			because: "the token is unresolvable in this build, and an unresolvable policy is refused rather than assumed public",
		},
		{
			name:    "the rights declaration does not parse (the zero document)",
			project: "public",
			policy:  nil,
			doc:     rights.Document{},
			want:    AudienceMembers,
			because: "a document that states no default cannot be read as one that grants",
		},
		{
			name:    "an unknown project visibility token",
			project: "internal",
			policy:  nil,
			doc:     projectPolicy,
			want:    AudienceMembers,
			because: "only the exact preset is public",
		},
		{
			name:    "the empty project visibility",
			project: "",
			policy:  nil,
			doc:     projectPolicy,
			want:    AudienceMembers,
			because: "a missing preset is not a public one",
		},
		{
			name:    "a version that pins its own visibility in a PRIVATE project",
			project: "private",
			policy:  policyPinned(),
			doc:     projectPolicy,
			want:    AudienceMembers,
			because: "both axes are required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AudienceFor(tc.project, tc.policy, tc.doc)
			if got != tc.want {
				t.Fatalf("AudienceFor(%q, %v, %+v) = %q, want %q — %s", tc.project, tc.policy, tc.doc.Visibility, got, tc.want, tc.because)
			}
		})
	}
}

// TestAudienceForIsTheOnlyAdmission: the network answer is reachable by
// exactly one combination of the inputs, and this pins that there is no
// second one — the property the table above asserts row by row, checked
// exhaustively over the small input space.
func TestAudienceForIsTheOnlyAdmission(t *testing.T) {
	projectPolicy := rights.New()
	other := rights.New()
	other.Visibility.Metadata = "public"
	docs := map[string]rights.Document{"project_policy": projectPolicy, "pinned": other, "zero": {}}
	policies := map[string]*string{"nil": nil, "pinned": policyPinned()}
	projects := []string{"public", "private", ""}

	for docName, doc := range docs {
		for polName, pol := range policies {
			for _, project := range projects {
				got := AudienceFor(project, pol, doc)
				admitted := project == "public" && pol == nil && doc.Visibility.Metadata == rights.MetadataProjectPolicy
				if admitted && got != AudienceNetwork {
					t.Errorf("project=%q policy=%s rights=%s: the one admitted combination answered %q", project, polName, docName, got)
				}
				if !admitted && got != AudienceMembers {
					t.Errorf("project=%q policy=%s rights=%s: answered %q, want members (fail closed)", project, polName, docName, got)
				}
			}
		}
	}
}

// TestPublishedKnowledgeAudienceUsesTheSameRule: the public read resolves
// a row and asks it who may read it; the answer must be AudienceFor's, on
// the version's own axis — not a second opinion computed from the project.
func TestPublishedKnowledgeAudienceUsesTheSameRule(t *testing.T) {
	k := PublishedKnowledge{
		PID:                testPID,
		ProjectID:          testProject,
		ProjectVisibility:  "public",
		VisibilityPolicyID: policyPinned(),
		Rights:             rights.New(),
		RightsValid:        true,
	}
	if got := k.Audience(); got != AudienceMembers {
		t.Errorf("a version with its own visibility policy in a public project is %q to the public read, want %q", got, AudienceMembers)
	}
	k.VisibilityPolicyID = nil
	if got := k.Audience(); got != AudienceNetwork {
		t.Errorf("the same publication with an inherited visibility is %q, want %q", got, AudienceNetwork)
	}
}

// TestValidPIDIsTheOneVocabulary: the command's predicate, the asset
// command's predicate and internal/domain are one function, and the shape
// it accepts is the CHECK both tables carry.
func TestValidPIDIsTheOneVocabulary(t *testing.T) {
	if !ValidPID(testPID) {
		t.Errorf("ValidPID(%q) = false", testPID)
	}
	if ValidPID(strings.ToUpper(testPID)) {
		t.Error("an uppercase pid was accepted; pids are printed lowercase to stay unambiguous")
	}
	for _, bad := range []string{"", testPID[:25], testPID + "2", "01j9z6k3m4n5p6q7r8s9t0v1wi", "01j9z6k3m4n5p6q7r8s9t0v1w-"} {
		if ValidPID(bad) {
			t.Errorf("ValidPID(%q) = true, want false", bad)
		}
	}
	if ValidPID(testPID) != assets.ValidPID(testPID) || ValidPID(testPID) != domain.ValidPID(testPID) {
		t.Error("the three names for the pid shape disagree")
	}
}

// TestApprovedReviewKindsNeedsBothDimensions: docs/09 §5 requires the two
// review dimensions to be recorded separately — never one aggregate
// approval — so a record that carries one of them is not a reviewed
// version.
func TestApprovedReviewKindsNeedsBothDimensions(t *testing.T) {
	cases := []struct {
		name    string
		records []releases.ReviewRecord
		want    []string
	}{
		{"no record at all", nil, nil},
		{"a record with no reviews", []releases.ReviewRecord{{Reviews: nil}}, nil},
		{"only scientific", []releases.ReviewRecord{{Reviews: []releases.Review{{ReviewKind: "scientific", Decision: "approved"}}}}, []string{"scientific"}},
		{"only integrity", []releases.ReviewRecord{{Reviews: []releases.Review{{ReviewKind: "integrity", Decision: "approved"}}}}, []string{"integrity"}},
		{
			"both, across two pull requests",
			[]releases.ReviewRecord{
				{Reviews: []releases.Review{{ReviewKind: "integrity", Decision: "approved"}}},
				{Reviews: []releases.Review{{ReviewKind: "scientific", Decision: "approved"}}},
			},
			[]string{"scientific", "integrity"},
		},
		{
			"a rejected review is not an approval",
			[]releases.ReviewRecord{{Reviews: []releases.Review{{ReviewKind: "scientific", Decision: "changes_requested"}, {ReviewKind: "integrity", Decision: "approved"}}}},
			[]string{"integrity"},
		},
		{
			"an unknown dimension grants nothing",
			[]releases.ReviewRecord{{Reviews: []releases.Review{{ReviewKind: "vibes", Decision: "approved"}}}},
			nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ApprovedReviewKinds(tc.records)
			if len(got) != len(tc.want) {
				t.Fatalf("ApprovedReviewKinds = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("ApprovedReviewKinds = %v, want %v", got, tc.want)
				}
			}
			f := Facts{Reviews: tc.records}
			if f.ReviewApproved() != (len(tc.want) == len(RequiredReviewKinds())) {
				t.Errorf("ReviewApproved() = %v for %v", f.ReviewApproved(), got)
			}
		})
	}
}

// TestJudgeReportsEveryBlockingEntryInOnePass: the judgement is the whole
// refusal report, not the first reason found — a caller that fixed one
// entry and retried into the next one would learn the rules by trial.
func TestJudgeReportsEveryBlockingEntryInOnePass(t *testing.T) {
	f := Facts{
		ProjectID:       testProject,
		ObjectVersionID: testVersionA,
		Reviews:         nil,
		Published:       &Published{ObjectVersionID: testVersionA, PublicVersion: "v1.0"},
	}
	reasons := Judge(f)
	if len(reasons) != 2 {
		t.Fatalf("Judge = %+v, want both entries", reasons)
	}
	if reasons[0].Code != ReasonAlreadyPublished || reasons[1].Code != ReasonReviewRequired {
		t.Errorf("codes = %q, %q", reasons[0].Code, reasons[1].Code)
	}
	for _, r := range reasons {
		if r.Detail == "" {
			t.Errorf("reason %q has no detail sentence", r.Code)
		}
	}
	if !strings.Contains(reasons[0].Detail, "v1.0") {
		t.Errorf("the already-published refusal does not name the publication: %q", reasons[0].Detail)
	}
	if !strings.Contains(reasons[1].Detail, "scientific and integrity") {
		t.Errorf("the review refusal does not name the missing dimensions: %q", reasons[1].Detail)
	}

	// A reviewed, unpublished version blocks on nothing.
	f.Reviews = []releases.ReviewRecord{{Reviews: []releases.Review{
		{ReviewKind: "scientific", Decision: "approved"},
		{ReviewKind: "integrity", Decision: "approved"},
	}}}
	f.Published = nil
	if got := Judge(f); len(got) != 0 {
		t.Errorf("a reviewed, unpublished version still blocks: %+v", got)
	}
}

// TestJudgeRefusesARetractedVersion: a version currently in the retracted
// state may not simultaneously be presented on the network as published
// knowledge, however well reviewed it is.
//
// The rule is NOT "aborted is terminal" — docs/43's version lifecycle is
// active → aborted → reopened → active — so the test carries its own
// control: the SAME version reopened blocks on nothing. A rule that refused
// a version for having ever been aborted would pass the first half and fail
// the second.
func TestJudgeRefusesARetractedVersion(t *testing.T) {
	reviewed := []releases.ReviewRecord{{Reviews: []releases.Review{
		{ReviewKind: "scientific", Decision: "approved"},
		{ReviewKind: "integrity", Decision: "approved"},
	}}}
	f := Facts{
		ProjectID:       testProject,
		ObjectVersionID: testVersionA,
		Reviews:         reviewed,
		LifecycleState:  string(domain.LifecycleAborted),
	}
	reasons := Judge(f)
	if len(reasons) != 1 || reasons[0].Code != ReasonLifecycleAborted {
		t.Fatalf("Judge of a retracted version = %+v, want exactly %q", reasons, ReasonLifecycleAborted)
	}
	detail := reasons[0].Detail
	// The sentence says what the rule is about: the version's state NOW and
	// the contradiction between holding it retracted and presenting it as
	// published. It must not read as a terminal-state argument, and it must
	// name the way out.
	for _, want := range []string{"retracted", "cannot simultaneously be presented", "reopened"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the retracted-version refusal does not say %q: %q", want, detail)
		}
	}
	if strings.Contains(detail, "final") && !strings.Contains(detail, "not about the state being final") {
		t.Errorf("the refusal reads as a terminal-state argument: %q", detail)
	}

	// Every other lifecycle value is publishable: reopened (the way out),
	// active, and superseded — which docs/43 lists on the same vocabulary
	// and which says nothing about whether the version is presented on the
	// network.
	for _, state := range []string{"", "active", "reopened", "superseded"} {
		f.LifecycleState = state
		for _, r := range Judge(f) {
			if r.Code == ReasonLifecycleAborted {
				t.Errorf("lifecycle_state %q was refused as retracted: %q", state, r.Detail)
			}
		}
	}
}

// TestRequiredReviewKindsIsACopy: the caller gets a list it can sort or
// truncate without editing the package's rule for the next request.
func TestRequiredReviewKindsIsACopy(t *testing.T) {
	got := RequiredReviewKinds()
	if len(got) != 2 || got[0] != "scientific" || got[1] != "integrity" {
		t.Fatalf("RequiredReviewKinds = %v", got)
	}
	got[0] = "mutated"
	if again := RequiredReviewKinds(); again[0] != "scientific" {
		t.Errorf("the package's rule was mutated through the returned slice: %v", again)
	}
}
