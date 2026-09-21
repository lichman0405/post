package attestations

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
)

// Command is the attestation use case (T0812): the governance write that
// lets a project state, in public, that it validated a public version
// somebody else's project published.
//
// # The order of the steps, and why it is the order
//
//  1. shape      — validate the request, before anything is read
//  2. agent      — the domain backstop, before anything is read
//  3. authorize  — membership class vs the matrix, before anything is read
//  4. resolve    — the store's read of what the decision is about
//  5. decide     — Judge over those facts
//  6. write      — the store's transaction, which re-resolves and re-runs
//     Judge over its own view before it writes (docs/22 §7)
//
// Steps 2 and 3 come before every lookup on purpose: a refusal there must
// not disclose whether the project or the target version exists — the same
// ordering and the same reason as knowledgepublish.Command.
//
// # The matrix row is evaluated for EVERY attestation
//
// authz.ActionPublishPrivateToPublic is the action, and it is the literal
// reading of the row rather than a convenient one: an attestation is a
// private project publishing a judgment to the network, and docs/12 §3
// puts every private→public transition behind an explicitly confirmed,
// authorised action. Its visible consequence is intended and fail-closed:
// with the matrix as it stands (maintainer = conditional, owner = allow)
// V1 attestations are an OWNER action, because a conditional cell whose
// condition has no specification resolves to a refusal —
// internal/authz's Permits() admits only `allow`.
type Command struct {
	members MembershipPort
	store   StorePort
	authz   authz.Engine
	newPID  func() (domain.PID, error)
}

// Deps carries the adapters a Command is wired over.
type Deps struct {
	// Members resolves the actor's project membership (the production value
	// is *persistence.ProjectStore).
	Members MembershipPort
	// Store is the attestation store (the production value is
	// *persistence.AttestationStore). It answers the fact resolution too —
	// including the organization's standing setting — so this command reads
	// nothing itself.
	Store StorePort
	// Authz is the permission-matrix engine (the production value is
	// authz.NewMatrixEngine()).
	Authz authz.Engine
	// NewPID mints the attestation's persistent identifier. Tests inject a
	// deterministic sequence; production leaves it nil and gets
	// domain.NewPID.
	NewPID func() (domain.PID, error)
}

// NewCommand wires the attestation command.
func NewCommand(deps Deps) *Command {
	c := &Command{
		members: deps.Members,
		store:   deps.Store,
		authz:   deps.Authz,
		newPID:  deps.NewPID,
	}
	if c.newPID == nil {
		c.newPID = func() (domain.PID, error) { return domain.NewPID() }
	}
	return c
}

// PublishParams is one attestation request — and it is the preview's
// parameters as well, which is a difference from knowledgepublish rather
// than an omission.
//
// There, the preview takes less than the publish, because the publication's
// public_version is part of EXECUTING a proposal rather than of asking
// whether one is admitted. Here the preview must carry everything the
// statement would say, because what a caller is previewing is precisely the
// difference between what it says and what that discloses: a preview that
// could not see org_visibility could not tell a caller whether the
// organization would be named.
type PublishParams struct {
	// ProjectID is the attesting project.
	ProjectID string
	// TargetRef names the version the attestation is about, as the
	// transport received it: `object_version:<uuid>` or
	// `asset_version:<uuid>` (ParseTargetRef).
	TargetRef string
	// ValidationType is one of ValidationTypes().
	ValidationType string
	// ValidationResult is one of ValidationResults().
	ValidationResult string
	// OrgVisibility is the attribution to record: one of
	// OrgVisibilities(). "named" is admitted only when the organization's
	// standing setting permits it, and only when the project has an
	// organization at all.
	OrgVisibility string
	// BasisStateID is the attesting project's OWN state the attestation
	// rests on. The private evidence lives in that state's manifest; the
	// id is recorded and never published.
	BasisStateID string
	// InternalReviewID is the review of exactly that state, on a pull
	// request of the attesting project, that authorised the statement.
	InternalReviewID string
}

// want renders the public half of the request.
func (p PublishParams) want() Want {
	return Want{
		ValidationType:   p.ValidationType,
		ValidationResult: p.ValidationResult,
		OrgVisibility:    p.OrgVisibility,
	}
}

