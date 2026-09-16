package assetpublish

import (
	"errors"
	"fmt"
	"strings"

	"github.com/lichman0405/post/internal/assets"
)

// Sentinel errors the service maps for its consumers (docs/45: stable
// outcomes, never dependency detail). Each one is a distinct OUTCOME the
// transport has to be able to answer differently; everything else is
// ErrStore.
var (
	// ErrValidation: the request is not a publish candidate at all — an
	// empty project, a version label outside the storable shape, an
	// unknown asset type or visibility, a pid that is not a pid, or the
	// asset-creation fields a create needs. The refusal names the field;
	// it never restates the request's content.
	ErrValidation = errors.New("assetpublish: validation failed")
	// ErrAgentNotPermitted: an agent actor attempted the publish. This is
	// the domain backstop, resolved before any lookup — see
	// AgentNotPermittedError and the package doc's two-lines-of-defence
	// note.
	ErrAgentNotPermitted = errors.New("assetpublish: agents may not publish research assets")
	// ErrForbidden: the actor's class does not permit
	// authz.ActionPublishPrivateToPublic. Resolved before any target
	// lookup, so the denial never discloses whether the project or the
	// asset exists (the releases.ErrForbidden rule).
	ErrForbidden = errors.New("assetpublish: the actor may not publish research assets here")
	// ErrProjectNotFound: no project row exists for the given id, or the
	// project is not visible to the caller (existence hiding, docs/45).
	ErrProjectNotFound = errors.New("assetpublish: project not found")
	// ErrAssetNotFound: the request named an asset pid that names no
	// stored asset, or one of another project. A publish CONTINUES an
	// asset or CREATES one; it never adopts an existing identity by
	// guessing (see ErrAssetExists for the other half).
	ErrAssetNotFound = errors.New("assetpublish: asset not found")
	// ErrAssetExists: a publish that asked to create an asset with
	// assets.NewPID() found that pid already taken, or named an asset
	// while asking to create one. A 32^26 pid collision does not happen
	// by accident; this exists so that it cannot be mistaken for a
	// successful create if it ever did.
	ErrAssetExists = errors.New("assetpublish: that pid already names an asset")
	// ErrVersionImmutable: the asset already publishes this version
	// label. research_asset_versions is append-only with
	// UNIQUE(asset_id, version) (migration 00010), so the refusal is the
	// database's own guarantee — a published version is immutable, and a
	// second publish of it is not a new row and not an update.
	ErrVersionImmutable = errors.New("assetpublish: this asset version is already published")
	// ErrIdempotencyConflict: the Idempotency-Key was already used for a
	// DIFFERENT publication (docs/45: 键复用到不同目标). A key that names
	// the same target replays instead; a key reused elsewhere is a
	// client error, because answering it with the other publication's
	// row would hand back a version the caller did not ask for.
	ErrIdempotencyConflict = errors.New("assetpublish: this Idempotency-Key was used for a different publication")
	// ErrPolicyRefused: the governance policy in force does not permit
	// this publication — see PolicyRefusedError.
	ErrPolicyRefused = errors.New("assetpublish: the policy in force does not permit this publication")
	// ErrRefused: the publish's own server-side re-run of the impact
	// preview refused it — see PublishRefused, which carries the whole
	// report.
	ErrRefused = errors.New("assetpublish: the publication was refused")
	// ErrStore: a persistence adapter failed, or the data it returned
	// cannot be rendered (cause kept for the log).
	ErrStore = errors.New("assetpublish: store failure")
)

// Wire codes (docs/45): one code per failure shape, so a client branches
// without parsing messages. The vocabulary the error model fixes is used
// where it exists (ASSET_VERSION_IMMUTABLE, IDEMPOTENCY_CONFLICT,
// PUBLISH_PRIVATE_TO_PUBLIC_REQUIRES_APPROVAL,
// RIGHTS_POLICY_BLOCKS_ACTION, VALIDATION_FAILED); the two codes below it
// does not list name outcomes this surface alone has.
const (
	// CodeValidationFailed: the request is not a publish candidate.
	CodeValidationFailed = "VALIDATION_FAILED"
	// CodePublishPrivateToPublicRequiresApproval: the actor's class does
	// not permit the publication. docs/45's own name for it: in V1 this
	// is the answer for every class except the project owner — a
	// maintainer's cell is `conditional`, and an unresolved condition is
	// a refusal (internal/authz default deny, issue #237).
	CodePublishPrivateToPublicRequiresApproval = "PRIVATE_TO_PUBLIC_REQUIRES_APPROVAL"
	// CodeAgentPublishDenied: an agent actor attempted the publish. A
	// code of its own, although the matrix denies agents too: the two
	// refusals come from different places (the domain backstop versus
	// the matrix cell) and a test that cannot tell them apart cannot show
	// that the backstop is an independent second line.
	CodeAgentPublishDenied = "ASSET_PUBLISH_AGENT_DENIED"
	// CodeProjectNotFound: the project does not exist or is not visible
	// to the caller (existence hiding).
	CodeProjectNotFound = "ASSET_PUBLISH_PROJECT_NOT_FOUND"
	// CodeAssetNotFound: the named asset does not exist, or belongs to
	// another project.
	CodeAssetNotFound = "ASSET_NOT_FOUND"
	// CodeAssetExists: a create found its freshly minted pid taken.
	CodeAssetExists = "ASSET_ALREADY_EXISTS"
	// CodeAssetVersionImmutable: the asset already publishes this
	// version label (docs/45's own code).
	CodeAssetVersionImmutable = "ASSET_VERSION_IMMUTABLE"
	// CodeIdempotencyConflict: the Idempotency-Key was used elsewhere
	// (docs/45's own code).
	CodeIdempotencyConflict = "IDEMPOTENCY_CONFLICT"
	// CodePolicyRefused: the policy in force blocks the publication
	// (docs/45's own code).
	CodePolicyRefused = "RIGHTS_POLICY_BLOCKS_ACTION"
	// CodePublishBlocked: the server-side impact re-check refused the
	// publication. The response carries the COMPLETE report
	// (PublishRefused.Preview), not a sentence about it — docs/23 §4's
	// no-hidden-private-dependency rule is only checkable by a client
	// that can see every entry the publish refused over.
	CodePublishBlocked = "ASSET_PUBLISH_BLOCKED"
	// CodeServiceUnavailable: the publish data is temporarily
	// unavailable.
	CodeServiceUnavailable = "SERVICE_UNAVAILABLE"
)

