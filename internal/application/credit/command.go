package credit

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/contribution"
	"github.com/lichman0405/post/internal/domain"
)

// Actor is the principal asking for the operation: the authenticated user,
// and whether the request arrived as a platform agent rather than a human
// session.
//
// The flag is a field of a value the TRANSPORT builds from the principal,
// never a field of a request body: a caller that could set it could clear
// it, and the only thing the flag does is refuse. It is the shape
// assetpublish.Actor and assetrights.Actor have, for the same reason — a
// second spelling of "this call is an agent" is a second place the flag
// could be read from a caller's bytes.
type Actor struct {
	// User is the authenticated user. An agent acts AS a user (the
	// token's owner), so this is never empty.
	User domain.User
	// IsAgent reports whether the request arrived as a platform agent
	// (MCP/API), not as a human session.
	IsAgent bool
}

// DeclareParams is one credit declaration request.
type DeclareParams struct {
	// ProjectID is the project the target belongs to. It is required
	// rather than derived, and that is the authorization's shape: the
	// actor's membership is resolved for THIS project before the target is
	// looked up at all, so the denial precedes the lookup (see
	// Command.authorize). The store refuses a target whose project is not
	// this one, so the pair cannot be used to authorize in one project and
	// write in another.
	ProjectID string
	// TargetRef is the canonical "kind:value" ref of the target
	// (contribution.NewCreditTargetRef): "asset:<pid>", "release:<uuid>"
	// or "finding:<uuid>". docs/13 §2's three targets and no others.
	TargetRef string
	// Parties are the declared parties, in the caller's order. Each
	// carries the role it is declared under; Position is assigned from
	// this order, per role.
	Parties []PartyInput
}

// PartyInput is one party the caller wants declared.
type PartyInput struct {
	// Kind is the identity kind: user or organization. A project is not a
	// credit party here (docs/13 §2 names people-shaped roles, and the
	// originating project is already a column on research_assets).
	Kind domain.PartyKind
	// ID is the row id of that kind, in text uuid form.
	ID string
	// Role is the credit role the party is declared under.
	Role contribution.CreditRole
}

// OpenDisputeParams is one dispute to raise.
type OpenDisputeParams struct {
	// ProjectID is the project the disputed credit belongs to.
	ProjectID string
	// TargetRef is the canonical ref of the disputed credit.
	TargetRef string
	// Claim is what the opener says is wrong with the credit, in their own
	// words (docs/13 §3).
	Claim string
	// EvidenceRefs are the ledger references the claim points at —
	// docs/13 §3's "附带 ledger evidence". Each must be a canonical
	// "kind:value" ref of a kind the Contribution Ledger produces
	// (contribution.ValidLedgerRefKind), so the evidence points at
	// something the ledger actually records. Order is preserved.
	EvidenceRefs []string
}

// CloseDisputeParams is one dispute decision.
type CloseDisputeParams struct {
	// ProjectID is the project the dispute was raised in.
	ProjectID string
	// DisputeID is the credit_disputes row id.
	DisputeID string
	// Outcome is the state the dispute moves to: resolved or rejected.
	// "open" is refused — a close decides.
	Outcome contribution.DisputeState
	// Resolution is governance's reasoning for the decision.
	Resolution string
}

// The free-text bounds. No spec pins a length for a dispute's claim or a
// resolution, so these are L1 decisions (recorded in the task result), and
// they are bounds rather than an absence of one: an unbounded text column
// on a governance record is a place a ledger's worth of prose can land.
const (
	// MaxClaimLen bounds a dispute's claim in characters.
	MaxClaimLen = 4000
	// MaxResolutionLen bounds a decision's reasoning in characters.
	MaxResolutionLen = 4000
	// MaxPartiesPerRole bounds how many parties one declaration may name
	// under one role. A credit list is a list of names, not a directory
	// dump.
	MaxPartiesPerRole = 50
	// MaxEvidenceRefs bounds a dispute's ledger evidence. An evidence list
	// that long is not evidence, it is a query result pasted in.
	MaxEvidenceRefs = 50
)

