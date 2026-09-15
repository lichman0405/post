package milestones

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/releases"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
)

// Command is the project milestone use case (T0609): record a
// research-timeline marker and read the project's timeline back. It
// authorizes the create against the matrix (ActionCreateRelease — the
// governance action whose maintainer/owner classes match the release
// surface; the milestone vocabulary has no action of its own yet and
// internal/authz is outside this task's scope, recorded in the task
// result), validates the optional release link against the project
// boundary, and persists through the milestone store.
//
// The reads (List/Get) do NOT authorize here: the transport resolves the
// project's read visibility first (the same gate every project read runs
// — a milestone is exactly as visible as the project), then calls them.
// They do enforce the project boundary: a milestone id of another
// project is "not found".
//
// Recording a milestone never touches the project lifecycle — no
// activity_status transition exists anywhere in this flow (the project
// state machine keeps no forced "completed" terminal state, docs/43 §1).
type Command struct {
	projects ProjectAuthzPort
	releases ReleasePort
	store    StorePort
	authz    authz.Engine
}

// NewCommand wires the milestone command over its ports.
func NewCommand(projects ProjectAuthzPort, releases ReleasePort, store StorePort, authz authz.Engine) *Command {
	return &Command{projects: projects, releases: releases, store: store, authz: authz}
}

// CreateMilestoneParams names one milestone record: the kind, the custom
// label (required for custom, optional otherwise), the occurred_at date
// (the timeline position), the optional release link and the caller's
// Idempotency-Key (nil when the request carried none).
type CreateMilestoneParams struct {
	ProjectID string
	Kind      domain.MilestoneKind
	Label     string
	// OccurredAt is the event's date — the milestone's position on the
	// timeline. It must be a real timestamp (the zero time is refused);
	// no range is enforced (a recorded date may be past or future).
	OccurredAt time.Time
	// ReleaseID optionally names the release this milestone documents;
	// when set it must be a release of the same project (the link is
	// optional — never required).
	ReleaseID *string
	// IdempotencyKey replays the create it names (docs/22): the same
	// key returns the milestone the first call created, forever.
	IdempotencyKey *string
}

// Create records one milestone and returns the stored row:
//
//  1. validate the input shape (kind, label rule, occurred_at);
//  2. authorize: the actor's project membership class vs
//     ActionCreateRelease — the denial precedes every lookup (never
//     disclose whether the project exists);
//  3. replay an Idempotency-Key that already created a milestone: the
//     stored row comes back as-is, before any release resolution — the
//     same key returns the first create's row forever, no link
//     re-validation (a replay is a read);
//  4. resolve the project row (the audit scope);
//  5. resolve the optional release link against the project boundary —
//     a link that does not name a release of this project answers
//     ErrReleaseNotFound (existence hiding, docs/45);
//  6. persist — the store writes the milestone row, the idempotency
//     ledger entry and the audit row in one transaction.
func (c *Command) Create(ctx context.Context, actor domain.User, in CreateMilestoneParams) (domain.Milestone, error) {
	kind, label, err := validateCreateParams(in)
	if err != nil {
		return domain.Milestone{}, err
	}
	if err := c.requireCreate(ctx, actor.ID, in.ProjectID); err != nil {
		return domain.Milestone{}, err
	}
	if in.IdempotencyKey != nil {
		replayed, err := c.store.LookupCreation(ctx, in.ProjectID, *in.IdempotencyKey)
		if err != nil {
			return domain.Milestone{}, mapStoreError(err)
		}
		if replayed != nil {
			return *replayed, nil
		}
	}
	project, err := c.projects.GetProject(ctx, in.ProjectID)
	if err != nil {
		if errors.Is(err, projects.ErrProjectNotFound) {
			return domain.Milestone{}, ErrProjectNotFound
		}
		return domain.Milestone{}, fmt.Errorf("%w: read project: %v", ErrStore, err)
	}
	if err := c.resolveReleaseLink(ctx, in.ProjectID, in.ReleaseID); err != nil {
		return domain.Milestone{}, err
	}
	m := domain.Milestone{
		ProjectID:  project.ID,
		Kind:       kind,
		Label:      label,
		OccurredAt: in.OccurredAt,
		ReleaseID:  in.ReleaseID,
		CreatedBy:  actor.ID,
		ID:         "", // the store's insert assigns it
	}
	stored, err := c.store.CreateMilestone(ctx, m, c.auditEntry(actor, project, m), in.IdempotencyKey)
	if err != nil {
		return domain.Milestone{}, mapStoreError(err)
	}
	return stored, nil
}

