package memstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
)

// The settings half of the in-memory Projects adapter (T0109): the member
// list, the role change and the purpose/activity-status edit. These
// methods exist because the merged ProjectStore port requires them; the
// invariants mirror the production transaction (last owner protected,
// audit entry recorded with every accepted write, oldest membership
// first). The production SQL adapter is covered by tests/integration
// against real PostgreSQL; this test keeps the in-memory stand-in honest.

func seedProjectMember(s *Projects, projectID, userID string, role domain.ProjectRole, joinedAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.members[[2]string{projectID, userID}] = domain.ProjectMembership{
		ProjectID: projectID, UserID: userID, Role: role, CreatedAt: joinedAt,
	}
}

func settingsMemStore() (*Projects, string) {
	store := NewProjects()
	// A fixed creation time (the adapter would stamp time.Now() when left
	// zero) so the owner's join time stays before every seed below.
	project, _, err := store.CreateProject(context.Background(), domain.Project{
		Slug: "settings-mem", Name: "Settings Mem", Purpose: "exercise the settings surface",
		Visibility: domain.VisibilityPrivate,
		CreatedAt:  time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC),
	}, "owner")
	if err != nil {
		panic(err)
	}
	return store, project.ID
}

func TestMemSettings(t *testing.T) {
	ctx := context.Background()

	t.Run("member list is oldest first with derived identity", func(t *testing.T) {
		store, projectID := settingsMemStore()
		seedProjectMember(store, projectID, "viewer", domain.ProjectRoleViewer,
			time.Date(2026, 9, 11, 8, 0, 0, 0, time.UTC))
		seedProjectMember(store, projectID, "maint", domain.ProjectRoleMaintainer,
			time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC))

		members, err := store.ListProjectMembers(ctx, projectID)
		if err != nil {
			t.Fatalf("ListProjectMembers: %v", err)
		}
		if len(members) != 3 {
			t.Fatalf("members = %d, want 3", len(members))
		}
		if members[0].UserID != "owner" || members[1].UserID != "viewer" || members[2].UserID != "maint" {
			t.Errorf("order = %v, want owner, viewer, maint (join time)", memberIDs(members))
		}
		for _, m := range members {
			if m.Handle == "" || m.DisplayName == "" || m.JoinedAt.IsZero() {
				t.Errorf("member %s missing identity or join date: %+v", m.UserID, m)
			}
		}
	})

	t.Run("role change updates the role and records the audit entry", func(t *testing.T) {
		store, projectID := settingsMemStore()
		seedProjectMember(store, projectID, "viewer", domain.ProjectRoleViewer,
			time.Date(2026, 9, 11, 8, 0, 0, 0, time.UTC))
		audit := domain.AuditEntry{
			ActorID: "owner", Via: domain.ViaSession,
			Action: domain.ActionProjectMemberRoleChanged, TargetRef: "user:viewer",
			ProjectID: projectID, CorrelationID: "mem-role",
		}

		updated, err := store.UpdateMembershipRole(ctx, projectID, "viewer", domain.ProjectRoleMaintainer, audit)
		if err != nil {
			t.Fatalf("UpdateMembershipRole: %v", err)
		}
		if updated.Role != domain.ProjectRoleMaintainer {
			t.Errorf("role = %q, want maintainer", updated.Role)
		}
		if len(store.audits) != 1 || store.audits[0].Action != domain.ActionProjectMemberRoleChanged {
			t.Errorf("audits = %+v, want the recorded role-change entry", store.audits)
		}
	})

	t.Run("last owner is protected and refused writes record nothing", func(t *testing.T) {
		store, projectID := settingsMemStore()
		_, err := store.UpdateMembershipRole(ctx, projectID, "owner", domain.ProjectRoleMaintainer, domain.AuditEntry{
			ActorID: "owner", Action: domain.ActionProjectMemberRoleChanged,
			TargetRef: "user:owner", ProjectID: projectID, CorrelationID: "mem-last-owner",
		})
		if !errors.Is(err, projects.ErrLastOwner) {
			t.Fatalf("last-owner demotion = %v, want ErrLastOwner", err)
		}
		if len(store.audits) != 0 {
			t.Errorf("refused write recorded %d audit entries, want 0", len(store.audits))
		}
		// The role is untouched.
		m, err := store.GetMembership(ctx, projectID, "owner")
		if err != nil || m.Role != domain.ProjectRoleOwner {
			t.Errorf("owner membership = %+v (%v), want owner role kept", m, err)
		}
	})

	t.Run("settings edit applies the non-nil fields and records the audit entry", func(t *testing.T) {
		store, projectID := settingsMemStore()
		purpose := "A sharper research goal."
		status := "active"
		updated, err := store.UpdateProjectSettings(ctx, projectID, &purpose, &status, domain.AuditEntry{
			ActorID: "owner", Action: domain.ActionProjectSettingsUpdated,
			ProjectID: projectID, CorrelationID: "mem-settings",
		})
		if err != nil {
			t.Fatalf("UpdateProjectSettings: %v", err)
		}
		if updated.Purpose != purpose || updated.ActivityStatus != "active" {
			t.Errorf("updated = %+v, want the new purpose/status", updated)
		}
		if len(store.audits) != 1 || store.audits[0].Action != domain.ActionProjectSettingsUpdated {
			t.Errorf("audits = %+v, want the recorded settings entry", store.audits)
		}
		// A partial edit keeps the other field.
		status = "paused"
		updated, err = store.UpdateProjectSettings(ctx, projectID, nil, &status, domain.AuditEntry{
			ActorID: "owner", Action: domain.ActionProjectSettingsUpdated,
			ProjectID: projectID, CorrelationID: "mem-settings-2",
		})
		if err != nil {
			t.Fatalf("partial UpdateProjectSettings: %v", err)
		}
		if updated.ActivityStatus != "paused" || updated.Purpose != purpose {
			t.Errorf("updated = %+v, want only the status changed", updated)
		}
	})

	t.Run("unknown project refuses the writes", func(t *testing.T) {
		store, _ := settingsMemStore()
		if _, err := store.UpdateMembershipRole(ctx, "no-such-project", "owner", domain.ProjectRoleViewer, domain.AuditEntry{}); !errors.Is(err, projects.ErrProjectNotFound) {
			t.Errorf("role change on unknown project = %v, want ErrProjectNotFound", err)
		}
		if _, err := store.UpdateProjectSettings(ctx, "no-such-project", nil, nil, domain.AuditEntry{}); !errors.Is(err, projects.ErrProjectNotFound) {
			t.Errorf("settings edit on unknown project = %v, want ErrProjectNotFound", err)
		}
		if len(store.audits) != 0 {
			t.Errorf("refused writes recorded %d audit entries, want 0", len(store.audits))
		}
	})
}

func memberIDs(members []domain.ProjectMember) []string {
	ids := make([]string, len(members))
	for i, m := range members {
		ids[i] = m.UserID
	}
	return ids
}
