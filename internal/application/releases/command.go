package releases

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// Command is the release governance use case (T0606): create the
// immutable release snapshot and read it back. It composes the T0605
// manifest builder with the pieces a release needs around it —
// authorization (ActionCreateRelease on the matrix), the policy in force
// (docs/12 §5, pinned explicitly so a later policy publication can never
// re-bind an existing release), the server-side release gate (docs/22
// §7: the command re-validates, it never trusts a precheck) and the
// append-only release store.
//
// The reads (List/Get/Manifest) do NOT authorize here: the transport
// resolves the project's read visibility first (the same gate every
// project read runs — a release of a project is exactly as visible as
// the project), then calls them. They do enforce the project boundary:
// a release id of another project is "not found".
type Command struct {
	builder  *Service
	projects ProjectAuthzPort
	policies PolicyLatestPort
	branches MainBranchPort
	heads    BranchHeadPort
	gate     ReleaseGatePort
	store    ReleaseStorePort
	authz    authz.Engine
}

// NewCommand wires the release command over its ports.
func NewCommand(
	builder *Service,
	projects ProjectAuthzPort,
	policies PolicyLatestPort,
	branches MainBranchPort,
	heads BranchHeadPort,
	gate ReleaseGatePort,
	store ReleaseStorePort,
	authz authz.Engine,
) *Command {
	return &Command{
		builder:  builder,
		projects: projects,
		policies: policies,
		branches: branches,
		heads:    heads,
		gate:     gate,
		store:    store,
		authz:    authz,
	}
}

// CreateReleaseParams names one release create: the version string, an
// optional display title, and the caller's Idempotency-Key (nil when the
// request carried none — then a same-version repeat is a conflict, never
// a silent replay).
type CreateReleaseParams struct {
	ProjectID string
	// Version is the release version string ([A-Za-z0-9._-], 1..64).
	Version string
	// Title is the display label; empty means "use the version".
	Title string
	// IdempotencyKey replays the create it names (docs/22): the same
	// key returns the release the first call created, forever.
	IdempotencyKey *string
}

// Create runs the release governance flow and returns the stored
// release:
//
//  1. validate the input shape (version label, title bound);
//  2. authorize: the actor's project membership class vs
//     ActionCreateRelease — the denial precedes every lookup (never
//     disclose whether the project exists);
//  3. replay an Idempotency-Key that already created a release: the
//     stored row comes back as-is, before any snapshot resolution — the
//     same key returns the first create's row forever, no gate re-run
//     (a replay is a read);
//  4. resolve the accepted main snapshot: the project's main branch and
//     its current head (ErrNotMainState when main has no state yet);
//  5. resolve the policy in force: the organization's latest version and
//     the project's latest version, pinned by id (a scope without a
//     policy contributes no pin — the gate then refuses);
//  6. run the release gate server-side over main's persisted snapshot
//     with the release facts; a blocking report refuses the create
//     (ErrReleaseGate carrying the report);
//  7. build the manifest from those pinned inputs and persist the row —
//     the store writes the release, the idempotency ledger entry, the
//     audit row and the release.published event in one transaction.
//
// The same version repeated with the SAME Idempotency-Key replays the
// first create (idempotent); with a different or missing key it answers
// ErrVersionTaken (conflict) — the acceptance "同 version 重复
// idempotent/conflict".
func (c *Command) Create(ctx context.Context, actor domain.User, in CreateReleaseParams) (domain.Release, error) {
	version, title, err := validateCreateParams(in)
	if err != nil {
		return domain.Release{}, err
	}
	if err := c.requireCreate(ctx, actor.ID, in.ProjectID); err != nil {
		return domain.Release{}, err
	}
	if in.IdempotencyKey != nil {
		replayed, err := c.store.LookupCreation(ctx, in.ProjectID, *in.IdempotencyKey)
		if err != nil {
			return domain.Release{}, mapCreateError(err)
		}
		if replayed != nil {
			return *replayed, nil
		}
	}
	project, mainBranch, head, err := c.resolveMainHead(ctx, in.ProjectID)
	if err != nil {
		return domain.Release{}, err
	}
	pin, err := c.resolvePolicyInForce(ctx, project)
	if err != nil {
		return domain.Release{}, err
	}
	if err := c.runReleaseGate(ctx, project.ID, mainBranch.ID, head.ID, pin); err != nil {
		return domain.Release{}, err
	}
	m, err := c.builder.Build(ctx, BuildParams{
		ProjectID:              project.ID,
		StateID:                head.ID,
		Version:                version,
		OrgPolicyVersionID:     pinID(pin.Organization),
		ProjectPolicyVersionID: pinID(pin.Project),
	})
	if err != nil {
		return domain.Release{}, err
	}
	doc, err := m.CanonicalJSON()
	if err != nil {
		return domain.Release{}, fmt.Errorf("%w: render release manifest: %v", ErrStore, err)
	}
	release := domain.Release{
		ProjectID:          project.ID,
		Version:            version,
		Title:              title,
		StateID:            head.ID,
		PolicyVersionID:    pinID(pin.Project),
		OrgPolicyVersionID: pinID(pin.Organization),
		Manifest:           json.RawMessage(doc),
		ManifestHash:       m.ManifestHash,
		CreatedBy:          actor.ID,
		CreatedAt:          m.GeneratedAt,
		ID:                 "", // the store's insert assigns it
	}
	stored, err := c.store.CreateRelease(ctx, release, c.auditEntry(actor, project, version, head.ID, m.ManifestHash), in.IdempotencyKey)
	if err != nil {
		return domain.Release{}, mapCreateError(err)
	}
	return stored, nil
}