// AgentNotPermittedError reports a publish refused because the actor is an
// agent. It is the domain's own refusal, not the matrix's: docs/23 §4
// keeps `visibility:publish` out of the agent token scope by default, and
// an agent never performs the human governance action of widening
// visibility (docs/12 §3). Shape copied from
// internal/application/contribution's AgentNotPermittedError, which
// refuses the same kind of action for the same reason.
type AgentNotPermittedError struct {
	// Action is the refused action's wire name ("publish").
	Action string
}

// Error implements error.
func (e *AgentNotPermittedError) Error() string {
	return fmt.Sprintf("assetpublish: agents cannot %s a research asset — publication controls visibility and is a human governance action (docs/23 §4, docs/12 §3)", e.Action)
}

// Unwrap keeps errors.Is(err, ErrAgentNotPermitted) working through the
// wrap.
func (e *AgentNotPermittedError) Unwrap() error { return ErrAgentNotPermitted }

// Code is the stable wire code of this outcome (docs/45).
func (e *AgentNotPermittedError) Code() string { return CodeAgentPublishDenied }

// PolicyRefusedError reports that the governance policy in force blocks
// the publication. Shape copied from merge.PolicyRefusedError, which
// refuses a merge over the same machinery (policy.Service.EffectivePolicy
// + the rule evaluator) for the same reason.
//
// What it does NOT say matters as much as what it does: an ABSENT
// public_asset_ip_review rule is not a refusal. docs/23 §4 lists the org
// policy approvals as optional, and the rule is documented as "requires an
// IP review before a public asset is published" — a policy that does not
// require one does not require one. A policy that could not be READ,
// however, is not a permissive one: the refusal that reports a failed read
// is this type with Found=false and a Reason saying so.
type PolicyRefusedError struct {
	// Rule is the rule key the refusal is about
	// (domain.RulePublicAssetIPReview).
	Rule string
	// Found reports whether the policy set the rule at all.
	Found bool
	// Bool is the rule's value when Found.
	Bool bool
	// Reason is the human-readable line naming which case fired.
	Reason string
	// Err is the underlying failure when the policy could not be read.
	Err error
}

// Error implements error.
func (e *PolicyRefusedError) Error() string {
	return "assetpublish: the policy in force does not permit this publication — " + e.Reason
}

// Unwrap keeps errors.Is(err, ErrPolicyRefused) working, and carries the
// store failure when the refusal was a failed read.
func (e *PolicyRefusedError) Unwrap() error {
	if e.Err != nil {
		return fmt.Errorf("%w: %w", ErrPolicyRefused, e.Err)
	}
	return ErrPolicyRefused
}

// Code is the stable wire code of this outcome (docs/45).
func (e *PolicyRefusedError) Code() string { return CodePolicyRefused }

// PublishRefused is the publication refusal: the publish re-ran the impact
// preview over the repository's CURRENT state, inside its own transaction,
// and something in that state must not ride along into the version it
// would write (docs/22 §7, docs/23 §4).
//
// It carries the whole preview, not a summary of it. That is the same
// decision releases.GateRefused makes for the release gate's report, and it
// matters more here: the refusal is about the things the publication would
// have exposed (a pin to a still-private version, a ref into another
// private project, a blob whose bytes are not open while the rights
// declaration promises they are), and a client that receives "forbidden"
// cannot tell which of its dependencies to fix, nor check that the refusal
// was about the entry it thinks it was.
type PublishRefused struct {
	// Preview is the complete impact preview the refusal was decided on —
	// the same document POST .../assets:publish-preview answers with, so
	// a caller can compare what it previewed against what the publish
	// saw.
	Preview assets.ImpactPreview
	// Reasons names, one line each, the entries that blocked. They are
	// derived from the preview (never from anything the preview does not
	// carry), and they are a convenience: the preview is the record.
	Reasons []string
}

// Error implements error: the reasons, joined, so a log line is readable
// without decoding the preview.
func (e *PublishRefused) Error() string {
	msg := ErrRefused.Error()
	if len(e.Reasons) > 0 {
		msg += ": " + strings.Join(e.Reasons, "; ")
	}
	return msg
}

// Unwrap keeps errors.Is(err, ErrRefused) working through the wrap.
func (e *PublishRefused) Unwrap() error { return ErrRefused }

// Code is the stable wire code of this outcome (docs/45).
func (e *PublishRefused) Code() string { return CodePublishBlocked }
