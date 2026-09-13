package projects

import (
	"context"
	"errors"
	"fmt"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/observability"
)

// The settings surface (T0109): the member list, member role changes and
// the project purpose/activity-status edit. Visibility is preview-only —
// UpdateSettings refuses any visibility change until the publishing guard
// lands (docs/12 §3: private-to-public is never automatic, always
// audited).
//
// Authorization. Every settings action runs two server-side checks:
//
//  1. the read path first (Get): a project the caller may not read
//     answers the existence-hiding ErrProjectNotFound, and the policy
//     engine's read action stays in the loop for what the matrix covers;
//  2. a maintainer-or-above gate on the actor's resolved membership role
//     (docs/04 §2: "Maintainer：治理 branch/PR/release/publish/member
//     policy"; T0108 set the same L1 gate on the Settings tab). A denied
//     gate answers ErrSettingsForbidden — the same answer for "not a
//     member" and "role too low", so the check discloses nothing.
//
// The permission matrix has no settings/member-management row (the CSV is
// the policy source of truth, and specs/ is outside this task's scope);
// the role gate above is the default-deny server-side rule until a
// policy-owning task adds the row (follow-up recorded for the
// Supervisor).
//
// Audit placeholder. Every owner/maintainer action here writes an
// audit_log row in the same transaction as the state change, carrying
// actor id, via, action, target, project and the request's correlation id
// (T0109 acceptance: owner/maintainer 动作有 audit placeholder). The
// entry type is T0110's shared domain.AuditEntry — settings actions and
// the audit surface (append-only Activity queries) are two halves of the
// same audit_log table, so there is exactly one type and one action-name
// registry (internal/domain/audit.go); T0110 owns the reading surface,
// these rows feed it.

// UpdateSettingsInput carries the settings edit. Nil fields stay
// unchanged. Visibility must stay nil: the setting is preview-only.
type UpdateSettingsInput struct {
	Purpose        *string
	ActivityStatus *string
	// Visibility must be nil. A non-nil value answers
	// ErrVisibilityChangeNotSupported — the client is told plainly that
	// the publishing guard is a later milestone, instead of having the
	// field silently dropped.
	Visibility *string
}

// ListMembers returns the project's member list (identity + role + join
// time), oldest membership first. Owner/maintainer only.
func (s *Service) ListMembers(ctx context.Context, actor domain.User, projectID string) ([]domain.ProjectMember, error) {
	if _, err := s.Get(ctx, actor, projectID); err != nil {
		return nil, err
	}
	if _, err := s.requireManager(ctx, projectID, actor.ID); err != nil {
		return nil, err
	}
	members, err := s.store.ListProjectMembers(ctx, projectID)
	if err != nil {
		return nil, wrapStoreError(err)
	}
	return members, nil
}

// SetMemberRole changes one membership's role. The rules:
//
//   - owner/maintainer only (the requireManager gate);
//   - a maintainer may not grant or revoke the owner role (owner
//     management is owner-only — L1, mirrors the org surface: owner is
//     the project's highest governance role, so letting a maintainer
//     promote anyone to owner would be privilege escalation);
//   - an actor may not change their own role (a self-demoting owner
//     could orphan the project);
//   - the project must keep at least one owner (enforced inside the
//     store transaction under the project-row lock).
//
// A no-op (same role) succeeds without writing an audit row: nothing
// changed, nothing is recorded.
func (s *Service) SetMemberRole(ctx context.Context, actor domain.User, projectID, targetUserID string, role domain.ProjectRole) (domain.ProjectMembership, error) {
	if !domain.ValidProjectRole(role) {
		return domain.ProjectMembership{}, fmt.Errorf("%w: role must be owner, maintainer, contributor or viewer", ErrValidation)
	}
	if _, err := s.Get(ctx, actor, projectID); err != nil {
		return domain.ProjectMembership{}, err
	}
	actorRole, err := s.requireManager(ctx, projectID, actor.ID)
	if err != nil {
		return domain.ProjectMembership{}, err
	}
	if targetUserID == actor.ID {
		return domain.ProjectMembership{}, ErrSelfRoleChange
	}
	target, err := s.store.GetMembership(ctx, projectID, targetUserID)
	if err != nil {
		if errors.Is(err, ErrMemberNotFound) {
			return domain.ProjectMembership{}, ErrTargetMemberNotFound
		}
		return domain.ProjectMembership{}, wrapStoreError(err)
	}
	if actorRole != domain.ProjectRoleOwner {
		if target.Role == domain.ProjectRoleOwner || role == domain.ProjectRoleOwner {
			return domain.ProjectMembership{}, ErrOwnerRoleChange
		}
	}
	if target.Role == role {
		return target, nil
	}
	audit, err := s.settingsAudit(ctx, actor, projectID,
		domain.ActionProjectMemberRoleChanged, "user:"+targetUserID,
		map[string]any{"user_id": targetUserID, "role": string(target.Role)},
		map[string]any{"user_id": targetUserID, "role": string(role)})
	if err != nil {
		return domain.ProjectMembership{}, err
	}
	updated, err := s.store.UpdateMembershipRole(ctx, projectID, targetUserID, role, audit)
	if err != nil {
		return domain.ProjectMembership{}, wrapStoreError(err)
	}
	return updated, nil
}