// auditEntry renders the milestone.created audit row the store writes in
// the create transaction (the same pattern as the release store: the
// owning store appends the audit row inside its own transaction). The
// target ref is left for the store: it writes the row after the milestone
// insert, so it names the assigned milestone id ("milestone:<id>").
func (c *Command) auditEntry(actor domain.User, project domain.Project, m domain.Milestone) domain.AuditEntry {
	summary := map[string]any{
		"kind":        string(m.Kind),
		"occurred_at": m.OccurredAt,
	}
	if m.Label != "" {
		summary["label"] = m.Label
	}
	if m.ReleaseID != nil {
		summary["release_id"] = *m.ReleaseID
	}
	return domain.AuditEntry{
		ActorID:      actor.ID,
		Via:          domain.ViaSession,
		Action:       domain.ActionMilestoneCreated,
		ProjectID:    project.ID,
		AfterSummary: summary,
	}
}

// List returns the project's milestone timeline: occurred_at ascending
// (the events' dates — the canonical kinds' natural progression),
// creation order breaking date ties. The store owns the order (its port
// contract); insertion order never influences it, so milestones recorded
// out of chronological order still render in research order. Visibility
// was resolved by the caller.
func (c *Command) List(ctx context.Context, projectID string) ([]domain.Milestone, error) {
	if projectID == "" {
		return nil, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	milestones, err := c.store.ListMilestones(ctx, projectID)
	if err != nil {
		return nil, mapStoreError(err)
	}
	return milestones, nil
}

// Get returns one milestone of the project; a milestone of another
// project — or an unknown id — answers ErrMilestoneNotFound (existence
// hiding, docs/45).
func (c *Command) Get(ctx context.Context, projectID, milestoneID string) (domain.Milestone, error) {
	if projectID == "" || milestoneID == "" {
		return domain.Milestone{}, fmt.Errorf("%w: project_id and milestone_id are required", ErrValidation)
	}
	milestone, err := c.store.GetMilestone(ctx, projectID, milestoneID)
	if err != nil {
		return domain.Milestone{}, mapStoreError(err)
	}
	return milestone, nil
}

// validateCreateParams checks the input shape and returns the stored
// forms: the label trimmed (the stored string is the one validation saw;
// empty for a canonical kind without a custom label).
func validateCreateParams(in CreateMilestoneParams) (domain.MilestoneKind, string, error) {
	if in.ProjectID == "" {
		return "", "", fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	if !domain.ValidMilestoneKind(in.Kind) {
		return "", "", fmt.Errorf("%w: unknown milestone kind %q", ErrValidation, in.Kind)
	}
	if in.OccurredAt.IsZero() {
		return "", "", fmt.Errorf("%w: occurred_at is required", ErrValidation)
	}
	label := strings.TrimSpace(in.Label)
	if !domain.ValidMilestoneLabel(label, in.Kind) {
		return "", "", fmt.Errorf("%w: a custom milestone requires a label; any label is at most %d characters",
			ErrValidation, domain.MaxMilestoneLabelLen)
	}
	return in.Kind, label, nil
}

// requireCreate authorizes the create: resolve the caller's membership
// role (the resolution itself runs the project read gate — an invisible
// project answers not-found before any role is decided), then evaluate
// ActionCreateRelease for the class. The denial precedes every lookup,
// so it never discloses whether the milestone target exists.
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
		return mapStoreError(err)
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

// resolveReleaseLink verifies the optional link: a named release must
// exist in the project — an unknown id, or a release of another project,
// answers ErrReleaseNotFound (same outcome, no foreign existence leak).
// A nil link is fine: the association is optional, never required.
func (c *Command) resolveReleaseLink(ctx context.Context, projectID string, releaseID *string) error {
	if releaseID == nil {
		return nil
	}
	if c.releases == nil {
		return fmt.Errorf("%w: no release gate configured", ErrStore)
	}
	if _, err := c.releases.GetRelease(ctx, projectID, *releaseID); err != nil {
		if errors.Is(err, releases.ErrReleaseNotFound) {
			return ErrReleaseNotFound
		}
		return fmt.Errorf("%w: read release link: %v", ErrStore, err)
	}
	return nil
}

// mapStoreError keeps the store's own sentinels (ErrMilestoneNotFound —
// the store reports it directly) and turns everything else into ErrStore.
func mapStoreError(err error) error {
	if err == nil || errors.Is(err, ErrMilestoneNotFound) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrStore, err)
}
