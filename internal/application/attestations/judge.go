package attestations

import (
	"fmt"

	"github.com/lichman0405/post/internal/domain"
)

// Facts is everything the attestation decision reads, and every decision
// below is a pure function of it. It is resolved by the store — once
// outside a transaction for the preview, once inside the publish's own
// transaction, over the state that transaction sees — and that is what
// makes the preview and the publish the same decision: the publish re-runs
// these functions over its own view rather than trusting the preview's
// (docs/22 §7).
//
// It carries the private side, because the decision is made on it: which
// project attests, which of its own states the attestation rests on, which
// of its reviews authorised it. None of it reaches the public read — see
// Row and Present, whose type has no field for any of it.
type Facts struct {
	// ProjectID is the attesting project — the project issuing the
	// statement. It is the subject of the private-side rules below.
	ProjectID string
	// Target is the version the attestation would be about, with the facts
	// that decide whether it is admissible as a target.
	Target TargetFacts
	// Basis is the attesting project's own state the attestation rests on.
	Basis BasisFacts
	// Review is the internal review that authorised the statement.
	Review ReviewFacts
	// OrganizationID is the attesting project's organization, and is empty
	// for a personal project (projects.organization_id is nullable).
	OrganizationID string
	// OrganizationSetting is organizations.attestation_attribution as it
	// stands NOW, and is empty when the project has no organization.
	OrganizationSetting string
}

// TargetFacts is the version a would-be attestation is about.
//
// The two kinds of target share one struct rather than two because the
// decision reads a different SUBSET of it per kind, and a second struct
// would have to re-state which subset: see TargetIsPublic.
type TargetFacts struct {
	// Kind is the target's kind. The store DERIVES it — from the object's
	// object_type for an object target, and as TargetKindAsset for an asset
	// target — and stores nothing: a target_kind column could disagree with
	// the row it points at (migration 00120).
	Kind TargetKind
	// VersionID is the pinned version (scientific_object_versions.id, or
	// research_asset_versions.id).
	VersionID string
	// ObjectID is the scientific_objects.id (an object target) or the
	// research_assets.id (an asset target).
	ObjectID string
	// Title is the version's title as stored.
	Title string

	// LifecycleState is scientific_object_versions.lifecycle_state
	// ('active', 'aborted', 'reopened', 'superseded'). Object targets only.
	LifecycleState string

	// VisibilityPolicyID is the object version's OWN visibility axis
	// (scientific_object_versions.visibility_policy_id, migration 00005).
	// Nil means the version inherits its project's visibility. Object
	// targets only.
	VisibilityPolicyID *string
	// OwningProjectVisibility is projects.visibility of the project that
	// OWNS the target — not the attesting project. For an object target
	// that is scientific_objects.project_id; for an asset target it is
	// research_assets.origin_project_id, the project the platform reads an
	// asset's reachability from everywhere else (the asset page's read
	// gate, the asset feed's existence, the subscription audience).
	OwningProjectVisibility string

	// AssetVisibility is research_asset_versions.visibility ('public' or
	// 'private'). Asset targets only.
	AssetVisibility string
}

// BasisFacts is the attesting project's own state the attestation rests on.
type BasisFacts struct {
	// StateID is project_states.id.
	StateID string
	// ProjectID is the project the state belongs to. It must equal
	// Facts.ProjectID (migration 00120 makes it unconditional).
	ProjectID string
	// StateHash is the state's manifest hash, read so a refusal can name
	// the state without a second lookup. It is never rendered publicly.
	StateHash string
}

// ReviewFacts is the internal review an attestation cites.
type ReviewFacts struct {
	// ReviewID is reviews.id.
	ReviewID string
	// Kind is reviews.review_kind ('scientific' or 'integrity'). Read, and
	// deliberately not narrowed — see Judge.
	Kind string
	// Decision is reviews.decision ('approved', 'changes_requested',
	// 'comment').
	Decision string
	// ReviewedStateID is reviews.reviewed_state_id — the state the review
	// judged. It must equal Basis.StateID: an attestation cites the review
	// of the state it rests on, not a review of some other state of the
	// same project.
	ReviewedStateID string
	// PullRequestProjectID is the project of the pull request the review
	// was recorded on. It must equal Facts.ProjectID.
	PullRequestProjectID string
}