// auditEntry renders the release.created audit row the store writes in
// the create transaction (the same pattern as the policy store: the
// owning store appends the audit row inside its own transaction). The
// target ref is left for the store: it writes the row after the release
// insert, so it names the assigned release id ("release:<id>").
func (c *Command) auditEntry(actor domain.User, project domain.Project, version, stateID, manifestHash string) domain.AuditEntry {
	return domain.AuditEntry{
		ActorID:   actor.ID,
		Via:       domain.ViaSession,
		Action:    domain.ActionReleaseCreated,
		ProjectID: project.ID,
		AfterSummary: map[string]any{
			"version":       version,
			"state_id":      stateID,
			"manifest_hash": manifestHash,
		},
	}
}

// List returns the project's releases, newest first. Visibility was
// resolved by the caller.
func (c *Command) List(ctx context.Context, projectID string) ([]domain.Release, error) {
	if projectID == "" {
		return nil, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	releases, err := c.store.ListReleases(ctx, projectID)
	if err != nil {
		return nil, mapCreateError(err)
	}
	return releases, nil
}

// Get returns one release of the project; a release of another project —
// or an unknown id — answers ErrReleaseNotFound (existence hiding,
// docs/45).
func (c *Command) Get(ctx context.Context, projectID, releaseID string) (domain.Release, error) {
	if projectID == "" || releaseID == "" {
		return domain.Release{}, fmt.Errorf("%w: project_id and release_id are required", ErrValidation)
	}
	release, err := c.store.GetRelease(ctx, projectID, releaseID)
	if err != nil {
		return domain.Release{}, mapCreateError(err)
	}
	return release, nil
}

// Manifest returns the release's canonical manifest document bytes —
// rendered ONLY from the stored snapshot row, verified against its
// manifest hash before it is served (a stored row whose content no
// longer verifies is refused, never shipped). This is the export: the
// same bytes forever, no matter how the project's current state evolves
// (acceptance: 旧 release 不受 current state 改变).
func (c *Command) Manifest(ctx context.Context, projectID, releaseID string) ([]byte, error) {
	release, err := c.Get(ctx, projectID, releaseID)
	if err != nil {
		return nil, err
	}
	var m ReleaseManifest
	if err := json.Unmarshal(release.Manifest, &m); err != nil {
		return nil, fmt.Errorf("%w: stored manifest of release %s does not parse: %v", ErrStore, release.ID, err)
	}
	if m.ManifestHash != release.ManifestHash || !m.VerifyHash() {
		return nil, fmt.Errorf("%w: stored manifest of release %s does not verify against its manifest hash", ErrStore, release.ID)
	}
	out, err := m.CanonicalJSON()
	if err != nil {
		return nil, fmt.Errorf("%w: render manifest of release %s: %v", ErrStore, release.ID, err)
	}
	return out, nil
}

// validateCreateParams checks the input shape and returns the stored
// forms: the version trimmed (the same rule the policy versions use —
// the stored string is the one validation saw) and the title trimmed
// (the version when blank).
func validateCreateParams(in CreateReleaseParams) (version, title string, err error) {
	if in.ProjectID == "" {
		return "", "", fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	version = strings.TrimSpace(in.Version)
	if !domain.ValidPolicyVersion(version) {
		return "", "", fmt.Errorf("%w: release version %q must be 1..64 characters of [A-Za-z0-9._-]", ErrValidation, in.Version)
	}
	title = strings.TrimSpace(in.Title)
	if title == "" {
		title = version
	}
	if !domain.ValidReleaseTitle(title) {
		return "", "", fmt.Errorf("%w: release title must be 1..%d characters", ErrValidation, domain.MaxReleaseTitleLen)
	}
	return version, title, nil
}

// requireCreate authorizes the create: resolve the caller's membership
// role (the resolution itself runs the project read gate — an invisible
// project answers not-found before any role is decided), then evaluate
// ActionCreateRelease for the class. The denial precedes every lookup,
// so it never discloses whether the release target exists.
func (c *Command) requireCreate(ctx context.Context, actorID, projectID string) error {
	if c.projects == nil {
		return fmt.Errorf("%w: no project gate configured", ErrStore)
	}
	membership, err := c.projects.GetMembership(ctx, projectID, actorID)
	var role *domain.ProjectRole
	switch {
	case err == nil:
		r := membership.Role
		role = &r
	case errors.Is(err, projects.ErrMemberNotFound):
		role = nil
	case errors.Is(err, projects.ErrProjectNotFound):
		return ErrProjectNotFound // existence hiding, never "forbidden"
	default:
		return mapCreateError(err)
	}
	if c.authz == nil {
		return fmt.Errorf("%w: no policy engine configured", ErrStore)
	}
	decision, err := c.authz.Authorize(ctx, authz.Request{
		Action: authz.ActionCreateRelease,
		Class:  authz.ClassOf(true, role, false),
	})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrStore, err)
	}
	if !decision.Permits() {
		return ErrForbidden
	}
	return nil
}

