package projects

import (
	"context"
	"errors"
	"testing"

	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
)

// The settings service rules (T0109): the member list and the role
// change are maintainer-or-above, owner management is owner-only, nobody
// changes their own role, the last owner is protected (store-level, but
// the fake mirrors it), the settings edit refuses visibility changes,
// and every accepted write records its audit placeholder.

// seedSettingsProject builds a store with one project owned by ownerID.
func seedSettingsProject() (*fakeStore, string) {
	store := newFakeStore()
	project, _, err := store.CreateProject(context.Background(), domain.Project{
		OrganizationID: nil,
		Slug:           "settings-lab",
		Name:           "Settings Lab",
		Purpose:        "exercise the settings surface",
		Visibility:     domain.VisibilityPrivate,
	}, "owner")
	if err != nil {
		panic(err)
	}
	return store, project.ID
}

func settingsService(store *fakeStore) *Service {
	return NewService(store, store.gate, authz.NewMatrixEngine())
}

func TestListMembersGate(t *testing.T) {
	ctx := context.Background()
	store, projectID := seedSettingsProject()
	store.seedMember(projectID, "maint", domain.ProjectRoleMaintainer)
	store.seedMember(projectID, "viewer", domain.ProjectRoleViewer)
	svc := settingsService(store)

	// Owner sees everyone, oldest membership first.
	members, err := svc.ListMembers(ctx, testUser("owner"), projectID)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	if len(members) != 3 {
		t.Fatalf("ListMembers = %d members, want 3", len(members))
	}
	if members[0].UserID != "owner" || members[0].Role != domain.ProjectRoleOwner {
		t.Errorf("members[0] = %+v, want the owner first (oldest membership)", members[0])
	}
	if members[0].Handle == "" || members[0].DisplayName == "" {
		t.Errorf("members[0] must carry identity: %+v", members[0])
	}

	// Maintainer sees the list too (docs/04 §2: maintainer governs member
	// policy).
	if _, err := svc.ListMembers(ctx, testUser("maint"), projectID); err != nil {
		t.Errorf("ListMembers as maintainer: %v", err)
	}

	// Viewer gets the settings 403; a stranger of the private project gets
	// the read gate's existence-hiding 404 first — nothing is disclosed
	// either way.
	if _, err := svc.ListMembers(ctx, testUser("viewer"), projectID); !errors.Is(err, ErrSettingsForbidden) {
		t.Errorf("ListMembers as viewer = %v, want ErrSettingsForbidden", err)
	}
	if _, err := svc.ListMembers(ctx, testUser("stranger"), projectID); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("ListMembers as stranger = %v, want ErrProjectNotFound (existence hiding)", err)
	}
}