// Reason is one entry of the refusal report: a stable code a client
// branches on and a sentence a human reads. Same shape as
// knowledgepublish.Reason, so the two publish surfaces report refusals the
// same way.
type Reason struct {
	// Code is the machine-readable reason (the reason_* constants below).
	Code string `json:"code"`
	// Detail names what blocked, in the words of the specs it comes from.
	Detail string `json:"detail"`
}

// The refusal codes Judge produces.
const (
	// ReasonTargetKindNotAttestable: the target is not a Protocol, a Claim
	// or an Asset.
	ReasonTargetKindNotAttestable = "target_kind_not_attestable"
	// ReasonTargetNotPublic: the target version is not readable by the
	// network.
	ReasonTargetNotPublic = "target_not_public"
	// ReasonTargetAborted: the target version is currently retracted
	// (lifecycle_state = 'aborted').
	ReasonTargetAborted = "target_aborted"
	// ReasonBasisStateNotOwned: the basis state is not a state of the
	// attesting project.
	ReasonBasisStateNotOwned = "basis_state_not_owned"
	// ReasonInternalReviewNotOfBasis: the internal review is not a review
	// of the basis state, on a pull request of the attesting project.
	ReasonInternalReviewNotOfBasis = "internal_review_not_of_basis"
	// ReasonInternalReviewNotApproved: the internal review does not carry
	// the approved decision.
	ReasonInternalReviewNotApproved = "internal_review_not_approved"
	// ReasonAttributionNotPermitted: the caller asked for the organization
	// to be named, and either the project has no organization or the
	// organization's own standing setting does not permit it.
	ReasonAttributionNotPermitted = "attribution_not_permitted"
)

// TargetIsPublic is the ONE definition of whether an attestation target is
// readable by the network, and it is the same rule knowledgepublish.
// AudienceFor applies to a publication — the version's OWN visibility axis
// plus the owning project's preset — minus the rights clause, which a bare
// version has no document for.
//
//  1. An object target must inherit its project's visibility (its
//     visibility_policy_id is NULL) and the owning project must be public.
//     A pinned policy resolves to NOT public: this build has no vocabulary
//     that turns a policy into a public grant, so the fail-closed reading
//     is the only honest one.
//  2. An asset target must be public on BOTH of its axes: its own
//     (research_asset_versions.visibility, written by the publish
//     command) and the origin project's (projects.visibility of
//     research_assets.origin_project_id). It is the same conjunction the
//     object arm applies, in this kind's vocabulary — a version flagged
//     'public' inside a private project is not a public target, because
//     the three other places the platform decides an asset's reachability
//     all read the origin project and all refuse that version (the asset
//     page's read gate, the asset feed's existence, the subscription
//     audience, T1002).
//
// A kind that is neither is not public — not because it failed a check,
// but because the caller should not have got this far: Judge reports it as
// not attestable, and this returns false for it rather than guessing.
//
// The rule is fail-closed in both senses. It is also why an attestation is
// a WIDENING-FREE operation on its own side: writing one changes nothing
// about the target's visibility, and this function only ever READS it.
func (f Facts) TargetIsPublic() bool {
	switch f.Target.Kind {
	case TargetKindProtocol, TargetKindClaim:
		return f.Target.VisibilityPolicyID == nil && f.Target.OwningProjectVisibility == "public"
	case TargetKindAsset:
		return f.Target.AssetVisibility == "public" && f.Target.OwningProjectVisibility == "public"
	default:
		return false
	}
}

// AttributionPermitted reports whether the organization may be NAMED on
// this attestation, which is a conjunction of two independent facts:
//
//   - the project has an organization at all (a personal project has
//     nobody to name), and
//   - that organization's standing setting says named.
//
// The attestation's own org_visibility is the third input and it is not
// read here: it is what the caller ASKED for, and it is checked against
// this. Present applies the same two facts to the value that was recorded.
func (f Facts) AttributionPermitted() bool {
	return f.OrganizationID != "" && f.OrganizationSetting == OrgVisibilityNamed
}