// Command is the credit use case: declaring a credit, raising a dispute
// against one, deciding a dispute.
//
// It owns the order of the steps, and the order is the contract:
//
//  1. agent  — the domain backstop, from the actor value alone
//  2. shape  — validate the request, before anything is read
//  3. role   — membership class vs the matrix, before any lookup
//  4. write  — the store's transaction: lock, resolve, append, record
//
// Steps 1 and 2 precede every read because neither needs one; step 3
// precedes the target and dispute lookups because a denial must not
// disclose whether they exist (an unknown ref and a target the caller may
// not govern are answered the same).
//
// The matrix row every operation is evaluated against is
// authz.ActionSubmitScientificReview; which classes it admits for which
// operation is Command.authorize's business, and the reasoning is in
// doc.go.
type Command struct {
	members MembershipPort
	store   StorePort
	authz   authz.Engine
}

// Deps carries the adapters a Command is wired over.
type Deps struct {
	// Members resolves the actor's project membership (the production
	// value is *persistence.ProjectStore).
	Members MembershipPort
	// Store is the credit store (the production value is
	// *persistence.CreditStore).
	Store StorePort
	// Authz is the permission-matrix engine (the production value is
	// authz.NewMatrixEngine()).
	Authz authz.Engine
}

// NewCommand wires the credit command.
func NewCommand(deps Deps) *Command {
	return &Command{members: deps.Members, store: deps.Store, authz: deps.Authz}
}

// DeclareAttribution appends one credit declaration for a target and
// returns the statement it wrote.
//
// It never revises one: a correction is this method called again, which
// appends the next ordinal. The declaration it corrects stays readable
// (AttributionHistory) and no ledger row is written, read or rewritten —
// docs/13 §2's "correction 作为新 event，旧 attribution ... 保留", with the
// append-only contribution_events the events that were already there.
func (c *Command) DeclareAttribution(ctx context.Context, actor Actor, in DeclareParams) (contribution.CreditAttribution, error) {
	if err := requireHuman(actor, "declare_attribution"); err != nil {
		return contribution.CreditAttribution{}, err
	}
	req, err := c.prepareDeclare(actor, in)
	if err != nil {
		return contribution.CreditAttribution{}, err
	}
	if err := c.authorize(ctx, actor, in.ProjectID, domain.ProjectRoleMaintainer); err != nil {
		return contribution.CreditAttribution{}, err
	}
	out, err := c.store.DeclareAttribution(ctx, req)
	if err != nil {
		return contribution.CreditAttribution{}, mapError(err)
	}
	return out, nil
}

// OpenDispute raises one credit dispute, recording the credit.dispute_opened
// event for it in the same transaction.
//
// docs/13 §3 gives this to a Contributor: the actor must hold Contributor
// or above in the project, and the evidence the claim carries is a list of
// ledger references — the claim points at the record, it does not rewrite
// it.
func (c *Command) OpenDispute(ctx context.Context, actor Actor, in OpenDisputeParams) (contribution.CreditDispute, error) {
	if err := requireHuman(actor, "open_dispute"); err != nil {
		return contribution.CreditDispute{}, err
	}
	req, err := c.prepareOpen(actor, in)
	if err != nil {
		return contribution.CreditDispute{}, err
	}
	if err := c.authorize(ctx, actor, in.ProjectID, domain.ProjectRoleContributor); err != nil {
		return contribution.CreditDispute{}, err
	}
	out, err := c.store.OpenDispute(ctx, req)
	if err != nil {
		return contribution.CreditDispute{}, mapError(err)
	}
	return out, nil
}

// CloseDispute decides one dispute, recording the credit.dispute_resolved
// event for it in the same transaction.
//
// docs/13 §3 gives this to "Maintainer/Organization governance": the actor
// must hold Maintainer or above. The dispute's current-state row is updated
// in place — that is the design of 00011's table — and its history is the
// event pair the ledger holds, which nothing here rewrites.
func (c *Command) CloseDispute(ctx context.Context, actor Actor, in CloseDisputeParams) (contribution.CreditDispute, error) {
	if err := requireHuman(actor, "close_dispute"); err != nil {
		return contribution.CreditDispute{}, err
	}
	req, err := c.prepareClose(actor, in)
	if err != nil {
		return contribution.CreditDispute{}, err
	}
	if err := c.authorize(ctx, actor, in.ProjectID, domain.ProjectRoleMaintainer); err != nil {
		return contribution.CreditDispute{}, err
	}
	out, err := c.store.CloseDispute(ctx, req)
	if err != nil {
		return contribution.CreditDispute{}, mapError(err)
	}
	return out, nil
}

