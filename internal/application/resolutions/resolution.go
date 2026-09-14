package resolutions

import (
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/conflict"
)

// Decision is one resolution decision as the client submits it: the full
// classifier key of the conflict it addresses (target kind + id, code,
// fields, payload keys, paired object) and the chosen human decision.
// There is no computed outcome — the kind is exactly what the human chose
// (package doc).
type Decision struct {
	TargetKind domain.ConflictResolutionTargetKind `json:"target_kind"`
	// TargetID is the object id or relation id the conflict covers.
	TargetID string `json:"target_id"`
	// Code is the detector's conflict code (e.g.
	// SCIENTIFIC_FIELD_DIVERGES).
	Code string `json:"code"`
	// Fields are the fields the conflict covers, canonical order.
	Fields []string `json:"fields"`
	// PayloadKeys are the payload keys the conflict covers (empty unless
	// "payload" is in Fields).
	PayloadKeys []string `json:"payload_keys"`
	// OtherObjectID is the paired object of an identity conflict (null for
	// every other category).
	OtherObjectID *string               `json:"other_object_id"`
	Kind          domain.ResolutionKind `json:"kind"`
	// Note is the decider's free-form reasoning (e.g. the name they
	// propose for a validation branch).
	Note string `json:"note"`
}

// SaveInput is one plan submission: the pinned three-way triple and the
// decisions. The triple is exactly the inputs the conflict report was
// computed over — decisions bind to states, not to branch heads that may
// have moved since.
type SaveInput struct {
	ProjectID     string
	BaseStateID   string
	SourceStateID string
	TargetStateID string
	Decisions     []Decision
}

// EvidenceItem is one evidence assertion rendered as the resolution UI's
// evidence context: the assertion facts joined with the evidence object
// it cites (the evidence object's type/title/version).
type EvidenceItem struct {
	RelationType string `json:"relation_type"`
	EvidenceType string `json:"evidence_type"`
	// Directness is the assertion's stored directness (docs/07 evidence
	// vocabulary).
	Directness    string  `json:"directness"`
	ReasoningNote *string `json:"reasoning_note"`
	ReviewState   string  `json:"review_state"`
	// EvidenceObjectID/Type/Title/VersionNo name the evidence object the
	// assertion cites, for rendering.
	EvidenceObjectID    string `json:"evidence_object_id"`
	EvidenceObjectType  string `json:"evidence_object_type"`
	EvidenceTitle       string `json:"evidence_title"`
	EvidenceObjectVerNo int    `json:"evidence_object_version_no"`
}

// ObjectEvidence is the per-side evidence context of one conflicted
// object: the assertions the source side's head version carries and the
// assertions the target side's head version carries. A side with no
// recorded assertions renders an empty list ("no evidence recorded") —
// absence is a fact the UI shows, not an error.
type ObjectEvidence struct {
	ObjectID       string         `json:"object_id"`
	SourceEvidence []EvidenceItem `json:"source_evidence"`
	TargetEvidence []EvidenceItem `json:"target_evidence"`
}

// View is the resolution UI's read: the conflict report (diff + verdicts,
// the base/A/B values ride along in the report's changes), the per-side
// evidence context of the conflicted objects, and the decisions already
// recorded for the triple.
type View struct {
	Report      *conflict.Report            `json:"report"`
	Evidence    []ObjectEvidence            `json:"evidence"`
	Resolutions []domain.ConflictResolution `json:"resolutions"`
}
