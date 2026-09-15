package milestones

import "errors"

// Sentinel errors the command maps for the transport layer (docs/45: the
// wire carries codes, never dependency detail).
var (
	// ErrValidation: an input fails the domain shape rules (an unknown
	// kind, a custom milestone without a label, an oversized label, a
	// missing or malformed occurred_at, a blank id).
	ErrValidation = errors.New("milestones: validation failed")
	// ErrForbidden: the actor's class denies ActionCreateRelease (the
	// governance action the milestone create reuses until the vocabulary
	// gains an action of its own).
	ErrForbidden = errors.New("milestones: the actor may not record milestones here")
	// ErrProjectNotFound: no project row exists for the given id, or the
	// project is not visible to the caller — one outcome, never a foreign
	// entity's existence (docs/45).
	ErrProjectNotFound = errors.New("milestones: project not found")
	// ErrReleaseNotFound: the named release does not exist in the project
	// — an unknown id, or a release of another project (same outcome, no
	// foreign existence leak). The release link is optional; this error
	// only names a link that was sent and does not resolve.
	ErrReleaseNotFound = errors.New("milestones: release not found in the project")
	// ErrMilestoneNotFound: no milestone row exists for the id, or the
	// milestone belongs to another project (same outcome, docs/45).
	ErrMilestoneNotFound = errors.New("milestones: milestone not found")
	// ErrStore: a persistence adapter failed (cause kept for the log).
	ErrStore = errors.New("milestones: store failure")
)

// Wire codes (docs/45). One outcome has one stable wire name.
const (
	// CodeMilestoneValidationFailed: the request body fails the domain
	// shape rules.
	CodeMilestoneValidationFailed = "MILESTONE_VALIDATION_FAILED"
	// CodeMilestoneForbidden: the actor's class denies the create.
	CodeMilestoneForbidden = "MILESTONE_FORBIDDEN"
	// CodeMilestoneProjectNotFound: the project does not exist or is not
	// visible to the caller.
	CodeMilestoneProjectNotFound = "MILESTONE_PROJECT_NOT_FOUND"
	// CodeMilestoneReleaseNotFound: the named release does not exist in
	// the project (a release link that does not resolve).
	CodeMilestoneReleaseNotFound = "MILESTONE_RELEASE_NOT_FOUND"
	// CodeMilestoneNotFound: no milestone row exists for the id, or it
	// belongs to another project.
	CodeMilestoneNotFound = "MILESTONE_NOT_FOUND"
	// CodeMilestoneServiceUnavailable: the milestone data is temporarily
	// unavailable.
	CodeMilestoneServiceUnavailable = "SERVICE_UNAVAILABLE"
)