// UpdateSettings edits the project purpose and/or activity status.
// Visibility changes are refused (preview-only). Owner/maintainer only.
func (s *Service) UpdateSettings(ctx context.Context, actor domain.User, projectID string, in UpdateSettingsInput) (domain.Project, error) {
	if _, err := s.Get(ctx, actor, projectID); err != nil {
		return domain.Project{}, err
	}
	if _, err := s.requireManager(ctx, projectID, actor.ID); err != nil {
		return domain.Project{}, err
	}
	if in.Visibility != nil {
		return domain.Project{}, ErrVisibilityChangeNotSupported
	}
	if in.Purpose == nil && in.ActivityStatus == nil {
		return domain.Project{}, fmt.Errorf("%w: nothing to update", ErrValidation)
	}
	if in.Purpose != nil && !domain.ValidProjectPurpose(*in.Purpose) {
		return domain.Project{}, fmt.Errorf("%w: purpose is required (max 4000 characters)", ErrValidation)
	}
	if in.ActivityStatus != nil && !domain.ValidActivityStatus(*in.ActivityStatus) {
		return domain.Project{}, fmt.Errorf("%w: activity status must be planning, active, paused or archived", ErrValidation)
	}
	current, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return domain.Project{}, wrapStoreError(err)
	}
	audit, err := s.settingsAudit(ctx, actor, projectID,
		domain.ActionProjectSettingsUpdated, "",
		map[string]any{"purpose": current.Purpose, "activity_status": current.ActivityStatus},
		map[string]any{
			"purpose":         firstOr(in.Purpose, current.Purpose),
			"activity_status": firstOr(in.ActivityStatus, current.ActivityStatus),
		})
	if err != nil {
		return domain.Project{}, err
	}
	updated, err := s.store.UpdateProjectSettings(ctx, projectID, in.Purpose, in.ActivityStatus, audit)
	if err != nil {
		return domain.Project{}, wrapStoreError(err)
	}
	return updated, nil
}

// requireManager resolves the actor's membership and enforces the
// maintainer-or-above gate. "Not a member" and "role too low" answer the
// same ErrSettingsForbidden (nothing is disclosed either way); a store
// failure answers ErrStore — fail closed, never guess.
func (s *Service) requireManager(ctx context.Context, projectID, userID string) (domain.ProjectRole, error) {
	membership, err := s.store.GetMembership(ctx, projectID, userID)
	if err != nil {
		if errors.Is(err, ErrMemberNotFound) {
			return "", ErrSettingsForbidden
		}
		return "", wrapStoreError(err)
	}
	if !membership.Role.AtLeast(domain.ProjectRoleMaintainer) {
		return "", ErrSettingsForbidden
	}
	return membership.Role, nil
}

// settingsAudit builds the audit placeholder entry (T0110's shared
// domain.AuditEntry — see the file header) for a settings action. Via is
// the audit vocabulary's ViaSession: every settings write arrives as a
// session-authenticated /api/v1 request. The correlation id is the
// request's own (the observability middleware attaches it); a context
// without one — a direct service call — gets a fresh id so the NOT NULL
// column always holds something traceable. A failure to mint the id
// refuses the action (fail closed: an audit-less governance write must
// not happen).
func (s *Service) settingsAudit(ctx context.Context, actor domain.User, projectID, action, targetRef string, before, after any) (domain.AuditEntry, error) {
	correlationID, ok := observability.FromContext(ctx)
	if !ok {
		id, err := observability.NewCorrelationID()
		if err != nil {
			return domain.AuditEntry{}, fmt.Errorf("%w: cannot mint audit correlation id: %v", ErrStore, err)
		}
		correlationID = id
	}
	return domain.AuditEntry{
		ActorID:       actor.ID,
		Via:           domain.ViaSession,
		Action:        action,
		TargetRef:     targetRef,
		ProjectID:     projectID,
		CorrelationID: correlationID.String(),
		BeforeSummary: before,
		AfterSummary:  after,
	}, nil
}

// firstOr resolves a settings field: the requested value, or the current
// one when the request left it unchanged.
func firstOr[T any](requested *T, current T) T {
	if requested == nil {
		return current
	}
	return *requested
}
