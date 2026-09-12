package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/testdb"
)

// projectStoreFixture seeds a real project store over the migrated test
// database with two users (alice, bob) and an organization owned by
// alice. The raw pool is kept for direct-SQL probes against the same
// database the store writes.
type projectStoreFixture struct {
	store *persistence.ProjectStore
	pool  *pgxpool.Pool
	alice domain.User
	bob   domain.User
	orgID string
}

func newProjectStoreFixture(t *testing.T, ctx context.Context) *projectStoreFixture {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), projectTaskID)
	users := persistence.NewCredentialStore(pool)
	alice, err := users.CreateWithPassword(ctx, "proj-store-alice@example.com", "hash", "proj-store-alice", "Alice")
	if err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	bob, err := users.CreateWithPassword(ctx, "proj-store-bob@example.com", "hash", "proj-store-bob", "Bob")
	if err != nil {
		t.Fatalf("seed bob: %v", err)
	}
	orgStore := persistence.NewOrgStore(pool)
	org, _, err := orgStore.CreateOrganization(ctx, domain.Organization{
		Slug: "proj-store-fixture", Name: "Proj Store Fixture",
	}, alice.ID, todayUTC())
	if err != nil {
		t.Fatalf("create fixture org: %v", err)
	}
	return &projectStoreFixture{
		store: persistence.NewProjectStore(pool),
		pool:  pool,
		alice: alice,
		bob:   bob,
		orgID: org.ID,
	}
}