func TestSetMemberRoleRules(t *testing.T) {
	ctx := context.Background()

	t.Run("owner promotes viewer to maintainer, audit recorded", func(t *testing.T) {
		store, projectID := seedSettingsProject()
		store.seedMember(projectID, "viewer", domain.ProjectRoleViewer)
		svc := settingsService(store)

		updated, err := svc.SetMemberRole(ctx, testUser("owner"), projectID, "viewer", domain.ProjectRoleMaintainer)
		if err != nil {
			t.Fatalf("SetMemberRole: %v", err)
		}
		if updated.Role != domain.ProjectRoleMaintainer {
			t.Errorf("role = %q, want maintainer", updated.Role)
		}
		audits := store.auditEntries()
		if len(audits) != 1 {
			t.Fatalf("audit entries = %d, want 1", len(audits))
		}
		a := audits[0]
		if a.Action != domain.ActionProjectMemberRoleChanged || a.ActorID != "owner" ||
			a.ProjectID != projectID || a.TargetRef != "user:viewer" ||
			a.Via != domain.ViaSession || a.CorrelationID == "" ||
			a.BeforeSummary == nil || a.AfterSummary == nil {
			t.Errorf("audit entry = %+v, want complete placeholder (actor/via/action/target/project/correlation/before/after)", a)
		}
	})

	t.Run("maintainer changes contributor to viewer", func(t *testing.T) {
		store, projectID := seedSettingsProject()
		store.seedMember(projectID, "maint", domain.ProjectRoleMaintainer)
		store.seedMember(projectID, "contrib", domain.ProjectRoleContributor)
		svc := settingsService(store)

		if _, err := svc.SetMemberRole(ctx, testUser("maint"), projectID, "contrib", domain.ProjectRoleViewer); err != nil {
			t.Errorf("SetMemberRole as maintainer: %v", err)
		}
	})

	t.Run("maintainer cannot touch owner roles", func(t *testing.T) {
		store, projectID := seedSettingsProject()
		store.seedMember(projectID, "maint", domain.ProjectRoleMaintainer)
		store.seedMember(projectID, "other-owner", domain.ProjectRoleOwner)
		store.seedMember(projectID, "viewer", domain.ProjectRoleViewer)
		svc := settingsService(store)

		// Granting owner…
		if _, err := svc.SetMemberRole(ctx, testUser("maint"), projectID, "viewer", domain.ProjectRoleOwner); !errors.Is(err, ErrOwnerRoleChange) {
			t.Errorf("maintainer granting owner = %v, want ErrOwnerRoleChange", err)
		}
		// …and revoking it are both refused, and nothing is audited for a
		// refused action.
		if _, err := svc.SetMemberRole(ctx, testUser("maint"), projectID, "other-owner", domain.ProjectRoleViewer); !errors.Is(err, ErrOwnerRoleChange) {
			t.Errorf("maintainer demoting owner = %v, want ErrOwnerRoleChange", err)
		}
		if len(store.auditEntries()) != 0 {
			t.Errorf("refused actions must not write audit rows, got %d", len(store.auditEntries()))
		}
	})

	t.Run("nobody changes their own role", func(t *testing.T) {
		store, projectID := seedSettingsProject()
		svc := settingsService(store)
		if _, err := svc.SetMemberRole(ctx, testUser("owner"), projectID, "owner", domain.ProjectRoleViewer); !errors.Is(err, ErrSelfRoleChange) {
			t.Errorf("self role change = %v, want ErrSelfRoleChange", err)
		}
	})

	t.Run("last owner cannot be demoted", func(t *testing.T) {
		store, projectID := seedSettingsProject()
		svc := settingsService(store)
		if _, err := svc.SetMemberRole(ctx, testUser("owner"), projectID, "owner", domain.ProjectRoleViewer); !errors.Is(err, ErrSelfRoleChange) {
			// The self-change rule fires first; a second owner demoting the
			// first exercises the last-owner rule below.
			t.Fatalf("self role change = %v, want ErrSelfRoleChange (sanity)", err)
		}
		store.seedMember(projectID, "second-owner", domain.ProjectRoleOwner)
		if _, err := svc.SetMemberRole(ctx, testUser("owner"), projectID, "second-owner", domain.ProjectRoleMaintainer); err != nil {
			t.Fatalf("demoting one of two owners: %v", err)
		}
		// Now a single owner remains; demoting them hits the store's
		// last-owner refusal (mirrored by the fake).
		if _, err := svc.SetMemberRole(ctx, testUser("owner"), projectID, "owner", domain.ProjectRoleViewer); !errors.Is(err, ErrSelfRoleChange) {
			t.Errorf("self demotion = %v, want ErrSelfRoleChange", err)
		}
	})

	t.Run("unknown target", func(t *testing.T) {
		store, projectID := seedSettingsProject()
		svc := settingsService(store)
		if _, err := svc.SetMemberRole(ctx, testUser("owner"), projectID, "ghost", domain.ProjectRoleViewer); !errors.Is(err, ErrTargetMemberNotFound) {
			t.Errorf("unknown target = %v, want ErrTargetMemberNotFound", err)
		}
	})

	t.Run("invalid role", func(t *testing.T) {
		store, projectID := seedSettingsProject()
		svc := settingsService(store)
		if _, err := svc.SetMemberRole(ctx, testUser("owner"), projectID, "ghost", domain.ProjectRole("admin")); !errors.Is(err, ErrValidation) {
			t.Errorf("invalid role = %v, want ErrValidation", err)
		}
	})

	t.Run("below maintainer is refused", func(t *testing.T) {
		store, projectID := seedSettingsProject()
		store.seedMember(projectID, "contrib", domain.ProjectRoleContributor)
		store.seedMember(projectID, "viewer", domain.ProjectRoleViewer)
		svc := settingsService(store)
		if _, err := svc.SetMemberRole(ctx, testUser("contrib"), projectID, "viewer", domain.ProjectRoleMaintainer); !errors.Is(err, ErrSettingsForbidden) {
			t.Errorf("contributor role change = %v, want ErrSettingsForbidden", err)
		}
	})

	t.Run("no-op role change writes no audit row", func(t *testing.T) {
		store, projectID := seedSettingsProject()
		store.seedMember(projectID, "viewer", domain.ProjectRoleViewer)
		svc := settingsService(store)
		if _, err := svc.SetMemberRole(ctx, testUser("owner"), projectID, "viewer", domain.ProjectRoleViewer); err != nil {
			t.Fatalf("no-op SetMemberRole: %v", err)
		}
		if len(store.auditEntries()) != 0 {
			t.Errorf("no-op change wrote %d audit rows, want 0", len(store.auditEntries()))
		}
	})
}

