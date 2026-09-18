package domain

import (
	"sort"
	"strings"
	"testing"
)

// The required-review calculation's unit proofs (T0604). The integration
// test (tests/integration/review_routing_test.go) drives the same
// calculation through the real product path over PostgreSQL; these cases
// pin the edges that are hard to reach there — every match kind, the
// union of several matching rules, the policy-driven branches, and the
// fail-closed answers that must never be reached by accident.

func rule(kind ResearchOwnerMatchKind, value, label string) ResearchOwnerRule {
	return ResearchOwnerRule{MatchKind: kind, MatchValue: value, Responsibility: label}
}

func requirementStrings(reqs []ReviewRequirement) []string {
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, r.String())
	}
	return out
}

// signatures renders the requirements as an order-independent set of
// "kind|responsibility" — the identity that decides WHICH approval a
// requirement needs. Origin is deliberately excluded: it is provenance
// for a human reading a refusal, not part of the identity. (The
// calculation keeps one entry per change, so the same label appears once
// per routed change; the set collapses that.)
func signatures(reqs []ReviewRequirement) []string {
	set := map[string]bool{}
	for _, r := range reqs {
		set[string(r.Kind)+"|"+r.Responsibility] = true
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func requireSignatureSet(t *testing.T, got []ReviewRequirement, want ...string) {
	t.Helper()
	sort.Strings(want)
	gotSet := signatures(got)
	if strings.Join(gotSet, ",") != strings.Join(want, ",") {
		t.Fatalf("requirement set = %v (%v), want %v", gotSet, requirementStrings(got), want)
	}
}

// TestBuildRequiredReviewsRoutesEveryChangeByRule: one protocol change,
// routed by object_type — exactly the acceptance shape ("protocol change
// automatically requires the corresponding reviewer"): the change needs a
// scientific approval under the rule's label, and the proposal needs its
// integrity judgment. The routing came from the rule data, not from an
// `if` on the object type: the same input under a rule for another value
// produces no requirement at all (asserted below).
func TestBuildRequiredReviewsRoutesEveryChangeByRule(t *testing.T) {
	subject := ReviewSubject{
		ObjectID:   "obj-1",
		ObjectType: "protocol",
		SchemaID:   "https://open-rd.example/schemas/protocol.schema.json",
		Domain:     "materials",
	}
	got := BuildRequiredReviews(RequiredReviewInput{
		HeadStateID: "head-1",
		Subjects:    []ReviewSubject{subject},
		Rules:       []ResearchOwnerRule{rule(ResearchOwnerMatchObjectType, "protocol", "IP Reviewer")},
	})
	if !got.Satisfiable() {
		t.Fatalf("calculation = %+v, want satisfiable", got)
	}
	if len(got.Requirements) != 2 {
		t.Fatalf("requirements = %v, want the routed scientific review and the proposal integrity review", requirementStrings(got.Requirements))
	}
	requireSignatureSet(t, got.Requirements, "scientific|IP Reviewer", "integrity|")
	if len(got.RoutedLabels) != 1 || got.RoutedLabels[0] != "IP Reviewer" {
		t.Fatalf("routed labels = %v, want [IP Reviewer]", got.RoutedLabels)
	}
	if len(got.Unrouted) != 0 {
		t.Fatalf("unrouted = %v, want none", got.Unrouted)
	}
	for _, req := range got.Requirements {
		if req.Kind == ReviewKindScientific {
			if !strings.Contains(req.Origin, "object_type") || !strings.Contains(req.Origin, "obj-1") {
				t.Fatalf("requirement origin = %q, want it to name the matching rule and the change", req.Origin)
			}
		}
	}

	// The same change under a rule for a DIFFERENT type routes nothing —
	// the answer tracks the project's data, not the object type's name.
	other := BuildRequiredReviews(RequiredReviewInput{
		HeadStateID: "head-1",
		Subjects:    []ReviewSubject{subject},
		Rules:       []ResearchOwnerRule{rule(ResearchOwnerMatchObjectType, "dataset", "IP Reviewer")},
	})
	if other.Satisfiable() {
		t.Fatalf("calculation = %+v, want unsatisfiable (no rule routes a protocol change)", other)
	}
	if len(other.Unrouted) != 1 || !strings.Contains(other.Unrouted[0], "obj-1") {
		t.Fatalf("unrouted = %v, want the unrouted protocol change named", other.Unrouted)
	}
}

// TestBuildRequiredReviewsMatchKinds: the three documented match kinds
// (docs/04 §3 — object type, schema, domain) each route, and each matches
// ONLY its own field: a domain rule never fires on a change whose payload
// has no domain (an absent field is not a wildcard), and a schema rule
// matches the exact pinned schema id (a canonical type schema or a
// project profile id — the ids the version row stores).
func TestBuildRequiredReviewsMatchKinds(t *testing.T) {
	subject := ReviewSubject{
		ObjectID:   "obj-1",
		ObjectType: "experiment",
		SchemaID:   "project:11111111-1111-1111-1111-111111111111:experiment_ext",
		Domain:     "catalysis",
	}
	cases := []struct {
		name  string
		rule  ResearchOwnerRule
		match bool
	}{
		{"object_type matches", rule(ResearchOwnerMatchObjectType, "experiment", "L"), true},
		{"object_type mismatches the schema", rule(ResearchOwnerMatchObjectType, "protocol", "L"), false},
		{"schema matches the pinned profile", rule(ResearchOwnerMatchSchema, subject.SchemaID, "L"), true},
		{"schema mismatches the canonical id", rule(ResearchOwnerMatchSchema, "https://open-rd.example/schemas/experiment.schema.json", "L"), false},
		{"domain matches the payload field", rule(ResearchOwnerMatchDomain, "catalysis", "L"), true},
		{"domain mismatches", rule(ResearchOwnerMatchDomain, "materials", "L"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := subject.Matches(tc.rule); got != tc.match {
				t.Fatalf("Matches(%+v) = %v, want %v", tc.rule, got, tc.match)
			}
		})
	}

	// A change with no domain and no schema matches neither rule kind —
	// absence is never a wildcard.
	blank := ReviewSubject{ObjectID: "obj-2", ObjectType: "claim"}
	if blank.Matches(rule(ResearchOwnerMatchDomain, "catalysis", "L")) {
		t.Fatal("a domain rule matched a change with no domain")
	}
	if blank.Matches(rule(ResearchOwnerMatchSchema, "", "L")) {
		t.Fatal("a schema rule with an empty value matched a change with no schema")
	}
	// An unknown match kind routes nothing (the table CHECK refuses one,
	// so this is defence in depth).
	if blank.Matches(rule("owner", "alice", "L")) {
		t.Fatal("an unknown match kind routed a change")
	}
}

// TestBuildRequiredReviewsUnionsMatchingRules: several rules matching one
// change each contribute their label — the change requires every matched
// responsibility, not the first one the iteration happened to see.
func TestBuildRequiredReviewsUnionsMatchingRules(t *testing.T) {
	got := BuildRequiredReviews(RequiredReviewInput{
		HeadStateID: "head-1",
		Subjects: []ReviewSubject{{
			ObjectID:   "obj-1",
			ObjectType: "protocol",
			SchemaID:   "https://open-rd.example/schemas/protocol.schema.json",
			Domain:     "materials",
		}},
		Rules: []ResearchOwnerRule{
			rule(ResearchOwnerMatchObjectType, "protocol", "Experimental Reviewer"),
			rule(ResearchOwnerMatchDomain, "materials", "IP Reviewer"),
			rule(ResearchOwnerMatchSchema, "https://open-rd.example/schemas/protocol.schema.json", "Project Lead"),
			rule(ResearchOwnerMatchDomain, "catalysis", "Unrelated"),
		},
	})
	requireSignatureSet(t, got.Requirements,
		"scientific|Experimental Reviewer", "scientific|IP Reviewer", "scientific|Project Lead", "integrity|")
	want := []string{"Experimental Reviewer", "IP Reviewer", "Project Lead"}
	if len(got.RoutedLabels) != len(want) {
		t.Fatalf("routed labels = %v, want %v", got.RoutedLabels, want)
	}
	for i, label := range want {
		if got.RoutedLabels[i] != label {
			t.Fatalf("routed labels = %v, want %v", got.RoutedLabels, want)
		}
	}
	if strings.Contains(strings.Join(requirementStrings(got.Requirements), " "), "Unrelated") {
		t.Fatalf("a non-matching rule contributed a requirement: %v", requirementStrings(got.Requirements))
	}
}

// TestBuildRequiredReviewsUnroutedIsUnsatisfiable: a change no rule
// routes leaves the proposal unsatisfiable, and the unrouted list names
// it. This is the "missing configuration must not auto-approve" rule at
// the calculation level (it is asserted end to end in the integration
// test).
func TestBuildRequiredReviewsUnroutedIsUnsatisfiable(t *testing.T) {
	got := BuildRequiredReviews(RequiredReviewInput{
		HeadStateID: "head-1",
		Subjects: []ReviewSubject{
			{ObjectID: "obj-1", ObjectType: "protocol"},
			{ObjectID: "obj-2", ObjectType: "dataset"},
		},
		Rules: []ResearchOwnerRule{rule(ResearchOwnerMatchObjectType, "protocol", "L")},
	})
	if got.Satisfiable() {
		t.Fatalf("calculation = %+v, want unsatisfiable (one change unrouted)", got)
	}
	if len(got.Unrouted) != 1 || !strings.Contains(got.Unrouted[0], "obj-2") {
		t.Fatalf("unrouted = %v, want the dataset change named", got.Unrouted)
	}
	if p := EvaluateRequiredReviews(got, []Review{
		{Kind: ReviewKindScientific, Decision: ReviewDecisionApproved, Responsibility: "L", ReviewedStateID: "head-1", ReviewerID: "u-1"},
		{Kind: ReviewKindIntegrity, Decision: ReviewDecisionApproved, Responsibility: "L", ReviewedStateID: "head-1", ReviewerID: "u-2"},
	}); p.Satisfied {
		t.Fatalf("progress = %+v, want unsatisfied while a change is unrouted", p)
	}

	// No rules at all: every change unrouted, nothing required — and
	// "nothing required" must never read as "nothing to do".
	empty := BuildRequiredReviews(RequiredReviewInput{
		HeadStateID: "head-1",
		Subjects:    []ReviewSubject{{ObjectID: "obj-1", ObjectType: "protocol"}},
	})
	if empty.Satisfiable() || len(empty.Requirements) != 0 {
		t.Fatalf("calculation with no rules = %+v, want unsatisfiable and empty", empty)
	}
	p := EvaluateRequiredReviews(empty, []Review{
		{Kind: ReviewKindScientific, Decision: ReviewDecisionApproved, ReviewedStateID: "head-1", ReviewerID: "u-1"},
	})
	if p.Satisfied {
		t.Fatalf("progress with no rules = %+v, want unsatisfied", p)
	}
	if !strings.Contains(p.Reason, "Research Owners") {
		t.Fatalf("reason = %q, want it to name the missing routing", p.Reason)
	}

	// A proposal with no object changes at all (relation-only, or empty):
	// no requirement can be met, so it is never approved.
	none := BuildRequiredReviews(RequiredReviewInput{
		HeadStateID: "head-1",
		Rules:       []ResearchOwnerRule{rule(ResearchOwnerMatchObjectType, "protocol", "L")},
	})
	if none.Satisfiable() {
		t.Fatalf("calculation with no changes = %+v, want unsatisfiable", none)
	}
	if p := EvaluateRequiredReviews(none, []Review{{Decision: ReviewDecisionApproved, Kind: ReviewKindIntegrity, Responsibility: "L", ReviewedStateID: "head-1", ReviewerID: "u-1"}}); p.Satisfied {
		t.Fatalf("progress with no changes = %+v, want unsatisfied", p)
	}
}

// TestBuildRequiredReviewsPolicyBranches: the two policy rules the
// calculation consumes.
//
//   - release_min_reviewers is the minimum number of distinct approving
//     reviewers, and it applies only to a merge into the released
//     lineage (the project's main branch).
//   - public_asset_ip_review turns the single proposal-level integrity
//     judgment into a per-change one under each change's routed label —
//     the IP review of public assets (docs/12 §5, docs/09 §5: rights and
//     visibility are integrity subject matter).
func TestBuildRequiredReviewsPolicyBranches(t *testing.T) {
	subjects := []ReviewSubject{
		{ObjectID: "obj-1", ObjectType: "protocol"},
		{ObjectID: "obj-2", ObjectType: "protocol"},
	}
	rules := []ResearchOwnerRule{rule(ResearchOwnerMatchObjectType, "protocol", "L")}

	plain := BuildRequiredReviews(RequiredReviewInput{HeadStateID: "h", Subjects: subjects, Rules: rules})
	if plain.MinApprovals != 0 || plain.ReleaseMerge || plain.PublicAssetIPReview {
		t.Fatalf("non-release calculation = %+v, want no minimum and no IP expansion", plain)
	}
	requireSignatureSet(t, plain.Requirements, "scientific|L", "integrity|")

	release := BuildRequiredReviews(RequiredReviewInput{
		HeadStateID: "h", Subjects: subjects, Rules: rules, ReleaseMerge: true, MinApprovals: 2,
	})
	if !release.ReleaseMerge || release.MinApprovals != 2 {
		t.Fatalf("release calculation = %+v, want the policy minimum", release)
	}
	// A release merge with the policy unset carries no minimum.
	if unset := BuildRequiredReviews(RequiredReviewInput{HeadStateID: "h", Subjects: subjects, Rules: rules, ReleaseMerge: true}); unset.MinApprovals != 0 {
		t.Fatalf("release calculation without the rule = %+v, want no minimum", unset)
	}
	// A minimum without a release merge is not carried either (the policy
	// bounds merges into the released lineage, not every proposal).
	if notRelease := BuildRequiredReviews(RequiredReviewInput{HeadStateID: "h", Subjects: subjects, Rules: rules, MinApprovals: 3}); notRelease.MinApprovals != 0 {
		t.Fatalf("non-release calculation = %+v, want the minimum dropped", notRelease)
	}

	ip := BuildRequiredReviews(RequiredReviewInput{
		HeadStateID: "h", Subjects: subjects, Rules: rules, PublicAssetIPReview: true,
	})
	if !ip.PublicAssetIPReview {
		t.Fatalf("calculation = %+v, want the public-asset IP review flagged", ip)
	}
	// The observable difference: the integrity judgment is now per change
	// and under a routed label — there is no label-free proposal-level
	// requirement left. The scientific review of each change is required
	// either way; the policy ADDS the IP review, it does not replace it.
	requireSignatureSet(t, ip.Requirements, "scientific|L", "integrity|L")
	if len(ip.Requirements) != 4 {
		t.Fatalf("requirements = %v, want a scientific and an integrity requirement per public change", requirementStrings(ip.Requirements))
	}
	for _, req := range ip.Requirements {
		if req.Responsibility == "" {
			t.Fatalf("requirement = %+v, want every public-asset requirement to name a routed label", req)
		}
	}

	// The public-asset policy does not invent a reviewer: with the
	// changes unrouted the proposal still requires nothing and approves
	// nothing (the policy sharpens what an approval must look like, it
	// does not create the routing).
	unrouted := BuildRequiredReviews(RequiredReviewInput{
		HeadStateID: "h", Subjects: subjects, PublicAssetIPReview: true,
	})
	if unrouted.Satisfiable() {
		t.Fatalf("calculation = %+v, want unsatisfiable", unrouted)
	}
}

// TestEvaluateRequiredReviewsCountsOnlyApprovalsAtTheHead: a review is a
// judgment about ONE state (docs/09 §5, migration 00061): only approvals
// pinned to the head the calculation is about count — an earlier head's
// approval, a comment or a changes_requested decision approves nothing,
// and an approval with no label never satisfies a routed requirement.
func TestEvaluateRequiredReviewsCountsOnlyApprovalsAtTheHead(t *testing.T) {
	required := BuildRequiredReviews(RequiredReviewInput{
		HeadStateID: "head-2",
		Subjects:    []ReviewSubject{{ObjectID: "obj-1", ObjectType: "protocol"}},
		Rules:       []ResearchOwnerRule{rule(ResearchOwnerMatchObjectType, "protocol", "L")},
	})
	approve := func(kind ReviewKind, label, head, reviewer string) Review {
		return Review{Kind: kind, Decision: ReviewDecisionApproved, Responsibility: label, ReviewedStateID: head, ReviewerID: reviewer}
	}

	// Everything approved at the head, under the routed label: satisfied.
	full := []Review{
		approve(ReviewKindScientific, "L", "head-2", "u-1"),
		approve(ReviewKindIntegrity, "L", "head-2", "u-2"),
	}
	if p := EvaluateRequiredReviews(required, full); !p.Satisfied {
		t.Fatalf("progress = %+v, want satisfied", p)
	}

	// The same approvals about the PREVIOUS head: nothing carries over.
	stale := []Review{
		approve(ReviewKindScientific, "L", "head-1", "u-1"),
		approve(ReviewKindIntegrity, "L", "head-1", "u-2"),
	}
	if p := EvaluateRequiredReviews(required, stale); p.Satisfied || p.Approvals != 0 {
		t.Fatalf("stale progress = %+v, want unsatisfied with no approvals", p)
	}

	// A comment and a changes_requested are not approvals.
	notApprovals := []Review{
		{Kind: ReviewKindScientific, Decision: ReviewDecisionComment, Responsibility: "L", ReviewedStateID: "head-2", ReviewerID: "u-1"},
		{Kind: ReviewKindIntegrity, Decision: ReviewDecisionChangesRequested, Responsibility: "L", ReviewedStateID: "head-2", ReviewerID: "u-2"},
	}
	if p := EvaluateRequiredReviews(required, notApprovals); p.Satisfied {
		t.Fatalf("progress = %+v, want unsatisfied", p)
	}

	// An approval recorded under a label the routing did not resolve is
	// not the answerable reviewer's signature on the change: the
	// scientific requirement stays missing.
	wrongLabel := []Review{
		approve(ReviewKindScientific, "Someone Else", "head-2", "u-1"),
		approve(ReviewKindIntegrity, "L", "head-2", "u-2"),
	}
	p := EvaluateRequiredReviews(required, wrongLabel)
	if p.Satisfied || len(p.Missing) != 1 {
		t.Fatalf("wrong-label progress = %+v, want one missing requirement", p)
	}
	if !strings.Contains(p.Reason, "L") {
		t.Fatalf("reason = %q, want it to name the missing responsibility", p.Reason)
	}

	// An approval with NO label satisfies neither the routed requirement
	// nor the proposal-level integrity one (which draws from the routed
	// labels).
	unlabelled := []Review{
		approve(ReviewKindScientific, "L", "head-2", "u-1"),
		approve(ReviewKindIntegrity, "", "head-2", "u-2"),
	}
	if got := EvaluateRequiredReviews(required, unlabelled); got.Satisfied {
		t.Fatalf("progress = %+v, want the unlabelled integrity approval refused", got)
	}
}

// TestEvaluateRequiredReviewsMinimumCountsPeopleNotDimensions: the
// release minimum bounds DISTINCT reviewers, so one person approving both
// dimensions does not count twice; a repeated decision cannot inflate the
// count either (the database enforces one decision per person per kind
// per head — migration 00061 — and this pins the calculation's half).
func TestEvaluateRequiredReviewsMinimumCountsPeopleNotDimensions(t *testing.T) {
	required := BuildRequiredReviews(RequiredReviewInput{
		HeadStateID:  "head-1",
		Subjects:     []ReviewSubject{{ObjectID: "obj-1", ObjectType: "protocol"}},
		Rules:        []ResearchOwnerRule{rule(ResearchOwnerMatchObjectType, "protocol", "L")},
		ReleaseMerge: true,
		MinApprovals: 2,
	})
	one := []Review{
		{Kind: ReviewKindScientific, Decision: ReviewDecisionApproved, Responsibility: "L", ReviewedStateID: "head-1", ReviewerID: "u-1"},
		{Kind: ReviewKindIntegrity, Decision: ReviewDecisionApproved, Responsibility: "L", ReviewedStateID: "head-1", ReviewerID: "u-1"},
	}
	p := EvaluateRequiredReviews(required, one)
	if p.Satisfied {
		t.Fatalf("progress = %+v, want unsatisfied: one reviewer is one approval", p)
	}
	if p.Approvals != 1 || p.RequiredApprovals != 2 {
		t.Fatalf("progress = %+v, want 1 of 2 approvals", p)
	}
	if !strings.Contains(p.Reason, "release_min_reviewers") || !strings.Contains(p.Reason, "2") {
		t.Fatalf("reason = %q, want it to name the policy rule and the bound", p.Reason)
	}

	// A second person signing the proposal reaches the minimum.
	two := append([]Review{}, one...)
	two = append(two, Review{Kind: ReviewKindScientific, Decision: ReviewDecisionApproved, Responsibility: "L", ReviewedStateID: "head-1", ReviewerID: "u-2"})
	if p := EvaluateRequiredReviews(required, two); !p.Satisfied {
		t.Fatalf("progress = %+v, want satisfied with two distinct reviewers", p)
	}

	// Distinct reviewers, but one approved only at another head: the
	// count is about the head the calculation is for.
	mixed := append([]Review{}, one...)
	mixed = append(mixed, Review{Kind: ReviewKindScientific, Decision: ReviewDecisionApproved, Responsibility: "L", ReviewedStateID: "head-0", ReviewerID: "u-2"})
	if p := EvaluateRequiredReviews(required, mixed); p.Satisfied {
		t.Fatalf("progress = %+v, want unsatisfied: the second approval is about another head", p)
	}
}

// TestEvaluateRequiredReviewsRefusesWhatCanNeverBeMet pins the verdict of
// the DECIDING function — EvaluateRequiredReviews, the execution point the
// submission projection reads (internal/persistence/review_store.go) — on a
// calculation no approval can ever meet.
//
// It deliberately does NOT assert Satisfiable() first: an assertion on the
// predicate only proves the predicate, which is the trap this test closes.
// The fail-closed rule lives in Satisfiable(), the deciding function asks
// it, and breaking the predicate must therefore break this verdict.
func TestEvaluateRequiredReviewsRefusesWhatCanNeverBeMet(t *testing.T) {
	// Reviews that cover every requirement the calculations below carry, so
	// a verdict of "satisfied" could only come from the unmet precondition.
	approvals := []Review{
		{Kind: ReviewKindScientific, Decision: ReviewDecisionApproved, Responsibility: "L", ReviewedStateID: "head-1", ReviewerID: "u-1"},
		{Kind: ReviewKindIntegrity, Decision: ReviewDecisionApproved, Responsibility: "L", ReviewedStateID: "head-1", ReviewerID: "u-2"},
	}

	// One routed change, one unrouted: the requirements are fully signed,
	// and the proposal still can never be approved.
	unrouted := BuildRequiredReviews(RequiredReviewInput{
		HeadStateID: "head-1",
		Subjects: []ReviewSubject{
			{ObjectID: "obj-1", ObjectType: "protocol"},
			{ObjectID: "obj-2", ObjectType: "dataset"},
		},
		Rules: []ResearchOwnerRule{rule(ResearchOwnerMatchObjectType, "protocol", "L")},
	})
	if p := EvaluateRequiredReviews(unrouted, approvals); p.Satisfied {
		t.Fatalf("verdict = %+v, want unsatisfied: an unrouted change can never be approved", p)
	}

	// No rules at all: nothing is required — and nothing is thereby
	// approved. The empty requirement set is not a free pass.
	unconfigured := BuildRequiredReviews(RequiredReviewInput{
		HeadStateID: "head-1",
		Subjects:    []ReviewSubject{{ObjectID: "obj-1", ObjectType: "protocol"}},
	})
	if p := EvaluateRequiredReviews(unconfigured, approvals); p.Satisfied {
		t.Fatalf("verdict = %+v, want unsatisfied: missing configuration is never a pass", p)
	}
}

// TestEvaluateRequiredReviewsWithoutHead: a calculation with no head (the
// zero value a caller passes when the routing could not be resolved)
// approves nothing, whatever the reviews say.
func TestEvaluateRequiredReviewsWithoutHead(t *testing.T) {
	required := BuildRequiredReviews(RequiredReviewInput{
		Subjects: []ReviewSubject{{ObjectID: "obj-1", ObjectType: "protocol"}},
		Rules:    []ResearchOwnerRule{rule(ResearchOwnerMatchObjectType, "protocol", "L")},
	})
	reviews := []Review{
		{Kind: ReviewKindScientific, Decision: ReviewDecisionApproved, Responsibility: "L", ReviewerID: "u-1"},
		{Kind: ReviewKindIntegrity, Decision: ReviewDecisionApproved, Responsibility: "L", ReviewerID: "u-2"},
	}
	p := EvaluateRequiredReviews(required, reviews)
	if p.Satisfied {
		t.Fatalf("progress = %+v, want unsatisfied (no head)", p)
	}
	if !strings.Contains(p.Reason, "head") {
		t.Fatalf("reason = %q, want it to name the missing head", p.Reason)
	}
	if p := EvaluateRequiredReviews(RequiredReviews{}, reviews); p.Satisfied {
		t.Fatalf("zero calculation = %+v, want unsatisfied", p)
	}
}

// TestEvaluateRequiredReviewsUnlabelledRouting: a proposal whose rule
// carries no responsibility label (the column is CHECKed non-empty, so
// this is the zero-value rule a caller might construct) routes nothing.
// A rule that names nobody answerable is missing routing, not permission:
// the change is unrouted, no phantom label enters the calculation, and an
// approval that carries no label — or any label — cannot stand for it.
func TestEvaluateRequiredReviewsUnlabelledRouting(t *testing.T) {
	required := BuildRequiredReviews(RequiredReviewInput{
		HeadStateID: "head-1",
		Subjects:    []ReviewSubject{{ObjectID: "obj-1", ObjectType: "protocol"}},
		Rules:       []ResearchOwnerRule{rule(ResearchOwnerMatchObjectType, "protocol", "")},
	})
	if len(required.RoutedLabels) != 0 {
		t.Fatalf("routed labels = %v, want none (an empty label is not a responsibility)", required.RoutedLabels)
	}
	if len(required.Requirements) != 0 || len(required.Unrouted) != 1 {
		t.Fatalf("calculation = %+v, want the change unrouted and nothing required", required)
	}
	if required.Satisfiable() {
		t.Fatalf("calculation = %+v, want unsatisfiable", required)
	}
	unlabelled := []Review{
		{Kind: ReviewKindScientific, Decision: ReviewDecisionApproved, ReviewedStateID: "head-1", ReviewerID: "u-1"},
		{Kind: ReviewKindIntegrity, Decision: ReviewDecisionApproved, Responsibility: "L", ReviewedStateID: "head-1", ReviewerID: "u-2"},
	}
	if p := EvaluateRequiredReviews(required, unlabelled); p.Satisfied {
		t.Fatalf("progress = %+v, want unsatisfied: a rule naming nobody routes nobody", p)
	}
}

// TestResearchOwnerValidators: the two shapes the store's CHECK
// constraints carry (migration 00084) refuse blanks and untrimmed values
// in the domain layer too, so a project owner's typo — a match value with
// invisible whitespace — is a validation error rather than a rule that
// silently routes nothing.
func TestResearchOwnerValidators(t *testing.T) {
	for _, v := range []string{"protocol", "https://open-rd.example/schemas/protocol.schema.json", "project:abc:ext"} {
		if !ValidResearchOwnerMatchValue(v) {
			t.Fatalf("ValidResearchOwnerMatchValue(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"", " ", " protocol", "protocol ", strings.Repeat("x", MaxResponsibilityLabelLen+1)} {
		if ValidResearchOwnerMatchValue(v) {
			t.Fatalf("ValidResearchOwnerMatchValue(%q) = true, want false", v)
		}
	}
	for _, l := range []string{"Data Reviewer", "IP Reviewer"} {
		if !ValidResponsibilityLabel(l) {
			t.Fatalf("ValidResponsibilityLabel(%q) = false, want true", l)
		}
	}
	for _, l := range []string{"", "   ", strings.Repeat("x", MaxResponsibilityLabelLen+1)} {
		if ValidResponsibilityLabel(l) {
			t.Fatalf("ValidResponsibilityLabel(%q) = true, want false", l)
		}
	}
	for _, k := range []ResearchOwnerMatchKind{ResearchOwnerMatchObjectType, ResearchOwnerMatchSchema, ResearchOwnerMatchDomain} {
		if !ValidResearchOwnerMatchKind(k) {
			t.Fatalf("ValidResearchOwnerMatchKind(%q) = false, want true", k)
		}
	}
	if ValidResearchOwnerMatchKind("owner") {
		t.Fatal("ValidResearchOwnerMatchKind(owner) = true, want false")
	}
}