// prepare validates the request shape and parses the target reference,
// before anything is read.
//
// It takes the actor because the resolution it builds is reader-relative:
// the target is resolved through the actor's own eyes, and the actor is the
// only honest source of that (a reader id from the request body would let a
// caller resolve a target as somebody else and undo the whole rule). The
// value travels with the request to the store, so the preview's read and the
// publish's re-read resolve the target for the same reader.
func (c *Command) prepare(a Actor, p PublishParams) (ResolveRequest, Want, error) {
	projectID := strings.TrimSpace(p.ProjectID)
	if !domain.ValidUUID(projectID) {
		return ResolveRequest{}, Want{}, fmt.Errorf("%w: project_id must name a project", ErrValidation)
	}
	ref, err := ParseTargetRef(p.TargetRef)
	if err != nil {
		return ResolveRequest{}, Want{}, err
	}
	w := p.want()
	if !ValidValidationType(w.ValidationType) {
		return ResolveRequest{}, Want{}, fmt.Errorf("%w: validation_type %q is not one of %s", ErrValidation, w.ValidationType, strings.Join(ValidationTypes(), ", "))
	}
	if !ValidValidationResult(w.ValidationResult) {
		return ResolveRequest{}, Want{}, fmt.Errorf("%w: validation_result %q is not one of %s", ErrValidation, w.ValidationResult, strings.Join(ValidationResults(), ", "))
	}
	if !ValidOrgVisibility(w.OrgVisibility) {
		return ResolveRequest{}, Want{}, fmt.Errorf("%w: org_visibility %q is not one of %s", ErrValidation, w.OrgVisibility, strings.Join(OrgVisibilities(), ", "))
	}
	basis := strings.TrimSpace(p.BasisStateID)
	if !domain.ValidUUID(basis) {
		return ResolveRequest{}, Want{}, fmt.Errorf("%w: basis_state_id must name a project state — it is the attesting project's own state the attestation rests on", ErrValidation)
	}
	review := strings.TrimSpace(p.InternalReviewID)
	if !domain.ValidUUID(review) {
		return ResolveRequest{}, Want{}, fmt.Errorf("%w: internal_review_id must name a review — it is the review of the basis state that authorised the statement (docs/22 §28 requires an attestation to be an authorised, internally reviewed act)", ErrValidation)
	}
	return ResolveRequest{
		ProjectID:             projectID,
		ReaderID:              a.User.ID,
		TargetObjectVersionID: ref.ObjectVersionID,
		TargetAssetVersionID:  ref.AssetVersionID,
		BasisStateID:          basis,
		InternalReviewID:      review,
	}, w, nil
}

// Preview answers what an attestation would publish and what it would
// withhold, without writing anything.
//
// It runs the same steps 1–5 the publish runs and stops before step 6. A
// preview that refuses reports the refusal as a *Refused — the caller asked
// a question and the answer is no, which is not an error condition the
// transport should turn into a status code without the reasons — and one
// that is admissible returns the document the publish would produce.
func (c *Command) Preview(ctx context.Context, actor Actor, params PublishParams) (Preview, error) {
	resolve, want, err := c.prepare(actor, params)
	if err != nil {
		return Preview{}, err
	}
	if err := requireAgent(actor); err != nil {
		return Preview{}, err
	}
	if err := c.requirePublish(ctx, actor.User.ID, resolve.ProjectID, actor.IsAgent); err != nil {
		return Preview{}, err
	}
	facts, err := c.store.ResolveFacts(ctx, resolve)
	if err != nil {
		return Preview{}, mapAttestError(err)
	}
	preview := BuildPreview(facts, want)
	if !preview.Admissible {
		return preview, &Refused{Preview: preview, Reasons: reasonLines(preview.Blocking)}
	}
	return preview, nil
}

// Publish writes one attestation.
//
// Everything before the store call is shape, permission and a decision the
// caller is shown; the WRITE is the store's transaction, which re-resolves
// the facts over its own view and re-runs Judge before it inserts. The
// facts resolved here are carried for the same reason knowledgepublish
// carries its own: a refusal can report the document the caller previewed
// against, and the store's copy is the one that decides.
func (c *Command) Publish(ctx context.Context, actor Actor, params PublishParams) (Attested, error) {
	resolve, want, err := c.prepare(actor, params)
	if err != nil {
		return Attested{}, err
	}
	if err := requireAgent(actor); err != nil {
		return Attested{}, err
	}
	if err := c.requirePublish(ctx, actor.User.ID, resolve.ProjectID, actor.IsAgent); err != nil {
		return Attested{}, err
	}
	pid, err := c.mintPID()
	if err != nil {
		return Attested{}, err
	}
	stored, err := c.store.Attest(ctx, AttestRequest{
		PID:              string(pid),
		Resolve:          resolve,
		ValidationType:   want.ValidationType,
		ValidationResult: want.ValidationResult,
		OrgVisibility:    want.OrgVisibility,
		Actor:            actor,
		Audit:            auditEntry(actor.User, resolve, want),
	})
	if err != nil {
		return Attested{}, mapAttestError(err)
	}
	return stored, nil
}