func (fx *projectStoreFixture) newProject(t *testing.T, ctx context.Context, slug string) domain.Project {
	t.Helper()
	orgID := fx.orgID
	p, m, err := fx.store.CreateProject(ctx, domain.Project{
		OrganizationID:  &orgID,
		Slug:            slug,
		Name:            "Project " + slug,
		Purpose:         "fixture purpose",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, fx.alice.ID)
	if err != nil {
		t.Fatalf("create project %s: %v", slug, err)
	}
	if m.Role != domain.ProjectRoleOwner || m.UserID != fx.alice.ID {
		t.Fatalf("membership = %+v, want alice owner", m)
	}
	return p
}

// TestProjectStoreCreateInvariants: the creation transaction enforces the
// gate and the slug scopes at the store level — a non-member, a departed
// member and a slug conflict are all refused before anything is written.
func TestProjectStoreCreateInvariants(t *testing.T) {
	ctx := testCtx(t)
	fx := newProjectStoreFixture(t, ctx)

	// The org-gate re-check: bob is not a member of the organization.
	orgID := fx.orgID
	_, _, err := fx.store.CreateProject(ctx, domain.Project{
		OrganizationID: &orgID, Slug: "intruder", Name: "Intruder", Purpose: "x",
		Visibility: domain.VisibilityPublic, ProvisionStatus: domain.ProvisionPending,
	}, fx.bob.ID)
	if !errors.Is(err, projects.ErrForbidden) {
		t.Errorf("non-member create = %v, want ErrForbidden", err)
	}

	// Slug conflict in the organization scope.
	fx.newProject(t, ctx, "taken")
	_, _, err = fx.store.CreateProject(ctx, domain.Project{
		OrganizationID: &orgID, Slug: "taken", Name: "Taken 2", Purpose: "x",
		Visibility: domain.VisibilityPublic, ProvisionStatus: domain.ProvisionPending,
	}, fx.alice.ID)
	if !errors.Is(err, projects.ErrSlugTaken) {
		t.Errorf("duplicate org slug = %v, want ErrSlugTaken", err)
	}

	// A departed member is refused even if the membership row exists
	// (the transaction re-checks activity, not mere presence). Alice needs
	// a second active owner first — the last active owner cannot leave.
	orgStore := persistence.NewOrgStore(fx.pool)
	if _, err := orgStore.AddMembership(ctx, domain.OrganizationMembership{
		OrganizationID:   fx.orgID,
		UserID:           fx.bob.ID,
		Role:             domain.OrgRoleOwner,
		AffiliationStart: todayUTC(),
		Verified:         true,
	}); err != nil {
		t.Fatalf("add bob as owner: %v", err)
	}
	if err := orgStore.EndAffiliation(ctx, fx.orgID, fx.alice.ID, todayUTC()); err != nil {
		t.Fatalf("end alice affiliation: %v", err)
	}
	_, _, err = fx.store.CreateProject(ctx, domain.Project{
		OrganizationID: &orgID, Slug: "after-leave", Name: "After Leave", Purpose: "x",
		Visibility: domain.VisibilityPublic, ProvisionStatus: domain.ProvisionPending,
	}, fx.alice.ID)
	if !errors.Is(err, projects.ErrForbidden) {
		t.Errorf("departed member create = %v, want ErrForbidden", err)
	}
}

// TestProjectStoreSlugScopesAtDBLevel probes the constraints directly:
// the partial unique index must reject a duplicate personal slug with
// SQLSTATE 23505, while the same slug stays free inside an organization
// scope — and the organization-scoped UNIQUE constraint still fires
// there.
func TestProjectStoreSlugScopesAtDBLevel(t *testing.T) {
	ctx := testCtx(t)
	fx := newProjectStoreFixture(t, ctx)
	pool := fx.pool

	insert := func(orgID, slug *string) error {
		var createdBy string
		if err := pool.QueryRow(ctx, `SELECT id FROM users WHERE handle = 'proj-store-alice'`).Scan(&createdBy); err != nil {
			t.Fatalf("resolve alice id: %v", err)
		}
		if orgID == nil {
			_, err := pool.Exec(ctx,
				`INSERT INTO projects (slug, name, purpose, visibility, created_by) VALUES ($1, 'x', 'x', 'private', $2)`,
				*slug, createdBy)
			return err
		}
		_, err := pool.Exec(ctx,
			`INSERT INTO projects (organization_id, slug, name, purpose, visibility, created_by) VALUES ($1, $2, 'x', 'x', 'private', $3)`,
			*orgID, *slug, createdBy)
		return err
	}
	isUniqueViolation := func(err error) bool {
		var pgErr *pgconn.PgError
		return errors.As(err, &pgErr) && pgErr.Code == "23505"
	}

	// Two personal projects with the same slug: the partial unique index
	// must reject the second.
	personal := "personal-slug"
	if err := insert(nil, &personal); err != nil {
		t.Fatalf("first personal insert: %v", err)
	}
	if err := insert(nil, &personal); !isUniqueViolation(err) {
		t.Errorf("duplicate personal slug = %v, want 23505", err)
	}

	// The same slug inside an organization is a different scope: it must
	// succeed once…
	orgSlug := "org-slug"
	if err := insert(&fx.orgID, &orgSlug); err != nil {
		t.Fatalf("first org insert: %v", err)
	}
	// …and stay free in the personal scope…
	if err := insert(nil, &orgSlug); err != nil {
		t.Errorf("personal insert of an org-scoped slug = %v, want success (scopes are independent)", err)
	}
	// …while a second insert in the org scope violates the canonical
	// UNIQUE(organization_id, slug) constraint.
	if err := insert(&fx.orgID, &orgSlug); !isUniqueViolation(err) {
		t.Errorf("duplicate org slug = %v, want 23505", err)
	}

	// provision_status accepts only the three canonical states.
	if _, err := pool.Exec(ctx,
		`UPDATE projects SET provision_status = 'provisioning' WHERE slug = 'org-slug'`); err == nil {
		t.Error("provision_status = 'provisioning' accepted, want CHECK violation")
	}
}

// TestProjectStoreProvisionPending: the canonical default makes every new
// project provision-pending — the store never has to set it explicitly.
func TestProjectStoreProvisionPending(t *testing.T) {
	ctx := testCtx(t)
	fx := newProjectStoreFixture(t, ctx)
	p := fx.newProject(t, ctx, "pending-project")
	if p.ProvisionStatus != domain.ProvisionPending {
		t.Errorf("provision_status = %q, want pending", p.ProvisionStatus)
	}
	got, err := fx.store.GetProject(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.ProvisionStatus != domain.ProvisionPending || got.Visibility != domain.VisibilityPrivate {
		t.Errorf("read-back = %+v, want pending + private", got)
	}
}
