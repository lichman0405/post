package releases

import (
	"errors"

	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// Sentinel errors the service maps for its consumers (docs/45: stable
// outcomes, never dependency detail).
var (
	// ErrValidation: an input fails the domain shape rules (empty ids,
	// a release version outside the bounded-label shape).
	ErrValidation = errors.New("releases: validation failed")
	// ErrStateNotFound: no state row exists for the given id, or the
	// state belongs to another project (existence-hiding — never leak a
	// foreign entity, docs/45).
	ErrStateNotFound = errors.New("releases: state not found")
	// ErrProjectNotFound: no project row exists for the given id, or the
	// project is not visible to the caller (read existence hiding,
	// docs/45).
	ErrProjectNotFound = errors.New("releases: project not found")
	// ErrNotMainState: the state exists but is not an accepted main
	// snapshot — a release manifests only accepted main state
	// (docs/11 §1). Also the project's main branch having no state yet.
	ErrNotMainState = errors.New("releases: state is not an accepted main snapshot")
	// ErrPolicyNotFound: a named policy version does not exist, or does
	// not belong to the release's organization/project scope.
	ErrPolicyNotFound = errors.New("releases: policy version not found")
	// ErrForbidden: the actor's class denies ActionCreateRelease (the
	// matrix) — resolved before any target lookup, so the denial never
	// discloses whether the project exists.
	ErrForbidden = errors.New("releases: the actor may not create releases here")
	// ErrVersionTaken: the project already has a release with this
	// version string, and the create carried no Idempotency-Key naming
	// that earlier create (the UNIQUE(project_id, version) guard).
	ErrVersionTaken = errors.New("releases: this version already exists")
	// ErrReleaseNotFound: no release row exists for the id, or the
	// release belongs to another project (existence-hiding, docs/45).
	ErrReleaseNotFound = errors.New("releases: release not found")
	// ErrReleaseGate: the release gate refused the snapshot — the
	// command's refusal carries the full validation report (docs/22 §7:
	// the command re-runs the gate server-side, never trusts a
	// precheck).
	ErrReleaseGate = errors.New("releases: the release gate refused the snapshot")
	// ErrStore: a persistence adapter failed, or the data it returned
	// cannot be rendered (cause kept for the log).
	ErrStore = errors.New("releases: store failure")
)

// GateRefused is the release-gate refusal: ErrReleaseGate wrapped with
// the complete validation report, so the transport can render the gate's
// own explanation (docs/22 §7 — the client sees the full result).
type GateRefused struct {
	Report rsgvalidation.Report
}

func (e *GateRefused) Error() string { return ErrReleaseGate.Error() }

// Unwrap keeps errors.Is(err, ErrReleaseGate) working through the wrap.
func (e *GateRefused) Unwrap() error { return ErrReleaseGate }

// Wire codes (docs/45): one code per failure shape, so a client branches
// without parsing messages.
const (
	// CodeReleaseValidationFailed: the request body or the version/title
	// shape is invalid.
	CodeReleaseValidationFailed = "VALIDATION_FAILED"
	// CodeReleaseForbidden: the actor's class denies ActionCreateRelease.
	CodeReleaseForbidden = "RELEASE_FORBIDDEN"
	// CodeReleaseProjectNotFound: the project does not exist or is not
	// visible to the caller (existence hiding).
	CodeReleaseProjectNotFound = "RELEASE_PROJECT_NOT_FOUND"
	// CodeReleaseNoMainState: the project's main branch has no accepted
	// state yet — there is no snapshot to release.
	CodeReleaseNoMainState = "RELEASE_NO_MAIN_STATE"
	// CodeReleaseVersionTaken: the project already has a release with
	// this version, and the create carried no key naming it.
	CodeReleaseVersionTaken = "RELEASE_VERSION_TAKEN"
	// CodeReleaseNotFound: no release row exists for the id, or it
	// belongs to another project.
	CodeReleaseNotFound = "RELEASE_NOT_FOUND"
	// CodeReleaseGateBlocked: the server-side release gate refused the
	// snapshot.
	CodeReleaseGateBlocked = "RELEASE_GATE_BLOCKED"
	// CodeReleaseServiceUnavailable: the release data is temporarily
	// unavailable.
	CodeReleaseServiceUnavailable = "SERVICE_UNAVAILABLE"
)
