package knowledgepublish

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinel errors the service maps for its consumers (docs/45: stable
// outcomes, never dependency detail). Each one is a distinct OUTCOME the
// transport has to be able to answer differently; everything else is
// ErrStore. The vocabulary mirrors internal/application/assetpublish's, so
// that the two publication surfaces answer the same shapes the same way.
var (
	// ErrValidation: the request is not a publication candidate at all —
	// an empty project, a version reference that is not an object version
	// uuid, a missing or oversized public_version, a rights document that
	// is not a rights document. The refusal names the field; it never
	// restates the request's content.
	ErrValidation = errors.New("knowledgepublish: validation failed")
	// ErrAgentNotPermitted: an agent actor attempted the publish. This is
	// the domain backstop, resolved before any lookup (docs/23 §3: a
	// publication controls visibility and is a human governance action).
	ErrAgentNotPermitted = errors.New("knowledgepublish: agents may not publish knowledge object versions")
	// ErrForbidden: the actor's class does not permit
	// authz.ActionPublishPrivateToPublic. Resolved before any target
	// lookup, so the denial never discloses whether the project or the
	// version exists (the releases.ErrForbidden rule).
	ErrForbidden = errors.New("knowledgepublish: the actor may not publish knowledge object versions here")
	// ErrProjectNotFound: no project row exists for the given id, or the
	// project is not visible to the caller (existence hiding, docs/45).
	ErrProjectNotFound = errors.New("knowledgepublish: project not found")
	// ErrVersionNotFound: the request named a scientific object version
	// that exists in no project, or in another one. A publish never
	// resolves a version across the project boundary, so a foreign id is
	// reported exactly as an unknown one (docs/45: no foreign entity
	// existence leaks).
	ErrVersionNotFound = errors.New("knowledgepublish: knowledge object version not found in this project")
	// ErrAlreadyPublished: this object version is already published to the
	// network, and owner ruling L3-20260916-1 #3 allows one publication
	// per version. The refusal is this package's, not the database's:
	// UNIQUE(object_version_id, public_version) would accept a second row
	// under a different public_version.
	ErrAlreadyPublished = errors.New("knowledgepublish: this knowledge object version is already published")
	// ErrReviewRequired: the version's state carries no approved
	// scientific and integrity review record in main's lineage, so it has
	// not passed publication_review (docs/43 §Publication: no automatic
	// published). The record is the existing research-PR review record —
	// the same one the release gate reads.
	ErrReviewRequired = errors.New("knowledgepublish: this knowledge object version has not passed publication review")
	// ErrIdempotencyConflict: the Idempotency-Key was already used for a
	// DIFFERENT publication (docs/45: 键复用到不同目标). A key that names
	// the same target replays instead; a key reused elsewhere is a client
	// error, because answering it with the other publication's row would
	// hand back a publication the caller did not ask for.
	ErrIdempotencyConflict = errors.New("knowledgepublish: this Idempotency-Key was used for a different publication")
	// ErrRefused: the publish's own server-side re-run of the publication
	// preview refused it — see PublicationRefused, which carries the
	// whole report.
	ErrRefused = errors.New("knowledgepublish: the publication was refused")
	// ErrStore: a persistence adapter failed, or the data it returned
	// cannot be rendered (cause kept for the log).
	ErrStore = errors.New("knowledgepublish: store failure")
)

// Wire codes (docs/45): one code per failure shape, so a client branches
// without parsing messages. The vocabulary the error model fixes is used
// where it exists (VALIDATION_FAILED, IDEMPOTENCY_CONFLICT,
// PRIVATE_TO_PUBLIC_REQUIRES_APPROVAL, RIGHTS_POLICY_BLOCKS_ACTION); the
// knowledge surface's own codes name outcomes it alone has.
const (
	// CodeValidationFailed: the request is not a publication candidate.
	CodeValidationFailed = "VALIDATION_FAILED"
	// CodePublishPrivateToPublicRequiresApproval: the actor's class does
	// not permit the publication. docs/45's own name for it: in V1 this is
	// the answer for every class except the project owner — a maintainer's
	// cell is `conditional`, and an unresolved condition is a refusal
	// (internal/authz default deny, issue #237). The same code the asset
	// publish answers with, because it is the same matrix row.
	CodePublishPrivateToPublicRequiresApproval = "PRIVATE_TO_PUBLIC_REQUIRES_APPROVAL"
	// CodeAgentPublishDenied: an agent actor attempted the publish. A code
	// of its own, although the matrix denies agents too: the two refusals
	// come from different places (the domain backstop versus the matrix
	// cell) and a test that cannot tell them apart cannot show that the
	// backstop is an independent second line.
	CodeAgentPublishDenied = "KNOWLEDGE_PUBLISH_AGENT_DENIED"
	// CodeProjectNotFound: the project does not exist or is not visible to
	// the caller (existence hiding).
	CodeProjectNotFound = "KNOWLEDGE_PUBLISH_PROJECT_NOT_FOUND"
	// CodeVersionNotFound: the named object version does not exist, or
	// belongs to another project.
	CodeVersionNotFound = "KNOWLEDGE_VERSION_NOT_FOUND"
	// CodeAlreadyPublished: the version already has a publication. Not
	// ASSET_VERSION_IMMUTABLE: nothing here is immutable and nothing is
	// being overwritten — the ruling forbids a second publication of one
	// version, which is a different fact than "the row already exists".
	CodeAlreadyPublished = "KNOWLEDGE_VERSION_ALREADY_PUBLISHED"
	// CodeReviewRequired: the version has not passed publication_review
	// (docs/43 §Publication).
	CodeReviewRequired = "KNOWLEDGE_PUBLICATION_REVIEW_REQUIRED"
	// CodeIdempotencyConflict: the Idempotency-Key was used elsewhere
	// (docs/45's own code).
	CodeIdempotencyConflict = "IDEMPOTENCY_CONFLICT"
	// CodePublishBlocked: the server-side re-run of the decision refused
	// the publication. The response carries the COMPLETE report, not a
	// sentence about it, so a caller can see every entry that blocked.
	CodePublishBlocked = "KNOWLEDGE_PUBLISH_BLOCKED"
	// CodeServiceUnavailable: the publish data is temporarily unavailable.
	CodeServiceUnavailable = "SERVICE_UNAVAILABLE"
)

// AgentNotPermittedError reports a publish refused because the actor is an
// agent. It is the domain's own refusal, not the matrix's: a publication
// controls visibility and is a human governance action (docs/12 §3), and
// the agent token carries no `visibility:publish` scope (docs/23 §3). The
// shape is copied from assetpublish.AgentNotPermittedError, which refuses
// the same action on the asset surface for the same reason.
type AgentNotPermittedError struct {
	// Action is the refused action's wire name ("publish").
	Action string
}

// Error implements error.
func (e *AgentNotPermittedError) Error() string {
	return fmt.Sprintf("knowledgepublish: agents cannot %s a knowledge object version — publication is a human governance action (docs/23 §3, docs/12 §3)", e.Action)
}

// Unwrap keeps errors.Is(err, ErrAgentNotPermitted) working through the
// wrap.
func (e *AgentNotPermittedError) Unwrap() error { return ErrAgentNotPermitted }

// Code is the stable wire code of this outcome (docs/45).
func (e *AgentNotPermittedError) Code() string { return CodeAgentPublishDenied }

// PublicationRefused is the publication refusal: the publish re-ran the
// publication decision over the repository's CURRENT state, inside its own
// transaction, and that state does not admit the publication.
//
// It carries the whole preview, not a summary of it — the same document
// POST /api/v1/projects/{projectId}/knowledge:publish-preview answers
// with, computed over the state the publish itself saw. The asset publish
// makes the same choice for the same reason: a caller told only "refused"
// cannot see which entry blocked it, nor check that the refusal was about
// the entry it thinks it was.
type PublicationRefused struct {
	// Preview is the complete preview the refusal was decided on.
	Preview Preview
	// Reasons names, one line each, the entries that blocked. They are
	// derived from the preview (never from anything the preview does not
	// carry), and they are a convenience: the preview is the record.
	Reasons []string
}

// Error implements error: the reasons, joined, so a log line is readable
// without decoding the preview.
func (e *PublicationRefused) Error() string {
	msg := ErrRefused.Error()
	if len(e.Reasons) > 0 {
		msg += ": " + strings.Join(e.Reasons, "; ")
	}
	return msg
}

// Unwrap keeps errors.Is(err, ErrRefused) working through the wrap.
func (e *PublicationRefused) Unwrap() error { return ErrRefused }

// Code is the stable wire code of this outcome (docs/45).
func (e *PublicationRefused) Code() string { return CodePublishBlocked }
