package domain

import (
	"fmt"
	"sort"
	"strings"
)

// The required-review calculation (T0604). Two places in the code hand
// this over by name:
//
//   - internal/application/reviews/service.go: "It does NOT decide when
//     the PR is approved: that needs the required-review calculation
//     (T0604: which responsibilities/kinds are required for which
//     changes), which is deliberately not invented here";
//   - internal/persistence/review_store.go: "The approved half is
//     deliberately absent: deciding WHEN the dimensions add up to an
//     approval is T0604's required-review calculation".
//
// The calculation is a PURE function of recorded facts — the changes the
// proposal carries, the project's Research Owners rules (docs/04 §3) and
// two project-policy rules (docs/12 §5) — and it is the same function for
// every write path (the review projection, and any later read that wants
// to show why a PR is not merge_ready yet). Everything about it is
// fail-closed: a change no rule routes is a change nobody is answerable
// for, and an unanswered change is never an approval.
//
// What is required, precisely:
//
//  1. Routing (docs/04 §3). For every changed object, the rules whose
//     (kind, value) match the change — object type, schema reference or
//     payload domain — name the responsibility labels answerable for it,
//     and each of them must sign the change's SCIENTIFIC review (docs/09
//     §5: "Scientific Review：科学方法/结论判断" — a per-change judgment
//     by the reviewer responsible for that change).
//
//  2. Integrity (docs/09 §5, docs/11 §1). The proposal as a whole needs
//     one INTEGRITY approval ("Integrity Review：schema、provenance、
//     dependency、rights、hash、visibility、required fields" — a judgment
//     about the proposal, not about one object) signed under any
//     responsibility the routing resolved for the proposal.
//
//  3. `release_min_reviewers` (docs/12 §5). A merge into the released
//     lineage — the project's main branch, the state a release is cut
//     from — carries at least the policy's number of distinct approving
//     reviewers. The value comes from the project's effective policy; an
//     absent rule adds nothing (the structural requirements above still
//     stand), and a rule that cannot be evaluated is an error the caller
//     fails closed on.
//
//  4. `public_asset_ip_review` (docs/12 §5). When the project's effective
//     policy requires it and the project is public — docs/12 §2: a public
//     project's accepted RSG is publicly visible, so every object it
//     accepts is a public asset — the integrity judgment is required PER
//     CHANGE under that change's own routed labels instead of once for
//     the proposal. This is the IP review the policy names: rights and
//     visibility are integrity-review subject matter (docs/09 §5), and
//     the responsible reviewer of each change is the one who signs it.
//     An unrouted change is then unsatisfiable rather than merely
//     unsigned, which is the fail-closed direction docs/12 §5 requires
//     ("Project policy 可更严格，不能静默放宽").
//
// Absent configuration is never a pass: a change with no matching rule
// (including a project with no rules at all) leaves the proposal
// unsatisfiable, and EvaluateRequiredReviews then never reports
// Satisfied — whatever the reviews say. That is the same discipline
// T0601 applied to the frozen-main flag and internal/authz applies to
// unregistered actions.

// ReviewSubject is one changed object as the routing sees it: the
// identity of the change plus the three values a Research Owners rule may
// match (docs/04 §3).
type ReviewSubject struct {
	// ObjectID names the changed object (the manifest's container id).
	ObjectID string
	// ObjectType is the object's scientific type (e.g. "protocol").
	ObjectType string
	// SchemaID is the schema reference the proposed version pins: a
	// canonical type schema id or a project schema profile id. Empty when
	// the version carries none (which matches no schema rule).
	SchemaID string
	// Domain is the proposed version payload's `domain` field. Empty when
	// the payload has none (which matches no domain rule — an absent
	// field is not a wildcard).
	Domain string
}

// Matches reports whether the rule routes this change.
func (s ReviewSubject) Matches(rule ResearchOwnerRule) bool {
	switch rule.MatchKind {
	case ResearchOwnerMatchObjectType:
		return s.ObjectType != "" && s.ObjectType == rule.MatchValue
	case ResearchOwnerMatchSchema:
		return s.SchemaID != "" && s.SchemaID == rule.MatchValue
	case ResearchOwnerMatchDomain:
		return s.Domain != "" && s.Domain == rule.MatchValue
	}
	return false
}

