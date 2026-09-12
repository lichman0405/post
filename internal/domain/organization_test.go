package domain

import (
	"testing"
	"time"
)

func TestValidOrgSlug(t *testing.T) {
	valid := []string{"acme", "acme-labs", "a1-b2", "ACME", "  acme  ", "x"}
	for _, s := range valid {
		if !ValidOrgSlug(s) {
			t.Errorf("ValidOrgSlug(%q) = false, want true", s)
		}
	}
	invalid := []string{"", "-acme", "acme-", "acme_labs", "ac me", "acme.labs", "acme/labs",
		string(make([]byte, 65)), "a b", "a\tb"}
	for _, s := range invalid {
		if ValidOrgSlug(s) {
			t.Errorf("ValidOrgSlug(%q) = true, want false", s)
		}
	}
	if got := NormalizeOrgSlug("  ACME-Labs  "); got != "acme-labs" {
		t.Errorf("NormalizeOrgSlug = %q, want %q", got, "acme-labs")
	}
}

func TestValidOrgName(t *testing.T) {
	if ValidOrgName("") || ValidOrgName("   ") {
		t.Error("blank names must be invalid")
	}
	if !ValidOrgName("Acme Research") {
		t.Error("normal name must be valid")
	}
	if ValidOrgName(string(make([]byte, 201))) {
		t.Error("name over 200 chars must be invalid")
	}
}

func TestValidOrgRole(t *testing.T) {
	for _, r := range []OrgRole{OrgRoleOwner, OrgRoleMaintainer, OrgRoleContributor, OrgRoleViewer} {
		if !ValidOrgRole(r) {
			t.Errorf("ValidOrgRole(%q) = false, want true", r)
		}
	}
	for _, r := range []OrgRole{"", "admin", "OWNER", "Owner"} {
		if ValidOrgRole(r) {
			t.Errorf("ValidOrgRole(%q) = true, want false", r)
		}
	}
}

func TestRoleGoverns(t *testing.T) {
	if !OrgRoleOwner.Governs() {
		t.Error("owner must govern")
	}
	for _, r := range []OrgRole{OrgRoleMaintainer, OrgRoleContributor, OrgRoleViewer} {
		if r.Governs() {
			t.Errorf("%s must not govern the organization", r)
		}
	}
}

func TestOrganizationActive(t *testing.T) {
	now := time.Now()
	active := Organization{}
	if !active.Active() {
		t.Error("nil deactivated_at must be active")
	}
	past := now.Add(-time.Hour)
	active.DeactivatedAt = &past
	if active.Active() {
		t.Error("past deactivated_at must be inactive")
	}
	future := now.Add(time.Hour)
	active.DeactivatedAt = &future
	if !active.Active() {
		t.Error("future deactivated_at must still be active")
	}
}

func TestMembershipActive(t *testing.T) {
	// The local calendar date at UTC midnight — the same truncation
	// Active() applies, so the test is timezone-independent.
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	m := OrganizationMembership{AffiliationStart: today}
	if !m.Active() {
		t.Error("open-ended started membership must be active")
	}
	end := today.Add(24 * time.Hour)
	m.AffiliationEnd = &end
	if !m.Active() {
		t.Error("membership with future end must be active")
	}
	endPast := today.Add(-24 * time.Hour)
	m.AffiliationEnd = &endPast
	if m.Active() {
		t.Error("membership with past end must be inactive")
	}
	futureStart := today.Add(24 * time.Hour)
	m = OrganizationMembership{AffiliationStart: futureStart}
	if m.Active() {
		t.Error("membership with future start must be inactive")
	}
}