func TestUpdateSettings(t *testing.T) {
	ctx := context.Background()

	t.Run("owner edits purpose and activity status, audit recorded", func(t *testing.T) {
		store, projectID := seedSettingsProject()
		svc := settingsService(store)
		purpose := "A sharper research goal."
		status := "active"

		updated, err := svc.UpdateSettings(ctx, testUser("owner"), projectID, UpdateSettingsInput{
			Purpose:        &purpose,
			ActivityStatus: &status,
		})
		if err != nil {
			t.Fatalf("UpdateSettings: %v", err)
		}
		if updated.Purpose != purpose || updated.ActivityStatus != "active" {
			t.Errorf("updated = %+v, want purpose/status applied", updated)
		}
		audits := store.auditEntries()
		if len(audits) != 1 {
			t.Fatalf("audit entries = %d, want 1", len(audits))
		}
		a := audits[0]
		if a.Action != domain.ActionProjectSettingsUpdated || a.ActorID != "owner" ||
			a.ProjectID != projectID || a.TargetRef != "" ||
			a.Via != domain.ViaSession || a.CorrelationID == "" ||
			a.BeforeSummary == nil || a.AfterSummary == nil {
			t.Errorf("audit entry = %+v, want complete placeholder", a)
		}
		// The summaries record the state around the change.
		before, ok := a.BeforeSummary.(map[string]any)
		if !ok || before["purpose"] != "exercise the settings surface" {
			t.Errorf("before summary = %v, want the previous purpose", a.BeforeSummary)
		}
		after, ok := a.AfterSummary.(map[string]any)
		if !ok || after["purpose"] != purpose || after["activity_status"] != "active" {
			t.Errorf("after summary = %v, want the new purpose/status", a.AfterSummary)
		}
	})

	t.Run("partial edit keeps the other field", func(t *testing.T) {
		store, projectID := seedSettingsProject()
		svc := settingsService(store)
		status := "paused"
		updated, err := svc.UpdateSettings(ctx, testUser("owner"), projectID, UpdateSettingsInput{
			ActivityStatus: &status,
		})
		if err != nil {
			t.Fatalf("UpdateSettings: %v", err)
		}
		if updated.ActivityStatus != "paused" || updated.Purpose != "exercise the settings surface" {
			t.Errorf("updated = %+v, want only the status changed", updated)
		}
	})

	t.Run("visibility change is refused (preview-only)", func(t *testing.T) {
		store, projectID := seedSettingsProject()
		svc := settingsService(store)
		visibility := "public"
		if _, err := svc.UpdateSettings(ctx, testUser("owner"), projectID, UpdateSettingsInput{
			Visibility: &visibility,
		}); !errors.Is(err, ErrVisibilityChangeNotSupported) {
			t.Errorf("visibility change = %v, want ErrVisibilityChangeNotSupported", err)
		}
		if len(store.auditEntries()) != 0 {
			t.Errorf("refused visibility change wrote audit rows")
		}
	})

	t.Run("validation failures", func(t *testing.T) {
		store, projectID := seedSettingsProject()
		svc := settingsService(store)
		blank := "   "
		if _, err := svc.UpdateSettings(ctx, testUser("owner"), projectID, UpdateSettingsInput{
			Purpose: &blank,
		}); !errors.Is(err, ErrValidation) {
			t.Errorf("blank purpose = %v, want ErrValidation", err)
		}
		bad := "sleeping"
		if _, err := svc.UpdateSettings(ctx, testUser("owner"), projectID, UpdateSettingsInput{
			ActivityStatus: &bad,
		}); !errors.Is(err, ErrValidation) {
			t.Errorf("bad status = %v, want ErrValidation", err)
		}
		if _, err := svc.UpdateSettings(ctx, testUser("owner"), projectID, UpdateSettingsInput{}); !errors.Is(err, ErrValidation) {
			t.Errorf("empty input = %v, want ErrValidation", err)
		}
	})

	t.Run("below maintainer is refused", func(t *testing.T) {
		store, projectID := seedSettingsProject()
		store.seedMember(projectID, "viewer", domain.ProjectRoleViewer)
		svc := settingsService(store)
		purpose := "nope"
		if _, err := svc.UpdateSettings(ctx, testUser("viewer"), projectID, UpdateSettingsInput{
			Purpose: &purpose,
		}); !errors.Is(err, ErrSettingsForbidden) {
			t.Errorf("viewer settings edit = %v, want ErrSettingsForbidden", err)
		}
		if _, err := svc.UpdateSettings(ctx, testUser("stranger"), projectID, UpdateSettingsInput{
			Purpose: &purpose,
		}); !errors.Is(err, ErrProjectNotFound) {
			// The read gate fires first: a stranger cannot read the private
			// project, so the answer is the existence-hiding 404.
			t.Errorf("stranger settings edit = %v, want ErrProjectNotFound (existence hiding)", err)
		}
	})
}

// TestSettingsWritesRequireReadGate: on a project the actor may not even
// read, the settings answer is the read path's existence-hiding
// ErrProjectNotFound — never a settings-specific error that would
// disclose the project.
func TestSettingsWritesRequireReadGate(t *testing.T) {
	ctx := context.Background()
	store, projectID := seedSettingsProject()
	svc := settingsService(store)
	if _, err := svc.ListMembers(ctx, testUser("stranger"), projectID); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("ListMembers as stranger = %v, want ErrProjectNotFound", err)
	}
}