// Describe renders the change for a human reading a refusal (the PR page
// and the logs both show it).
func (s ReviewSubject) Describe() string {
	parts := []string{"object " + s.ObjectID}
	if s.ObjectType != "" {
		parts = append(parts, "type "+s.ObjectType)
	}
	if s.SchemaID != "" {
		parts = append(parts, "schema "+s.SchemaID)
	}
	if s.Domain != "" {
		parts = append(parts, "domain "+s.Domain)
	}
	return strings.Join(parts, ", ")
}

// ReviewRequirement is one approval the proposal needs: one review
// dimension signed under one responsibility.
type ReviewRequirement struct {
	// Kind is the review dimension that must be approved.
	Kind ReviewKind
	// Responsibility is the label the approving review must have been
	// recorded under. Empty means "any responsibility routed for this
	// proposal" — the whole-proposal integrity judgment (see the package
	// comment), which is why RequiredReviews carries the routed label
	// universe beside it.
	Responsibility string
	// Origin names where the requirement comes from: the change it
	// routes, or the policy rule that added it. It is shown to a reader
	// asking why the PR is not approved.
	Origin string
}

// String renders the requirement for a refusal message.
func (r ReviewRequirement) String() string {
	label := r.Responsibility
	if label == "" {
		label = "any routed responsibility"
	}
	return fmt.Sprintf("%s review by %q (%s)", r.Kind, label, r.Origin)
}

// RequiredReviews is one proposal's required-review calculation, about
// ONE head: the state the proposal currently offers for review. It travels
// with the head it was computed for, so a projection that evaluates it can
// tell a calculation about the proposal it is deciding from a stale one
// computed before the head moved — and a stale calculation never advances
// anything.
type RequiredReviews struct {
	// HeadStateID is the proposed head this calculation is about. Empty
	// means "no head": nothing can be approved against it.
	HeadStateID string
	// Requirements are the approvals the proposal needs, in a
	// deterministic order.
	Requirements []ReviewRequirement
	// RoutedLabels is every responsibility label the routing resolved for
	// the proposal (sorted, deduplicated). It is the universe an
	// unspecified Responsibility draws from.
	RoutedLabels []string
	// Unrouted names every change no rule routed (sorted). A non-empty
	// list makes the proposal unsatisfiable — nobody is answerable for
	// those changes, so no approval can stand for them.
	Unrouted []string
	// ReleaseMerge reports that the PR's target is the released lineage,
	// so MinApprovals applies.
	ReleaseMerge bool
	// MinApprovals is the policy's release_min_reviewers (0 when the
	// policy does not set it, or the PR is not a release merge).
	MinApprovals int
	// PublicAssetIPReview reports that the project's policy requires an
	// IP review of public assets and the project is public, so the
	// integrity judgment was expanded per change.
	PublicAssetIPReview bool
}

// Satisfiable reports whether the calculation can ever be met: at least
// one requirement, and every change routed. A project with no rules (or a
// change with no matching rule) is unsatisfiable, which is what makes
// "no configuration" refuse instead of approve.
//
// It is the precondition, not the verdict: EvaluateRequiredReviews —
// the function that decides, and whose ReviewProgress.Satisfied the
// submission projection advances on — asks it first and refuses outright
// when it is false, whatever the reviews say. It is deliberately the ONLY
// encoding of that rule: a copy of it beside the deciding function would
// be free to drift from this one.
func (r RequiredReviews) Satisfiable() bool {
	return len(r.Requirements) > 0 && len(r.Unrouted) == 0
}

// RequiredReviewInput carries the facts the calculation runs on.
type RequiredReviewInput struct {
	// Subjects are the changes the proposal carries.
	Subjects []ReviewSubject
	// Rules are the project's Research Owners rules.
	Rules []ResearchOwnerRule
	// HeadStateID is the proposed head the calculation is about (the PR's
	// pinned proposed state at the moment the calculation ran).
	HeadStateID string
	// ReleaseMerge is true when the PR targets the released lineage.
	ReleaseMerge bool
	// MinApprovals is the effective policy's release_min_reviewers (0
	// when unset).
	MinApprovals int
	// PublicAssetIPReview is the effective policy's public_asset_ip_review
	// and the project's public visibility, already composed by the caller
	// (both must hold).
	PublicAssetIPReview bool
}

