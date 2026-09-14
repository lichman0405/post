package integrity

import (
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/manifest"
)

// Snapshot is the complete set of facts one integrity check run consumes:
// the PR's pinned states, the two branches' state chains, the source
// branch's commits, the base and proposed lineage members (the manifest
// snapshot rows in stored form), and the policy versions the pins may
// resolve to. It is assembled by the application layer from persistence
// (internal/application/prchecks); this package never reads storage
// itself.
type Snapshot struct {
	// ProjectID is the research boundary every member belongs to.
	ProjectID string
	// PRNumber is the PR's per-project number (the report's address).
	PRNumber int64
	// Base is the PR's fixed base state (target branch head at creation).
	Base StateRef
	// Proposed is the PR's proposed head state (source branch head,
	// re-pinned only by the explicit refresh).
	Proposed StateRef
	// TargetStates is the target branch's state chain, oldest first.
	TargetStates []domain.ProjectState
	// SourceStates is the source branch's state chain, oldest first —
	// the chain from the fork point to the branch's CURRENT head (which
	// may be ahead of the proposed state).
	SourceStates []domain.ProjectState
	// SourceCommits is the source branch's commit history, oldest first.
	SourceCommits []domain.StateCommit
	// TargetBoundaries names the states the TARGET branch's chain may
	// legitimately touch without containing: the state the chain's root
	// builds on (the fork point — the branches.base_state_id at fork time,
	// which may be the genesis root or a state of a third branch), or the
	// branch's current head when the branch has no states of its own. The
	// application layer resolves them from persistence.
	//
	// The two sides are deliberately separate maps, never one shared set:
	// each check may accept only its OWN side's boundaries, or a pin that
	// merely names the other side's boundary would pass a provenance check
	// it must fail (e.g. a base pinned to the source branch's fork point on
	// a third branch passes base_on_target_chain over a shared set).
	TargetBoundaries map[string]bool
	// SourceBoundaries names the SOURCE branch chain's legitimate exits, in
	// the same sense as TargetBoundaries: the state the source chain roots
	// from (its fork point), or the branch's current head when the branch
	// has no states of its own. Only the source-side checks
	// (proposed_on_source_chain, source_chain_unbroken) consult it.
	SourceBoundaries map[string]bool
	// BaseObjects is the base state's lineage object versions (stored
	// payload form, manifest.Snapshot rows).
	BaseObjects []manifest.ObjectVersion
	// ProposedObjects is the proposed state's lineage object versions.
	ProposedObjects []manifest.ObjectVersion
	// BaseRelations / ProposedRelations are the two lineages' relation
	// versions.
	BaseRelations     []manifest.RelationVersion
	ProposedRelations []manifest.RelationVersion
	// BaseBlobRefs / ProposedBlobRefs are the two lineages' manifest
	// blob refs — the blob universe a payload reference may resolve to.
	BaseBlobRefs     []manifest.BlobRef
	ProposedBlobRefs []manifest.BlobRef
	// PolicyVersions is every policy version the proposal's pins may
	// resolve to: the organization's and the project's version history
	// (docs/12 §5: org policy is the lower bound, project policy may
	// only be stricter).
	PolicyVersions []domain.PolicyVersion
}

// StateRef names one pinned state of the PR: its identity only. The rest
// of the state's facts travel in the chain reads.
type StateRef struct {
	ID string `json:"id"`
}

// ManifestRefs returns the union of the base and proposed blob refs, in
// canonical order — the universe a payload blob reference may resolve to.
func (s Snapshot) ManifestRefs() []manifest.BlobRef {
	seen := make(map[string]bool, len(s.BaseBlobRefs)+len(s.ProposedBlobRefs))
	out := make([]manifest.BlobRef, 0, len(s.BaseBlobRefs)+len(s.ProposedBlobRefs))
	for _, ref := range s.BaseBlobRefs {
		if !seen[ref.ID] {
			seen[ref.ID] = true
			out = append(out, ref)
		}
	}
	for _, ref := range s.ProposedBlobRefs {
		if !seen[ref.ID] {
			seen[ref.ID] = true
			out = append(out, ref)
		}
	}
	return out
}
