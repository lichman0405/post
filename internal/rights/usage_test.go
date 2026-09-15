package rights

import (
	"strings"
	"testing"
)

// TestUsageVocabularies pins the three closed vocabularies of docs/12 §4
// and specs/policies/rights-template.yaml, value by value, against the
// strings themselves. The vocabulary is a wire and storage format, so a
// renamed constant that compiles is a compatibility change — only a test
// that spells the value out catches it.
func TestUsageVocabularies(t *testing.T) {
	if got := AllPermissions(); !equalPermissions(got, []Permission{"allowed", "restricted", "unspecified"}) {
		t.Errorf("AllPermissions() = %v, want [allowed restricted unspecified]", got)
	}
	if got := AllAttributions(); !equalAttributions(got, []Attribution{"required", "not_required", "unspecified"}) {
		t.Errorf("AllAttributions() = %v, want [required not_required unspecified]", got)
	}
	if got := AllPatentGrants(); !equalPatentGrants(got, []PatentGrant{"none", "see_agreement", "explicit"}) {
		t.Errorf("AllPatentGrants() = %v, want [none see_agreement explicit]", got)
	}
	if got := AllDataAccess(); !equalDataAccess(got, []DataAccess{"open", "restricted"}) {
		t.Errorf("AllDataAccess() = %v, want [open restricted]", got)
	}
}

// TestUsageParsersRejectNearMisses walks each parser over the values a
// caller actually gets wrong: the empty string, whitespace, a value from a
// neighbouring vocabulary, and a case variant. Accepted only after
// trimming — " allowed " trims to a legal value and is accepted, because a
// form field's surrounding space is not a declaration.
func TestUsageParsersRejectNearMisses(t *testing.T) {
	permissionCases := []struct {
		in   string
		want Permission
		ok   bool
	}{
		{"allowed", PermissionAllowed, true},
		{" allowed ", PermissionAllowed, true},
		{"restricted", PermissionRestricted, true},
		{"unspecified", PermissionUnspecified, true},
		{"", "", false},
		{"   ", "", false},
		{"required", "", false},
		{"Allowed", "", false},
		{"permitted", "", false},
	}
	for _, tc := range permissionCases {
		got, ok := ParsePermission(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ParsePermission(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}

	attributionCases := []struct {
		in   string
		want Attribution
		ok   bool
	}{
		{"required", AttributionRequired, true},
		{"not_required", AttributionNotRequired, true},
		{"unspecified", AttributionUnspecified, true},
		{"allowed", "", false},
		{"not-required", "", false},
		{"Required", "", false},
	}
	for _, tc := range attributionCases {
		got, ok := ParseAttribution(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ParseAttribution(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}

	patentCases := []struct {
		in   string
		want PatentGrant
		ok   bool
	}{
		{"none", PatentGrantNone, true},
		{"see_agreement", PatentGrantSeeAgreement, true},
		{"explicit", PatentGrantExplicit, true},
		{"unspecified", "", false}, // the template gives this field no such value
		{"see-agreement", "", false},
		{"", "", false},
	}
	for _, tc := range patentCases {
		got, ok := ParsePatentGrant(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ParsePatentGrant(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// TestUsageValidateReportsPerFieldCode checks that every usage rule
// reports the usage code and its own field path, so a caller can point at
// the field a form should highlight.
func TestUsageValidateReportsPerFieldCode(t *testing.T) {
	u := Usage{
		CommercialUse:  "yes",
		Derivatives:    PermissionAllowed,
		Redistribution: PermissionRestricted,
		ModelTraining:  PermissionUnspecified,
		Attribution:    "maybe",
		PatentGrant:    PatentGrantNone,
	}
	err := u.Validate()
	if err == nil {
		t.Fatal("Usage.Validate() = nil, want the two broken fields reported")
	}
	reported := map[string]bool{}
	for _, e := range flatten(err) {
		if e.Code != CodeInvalidUsageValue {
			t.Errorf("field %s code = %q, want %q", e.Field, e.Code, CodeInvalidUsageValue)
		}
		reported[e.Field] = true
	}
	if !reported["usage.commercial_use"] || !reported["usage.attribution"] {
		t.Errorf("reported %v, want usage.commercial_use and usage.attribution", reported)
	}
	if len(reported) != 2 {
		t.Errorf("reported %d fields, want exactly the 2 broken ones", len(reported))
	}
	// The detail names the accepted values, so the message is usable
	// without reading this package's source.
	if !strings.Contains(err.Error(), "allowed|restricted|unspecified") {
		t.Errorf("detail %q does not name the accepted values", err.Error())
	}
}

func equalPermissions(got, want []Permission) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func equalAttributions(got, want []Attribution) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func equalPatentGrants(got, want []PatentGrant) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func equalDataAccess(got, want []DataAccess) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