// BuildRequiredReviews computes the proposal's requirements. Pure: the
// same facts always produce the same calculation, which is what lets the
// projection evaluate it inside the submission transaction and a read
// render it later.
func BuildRequiredReviews(in RequiredReviewInput) RequiredReviews {
	out := RequiredReviews{
		HeadStateID:         in.HeadStateID,
		Unrouted:            []string{},
		Requirements:        []ReviewRequirement{},
		RoutedLabels:        []string{},
		ReleaseMerge:        in.ReleaseMerge,
		PublicAssetIPReview: in.PublicAssetIPReview,
	}
	if in.ReleaseMerge && in.MinApprovals > 0 {
		out.MinApprovals = in.MinApprovals
	}
	labelSet := map[string]bool{}
	requirementSet := map[ReviewRequirement]bool{}
	add := func(r ReviewRequirement) {
		if requirementSet[r] {
			return
		}
		requirementSet[r] = true
		out.Requirements = append(out.Requirements, r)
	}
	for _, subject := range in.Subjects {
		var matched []ResearchOwnerRule
		for _, rule := range in.Rules {
			// A rule with no responsibility names nobody answerable: the
			// migration's CHECK (00084) makes one unreachable from storage,
			// and here it is refused the same way — the change counts as
			// unrouted rather than as needing a review by "".
			if rule.Responsibility != "" && subject.Matches(rule) {
				matched = append(matched, rule)
			}
		}
		if len(matched) == 0 {
			// Fail closed: an unrouted change is nobody's review, and a
			// missing rule must never read as "no reviewer needed".
			out.Unrouted = append(out.Unrouted, subject.Describe())
			continue
		}
		for _, rule := range matched {
			labelSet[rule.Responsibility] = true
			add(ReviewRequirement{
				Kind:           ReviewKindScientific,
				Responsibility: rule.Responsibility,
				Origin:         fmt.Sprintf("%s match %q routes %s", rule.MatchKind, rule.MatchValue, subject.Describe()),
			})
			if out.PublicAssetIPReview {
				add(ReviewRequirement{
					Kind:           ReviewKindIntegrity,
					Responsibility: rule.Responsibility,
					Origin:         fmt.Sprintf("public asset IP review of %s (responsibility %q)", subject.Describe(), rule.Responsibility),
				})
			}
		}
	}
	if !out.PublicAssetIPReview && len(labelSet) > 0 {
		// The whole-proposal integrity judgment (docs/09 §5), once for the
		// proposal, signed under any responsibility the routing resolved.
		add(ReviewRequirement{
			Kind:   ReviewKindIntegrity,
			Origin: "proposal integrity review (docs/09 §5)",
		})
	}
	out.RoutedLabels = sortedKeys(labelSet)
	sort.Slice(out.Requirements, func(i, j int) bool {
		a, b := out.Requirements[i], out.Requirements[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Responsibility != b.Responsibility {
			return a.Responsibility < b.Responsibility
		}
		return a.Origin < b.Origin
	})
	sort.Strings(out.Unrouted)
	out.Unrouted = dedupeSorted(out.Unrouted)
	return out
}

// ReviewProgress is the verdict of one required-review calculation against
// the reviews recorded on the PR's current head.
type ReviewProgress struct {
	// Satisfied reports that every requirement is met and the release
	// minimum (if any) is reached.
	Satisfied bool
	// Missing lists the requirements no recorded approval meets.
	Missing []ReviewRequirement
	// Approvals counts the DISTINCT reviewers with an approving review
	// pinned to the head — the count release_min_reviewers bounds. A
	// reviewer who approved several dimensions counts once, and the
	// database's UNIQUE(pull_request_id, reviewer_id, review_kind,
	// reviewed_state_id) (migration 00061) keeps one person from
	// inflating it with a repeated decision.
	Approvals int
	// RequiredApprovals is the bound the count is measured against (0
	// when the policy sets none, or the PR is not a release merge).
	RequiredApprovals int
	// Unrouted repeats the calculation's unrouted changes, so a caller
	// can render the refusal without re-running the calculation.
	Unrouted []string
	// Reason is a one-line explanation of an unsatisfied verdict ("" when
	// Satisfied).
	Reason string
}

// EvaluateRequiredReviews decides whether the reviews recorded for the
// calculation's head satisfy it. Only approvals about that head count
// (docs/09 §5, migration 00061: a review is a judgment about ONE state — a
// decision about an earlier head is history, never a vote on the current
// proposal), and only approvals (a comment or a changes_requested decision
// approves nothing).
//
// An unrouted change or an empty requirement set can never be satisfied:
// missing configuration is not a pass. That half is the calculation's own
// Satisfiable() predicate, asked here rather than re-derived, so the
// refusal follows the calculation instead of a copy of its rule.
func EvaluateRequiredReviews(required RequiredReviews, reviews []Review) ReviewProgress {
	progress := ReviewProgress{
		Unrouted:          append([]string{}, required.Unrouted...),
		RequiredApprovals: required.MinApprovals,
		Missing:           []ReviewRequirement{},
	}
	headStateID := required.HeadStateID
	if headStateID == "" {
		progress.Reason = "the proposal has no head state to review"
		return progress
	}
	if !required.Satisfiable() {
		progress.Reason = unsatisfiableReason(required)
		return progress
	}
	approved := make([]Review, 0, len(reviews))
	reviewers := map[string]bool{}
	for _, r := range reviews {
		if r.Decision != ReviewDecisionApproved || r.ReviewedStateID != headStateID {
			continue
		}
		approved = append(approved, r)
		if r.ReviewerID != "" {
			reviewers[r.ReviewerID] = true
		}
	}
	progress.Approvals = len(reviewers)
	routed := stringSet(required.RoutedLabels)
	for _, req := range required.Requirements {
		met := false
		for _, r := range approved {
			if r.Kind != req.Kind {
				continue
			}
			if req.Responsibility == "" {
				// The whole-proposal judgment: any responsibility the
				// routing resolved, and never an empty label (a reviewer
				// who holds no responsibility did not sign under one).
				if r.Responsibility != "" && routed[r.Responsibility] {
					met = true
					break
				}
				continue
			}
			if r.Responsibility == req.Responsibility {
				met = true
				break
			}
		}
		if !met {
			progress.Missing = append(progress.Missing, req)
		}
	}
	switch {
	case len(progress.Missing) > 0:
		progress.Reason = fmt.Sprintf("%d required review(s) still missing: %s", len(progress.Missing), joinRequirements(progress.Missing))
	case progress.Approvals < progress.RequiredApprovals:
		progress.Reason = fmt.Sprintf("the policy requires %d approving reviewers, %d recorded (docs/12 §5 release_min_reviewers)",
			progress.RequiredApprovals, progress.Approvals)
	default:
		progress.Satisfied = true
	}
	return progress
}

// unsatisfiableReason explains a proposal no configuration can ever
// approve: an unrouted change named, or the empty requirement set. It is
// the refusal message of the Satisfiable() precondition above.
func unsatisfiableReason(required RequiredReviews) string {
	if len(required.Unrouted) > 0 {
		return fmt.Sprintf("no Research Owners rule routes %d change(s) (docs/04 §3); missing routing is never an approval: %s",
			len(required.Unrouted), strings.Join(required.Unrouted, "; "))
	}
	return "the proposal carries no change a rule routes (docs/04 §3); there is nothing to approve"
}

func joinRequirements(reqs []ReviewRequirement) string {
	parts := make([]string, 0, len(reqs))
	for _, r := range reqs {
		parts = append(parts, r.String())
	}
	return strings.Join(parts, "; ")
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func dedupeSorted(list []string) []string {
	if len(list) == 0 {
		return list
	}
	out := list[:1]
	for _, s := range list[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}
