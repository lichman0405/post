package merge

import (
	"fmt"
	"sort"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/conflict"
)

// classifierKey is the identity of one classified conflict — the same key
// the resolution store's unique index uses (T0407): the conflicted
// target, the conflict code, the fields and payload keys it covers, and
// the paired object of an identity conflict. Merging by this key is what
// makes a human decision addressable to exactly one conflict and
// impossible to misread as one about another.
type classifierKey struct {
	targetKind    domain.ConflictResolutionTargetKind
	targetID      string
	code          string
	fields        string
	payloadKeys   string
	otherObjectID string
}

// keyOf renders the classifier key of one reported conflict on one
// target.
func keyOf(targetKind domain.ConflictResolutionTargetKind, targetID string, c conflict.Conflict) classifierKey {
	return classifierKey{
		targetKind:    targetKind,
		targetID:      targetID,
		code:          c.Code,
		fields:        joinStrings(c.Fields),
		payloadKeys:   joinStrings(c.PayloadKeys),
		otherObjectID: c.OtherObjectID,
	}
}

// keyOfDecision renders the dedup key of a submitted decision, which must
// name the same conflict the report classified.
func keyOfDecision(d Decision) classifierKey {
	return classifierKey{
		targetKind:    d.TargetKind,
		targetID:      d.TargetID,
		code:          d.Code,
		fields:        joinStrings(d.Fields),
		payloadKeys:   joinStrings(d.PayloadKeys),
		otherObjectID: d.OtherObjectID,
	}
}

// indexDecisions validates the human plan against the report and returns
// it keyed by classifier key. Every decision must be well formed, must
// name a human, and must address a conflict the detector actually
// classified for this triple; a decision that addresses nothing is an
// error (ErrDecisionNotInReport), never a silent no-op — a human recorded
// it about this proposal, and a merge that ignored it would be dropping a
// scientific judgement.
func indexDecisions(report *conflict.Report, decisions []Decision) (map[classifierKey]Decision, error) {
	universe := conflictUniverse(report)
	out := make(map[classifierKey]Decision, len(decisions))
	for _, d := range decisions {
		if err := validateDecision(d); err != nil {
			return nil, err
		}
		key := keyOfDecision(d)
		if !universe[key] {
			return nil, fmt.Errorf("%w: %s %s conflict %s (fields %v, payload keys %v)",
				ErrDecisionNotInReport, d.TargetKind, d.TargetID, d.Code, d.Fields, d.PayloadKeys)
		}
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("%w: %s %s conflict %s", ErrDuplicateDecision, d.TargetKind, d.TargetID, d.Code)
		}
		out[key] = d
	}
	return out, nil
}

// validateDecision rejects the malformed decisions the plan must not see:
// a kind outside the docs/09 §8 vocabulary (including any computed or
// agent-chosen spelling), a missing target or code, or no human decider.
func validateDecision(d Decision) error {
	if !domain.ValidResolutionTargetKind(d.TargetKind) {
		return fmt.Errorf("%w: unknown target kind %q", ErrInvalidDecision, string(d.TargetKind))
	}
	if d.TargetID == "" {
		return fmt.Errorf("%w: decision has no target id", ErrInvalidDecision)
	}
	if d.Code == "" {
		return fmt.Errorf("%w: decision on %s %s names no conflict code", ErrInvalidDecision, d.TargetKind, d.TargetID)
	}
	if !domain.ValidResolutionKind(d.Kind) {
		// Includes every spelling the spec does not name — an averaged or
		// auto-chosen kind can never reach the merge this way.
		return fmt.Errorf("%w: unknown resolution kind %q", ErrInvalidDecision, string(d.Kind))
	}
	if d.DecidedBy == "" {
		// docs/60: scientific conflict resolution is human-governed; a
		// decision nobody is accountable for is not a decision.
		return fmt.Errorf("%w: decision on %s %s conflict %s names no human decider",
			ErrInvalidDecision, d.TargetKind, d.TargetID, d.Code)
	}
	return nil
}

// conflictUniverse flattens the report into the set of conflicts a
// decision may address.
func conflictUniverse(report *conflict.Report) map[classifierKey]bool {
	universe := make(map[classifierKey]bool)
	for _, v := range report.ObjectVerdicts {
		for _, c := range v.Conflicts {
			universe[keyOf(domain.ConflictResolutionTargetObject, v.ObjectID, c)] = true
		}
	}
	for _, v := range report.RelationVerdicts {
		for _, c := range v.Conflicts {
			universe[keyOf(domain.ConflictResolutionTargetRelation, v.RelationID, c)] = true
		}
	}
	return universe
}

// joinStrings renders the classifier-key join of one identity string set:
// sorted, NUL-joined, so the same set matches the same key however its
// order arrives. It is the same normalization the resolution store
// persists (T0407), which is what makes the two agree on which conflict a
// decision belongs to.
func joinStrings(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	sorted := append([]string(nil), ss...)
	sort.Strings(sorted)
	out := ""
	for i, s := range sorted {
		if i > 0 {
			out += "\x00"
		}
		out += s
	}
	return out
}