// requireHuman is the domain backstop: an agent never declares credit and
// never decides a dispute, whatever the matrix says. It reads the actor
// value alone, so it is resolved before any lookup — the same two lines of
// defence a publish and a rights-holder change carry, and for the same
// reason: the matrix's agent column is a policy document a governance
// change may edit, while docs/60 §2's "credit dispute resolution" is a
// product rule the edit cannot waive.
func requireHuman(actor Actor, operation string) error {
	if actor.IsAgent {
		return &AgentNotPermittedError{Operation: operation}
	}
	return nil
}

// prepareDeclare validates the request shape and renders the store request.
//
// Every refusal here is a statement about the REQUEST, made before anything
// is read: a project must be named, the target ref must be a canonical ref
// of a kind docs/13 §2 names, and at least one party must be declared —
// each under one of the two admitted roles, named by a kind AND a uuid, and
// named once. Whether the rows behind those ids EXIST is the store's read,
// because a command that cannot look anything up cannot decide it.
func (c *Command) prepareDeclare(actor Actor, in DeclareParams) (DeclareRequest, error) {
	projectID := strings.TrimSpace(in.ProjectID)
	if projectID == "" {
		return DeclareRequest{}, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	ref, ok := normaliseTargetRef(in.TargetRef)
	if !ok {
		return DeclareRequest{}, fmt.Errorf("%w: target_ref %q is not a canonical \"kind:value\" ref of a kind docs/13 §2 names (asset:<pid>, release:<uuid>, finding:<uuid>)", ErrValidation, in.TargetRef)
	}
	if len(in.Parties) == 0 {
		return DeclareRequest{}, fmt.Errorf("%w: a credit declaration names at least one party", ErrValidation)
	}
	parties := make([]DeclaredParty, 0, len(in.Parties))
	positions := map[contribution.CreditRole]int{}
	seen := map[string]bool{}
	perRole := map[contribution.CreditRole]int{}
	for i, p := range in.Parties {
		if !contribution.ValidCreditRole(p.Role) {
			return DeclareRequest{}, fmt.Errorf("%w: parties[%d].role %q is not one of the two admitted credit roles %v", ErrValidation, i, p.Role, contribution.CreditRoles())
		}
		if !p.Kind.IsIdentity() {
			// A project is not a credit party: docs/13 §2 names
			// people-shaped roles, and the originating project is already
			// a column on research_assets.
			return DeclareRequest{}, fmt.Errorf("%w: parties[%d] names kind %q, and a credit party is a user or an organization", ErrValidation, i, p.Kind)
		}
		id := strings.TrimSpace(p.ID)
		if !domain.ValidUUID(id) {
			return DeclareRequest{}, fmt.Errorf("%w: parties[%d].id %q is not a uuid, so it names no %s row", ErrValidation, i, p.ID, p.Kind)
		}
		// One party once per role: a declaration naming the same person
		// twice under one role is a malformed list, not a stronger claim
		// (and the database's UNIQUE(statement_id, role, party_id) refuses
		// it too — refused here so the caller gets a validation outcome
		// rather than a constraint violation).
		key := string(p.Role) + "\x00" + string(p.Kind) + "\x00" + strings.ToLower(id)
		if seen[key] {
			return DeclareRequest{}, fmt.Errorf("%w: parties[%d] names the same %s under role %q twice", ErrValidation, i, p.Kind, p.Role)
		}
		seen[key] = true
		perRole[p.Role]++
		if perRole[p.Role] > MaxPartiesPerRole {
			return DeclareRequest{}, fmt.Errorf("%w: a declaration names at most %d parties per role", ErrValidation, MaxPartiesPerRole)
		}
		position := positions[p.Role]
		positions[p.Role] = position + 1
		parties = append(parties, DeclaredParty{
			Party:    domain.Party{Kind: p.Kind, ID: id},
			Role:     p.Role,
			Position: position,
		})
	}
	return DeclareRequest{
		ProjectID: projectID,
		TargetRef: ref,
		Parties:   parties,
		Actor:     actor,
		Audit: domain.AuditEntry{
			ActorID:   actor.User.ID,
			Via:       domain.ViaSession,
			Action:    contribution.AuditActionCreditAttributionDeclared,
			TargetRef: ref,
			ProjectID: projectID,
		},
	}, nil
}

// prepareOpen validates a dispute request and renders the store request.
func (c *Command) prepareOpen(actor Actor, in OpenDisputeParams) (OpenDisputeRequest, error) {
	projectID := strings.TrimSpace(in.ProjectID)
	if projectID == "" {
		return OpenDisputeRequest{}, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	ref, ok := normaliseTargetRef(in.TargetRef)
	if !ok {
		return OpenDisputeRequest{}, fmt.Errorf("%w: target_ref %q is not a canonical \"kind:value\" ref of a kind docs/13 §2 names (asset:<pid>, release:<uuid>, finding:<uuid>)", ErrValidation, in.TargetRef)
	}
	claim := strings.TrimSpace(in.Claim)
	if claim == "" {
		return OpenDisputeRequest{}, fmt.Errorf("%w: a dispute states a claim", ErrValidation)
	}
	if len(claim) > MaxClaimLen {
		return OpenDisputeRequest{}, fmt.Errorf("%w: the claim is longer than %d characters", ErrValidation, MaxClaimLen)
	}
	evidence, err := normaliseEvidence(in.EvidenceRefs)
	if err != nil {
		return OpenDisputeRequest{}, err
	}
	return OpenDisputeRequest{
		ProjectID:    projectID,
		TargetRef:    ref,
		Claim:        claim,
		EvidenceRefs: evidence,
		Actor:        actor,
		Audit: domain.AuditEntry{
			ActorID:   actor.User.ID,
			Via:       domain.ViaSession,
			Action:    contribution.AuditActionCreditDisputeOpened,
			TargetRef: ref,
			ProjectID: projectID,
		},
	}, nil
}

// prepareClose validates a decision and renders the store request.
func (c *Command) prepareClose(actor Actor, in CloseDisputeParams) (CloseDisputeRequest, error) {
	projectID := strings.TrimSpace(in.ProjectID)
	if projectID == "" {
		return CloseDisputeRequest{}, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	disputeID := strings.TrimSpace(in.DisputeID)
	if !domain.ValidUUID(disputeID) {
		return CloseDisputeRequest{}, fmt.Errorf("%w: dispute_id %q is not a uuid, so it names no dispute row", ErrValidation, in.DisputeID)
	}
	// Only the two decided states close a dispute. "open" as an outcome is
	// refused rather than read as "leave it open": a close decides, and a
	// request that decides nothing is a malformed request, not a no-op
	// (00103's guard refuses the same value at the database).
	if !in.Outcome.Terminal() {
		return CloseDisputeRequest{}, fmt.Errorf("%w: outcome %q is not a decision — a dispute closes as %q or %q", ErrValidation, in.Outcome, contribution.DisputeStateResolved, contribution.DisputeStateRejected)
	}
	resolution := strings.TrimSpace(in.Resolution)
	if resolution == "" {
		return CloseDisputeRequest{}, fmt.Errorf("%w: closing a dispute states a resolution", ErrValidation)
	}
	if len(resolution) > MaxResolutionLen {
		return CloseDisputeRequest{}, fmt.Errorf("%w: the resolution is longer than %d characters", ErrValidation, MaxResolutionLen)
	}
	return CloseDisputeRequest{
		ProjectID:  projectID,
		DisputeID:  disputeID,
		Outcome:    in.Outcome,
		Resolution: resolution,
		Actor:      actor,
		Audit: domain.AuditEntry{
			ActorID:   actor.User.ID,
			Via:       domain.ViaSession,
			Action:    contribution.AuditActionCreditDisputeResolved,
			TargetRef: string(contribution.RefCreditDispute) + ":" + disputeID,
			ProjectID: projectID,
		},
	}, nil
}

// authorize resolves the actor's membership role and evaluates the re-used
// matrix row for the class, resolving its `conditional` cell as "the actor
// holds minRole or above in this project".
//
// specs/policies/permissions-matrix.csv's submit_scientific_review row is
// `deny,conditional,conditional,conditional,allow,allow,proposal_or_scoped`
// — the one row whose cells fit all three of the sentences this surface
// implements (see doc.go). The reading is:
//
//	VerdictAllow          maintainer/owner. Unconditional: no condition to
//	                      resolve, so it permits.
//	VerdictConditional    viewer, contributor, authenticated non-member. The
//	                      condition is per-resource and this is its
//	                      enforcement site: the actor holds minRole or
//	                      above. A nil role (non-member, unknown project) is
//	                      a refusal — the boundary the task reports.
//	everything else       refused, including agent_default's
//	                      proposal_or_scoped: nothing here defines a scoped
//	                      dispute, so there is no scoped form to resolve and
//	                      the safe default is refusal.
//
// The denial precedes every lookup of the target, and that is the point
// rather than an artifact of the ordering: the membership resolution
// answers projects.ErrMemberNotFound for a project that does not exist
// exactly as it does for one the caller does not belong to, so a caller
// probing refs through this command is answered "not permitted" either way.
// Only projects.ErrProjectNotFound — the deliberate disclosure the project
// surface makes for an invisible project — is relayed, and only when the
// caller could not be resolved a role.
//
// A nil or erroring engine is a wiring failure, never a permission: the
// refusal is ErrStore.
func (c *Command) authorize(ctx context.Context, actor Actor, projectID string, minRole domain.ProjectRole) error {
	if c.members == nil || c.authz == nil {
		return fmt.Errorf("%w: credit command not fully wired", ErrStore)
	}
	membership, err := c.members.GetMembership(ctx, projectID, actor.User.ID)
	var role *domain.ProjectRole
	switch {
	case err == nil:
		r := membership.Role
		role = &r
	case errors.Is(err, projects.ErrMemberNotFound):
		// Not a member (or the project does not exist): no role, so the
		// condition below cannot hold and the class is the
		// authenticated-non-member one.
		role = nil
	case errors.Is(err, projects.ErrProjectNotFound):
		return ErrProjectNotFound
	default:
		return mapError(err)
	}
	decision, err := c.authz.Authorize(ctx, authz.Request{
		Action: authz.ActionSubmitScientificReview,
		Class:  authz.ClassOf(true, role, actor.IsAgent),
	})
	if err != nil {
		return fmt.Errorf("%w: authorize credit operation: %v", ErrStore, err)
	}
	switch {
	case decision.Permits():
		return nil
	case decision.Verdict == authz.VerdictConditional:
		if role != nil && role.AtLeast(minRole) {
			return nil
		}
		return ErrForbidden
	default:
		return ErrForbidden
	}
}

// normaliseTargetRef trims and re-validates a canonical target ref,
// returning it in the one spelling the column stores. A ref whose kind is
// not one of docs/13 §2's three, or whose value is empty or itself carries
// the separator, is not a ref (contribution.ParseCreditTargetRef).
func normaliseTargetRef(ref string) (string, bool) {
	kind, value, ok := contribution.ParseCreditTargetRef(strings.TrimSpace(ref))
	if !ok {
		return "", false
	}
	return contribution.NewCreditTargetRef(kind, value)
}

// normaliseEvidence validates the ledger evidence refs and removes exact
// duplicates, preserving the caller's order. Every ref must be canonical
// and its kind must be one the Contribution Ledger produces
// (contribution.ValidLedgerRefKind): evidence that points at something the
// ledger cannot hold is not evidence of a ledger fact, and recording it
// would make the claim look supported by a row that cannot exist.
func normaliseEvidence(refs []string) ([]string, error) {
	if len(refs) > MaxEvidenceRefs {
		return nil, fmt.Errorf("%w: at most %d evidence refs are recorded", ErrValidation, MaxEvidenceRefs)
	}
	out := make([]string, 0, len(refs))
	seen := map[string]bool{}
	for i, raw := range refs {
		ref := strings.TrimSpace(raw)
		kind, value, ok := strings.Cut(ref, ":")
		if !ok || value == "" || strings.Contains(value, ":") || !contribution.ValidLedgerRefKind(contribution.LedgerRefKind(kind)) {
			return nil, fmt.Errorf("%w: evidence_refs[%d] %q is not a canonical \"kind:value\" ledger ref", ErrValidation, i, raw)
		}
		if seen[ref] {
			continue
		}
		seen[ref] = true
		out = append(out, ref)
	}
	return out, nil
}

// mapError keeps the sentinels of this package and of its ports, and turns
// everything else into ErrStore.
func mapError(err error) error {
	switch {
	case err == nil,
		errors.Is(err, ErrValidation),
		errors.Is(err, ErrAgentNotPermitted),
		errors.Is(err, ErrForbidden),
		errors.Is(err, ErrProjectNotFound),
		errors.Is(err, ErrTargetNotFound),
		errors.Is(err, ErrPartyNotFound),
		errors.Is(err, ErrDisputeNotFound),
		errors.Is(err, ErrDisputeClosed),
		errors.Is(err, ErrStore):
		return err
	}
	return fmt.Errorf("%w: %v", ErrStore, err)
}
