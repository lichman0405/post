package knowledgepublish

import (
	"fmt"

	"github.com/lichman0405/post/internal/application/releases"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rights"
)

// Facts is everything the publication decision reads about the version
// being published and the world it would be published into. It is resolved
// by the store — once outside a transaction for the preview, once inside
// the publish's own transaction, over the state that transaction sees —
// and every decision below is a pure function of it. That is what makes
// the preview and the publish the same decision: the publish re-runs these
// functions over its own view rather than trusting the preview's.
type Facts struct {
	// ObjectVersionID is the scientific_object_versions row.
	ObjectVersionID string
	// ObjectID is the scientific_objects row the version belongs to.
	ObjectID string
	// ObjectType is the object's type (research_question, hypothesis,
	// claim, finding, ...): docs/03 defines the Published Knowledge Object
	// as one of these, published.
	ObjectType string
	// ProjectID is the project that owns the object.
	ProjectID string
	// Title is the version's title — used by the read model, deliberately
	// NOT returned by the preview (see Preview).
	Title string
	// LifecycleState is the version's lifecycle_state column.
	LifecycleState string
	// VisibilityPolicyID is the version's OWN visibility axis
	// (scientific_object_versions.visibility_policy_id, migration 00005).
	// Nil means the version inherits the project's visibility — the rule
	// internal/application/sciobjects states for the column ("nil inherits
	// the project default"). Non-nil means the version's visibility is
	// governed by an explicit policy of its own.
	VisibilityPolicyID *string
	// StateID is the project state the version was created in; the review
	// record is read over its lineage into main.
	StateID string
	// MainBranchID is the project's main branch, or empty when the project
	// has none (see Judge for what an empty one means).
	MainBranchID string
	// ProjectVisibility is projects.visibility ('public' or 'private').
	ProjectVisibility string
	// Rights is the rights declaration the publication stores. For a
	// preview it is the document the caller sent (parsed, never stored);
	// for the read model it is the document the publication row holds.
	Rights rights.Document
	// Reviews is the review record of the version's lineage into main —
	// the same record, of the same shape, that the release gate reads
	// (releases.ReleaseReviewPort).
	Reviews []releases.ReviewRecord
	// Published is the version's existing publication, or nil when it has
	// never been published.
	Published *Published
}

// ReviewKinds returns the review dimensions that carry at least one
// approved review in this version's lineage, in the order the refusal
// report names them.
func (f Facts) ReviewKinds() []string { return ApprovedReviewKinds(f.Reviews) }

// ReviewApproved reports whether the version passed EVERY required review
// dimension — the publication_review cell of docs/43 §Publication. False
// means the version is still a private candidate and may not become
// published.
func (f Facts) ReviewApproved() bool { return len(f.ReviewKinds()) == len(requiredReviewKinds) }

// requiredReviewKinds are the two review dimensions a version must have
// passed before it may be published. They are named in docs/09 §5, which
// requires the two to be recorded SEPARATELY — never one aggregate
// approval — and they are the same two the release gate demands
// (internal/application/releases: approvedKinds(records, "scientific",
// "integrity")). A publication demands them for the same reason a release
// does: both put a version's content outside the project that made it.
var requiredReviewKinds = []string{"scientific", "integrity"}

// RequiredReviewKinds returns the review dimensions a publication
// requires, in the order the refusal report names them.
func RequiredReviewKinds() []string {
	out := make([]string, len(requiredReviewKinds))
	copy(out, requiredReviewKinds)
	return out
}

// ApprovedReviewKinds returns the subset of kinds that carry at least one
// approved review in the record, in the required order. It reads the
// record the release gate reads — the reviews of the research PRs that
// accepted states in this state's lineage into main — so "this version
// passed review" has one meaning on both surfaces.
func ApprovedReviewKinds(records []releases.ReviewRecord) []string {
	have := make(map[string]bool, len(requiredReviewKinds))
	for _, rec := range records {
		for _, r := range rec.Reviews {
			if r.Decision == "approved" {
				have[r.ReviewKind] = true
			}
		}
	}
	out := make([]string, 0, len(requiredReviewKinds))
	for _, k := range requiredReviewKinds {
		if have[k] {
			out = append(out, k)
		}
	}
	return out
}

// Audience is what the network — a caller who is not a member of the
// project that owns the object — may see.
type Audience string

const (
	// AudienceNetwork: the publication is readable by anyone, including an
	// anonymous caller.
	AudienceNetwork Audience = "network"
	// AudienceMembers: only the project's members may read it. This is the
	// fail-closed answer, and it is the answer for every case the rule
	// below does not explicitly admit.
	AudienceMembers Audience = "members"
)

