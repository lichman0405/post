package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func mustPolicy(t *testing.T, doc string) Policy {
	t.Helper()
	p, err := PolicyFromJSON([]byte(doc))
	if err != nil {
		t.Fatalf("PolicyFromJSON(%s): %v", doc, err)
	}
	return p
}

func mustRaw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func TestPolicyFromJSONRoundTripsDeterministically(t *testing.T) {
	p := mustPolicy(t, `{"release_min_reviewers":2,"main_protected":true}`)
	out, err := p.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	// Keys sorted: main_protected before release_min_reviewers.
	want := `{"main_protected":true,"release_min_reviewers":2}`
	if string(out) != want {
		t.Errorf("MarshalJSON = %s, want %s", out, want)
	}
	back, err := PolicyFromJSON(out)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if len(back.Rules) != 2 {
		t.Errorf("round trip: %d rules, want 2", len(back.Rules))
	}
}

func TestPolicyFromJSONRejectsMalformedDocuments(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string // substring of the error
	}{
		{"not an object", `[1,2]`, "invalid JSON"},
		{"empty rule key", `{"":true}`, "rule key"},
		{"bad key charset", `{"Main-Protected":true}`, "rule key"},
		{"null value", `{"main_protected":null}`, "omit the rule"},
		{"bool rule with string", `{"main_protected":"yes"}`, "must be a boolean"},
		{"int rule with float", `{"release_min_reviewers":1.5}`, "must be an integer"},
		{"list rule with object", `{"required_schema_profiles":{"a":1}}`, "must be a list of strings"},
		{"too many rules", tooManyRulesDoc(), "at most 64"},
		{"garbage json", `{`, "invalid JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PolicyFromJSON([]byte(tc.doc))
			if err == nil {
				t.Fatalf("PolicyFromJSON(%s) succeeded, want error containing %q", tc.doc, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func tooManyRulesDoc() string {
	pairs := make([]byte, 0, 1024)
	pairs = append(pairs, '{')
	for i := 0; i < 65; i++ {
		if i > 0 {
			pairs = append(pairs, ',')
		}
		key := "rule_" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		pairs = append(pairs, '"')
		pairs = append(pairs, []byte(key)...)
		pairs = append(pairs, '"', ':', '1')
	}
	pairs = append(pairs, '}')
	return string(pairs)
}

func TestCompareRuleRegisteredOrders(t *testing.T) {
	cases := []struct {
		name      string
		key       string
		baseline  any
		candidate any
		want      Strictness
	}{
		{"bool equal", RuleMainProtected, true, true, StrictnessEqual},
		{"bool stricter", RuleMainProtected, false, true, StrictnessStricter},
		{"bool weaker", RulePublicAssetIPReview, true, false, StrictnessWeaker},
		{"int equal", RuleReleaseMinReviewers, 2, 2, StrictnessEqual},
		{"int stricter", RuleReleaseMinReviewers, 2, 3, StrictnessStricter},
		{"int weaker", RuleRawDataRetentionDays, 90, 30, StrictnessWeaker},
		{"list equal", RuleRequiredSchemaProfiles, []string{"core", "mof"}, []string{"mof", "core"}, StrictnessEqual},
		{"list stricter (superset)", RuleRequiredSchemaProfiles, []string{"core"}, []string{"core", "mof"}, StrictnessStricter},
		{"list weaker (subset)", RuleRequiredSchemaProfiles, []string{"core", "mof"}, []string{"core"}, StrictnessWeaker},
		{"list incomparable", RuleRequiredSchemaProfiles, []string{"core"}, []string{"mof"}, StrictnessIncomparable},
		{"unknown equal", "future_rule", map[string]any{"a": 1}, map[string]any{"a": 1}, StrictnessEqual},
		{"unknown different object", "future_rule", map[string]any{"a": 1}, map[string]any{"a": 2}, StrictnessIncomparable},
		{"unknown equal despite key order", "future_rule", map[string]any{"a": 1, "b": 2}, map[string]any{"b": 2, "a": 1}, StrictnessEqual},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CompareRule(tc.key, mustRaw(tc.baseline), mustRaw(tc.candidate))
			if err != nil {
				t.Fatalf("CompareRule: %v", err)
			}
			if got != tc.want {
				t.Errorf("CompareRule = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestValidateProjectPolicyRefusesRelaxation(t *testing.T) {
	org := mustPolicy(t, `{
		"main_protected": true,
		"release_min_reviewers": 2,
		"required_schema_profiles": ["core"],
		"future_rule": {"x": 1}
	}`)

	cases := []struct {
		name    string
		project string
		ok      bool
		keys    []string // violating keys when !ok
	}{
		{"stricter everywhere", `{
			"main_protected": true,
			"release_min_reviewers": 3,
			"required_schema_profiles": ["core","mof"],
			"future_rule": {"x": 1}
		}`, true, nil},
		{"relaxes a bool", `{"main_protected": false}`, false, []string{RuleMainProtected}},
		{"relaxes an int", `{"release_min_reviewers": 1}`, false, []string{RuleReleaseMinReviewers}},
		{"narrows the list", `{"required_schema_profiles": []}`, false, []string{RuleRequiredSchemaProfiles}},
		{"rewrites an unknown rule", `{"future_rule": {"x": 2}}`, false, []string{"future_rule"}},
		{"relaxes two at once", `{"main_protected": false, "release_min_reviewers": 1}`, false, []string{RuleMainProtected, RuleReleaseMinReviewers}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateProjectPolicy(org, mustPolicy(t, tc.project))
			if tc.ok {
				if err != nil {
					t.Fatalf("ValidateProjectPolicy = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("ValidateProjectPolicy = nil, want violation")
			}
			var mv *MergeViolations
			if !errors.As(err, &mv) {
				t.Fatalf("error is %T, want *MergeViolations: %v", err, err)
			}
			if len(mv.Violations) != len(tc.keys) {
				t.Fatalf("violations = %d (%v), want %d", len(mv.Violations), mv.Violations, len(tc.keys))
			}
			for i, v := range mv.Violations {
				if v.Key != tc.keys[i] {
					t.Errorf("violation[%d].Key = %q, want %q", i, v.Key, tc.keys[i])
				}
			}
		})
	}
}

func TestValidateProjectPolicyProjectOnlyRulesAlwaysAllowed(t *testing.T) {
	org := mustPolicy(t, `{"main_protected": true}`)
	project := mustPolicy(t, `{"release_min_reviewers": 5, "raw_data_retention_days": 90}`)
	if err := ValidateProjectPolicy(org, project); err != nil {
		t.Errorf("project-only rules must be allowed: %v", err)
	}
	// And against an empty org policy anything goes.
	if err := ValidateProjectPolicy(EmptyPolicy(), project); err != nil {
		t.Errorf("empty org policy must accept anything: %v", err)
	}
}

func TestMergeEffectiveOrgIsLowerBound(t *testing.T) {
	org := mustPolicy(t, `{
		"main_protected": true,
		"release_min_reviewers": 2,
		"required_schema_profiles": ["core"],
		"org_only": true
	}`)
	project := mustPolicy(t, `{
		"main_protected": true,
		"release_min_reviewers": 3,
		"required_schema_profiles": ["core","mof"],
		"project_only": 42
	}`)
	eff := MergeEffective(org, project)
	want := map[string]any{
		"main_protected":           true,
		"release_min_reviewers":    3.0, // the stricter wins
		"required_schema_profiles": []any{"core", "mof"},
		"org_only":                 true,
		"project_only":             42.0,
	}
	got := map[string]any{}
	if err := json.Unmarshal(mustMarshal(t, eff), &got); err != nil {
		t.Fatalf("effective policy: %v", err)
	}
	if !jsonEqual(mustRaw(want), mustRaw(got)) {
		t.Errorf("effective = %v, want %v", got, want)
	}
}

func TestMergeEffectiveOrgWinsOnConflict(t *testing.T) {
	// The org policy tightened after the project version was written: the
	// project's stored value is weaker — the lower bound wins, evaluation
	// never errors.
	org := mustPolicy(t, `{"release_min_reviewers": 4}`)
	project := mustPolicy(t, `{"release_min_reviewers": 2}`)
	eff := MergeEffective(org, project)
	v, ok := eff.Rule(RuleReleaseMinReviewers)
	if !ok {
		t.Fatal("effective policy lost the rule")
	}
	if string(v) != "4" {
		t.Errorf("effective rule = %s, want 4 (org lower bound)", v)
	}
	// Unknown rule rewritten: org value stays authoritative.
	org2 := mustPolicy(t, `{"future_rule": {"x": 9}}`)
	project2 := mustPolicy(t, `{"future_rule": {"x": 1}}`)
	eff2 := MergeEffective(org2, project2)
	v2, _ := eff2.Rule("future_rule")
	if !jsonEqual(v2, mustRaw(map[string]any{"x": 9.0})) {
		t.Errorf("unknown-rule conflict: got %s, want org value", v2)
	}
}

func mustMarshal(t *testing.T, p Policy) []byte {
	t.Helper()
	b, err := p.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	return b
}

func TestValidPolicyVersion(t *testing.T) {
	cases := []struct {
		v    string
		want bool
	}{
		{"1", true},
		{"v1", true},
		{"1.0", true},
		{"2026-01-13", true},
		{"draft_policy-2", true},
		{"", false},
		{"   ", false},
		{"has space", false},
		{"sl/ash", false},
	}
	for _, tc := range cases {
		if got := ValidPolicyVersion(tc.v); got != tc.want {
			t.Errorf("ValidPolicyVersion(%q) = %v, want %v", tc.v, got, tc.want)
		}
	}
}

func TestPolicyScopeValid(t *testing.T) {
	if (PolicyScope{OrganizationID: "o"}).Valid() != true {
		t.Error("org scope must be valid")
	}
	if (PolicyScope{ProjectID: "p"}).Valid() != true {
		t.Error("project scope must be valid")
	}
	if (PolicyScope{}).Valid() {
		t.Error("empty scope must be invalid")
	}
	if (PolicyScope{OrganizationID: "o", ProjectID: "p"}).Valid() {
		t.Error("both-set scope must be invalid")
	}
}

func TestRuleKindOf(t *testing.T) {
	cases := []struct {
		key  string
		want RuleKind
	}{
		{RuleMainProtected, RuleKindBool},
		{RulePublicAssetIPReview, RuleKindBool},
		{RuleReleaseMinReviewers, RuleKindInt},
		{RuleRawDataRetentionDays, RuleKindInt},
		{RuleRequiredSchemaProfiles, RuleKindStringList},
		{"unknown_key", RuleKindUnknown},
	}
	for _, tc := range cases {
		if got := RuleKindOf(tc.key); got != tc.want {
			t.Errorf("RuleKindOf(%q) = %v, want %v", tc.key, got, tc.want)
		}
	}
}
