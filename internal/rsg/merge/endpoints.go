package merge

import (
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/diff"
)

// endpointIndex answers one question for every relation the merge would
// write: will both of its endpoints exist in the accepted state, and if
// the endpoint is one of the versions this merge writes, what must it be
// re-pinned to?
//
// Relations are version-pinned edges (docs/07 §3): a relation version names
// exact object version ids. A merge that writes new object versions
// therefore cannot copy the source relation version's pins verbatim — they
// name source-lineage rows, and a state whose edge points at a version the
// state does not contain is exactly the state_linkage failure the main gate
// rejects. Pins are rewritten only when they are not already part of the
// accepted lineage: a pin the target already has (the source deliberately
// depended on that version) is left exactly as the author wrote it.
type endpointIndex struct {
	// objectOfVersion maps every source-lineage object version id to its
	// object id.
	objectOfVersion map[string]string
	// targetVersions is the set of object version ids the target lineage
	// already contains.
	targetVersions map[string]bool
	// materializedByObject maps an object id to the source version that
	// will be written into the accepted state for it.
	materializedByObject map[string]string
	// publicationHeld is the set of object ids whose change was withheld
	// for the Publication Gate rather than for a structural reason.
	publicationHeld map[string]bool
}

// newEndpointIndex builds the index from the three-state input and the
// already-planned object changes.
func newEndpointIndex(in diff.Inputs, changes []Change) *endpointIndex {
	idx := &endpointIndex{
		objectOfVersion:      make(map[string]string, len(in.SourceSnapshot.ObjectVersions)),
		targetVersions:       make(map[string]bool, len(in.TargetSnapshot.ObjectVersions)),
		materializedByObject: make(map[string]string),
		publicationHeld:      make(map[string]bool),
	}
	for _, v := range in.SourceSnapshot.ObjectVersions {
		idx.objectOfVersion[v.ID] = v.ObjectID
	}
	for _, v := range in.TargetSnapshot.ObjectVersions {
		idx.targetVersions[v.ID] = true
	}
	for _, c := range changes {
		if c.TargetKind != domain.ConflictResolutionTargetObject {
			continue
		}
		if c.Materialize {
			idx.materializedByObject[c.TargetID] = c.SourceVersionID
			continue
		}
		if c.WithholdReason == ReasonPublicationGate {
			idx.publicationHeld[c.TargetID] = true
		}
	}
	return idx
}

// resolve returns the endpoint rewrites one relation version needs, the
// withhold reason when it cannot be written, and whether it can be written
// at all. An empty (non-nil) map means the pins are used as-is.
func (idx *endpointIndex) resolve(pins ...string) (map[string]string, string, bool) {
	rewrites := make(map[string]string)
	for _, pin := range pins {
		if pin == "" {
			continue
		}
		if idx.targetVersions[pin] {
			continue
		}
		objectID, known := idx.objectOfVersion[pin]
		if !known {
			// A pin outside both lineages: the source manifest itself
			// does not vouch for it. Never write an edge to a version
			// nobody can name.
			return nil, ReasonFoundationHeld, false
		}
		if _, ok := idx.materializedByObject[objectID]; ok {
			rewrites[pin] = objectID
			continue
		}
		if idx.publicationHeld[objectID] {
			// The endpoint is held because publishing it is the
			// Publication Gate's call, not because the decision found the
			// edge unsound: report it the same way, so the gate sees the
			// whole private cluster rather than half of it.
			return nil, ReasonPublicationGate, false
		}
		// The endpoint exists on the source side only and its change does
		// not land (accepted target, carried, aborted, or blocked): an
		// edge to it would dangle in the merged state.
		return nil, ReasonFoundationHeld, false
	}
	return rewrites, "", true
}