// Judge runs the attestation decision over the facts and returns the
// entries that block it; an empty result means the attestation may be
// written.
//
// It is the whole decision, and it is run twice on the publish path: once
// for the preview the caller sees, and once inside the store's transaction
// over the state that transaction holds. The two runs are the same code, so
// a preview and the publish it precedes cannot disagree about what the
// rules are; only the state can differ, and that is the difference the
// re-run exists to catch (docs/22 §7).
//
// wantNamed is the attribution the caller asked for. Only the `named` side
// is a rule: `anonymous` is admissible everywhere, including on a project
// whose organization has chosen to be named — a standing setting is a
// FLOOR on disclosure, not a mandate to disclose.
//
// # Two things Judge deliberately does NOT check
//
//  1. Whether the target belongs to the attesting project. Attesting
//     somebody else's public work is the whole point of the surface
//     (docs/12 §2: a private project may explicitly publish an
//     attestation), and self-attestation is not forbidden by any
//     specification this task could find. Refusing it here would be
//     inventing a product rule; it is reported to the Supervisor instead.
//  2. Which review KIND authorised the attestation. The review must be
//     approved, of the basis state, on a pull request of the attesting
//     project — but reviews.review_kind is only 'scientific' or
//     'integrity', and no specification says which dimension authorises an
//     attestation. Narrowing it here would be the same invention, and
//     demanding both dimensions (what a release and a publication demand,
//     knowledgepublish.RequiredReviewKinds) would be a third.
//
// The order is the order the refusals are reported in: the target first
// (what the statement would be ABOUT — there is nothing to weigh if that is
// wrong), then the private side (what it rests on), then the attribution
// (what it would say).
func Judge(f Facts, wantNamed bool) []Reason {
	var reasons []Reason

	if !f.Target.Kind.Attestable() {
		reasons = append(reasons, Reason{
			Code:   ReasonTargetKindNotAttestable,
			Detail: fmt.Sprintf("an attestation names a public Protocol, Claim or Asset; this target is %q, which is not one of the three kinds the requirement names", string(f.Target.Kind)),
		})
	} else {
		if !f.TargetIsPublic() {
			reasons = append(reasons, Reason{
				Code:   ReasonTargetNotPublic,
				Detail: fmt.Sprintf("this target version is not readable by the network (%s): an attestation is a public statement about a public version, and publishing one must never be the first disclosure of what it names", targetVisibilityRule(f)),
			})
		}
		if f.Target.Kind != TargetKindAsset && f.Target.LifecycleState == string(domain.LifecycleAborted) {
			reasons = append(reasons, Reason{
				Code:   ReasonTargetAborted,
				Detail: fmt.Sprintf("this target version is retracted (lifecycle_state = %q): a version the repository currently holds as retracted cannot simultaneously carry a public statement that it was validated. Reopening the version (aborted → reopened, docs/43) lifts this refusal — it is about the state the version is in, not about the state being final", domain.LifecycleAborted),
			})
		}
	}

	if f.Basis.ProjectID != f.ProjectID || f.Basis.StateID == "" {
		reasons = append(reasons, Reason{
			Code:   ReasonBasisStateNotOwned,
			Detail: "the state this attestation would rest on is not a state of the attesting project: the work underneath an attestation is the attesting project's own, and the database refuses the row unconditionally (migration 00120)",
		})
	}

	switch {
	case f.Review.ReviewID == "":
		reasons = append(reasons, Reason{
			Code:   ReasonInternalReviewNotOfBasis,
			Detail: "no internal review was named: docs/22 §28 lists creating an attestation among the high-risk commands that need server-side authorization plus policy validation, and the review is the record that the judgment was made inside the project before it was stated outside it",
		})
	case f.Review.PullRequestProjectID != f.ProjectID || f.Review.ReviewedStateID != f.Basis.StateID:
		reasons = append(reasons, Reason{
			Code:   ReasonInternalReviewNotOfBasis,
			Detail: "the internal review cited is not a review of the state this attestation rests on, on a pull request of the attesting project: an attestation cites the review of the state it rests on, not a review of some other state of the same project (migration 00120 makes this unconditional)",
		})
	default:
		if f.Review.Decision != string(domain.ReviewDecisionApproved) {
			reasons = append(reasons, Reason{
				Code:   ReasonInternalReviewNotApproved,
				Detail: fmt.Sprintf("the internal review cited carries the decision %q, not %q: an attestation is only issued once the project's own review of the state it rests on has accepted it", f.Review.Decision, domain.ReviewDecisionApproved),
			})
		}
	}

	if wantNamed && !f.AttributionPermitted() {
		reasons = append(reasons, Reason{
			Code:   ReasonAttributionNotPermitted,
			Detail: fmt.Sprintf("this attestation asked for the organization to be named, and it may not be: %s. Being named is a widening, and docs/12 §3 requires an explicit confirmation for one — the organization's own standing setting (organizations.attestation_attribution) is that confirmation, and it defaults to %q", attributionRule(f), OrgVisibilityAnonymous),
		})
	}

	return reasons
}

