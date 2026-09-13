package domain

import "testing"

func TestValidBranchName(t *testing.T) {
	valid := []string{"main", "feature/screening", "v2", "dev-2.x", "a_b", "exp/2026/09", "A/B.C"}
	for _, s := range valid {
		if !ValidBranchName(s) {
			t.Errorf("ValidBranchName(%q) = false, want true", s)
		}
	}
	invalid := []string{
		"", "  main", "main ", "has space", "has\ttab", " leading",
		"/leading", "trailing/", "a//b", "a/./b", "a/../b", ".", "..",
		"trail.", ".lead", "x.lock", "a@{b", "HEAD", "head", "a:b",
		"a\\b", "a?b", "a*b", "a~b", "a^b", "大写", "é",
		"x" + repeat("a", 128),
	}
	for _, s := range invalid {
		if ValidBranchName(s) {
			t.Errorf("ValidBranchName(%q) = true, want false", s)
		}
	}
}

func TestBranchVisibilityAndLifecycleValidation(t *testing.T) {
	for _, v := range []BranchVisibility{BranchVisibilityPublic, BranchVisibilityPrivate} {
		if !ValidBranchVisibility(v) {
			t.Errorf("ValidBranchVisibility(%q) = false, want true", v)
		}
	}
	if ValidBranchVisibility("internal") {
		t.Error("ValidBranchVisibility(internal) = true, want false")
	}
	for _, l := range []BranchLifecycle{BranchLifecycleActive, BranchLifecycleMerged, BranchLifecycleAborted} {
		if !ValidBranchLifecycle(l) {
			t.Errorf("ValidBranchLifecycle(%q) = false, want true", l)
		}
	}
	if ValidBranchLifecycle("archived") {
		t.Error("ValidBranchLifecycle(archived) = true, want false")
	}
}

func TestBranchLifecycleAcceptsCommits(t *testing.T) {
	if !BranchLifecycleActive.AcceptsCommits() {
		t.Error("active.AcceptsCommits() = false, want true")
	}
	if BranchLifecycleMerged.AcceptsCommits() || BranchLifecycleAborted.AcceptsCommits() {
		t.Error("merged/aborted accept commits, want false (docs/43: history immutable)")
	}
}

func TestBranchIsMain(t *testing.T) {
	if !(Branch{Name: "main"}).IsMain() {
		t.Error("IsMain() = false for name main")
	}
	if (Branch{Name: "feature/main"}).IsMain() {
		t.Error("IsMain() = true for feature/main")
	}
	if (Branch{Name: "Main"}).IsMain() {
		t.Error("IsMain() = true for Main — the canonical name is exact")
	}
}

func TestValidBranchPurpose(t *testing.T) {
	p := "screen MOFs under humid conditions"
	if !ValidBranchPurpose(&p) {
		t.Error("ValidBranchPurpose = false, want true")
	}
	if !ValidBranchPurpose(nil) {
		t.Error("ValidBranchPurpose(nil) = false, want true")
	}
	blank := "   "
	if ValidBranchPurpose(&blank) {
		t.Error("ValidBranchPurpose(blank) = true, want false")
	}
	huge := repeat("a", 2001)
	if ValidBranchPurpose(&huge) {
		t.Error("ValidBranchPurpose(2001 chars) = true, want false")
	}
}