// requirePublish authorizes the attestation: resolve the actor's membership
// role and evaluate authz.ActionPublishPrivateToPublic for the class.
//
// The denial precedes every lookup, and that is the point rather than an
// artifact of the ordering: the membership resolution answers
// projects.ErrMemberNotFound for a project that does not exist exactly as it
// does for one the caller does not belong to, so a caller probing for
// projects with this route is answered "not permitted" either way. Only
// projects.ErrProjectNotFound — the deliberate disclosure the project
// surface makes for an invisible project — is relayed as a not-found.
//
// A nil or erroring engine is a wiring failure, never a permission: the
// refusal is ErrStore. Same shape as knowledgepublish.Command.requirePublish.
func (c *Command) requirePublish(ctx context.Context, actorID, projectID string, isAgent bool) error {
	if c.members == nil || c.authz == nil {
		return fmt.Errorf("%w: attestation command not fully wired", ErrStore)
	}
	membership, err := c.members.GetMembership(ctx, projectID, actorID)
	var role *domain.ProjectRole
	switch {
	case err == nil:
		r := membership.Role
		role = &r
	case errors.Is(err, projects.ErrMemberNotFound):
		role = nil
	case errors.Is(err, projects.ErrProjectNotFound):
		return ErrProjectNotFound
	default:
		return mapAttestError(err)
	}
	decision, err := c.authz.Authorize(ctx, authz.Request{
		Action: authz.ActionPublishPrivateToPublic,
		Class:  authz.ClassOf(true, role, isAgent),
	})
	if err != nil {
		return fmt.Errorf("%w: authorize attestation: %v", ErrStore, err)
	}
	if !decision.Permits() {
		return ErrForbidden
	}
	return nil
}

// requireAgent is the domain backstop: an agent never issues an attestation,
// whatever the matrix says.
//
// It is a package rule and not a matrix cell because it is not a question
// about authority: an attestation is a statement a GOVERNED BODY makes about
// somebody else's work, and docs/23 §4 puts the platform's highest-risk
// operations behind a human's explicit action. An agent may prepare one, and
// may be given the token of a user who could issue it; the act itself is a
// human's. It reads the actor value alone, so it too is resolved before any
// lookup.
func requireAgent(actor Actor) error {
	if actor.IsAgent {
		return &AgentNotPermittedError{Action: "attest"}
	}
	return nil
}

// mintPID mints the attestation's persistent identifier and checks the
// generator's work: a pid that does not satisfy the format is a wiring
// fault, not a caller error, and the column CHECK would refuse the row
// anyway — this answers with a sentence instead of a constraint violation.
func (c *Command) mintPID() (domain.PID, error) {
	pid, err := c.newPID()
	if err != nil {
		return "", fmt.Errorf("%w: mint a pid: %v", ErrStore, err)
	}
	if !assets.ValidPID(string(pid)) {
		return "", fmt.Errorf("%w: the pid generator produced %q, which is not a persistent identifier (26 lowercase Crockford base32 characters)", ErrStore, pid)
	}
	return pid, nil
}

// auditEntry renders the audit row the store appends inside the attest
// transaction. TargetRef is left empty: the store fills it with
// "attestation:<pid>" once the insert has assigned the pid.
//
// The audit row is project-scoped (audit_log.ProjectID) and read only by
// the project's own Activity page, so it may record what the public record
// deliberately does not: which version was attested, and which state the
// statement rested on. An audit that could not say what was done would not
// be an audit.
func auditEntry(actor domain.User, resolve ResolveRequest, w Want) domain.AuditEntry {
	target := resolve.TargetObjectVersionID
	if target == "" {
		target = resolve.TargetAssetVersionID
	}
	return domain.AuditEntry{
		ActorID:   actor.ID,
		Via:       domain.ViaSession,
		Action:    ActionAttestationCreated,
		ProjectID: resolve.ProjectID,
		AfterSummary: map[string]any{
			"validation_type":    w.ValidationType,
			"validation_result":  w.ValidationResult,
			"org_visibility":     w.OrgVisibility,
			"target_version_id":  target,
			"basis_state_id":     resolve.BasisStateID,
			"internal_review_id": resolve.InternalReviewID,
		},
	}
}

// reasonLines renders a refusal report's entries as the one-line reasons
// *Refused carries beside the preview.
func reasonLines(reasons []Reason) []string {
	out := make([]string, 0, len(reasons))
	for _, r := range reasons {
		out = append(out, r.Detail)
	}
	return out
}

// mapAttestError keeps the sentinels of this package and of its ports (the
// port contracts) and turns everything else into ErrStore.
func mapAttestError(err error) error {
	switch {
	case err == nil,
		errors.Is(err, ErrValidation),
		errors.Is(err, ErrAgentNotPermitted),
		errors.Is(err, ErrForbidden),
		errors.Is(err, ErrProjectNotFound),
		errors.Is(err, ErrTargetNotFound),
		errors.Is(err, ErrBasisNotFound),
		errors.Is(err, ErrReviewNotFound),
		errors.Is(err, ErrRefused),
		errors.Is(err, ErrStore):
		return err
	}
	return fmt.Errorf("%w: %v", ErrStore, err)
}
