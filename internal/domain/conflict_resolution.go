package domain

import "time"

// Conflict resolution (T0407): one human decision on one classified
// conflict of a three-way semantic diff (docs/09 §8). The decisions are
// exactly the five explicit kinds docs/09 §8 names — no kind computes,
// averages or derives parameters; the merge engine (T0406) executes the
// chosen kind verbatim.

// ResolutionKind is the decision the human made about one conflict.
type ResolutionKind string

const (
	// ResolutionAcceptSource accepts A — the source branch's proposed
	// value wins (docs/09 §8 "Accept A").
	ResolutionAcceptSource ResolutionKind = "accept_source"
	// ResolutionAcceptTarget accepts B — the target branch's current
	// value wins (docs/09 §8 "Accept B").
	ResolutionAcceptTarget ResolutionKind = "accept_target"
	// ResolutionKeepBoth keeps both versions: explicit coexistence,
	// nothing is dropped (docs/09 §8 "Keep both versions").
	ResolutionKeepBoth ResolutionKind = "keep_both"
	// ResolutionValidationBranch sends the combined content to a
	// validation branch instead of deciding now (docs/09 §8 "Create
	// validation branch").
	ResolutionValidationBranch ResolutionKind = "validation_branch"
	// ResolutionUnresolved records the conflict as contested and leaves
	// it unresolved in main (docs/09 §8: scientific conflict 可在 main
	// 中保持 contested/unresolved).
	ResolutionUnresolved ResolutionKind = "unresolved"
)

// ConflictResolutionTargetKind names what the conflict is about.
type ConflictResolutionTargetKind string

const (
	// ConflictResolutionTargetObject: the conflict covers a scientific
	// object's change (the verdict's object_id).
	ConflictResolutionTargetObject ConflictResolutionTargetKind = "object"
	// ConflictResolutionTargetRelation: the conflict covers a relation's
	// change (the verdict's relation_id).
	ConflictResolutionTargetRelation ConflictResolutionTargetKind = "relation"
)

// ConflictResolution is one persisted human decision (the
// conflict_resolutions row). The conflict identity fields (target kind +
// id, code, fields, payload keys, paired object) are the detector
// report's classifier key — a decision can never be misread as a
// decision on another conflict.
type ConflictResolution struct {
	ID            string
	ProjectID     string
	BaseStateID   string
	SourceStateID string
	TargetStateID string
	// TargetKind and TargetID name the conflicted change (object or
	// relation).
	TargetKind ConflictResolutionTargetKind
	TargetID   string
	// Code is the detector's conflict code (e.g.
	// SCIENTIFIC_FIELD_DIVERGES).
	Code string
	// Fields are the object/relation fields the conflict covers, in the
	// report's canonical order.
	Fields []string
	// PayloadKeys are the payload keys the conflict covers (empty unless
	// "payload" is in Fields).
	PayloadKeys []string
	// OtherObjectID is the paired object of an identity conflict ("" for
	// every other category).
	OtherObjectID *string
	// Kind is the human decision.
	Kind ResolutionKind
	// Note is the decider's free-form reasoning.
	Note string
	// DecidedBy is the human who decided (never an agent — docs/60:
	// scientific conflict final resolution must be human-governed).
	DecidedBy string
	DecidedAt time.Time
	UpdatedAt time.Time
}

// ValidResolutionKind reports whether k is one of the five decision kinds.
func ValidResolutionKind(k ResolutionKind) bool {
	switch k {
	case ResolutionAcceptSource, ResolutionAcceptTarget, ResolutionKeepBoth,
		ResolutionValidationBranch, ResolutionUnresolved:
		return true
	}
	return false
}

// ValidResolutionTargetKind reports whether k names a conflict target.
func ValidResolutionTargetKind(k ConflictResolutionTargetKind) bool {
	return k == ConflictResolutionTargetObject || k == ConflictResolutionTargetRelation
}