// resolveMainHead resolves the release's premise: the project row, its
// main branch and main's current head state.
func (c *Command) resolveMainHead(ctx context.Context, projectID string) (domain.Project, domain.Branch, domain.ProjectState, error) {
	project, err := c.projects.GetProject(ctx, projectID)
	if err != nil {
		if errors.Is(err, projects.ErrProjectNotFound) {
			return domain.Project{}, domain.Branch{}, domain.ProjectState{}, ErrProjectNotFound
		}
		return domain.Project{}, domain.Branch{}, domain.ProjectState{}, fmt.Errorf("%w: read project: %v", ErrStore, err)
	}
	branches, err := c.branches.ListBranches(ctx, projectID)
	if err != nil {
		return domain.Project{}, domain.Branch{}, domain.ProjectState{}, fmt.Errorf("%w: read branches: %v", ErrStore, err)
	}
	var mainBranch domain.Branch
	for _, b := range branches {
		if b.IsMain() {
			mainBranch = b
			break
		}
	}
	if mainBranch.ID == "" {
		return domain.Project{}, domain.Branch{}, domain.ProjectState{}, ErrNotMainState
	}
	head, err := c.heads.GetBranchHead(ctx, mainBranch.ID)
	if err != nil {
		// Main without a state is not a releasable snapshot.
		return domain.Project{}, domain.Branch{}, domain.ProjectState{}, ErrNotMainState
	}
	return project, mainBranch, head, nil
}