// AudienceFor is the ONE definition of who may read a published knowledge
// object, and it is a function of the version's OWN visibility axis, not
// of the project's visibility alone.
//
// A publication is readable by the network only when all three of these
// hold:
//
//  1. the version inherits the project's visibility — its
//     visibility_policy_id is NULL
//     (scientific_object_versions.visibility_policy_id, migration 00005;
//     internal/application/sciobjects: "nil inherits the project
//     default"). A version that pins a policy of its own is governed by
//     that policy, and this build has no vocabulary that turns a pinned
//     policy into a public grant — so a pinned policy resolves to
//     members-only, which is the fail-closed reading of "the version's
//     visibility is decided by something other than the project default".
//
//  2. the owning project is public (projects.visibility = 'public',
//     docs/12 §2 — the preset a private project's content is invisible
//     under).
//
//  3. the rights declaration the publication stores does not pin a
//     metadata visibility of its own: rights.Visibility.Metadata must be
//     the rights model's own fail-closed default,
//     rights.MetadataProjectPolicy, whose documented meaning is "whatever
//     the owning project's policy says, evaluated at read time"
//     (internal/rights/visibility.go). The token vocabulary is open by
//     design (a policy id, a preset, or project_policy), and this build
//     resolves no metadata policy token, so any other token is
//     unresolvable and is refused rather than assumed to be public. A
//     document that does not parse states no default either, and is
//     refused for the same reason.
//
// Every step is fail-closed, and the consequence is the one T0805 was
// written around: a version in a PUBLIC project whose own visibility is
// restricted stays members-only after publication. Publishing it changed
// nothing about who may read it — which is what "发布不等于公开" means.
//
// The data_access axis of the rights document is deliberately NOT read
// here: it governs the blobs, not the metadata read this function answers
// (internal/rights/visibility.go: the two axes cannot be derived from one
// another).
func AudienceFor(projectVisibility string, visibilityPolicyID *string, rightsDoc rights.Document) Audience {
	if visibilityPolicyID != nil {
		return AudienceMembers
	}
	if projectVisibility != "public" {
		return AudienceMembers
	}
	if rightsDoc.Visibility.Metadata != rights.MetadataProjectPolicy {
		return AudienceMembers
	}
	return AudienceNetwork
}

// Reason is one entry of the publication's refusal report: a stable code a
// client branches on and a sentence a human reads.
type Reason struct {
	// Code is the machine-readable reason (ReasonAlreadyPublished,
	// ReasonReviewRequired).
	Code string `json:"code"`
	// Detail names what blocked, in the words of the specs it comes from.
	Detail string `json:"detail"`
}

// The refusal codes Judge produces.
const (
	// ReasonAlreadyPublished: the version already has a publication
	// (owner ruling L3-20260916-1 #3).
	ReasonAlreadyPublished = "already_published"
	// ReasonReviewRequired: the version's state carries no approved
	// scientific and integrity review record (docs/43 §Publication).
	ReasonReviewRequired = "review_required"
	// ReasonLifecycleAborted: the version is currently RETRACTED
	// (scientific_object_versions.lifecycle_state = 'aborted', docs/43:
	// active → aborted → reopened → active).
	ReasonLifecycleAborted = "lifecycle_aborted"
)

// Judge runs the publication decision over the facts and returns the
// entries that block it; an empty result means the publication may be
// written.
//
// It is the whole decision, and it is run twice on the publish path: once
// for the preview the caller sees, and once inside the store's transaction
// over the state that transaction holds (docs/22 §7 — a command re-runs
// its own gate server-side rather than trust a precheck). The two runs are
// the same code, so a preview and the publish it precedes cannot disagree
// about what the rules are; only the state can differ, and that is exactly
// the difference the re-run exists to catch.
//
// The order is the order the refusals are reported in, and it is the order
// of specificity: a version that is already published is answered as
// already published, whatever else is true of it (it passed review when it
// was published, so in practice both hold together).
//
// A version whose project has no main branch has no review record to
// read — nothing can have been accepted into a main that does not exist —
// and is refused as unreviewed rather than assumed reviewed.
//
// The lifecycle refusal is about the state the version is in NOW, not about
// the state being final: docs/43's version lifecycle is
// active → aborted → reopened → active, so a retracted version that is
// reopened is publishable again and this entry simply stops being produced.
// The rule it states is the one the platform would otherwise contradict:
// a version the repository currently holds as retracted cannot
// simultaneously be presented on the network as published knowledge — the
// two are statements about the same version, and a feed, a read and a
// release all cite the version as the thing they are about.
func Judge(f Facts) []Reason {
	var reasons []Reason
	if f.Published != nil {
		reasons = append(reasons, Reason{
			Code: ReasonAlreadyPublished,
			Detail: fmt.Sprintf("this knowledge object version is already published as %q; a version is published to the network once, and a change is a new version (owner ruling L3-20260916-1)",
				f.Published.PublicVersion),
		})
	}
	if f.LifecycleState == string(domain.LifecycleAborted) {
		reasons = append(reasons, Reason{
			Code: ReasonLifecycleAborted,
			Detail: fmt.Sprintf("this knowledge object version is retracted (lifecycle_state = %q): a version the repository currently holds as retracted cannot simultaneously be presented on the network as published knowledge. Reopening the version (aborted → reopened, docs/43) lifts this refusal — it is about the state the version is in, not about the state being final",
				domain.LifecycleAborted),
		})
	}
	if !f.ReviewApproved() {
		reasons = append(reasons, Reason{
			Code: ReasonReviewRequired,
			Detail: fmt.Sprintf("the record of this version's lineage into main carries no approved %s review, so the version has not passed publication review (docs/43: private candidate → publication_review → published, and there is no automatic published)",
				joinKinds(missingReviewKinds(f.ReviewKinds()))),
		})
	}
	return reasons
}

// missingReviewKinds returns the required kinds the approved set lacks, in
// the required order.
func missingReviewKinds(approved []string) []string {
	have := make(map[string]bool, len(approved))
	for _, k := range approved {
		have[k] = true
	}
	var missing []string
	for _, k := range requiredReviewKinds {
		if !have[k] {
			missing = append(missing, k)
		}
	}
	return missing
}

// joinKinds renders a kind list for a sentence ("scientific and integrity",
// "scientific").
func joinKinds(kinds []string) string {
	switch len(kinds) {
	case 0:
		return "scientific and integrity"
	case 1:
		return kinds[0]
	default:
		out := kinds[0]
		for _, k := range kinds[1 : len(kinds)-1] {
			out += ", " + k
		}
		return out + " and " + kinds[len(kinds)-1]
	}
}
