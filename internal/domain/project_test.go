package domain

import "testing"

func TestValidProjectSlug(t *testing.T) {
	valid := []string{"mof-screening", "MOF-SCREENING", "p1", "a1-b2-c3", "x"}
	for _, s := range valid {
		if !ValidProjectSlug(s) {
			t.Errorf("ValidProjectSlug(%q) = false, want true", s)
		}
	}
	invalid := []string{
		"", " ", "-lead", "trail-", "has space", "under_score", "大写",
		"a/b", "dot.slug", "é", "x" + repeat("a", 64),
	}
	for _, s := range invalid {
		if ValidProjectSlug(s) {
			t.Errorf("ValidProjectSlug(%q) = true, want false", s)
		}
	}
	if n := NormalizeProjectSlug("  MOF-Lab "); n != "mof-lab" {
		t.Errorf("NormalizeProjectSlug = %q, want mof-lab", n)
	}
}

func TestValidProjectName(t *testing.T) {
	if !ValidProjectName("MOF Screening") {
		t.Error("ValidProjectName(MOF Screening) = false, want true")
	}
	for _, s := range []string{"", "   ", "x" + repeat("a", 200)} {
		if ValidProjectName(s) {
			t.Errorf("ValidProjectName(%q) = true, want false", s)
		}
	}
}

func TestValidProjectPurpose(t *testing.T) {
	if !ValidProjectPurpose("screen MOFs for gas separation") {
		t.Error("ValidProjectPurpose = false, want true")
	}
	for _, s := range []string{"", "  ", "x" + repeat("a", 4000)} {
		if ValidProjectPurpose(s) {
			t.Errorf("ValidProjectPurpose(len %d) = true, want false", len(s))
		}
	}
}

func TestProjectVisibility(t *testing.T) {
	if !ValidProjectVisibility(VisibilityPublic) || !ValidProjectVisibility(VisibilityPrivate) {
		t.Error("canonical presets must be valid")
	}
	if ValidProjectVisibility("internal") || ValidProjectVisibility("") {
		t.Error("non-preset visibilities must be invalid")
	}
}

func TestProvisionStatus(t *testing.T) {
	for _, s := range []ProvisionStatus{ProvisionPending, ProvisionProvisioned, ProvisionFailed} {
		if !ValidProvisionStatus(s) {
			t.Errorf("ValidProvisionStatus(%q) = false, want true", s)
		}
	}
	if ValidProvisionStatus("provisioning") || ValidProvisionStatus("") {
		t.Error("unknown provision states must be invalid")
	}
}

func TestProjectRole(t *testing.T) {
	for _, r := range []ProjectRole{ProjectRoleOwner, ProjectRoleMaintainer, ProjectRoleContributor, ProjectRoleViewer} {
		if !ValidProjectRole(r) {
			t.Errorf("ValidProjectRole(%q) = false, want true", r)
		}
	}
	if ValidProjectRole("admin") || ValidProjectRole("") {
		t.Error("unknown roles must be invalid")
	}
}

func TestProjectPersonal(t *testing.T) {
	if !(Project{}).Personal() {
		t.Error("a project without an organization is personal")
	}
	org := "o1"
	if (Project{OrganizationID: &org}).Personal() {
		t.Error("a project with an organization is not personal")
	}
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}
