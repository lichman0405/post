package domain_test

import (
	"testing"

	"github.com/lichman0405/post/internal/domain"
)

// Task T0806: the three network evidence classes docs/10 §7 names are
// COMPUTED from the two axes that exist (project comparison + review
// state), never stored. These tests pin the computation's edges — the ones
// a stored column would get wrong the first time an assertion was reviewed.

func TestClassifyEvidenceNetwork(t *testing.T) {
	cases := []struct {
		name     string
		external bool
		review   domain.EvidenceReviewState
		want     domain.EvidenceNetworkClass
		because  string
	}{
		{
			name: "same project, unreviewed", external: false, review: domain.EvidenceReviewUnreviewed,
			want:    domain.EvidenceClassOrigin,
			because: "the asserting project owns the published version: origin evidence, whatever its review state",
		},
		{
			name: "same project, reviewed", external: false, review: domain.EvidenceReviewReviewed,
			want:    domain.EvidenceClassOrigin,
			because: "reviewing an origin assertion does not make it external evidence",
		},
		{
			name: "same project, rejected", external: false, review: domain.EvidenceReviewRejected,
			want:    domain.EvidenceClassOrigin,
			because: "a rejected origin assertion is still the origin's own evidence",
		},
		{
			name: "other project, unreviewed", external: true, review: domain.EvidenceReviewUnreviewed,
			want:    domain.EvidenceClassUnreviewedExternal,
			because: "external and not reviewed: the class that claims nothing",
		},
		{
			name: "other project, reviewed", external: true, review: domain.EvidenceReviewReviewed,
			want:    domain.EvidenceClassReviewedExternal,
			because: "the only value that supports the 'reviewed' claim",
		},
		{
			name: "other project, rejected", external: true, review: domain.EvidenceReviewRejected,
			want:    domain.EvidenceClassUnreviewedExternal,
			because: "rejected is not 'reviewed': it stays visible (invariant 8) under the class that asserts no review",
		},
		{
			name: "other project, unknown review state", external: true, review: domain.EvidenceReviewState("pending"),
			want:    domain.EvidenceClassUnreviewedExternal,
			because: "fail closed: an unknown state never earns the reviewed class",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := domain.ClassifyEvidenceNetwork(tc.external, tc.review); got != tc.want {
				t.Errorf("ClassifyEvidenceNetwork(%v, %q) = %q, want %q — %s",
					tc.external, tc.review, got, tc.want, tc.because)
			}
		})
	}
}

// TestClassifyEvidenceNetworkIsTotal: every combination of the two axes has
// one class, so no input can leave an assertion unclassified (and therefore
// silently unrendered).
func TestClassifyEvidenceNetworkIsTotal(t *testing.T) {
	for _, review := range []domain.EvidenceReviewState{
		domain.EvidenceReviewUnreviewed, domain.EvidenceReviewReviewed, domain.EvidenceReviewRejected,
		domain.EvidenceReviewState(""), domain.EvidenceReviewState("something-else"),
	} {
		for _, external := range []bool{false, true} {
			got := domain.ClassifyEvidenceNetwork(external, review)
			var seen bool
			for _, c := range domain.CanonicalEvidenceNetworkClasses() {
				if got == c {
					seen = true
				}
			}
			if !seen {
				t.Errorf("ClassifyEvidenceNetwork(%v, %q) = %q, which is not a canonical class",
					external, review, got)
			}
		}
	}
}

// TestAssertionIsExternalFailClosed: the axis-1 comparison treats an unknown
// owner as external. Presenting an assertion whose owner could not be
// resolved as the origin's OWN evidence would be the wrong direction — it
// would let an unreadable row borrow the published version's authority.
func TestAssertionIsExternalFailClosed(t *testing.T) {
	if !domain.AssertionIsExternal("", "proj-a") {
		t.Error(`AssertionIsExternal("", "proj-a") = false: an unknown asserting project must not claim the origin side`)
	}
	if !domain.AssertionIsExternal("proj-a", "") {
		t.Error(`AssertionIsExternal("proj-a", "") = false: an unknown target owner must not claim the origin side`)
	}
	if !domain.AssertionIsExternal("", "") {
		t.Error(`AssertionIsExternal("", "") = false: two unknowns are not the same project`)
	}
	if domain.AssertionIsExternal("proj-a", "proj-a") {
		t.Error("AssertionIsExternal over one project id = false: the same project is the origin side")
	}
	if !domain.AssertionIsExternal("proj-b", "proj-a") {
		t.Error("AssertionIsExternal(proj-b, proj-a) = false: a different project is external")
	}
}

// TestCanonicalEvidenceNetworkClassesMatchTheThreeNamedClasses: the read
// surface presents exactly docs/10 §7's three classes, in order. A fourth
// bucket, or a renamed one, is a contract decision this task does not make.
func TestCanonicalEvidenceNetworkClassesMatchTheThreeNamedClasses(t *testing.T) {
	got := domain.CanonicalEvidenceNetworkClasses()
	want := []domain.EvidenceNetworkClass{"origin", "reviewed_external", "unreviewed_external"}
	if len(got) != len(want) {
		t.Fatalf("classes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("class[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