// targetVisibilityRule names, in the refusal's words, which of the two
// visibility axes was read for this target — so a caller knows where to
// look rather than being told only that it was not public.
//
// The arms that quote projects.visibility (the last one for an object
// target, and the asset arm once the version's own axis has passed) name
// the project that OWNS — or, for an asset, ORIGINATED — the target, and a
// caller reading this refusal can only ever be a member of that project:
// the store resolves the target through the reader
// (ResolveRequest.ReaderID), so a non-public target reaches Judge at all
// only when the reader holds a membership row for that project, or when
// the version's own axis is public and the project's is not — which a
// non-member never gets past either, since the resolution is what refuses
// them. The value quoted is therefore one the reader can already read on
// the project itself. Do NOT route facts here from anywhere that skipped
// that read — the sentence is safe because of who can reach it, not
// because of what it says.
func targetVisibilityRule(f Facts) string {
	if f.Target.Kind == TargetKindAsset {
		// The asset arm has two axes, so the sentence names the one that
		// failed rather than asserting a fixed half of the rule: a version
		// that IS public on its own axis, inside a private project, was
		// being told "research_asset_versions.visibility is not 'public'"
		// about a value that is 'public'.
		if f.Target.AssetVisibility != "public" {
			return "research_asset_versions.visibility is not 'public'"
		}
		return fmt.Sprintf("the project that originated it is %q", f.Target.OwningProjectVisibility)
	}
	if f.Target.VisibilityPolicyID != nil {
		return "the version pins a visibility policy of its own (scientific_object_versions.visibility_policy_id), so it does not inherit its project's visibility, and this build has no vocabulary that turns a pinned policy into a public grant"
	}
	return fmt.Sprintf("the project that owns it is %q", f.Target.OwningProjectVisibility)
}

// attributionRule names why the organization may not be named.
func attributionRule(f Facts) string {
	if f.OrganizationID == "" {
		return "this project has no organization, so there is nobody to name"
	}
	return fmt.Sprintf("its standing setting is %q", f.OrganizationSetting)
}

// Attestable reports whether the kind is one of the three the requirement
// names.
func (k TargetKind) Attestable() bool {
	switch k {
	case TargetKindProtocol, TargetKindClaim, TargetKindAsset:
		return true
	}
	return false
}

// ValidValidationType reports whether t is one of the four validation
// types. It is the same closed vocabulary the table's CHECK enforces
// (migration 00120): the application refuses it with a named reason, and
// the database refuses the row unconditionally.
func ValidValidationType(t string) bool {
	for _, v := range ValidationTypes() {
		if t == v {
			return true
		}
	}
	return false
}

// ValidValidationResult reports whether r is one of the three results.
func ValidValidationResult(r string) bool {
	for _, v := range ValidationResults() {
		if r == v {
			return true
		}
	}
	return false
}

// ValidOrgVisibility reports whether v is one of the two attribution
// values.
func ValidOrgVisibility(v string) bool {
	for _, o := range OrgVisibilities() {
		if v == o {
			return true
		}
	}
	return false
}
