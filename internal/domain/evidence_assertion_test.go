package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCanonicalEvidenceRelations(t *testing.T) {
	rels := CanonicalEvidenceRelations()
	if len(rels) != 9 {
		t.Fatalf("CanonicalEvidenceRelations() = %d relations, want exactly the 9 of docs/10 §4: %v", len(rels), rels)
	}
	seen := map[string]bool{}
	for _, s := range rels {
		if !ValidEvidenceRelation(s) {
			t.Errorf("canonical relation %q fails ValidEvidenceRelation", s)
		}
		if seen[s] {
			t.Errorf("canonical relation %q listed twice", s)
		}
		seen[s] = true
	}
}

func TestValidEvidenceRelation(t *testing.T) {
	valid := []string{"supports", "contradicts", "consistent_with", "inconsistent_with", "reproduces", "fails_to_reproduce", "validates", "challenges", "contextualizes"}
	for _, s := range valid {
		if !ValidEvidenceRelation(s) {
			t.Errorf("ValidEvidenceRelation(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "Supports", "refutes", "disproves", "correlates", "agrees_with", "proves"} {
		if ValidEvidenceRelation(s) {
			t.Errorf("ValidEvidenceRelation(%q) = true, want false", s)
		}
	}
}

func TestRelationStance(t *testing.T) {
	// The transparent bucket rule of docs/10 §8: a label, not a weight.
	want := map[string]EvidenceStance{
		"supports":           EvidenceStanceSupporting,
		"validates":          EvidenceStanceSupporting,
		"reproduces":         EvidenceStanceSupporting,
		"consistent_with":    EvidenceStanceSupporting,
		"contradicts":        EvidenceStanceContesting,
		"challenges":         EvidenceStanceContesting,
		"inconsistent_with":  EvidenceStanceContesting,
		"fails_to_reproduce": EvidenceStanceContesting,
		"contextualizes":     EvidenceStanceNeutral,
	}
	for rel, stance := range want {
		got, ok := RelationStance(EvidenceRelation(rel))
		if !ok || got != stance {
			t.Errorf("RelationStance(%q) = (%q, %v), want (%q, true)", rel, got, ok, stance)
		}
	}
	// Every canonical relation maps (the stance function must not leave
	// gaps for the nine), and nothing outside the nine maps.
	for _, rel := range CanonicalEvidenceRelations() {
		if _, ok := RelationStance(EvidenceRelation(rel)); !ok {
			t.Errorf("RelationStance(%q) = ok false, want a bucket for every canonical relation", rel)
		}
	}
	if _, ok := RelationStance("disproves"); ok {
		t.Error(`RelationStance("disproves") = ok true, want false for a non-canonical relation`)
	}
	if _, ok := RelationStance(""); ok {
		t.Error(`RelationStance("") = ok true, want false`)
	}
}

func TestRelationStanceIsALabelNotAScore(t *testing.T) {
	// The stance bucket must stay a bucket: a string label, three values,
	// nothing numeric, nothing ordered. Grouping a mixed set of assertions
	// (acceptance: support and refutation coexist) yields two buckets —
	// it never yields a number.
	mixed := []EvidenceRelation{
		EvidenceRelationSupports,
		EvidenceRelationContradicts,
		EvidenceRelationSupports,
		EvidenceRelationChallenges,
		EvidenceRelationContextualizes,
	}
	buckets := map[EvidenceStance]int{}
	for _, r := range mixed {
		s, ok := RelationStance(r)
		if !ok {
			t.Fatalf("RelationStance(%q) not ok", r)
		}
		buckets[s]++
	}
	if buckets[EvidenceStanceSupporting] != 2 || buckets[EvidenceStanceContesting] != 2 || buckets[EvidenceStanceNeutral] != 1 {
		t.Errorf("buckets = %v, want supporting:2 contesting:2 neutral:1 — both sides present, neither side erased", buckets)
	}
	// The model itself has no numeric surface: an EvidenceAssertion
	// marshals to the schema shape and carries no weight/score field.
	var a EvidenceAssertion
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal EvidenceAssertion: %v", err)
	}
	if strings.Contains(strings.ToLower(string(raw)), "score") || strings.Contains(strings.ToLower(string(raw)), "weight") {
		t.Errorf("EvidenceAssertion wire shape = %s, must carry no score/weight surface (docs/10 §4: V1 不自动赋数值权重)", raw)
	}
}

func TestValidEvidenceType(t *testing.T) {
	for _, s := range []string{"experimental", "computational", "dataset", "literature", "external_attestation", "other"} {
		if !ValidEvidenceType(s) {
			t.Errorf("ValidEvidenceType(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "Literature", "doi", "anecdote", "expert_opinion"} {
		if ValidEvidenceType(s) {
			t.Errorf("ValidEvidenceType(%q) = true, want false", s)
		}
	}
}

func TestValidEvidenceDirectness(t *testing.T) {
	for _, s := range []string{"direct", "indirect", "unknown"} {
		if !ValidEvidenceDirectness(s) {
			t.Errorf("ValidEvidenceDirectness(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "semi-direct", "partial", "Direct"} {
		if ValidEvidenceDirectness(s) {
			t.Errorf("ValidEvidenceDirectness(%q) = true, want false", s)
		}
	}
}

func TestValidEvidenceInferenceNature(t *testing.T) {
	for _, s := range []string{"observational", "associational", "causal", "mechanistic", "predictive", "descriptive", "unknown"} {
		if !ValidEvidenceInferenceNature(s) {
			t.Errorf("ValidEvidenceInferenceNature(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "correlational", "inferential", "Causal"} {
		if ValidEvidenceInferenceNature(s) {
			t.Errorf("ValidEvidenceInferenceNature(%q) = true, want false", s)
		}
	}
}

func TestValidEvidenceReviewState(t *testing.T) {
	for _, s := range []string{"unreviewed", "reviewed", "rejected"} {
		if !ValidEvidenceReviewState(s) {
			t.Errorf("ValidEvidenceReviewState(%q) = false, want true", s)
		}
	}
	// "accepted" is a Finding's assessment (docs/08 §Finding; the
	// finding schema's properties.assessment enum), NOT an evidence
	// review state (docs/43 does not carry it): the two vocabularies must
	// not blur.
	for _, s := range []string{"", "accepted", "approved", "Reviewed"} {
		if ValidEvidenceReviewState(s) {
			t.Errorf("ValidEvidenceReviewState(%q) = true, want false", s)
		}
	}
}

func validAssertion() EvidenceAssertion {
	return EvidenceAssertion{
		TargetVersionRef:   "claim-1@1",
		EvidenceVersionRef: "exp-7@2",
		Relation:           EvidenceRelationSupports,
		EvidenceType:       EvidenceTypeExperimental,
		Scope:              json.RawMessage(`{"population":"as-synthesized powder"}`),
		Directness:         EvidenceDirectnessDirect,
		InferenceNature:    EvidenceInferenceObservational,
		ReasoningNote:      "measured uptake tracks the claim",
		ReviewState:        EvidenceReviewUnreviewed,
	}
}

func TestEvidenceAssertionValidate(t *testing.T) {
	if err := validAssertion().Validate(); err != nil {
		t.Errorf("valid assertion failed Validate: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*EvidenceAssertion)
		want   string // substring of the joined error
	}{
		{"empty target pin", func(a *EvidenceAssertion) { a.TargetVersionRef = "" }, "target version pin"},
		{"blank target pin", func(a *EvidenceAssertion) { a.TargetVersionRef = "  " }, "target version pin"},
		{"empty evidence pin", func(a *EvidenceAssertion) { a.EvidenceVersionRef = "" }, "evidence version pin"},
		{"self pin", func(a *EvidenceAssertion) { a.EvidenceVersionRef = a.TargetVersionRef }, "same version"},
		{"non-canonical relation", func(a *EvidenceAssertion) { a.Relation = "disproves" }, "relation"},
		{"non-canonical evidence type", func(a *EvidenceAssertion) { a.EvidenceType = "doi" }, "evidence type"},
		{"non-canonical directness", func(a *EvidenceAssertion) { a.Directness = "semi" }, "directness"},
		{"non-canonical inference nature", func(a *EvidenceAssertion) { a.InferenceNature = "correlational" }, "inference nature"},
		{"empty review state", func(a *EvidenceAssertion) { a.ReviewState = "" }, "review state"},
		{"non-canonical review state", func(a *EvidenceAssertion) { a.ReviewState = "accepted" }, "review state"},
		{"string scope", func(a *EvidenceAssertion) { a.Scope = json.RawMessage(`"298 K"`) }, "scope"},
		{"array scope", func(a *EvidenceAssertion) { a.Scope = json.RawMessage(`[{"name":"temperature"}]`) }, "scope"},
		{"null scope", func(a *EvidenceAssertion) { a.Scope = json.RawMessage(`null`) }, "scope"},
		{"malformed scope", func(a *EvidenceAssertion) { a.Scope = json.RawMessage(`{"population":`) }, "scope"},
	}
	for _, tc := range cases {
		a := validAssertion()
		tc.mutate(&a)
		err := a.Validate()
		if err == nil {
			t.Errorf("%s: Validate() = nil, want error containing %q", tc.name, tc.want)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Validate() = %v, want it to contain %q", tc.name, err, tc.want)
		}
	}
}

func TestEvidenceAssertionValidateCollectsEveryProblem(t *testing.T) {
	a := EvidenceAssertion{} // every required field empty
	err := a.Validate()
	if err == nil {
		t.Fatal("Validate() of the zero assertion = nil, want errors")
	}
	// errors.Join collects: the zero assertion violates at least the two
	// pins, the relation, the evidence type and the review state — all of
	// them must be reported, not just the first.
	for _, want := range []string{"target version pin", "evidence version pin", "relation", "evidence type", "review state"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("joined error %q does not mention %q", err, want)
		}
	}
}

func TestEvidenceAssertionValidateBlankPinsAreNotASelfPin(t *testing.T) {
	// Two blank pins are two missing pins, not a self-reference: the
	// joined error must not mention "same version".
	a := EvidenceAssertion{Relation: EvidenceRelationSupports, EvidenceType: EvidenceTypeOther, ReviewState: EvidenceReviewUnreviewed}
	err := a.Validate()
	if err == nil {
		t.Fatal("Validate() with two blank pins = nil, want errors")
	}
	if !strings.Contains(err.Error(), "target version pin") || !strings.Contains(err.Error(), "evidence version pin") {
		t.Errorf("joined error %q must report both missing pins", err)
	}
	if strings.Contains(err.Error(), "same version") {
		t.Errorf("joined error %q reports a self-pin for two blank pins — blank is missing, not identical", err)
	}
}

func TestEvidenceAssertionOptionalFields(t *testing.T) {
	// directness, inference_nature and scope are optional per the schema:
	// absent (not declared) is legal; the storage layer defaults the two
	// enums to unknown and the scope to {}.
	a := validAssertion()
	a.Directness = ""
	a.InferenceNature = ""
	a.Scope = nil
	if err := a.Validate(); err != nil {
		t.Errorf("assertion without directness/inference/scope failed Validate: %v", err)
	}
	// Declared "unknown" is also canonical (the default the storage layer
	// writes).
	a.Directness = EvidenceDirectnessUnknown
	a.InferenceNature = EvidenceInferenceUnknown
	if err := a.Validate(); err != nil {
		t.Errorf("assertion with unknown directness/inference failed Validate: %v", err)
	}
}

func TestEvidenceAssertionRoundTrip(t *testing.T) {
	// EvidenceAssertion is an INTERNAL model, not a wire DTO (see the
	// struct comment): it carries no json tags, so encoding/json round
	// trips it under Go field names. This test pins the in-process round
	// trip of every field, with a scope present — and pins the untagged
	// shape itself, so the day the model gains schema-shaped tags (i.e.
	// becomes a DTO on a JSON surface) this test says so and the struct
	// comment gets updated with it.
	//
	// Known wrinkle, deliberately not papered over here: a nil scope
	// marshals to "Scope":null and comes back as the literal bytes
	// "null", which Validate rejects (a present-but-null scope is not an
	// object — the same reading as Claim's ParseClaimScope). No storage
	// path produces that shape: evidence_assertions.scope is NOT NULL
	// DEFAULT '{}'.
	a := validAssertion()
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), `"target_version_ref"`) {
		t.Errorf("marshaled %s uses the schema's snake_case names — the model gained json tags; update the struct comment and extend this test to pin them", raw)
	}
	var back EvidenceAssertion
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Relation != a.Relation || back.EvidenceType != a.EvidenceType ||
		back.TargetVersionRef != a.TargetVersionRef || back.EvidenceVersionRef != a.EvidenceVersionRef ||
		back.ReviewState != a.ReviewState || back.InferenceNature != a.InferenceNature ||
		back.Directness != a.Directness || string(back.Scope) != string(a.Scope) {
		t.Errorf("round-trip = %+v, want %+v", back, a)
	}
	if err := back.Validate(); err != nil {
		t.Errorf("round-tripped assertion failed Validate: %v", err)
	}

	// The wrinkle above, pinned as behavior rather than prose: nil scope
	// does NOT survive as nil, and the round-tripped value does not
	// validate — a caller that means "undeclared" must leave Scope unset
	// on the model, never pass it through JSON with a nil value.
	undeclared := validAssertion()
	undeclared.Scope = nil
	rawNil, err := json.Marshal(undeclared)
	if err != nil {
		t.Fatalf("marshal (nil scope): %v", err)
	}
	var backNil EvidenceAssertion
	if err := json.Unmarshal(rawNil, &backNil); err != nil {
		t.Fatalf("unmarshal (nil scope): %v", err)
	}
	if backNil.Scope == nil {
		t.Errorf("nil scope round-tripped to nil; the probe says it becomes %q — behavior changed, update this test and the struct comment", backNil.Scope)
	} else if err := backNil.Validate(); err == nil {
		t.Errorf("round-tripped nil scope (%q) validates; the documented wrinkle changed", backNil.Scope)
	}
}

func TestSupportAndRefutationCoexist(t *testing.T) {
	// Acceptance criterion: 支持与反驳可同时存在. The same evidence
	// version may carry both a supporting and a contradicting assertion
	// on the same target version — the model holds both edges side by
	// side and validates both; nothing collapses them (no stance
	// uniqueness, no net-position arithmetic).
	support := validAssertion()
	support.Relation = EvidenceRelationSupports
	refute := validAssertion()
	refute.Relation = EvidenceRelationContradicts
	refute.ReviewState = EvidenceReviewReviewed
	if err := support.Validate(); err != nil {
		t.Fatalf("supporting assertion failed Validate: %v", err)
	}
	if err := refute.Validate(); err != nil {
		t.Fatalf("contradicting assertion failed Validate: %v", err)
	}
	if support.TargetVersionRef != refute.TargetVersionRef || support.EvidenceVersionRef != refute.EvidenceVersionRef {
		t.Fatalf("test bug: the two assertions must name the same pair")
	}
	stanceOf := func(a EvidenceAssertion) EvidenceStance {
		s, ok := RelationStance(a.Relation)
		if !ok {
			t.Fatalf("RelationStance(%q) not ok", a.Relation)
		}
		return s
	}
	if got, want := stanceOf(support), EvidenceStanceSupporting; got != want {
		t.Errorf("support stance = %q, want %q", got, want)
	}
	if got, want := stanceOf(refute), EvidenceStanceContesting; got != want {
		t.Errorf("refute stance = %q, want %q", got, want)
	}
}
