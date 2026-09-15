package domain

import (
	"strings"
	"time"
)

// Milestone is one research-timeline marker on a project (canonical table
// project_milestones, migration 00063, T0609). It is deliberately a
// separate object from Release (docs/11, CLAUDE.md §9.5): a release is an
// immutable snapshot of accepted state; a milestone is a recorded research
// event on the project's timeline — a candidate selected, a paper
// submitted, a patent filed, an external validation, or a custom-labeled
// marker. A milestone MAY name the release it documents (ReleaseID); it
// never requires one.
//
// Milestones never touch the project lifecycle: recording them does not
// advance projects.activity_status, and the project state machine keeps no
// forced "completed" terminal state (docs/43 §1 — planning → active ↔
// paused → archived, archived reactivatable). Research progress is a
// timeline of recorded facts, not a lifecycle transition.
type Milestone struct {
	// ID is the uuid v4 text form (matches the project_milestones.id
	// uuid column).
	ID string
	// ProjectID is the research boundary the milestone belongs to.
	ProjectID string
	// Kind is the canonical milestone kind, or MilestoneCustom for a
	// custom-labeled marker (project_milestones.kind CHECK).
	Kind MilestoneKind
	// Label is the custom display label; empty for a canonical kind
	// without a custom label. For MilestoneCustom it is required — the
	// label IS the milestone's name.
	Label string
	// OccurredAt is the milestone's position on the timeline — the date
	// the event happened (paper submitted, patent filed, ...). The
	// timeline orders by it; creation order only breaks date ties.
	OccurredAt time.Time
	// ReleaseID optionally links the release this milestone documents
	// (project_milestones.release_id, nullable). When set it must be a
	// release of the same project — the command enforces the boundary
	// (the FK only enforces existence).
	ReleaseID *string
	// CreatedBy is the user id of the recording actor.
	CreatedBy string
	CreatedAt time.Time
}

// MilestoneKind is the canonical milestone vocabulary
// (project_milestones.kind CHECK): the four research-event kinds of the
// canonical MOF workflow plus custom. The kinds are recorded facts about
// research progress — none of them is a project lifecycle state, and in
// particular none of them means "completed".
type MilestoneKind string

const (
	// MilestoneCandidateSelected: a research candidate was selected for
	// the project's goal (the MOF workflow's candidate phase).
	MilestoneCandidateSelected MilestoneKind = "candidate_selected"
	// MilestonePaperSubmitted: a paper describing the research was
	// submitted to a venue.
	MilestonePaperSubmitted MilestoneKind = "paper_submitted"
	// MilestonePatentFiled: a patent filing was made.
	MilestonePatentFiled MilestoneKind = "patent_filed"
	// MilestoneExternalValidation: the result was validated by an
	// external party.
	MilestoneExternalValidation MilestoneKind = "external_validation"
	// MilestoneCustom: a custom-labeled marker outside the canonical
	// vocabulary — the Label carries its name (required).
	MilestoneCustom MilestoneKind = "custom"
)

// ValidMilestoneKind reports whether k is one of the canonical kinds
// (project_milestones.kind CHECK).
func ValidMilestoneKind(k MilestoneKind) bool {
	switch k {
	case MilestoneCandidateSelected, MilestonePaperSubmitted, MilestonePatentFiled,
		MilestoneExternalValidation, MilestoneCustom:
		return true
	}
	return false
}

// DisplayName is the human-facing name of a canonical kind (the UI shows
// it when the milestone carries no custom label). A custom kind without a
// label cannot occur (the label is required); its display is the label.
func (k MilestoneKind) DisplayName() string {
	switch k {
	case MilestoneCandidateSelected:
		return "Candidate selected"
	case MilestonePaperSubmitted:
		return "Paper submitted"
	case MilestonePatentFiled:
		return "Patent filed"
	case MilestoneExternalValidation:
		return "External validation"
	case MilestoneCustom:
		return "Custom"
	}
	return string(k)
}

// MaxMilestoneLabelLen bounds the milestone label. It is display
// metadata, not identity: long enough for a real annotation ("JACS
// 2026, doi:10.xxxx/..."), short enough that a timeline stays readable.
const MaxMilestoneLabelLen = 200

// ValidMilestoneLabel reports whether the raw label is storable for the
// kind after trimming: custom requires a non-blank label (it is the
// marker's name), canonical kinds may carry a blank label (the kind's
// DisplayName renders then); any label is bounded.
func ValidMilestoneLabel(label string, kind MilestoneKind) bool {
	t := strings.TrimSpace(label)
	if len(t) > MaxMilestoneLabelLen {
		return false
	}
	if kind == MilestoneCustom {
		return t != ""
	}
	return true
}