// resolvePolicyInForce pins the two policy versions in force at release
// time (docs/12 §5): the organization's latest (when the project has an
// organization) and the project's latest. A scope without a policy
// contributes no pin — the release gate then refuses on release_rights,
// as it should (a release without a rights snapshot is not a document).
func (c *Command) resolvePolicyInForce(ctx context.Context, project domain.Project) (*PolicyPin, error) {
	var pin PolicyPin
	if project.OrganizationID != nil {
		v, err := c.policies.Latest(ctx, domain.PolicyScope{OrganizationID: *project.OrganizationID})
		switch {
		case err == nil:
			pinned, err := pinnedPolicyVersion(v)
			if err != nil {
				return nil, err
			}
			pin.Organization = &pinned
		case errors.Is(err, policy.ErrPolicyNotFound):
			// no org policy — no pin
		default:
			return nil, fmt.Errorf("%w: read org policy in force: %v", ErrStore, err)
		}
	}
	v, err := c.policies.Latest(ctx, domain.PolicyScope{ProjectID: project.ID})
	switch {
	case err == nil:
		pinned, err := pinnedPolicyVersion(v)
		if err != nil {
			return nil, err
		}
		pin.Project = &pinned
	case errors.Is(err, policy.ErrPolicyNotFound):
		// no project policy — no pin
	default:
		return nil, fmt.Errorf("%w: read project policy in force: %v", ErrStore, err)
	}
	if pin.Organization == nil && pin.Project == nil {
		return nil, nil
	}
	return &pin, nil
}

// pinnedPolicyVersion renders one policy version row as its pin: the
// canonical document bytes plus the row identity. A stored policy that
// does not marshal is a store failure — never silently dropped (the
// same rule as the builder's resolvePolicyPin).
func pinnedPolicyVersion(v domain.PolicyVersion) (PinnedPolicyVersion, error) {
	doc, err := v.Policy.MarshalJSON()
	if err != nil {
		return PinnedPolicyVersion{}, fmt.Errorf("%w: render policy version %s: %v", ErrStore, v.ID, err)
	}
	return PinnedPolicyVersion{
		ID:        v.ID,
		Version:   v.Version,
		Document:  doc,
		CreatedBy: v.CreatedBy,
		CreatedAt: v.CreatedAt,
	}, nil
}

// runReleaseGate assembles the release facts and runs the gate engine
// over main's persisted snapshot — the command's own re-validation
// (docs/22 §7). A blocking report refuses the create with the full
// report attached.
func (c *Command) runReleaseGate(ctx context.Context, projectID, mainBranchID, headID string, pin *PolicyPin) error {
	var orgPinID, projectPinID *string
	if pin != nil {
		orgPinID = pinID(pin.Organization)
		projectPinID = pinID(pin.Project)
	}
	facts, err := c.builder.ReleaseFacts(ctx, FactsParams{
		ProjectID:              projectID,
		StateID:                headID,
		OrgPolicyVersionID:     orgPinID,
		ProjectPolicyVersionID: projectPinID,
	})
	if err != nil {
		return err
	}
	report, err := c.gate.ValidateBranchWithFacts(ctx, projectID, mainBranchID, rsgvalidation.GateRelease, &facts, nil)
	if err != nil {
		return mapCreateError(err)
	}
	if report.Blocked() {
		return &GateRefused{Report: report}
	}
	return nil
}

// pinID extracts the pinned version id; nil pins carry nil ids.
func pinID(p *PinnedPolicyVersion) *string {
	if p == nil {
		return nil
	}
	id := p.ID
	return &id
}

// mapCreateError keeps the store's own sentinels (ErrVersionTaken,
// ErrReleaseNotFound — the store reports them directly) and turns
// everything else into ErrStore.
func mapCreateError(err error) error {
	if err == nil ||
		errors.Is(err, ErrVersionTaken) ||
		errors.Is(err, ErrReleaseNotFound) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrStore, err)
}
