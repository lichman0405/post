package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/application/orgs"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/testdb"
)

// orgStoreFixture seeds a real org store over the migrated test database
// with two users (alice, bob) and an organization owned by alice.
type orgStoreFixture struct {
	store *persistence.OrgStore
	alice domain.User
	bob   domain.User
	org   domain.Organization
}

func newOrgStoreFixture(t *testing.T, ctx context.Context) *orgStoreFixture {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), orgTaskID)
	users := persistence.NewCredentialStore(pool)
	alice, err := users.CreateWithPassword(ctx, "org-store-alice@example.com", "hash", "org-store-alice", "Alice")
	if err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	bob, err := users.CreateWithPassword(ctx, "org-store-bob@example.com", "hash", "org-store-bob", "Bob")
	if err != nil {
		t.Fatalf("seed bob: %v", err)
	}
	store := persistence.NewOrgStore(pool)
	org, _, err := store.CreateOrganization(ctx, domain.Organization{
		Slug:        "store-fixture",
		Name:        "Store Fixture",
		Description: "",
	}, alice.ID, todayUTC())
	if err != nil {
		t.Fatalf("create fixture org: %v", err)
	}
	return &orgStoreFixture{store: store, alice: alice, bob: bob, org: org}
}

// TestOrgStoreUpdateMembershipPreservesAffiliationEnd — B1 fix: adjusting
// role/start/verified of a membership must NEVER write affiliation_end.
// An owner adjusting an ex-member's record (or any other update path)
// must leave the historical departure date untouched: affiliation_end is
// EndAffiliation's exclusive job.
func TestOrgStoreUpdateMembershipPreservesAffiliationEnd(t *testing.T) {
	ctx := testCtx(t)
	fx := newOrgStoreFixture(t, ctx)
	store := fx.store

	if _, err := store.AddMembership(ctx, domain.OrganizationMembership{
		OrganizationID:   fx.org.ID,
		UserID:           fx.bob.ID,
		Role:             domain.OrgRoleContributor,
		AffiliationStart: todayUTC(),
	}); err != nil {
		t.Fatalf("add bob: %v", err)
	}
	end := todayUTC().Add(24 * time.Hour)
	if err := store.EndAffiliation(ctx, fx.org.ID, fx.bob.ID, end); err != nil {
		t.Fatalf("end bob: %v", err)
	}

	// A later adjustment (different role, different start, different
	// verified) must not touch affiliation_end.
	updated, err := store.UpdateMembershipRoleAndDates(ctx, fx.org.ID, fx.bob.ID,
		domain.OrgRoleMaintainer, todayUTC().Add(2*24*time.Hour), true)
	if err != nil {
		t.Fatalf("adjust ended membership: %v", err)
	}
	if updated.AffiliationEnd == nil {
		t.Fatal("adjustment cleared affiliation_end — the departure date must survive")
	}
	if !updated.AffiliationEnd.Equal(end) {
		t.Errorf("affiliation_end = %v, want the original departure date %v", *updated.AffiliationEnd, end)
	}
	// And the stored row agrees (the UPDATE ran, not just the returned copy).
	m, err := store.GetMembership(ctx, fx.org.ID, fx.bob.ID)
	if err != nil {
		t.Fatalf("reload membership: %v", err)
	}
	if m.AffiliationEnd == nil || !m.AffiliationEnd.Equal(end) {
		t.Errorf("stored affiliation_end = %v, want %v", m.AffiliationEnd, end)
	}
	if m.Role != domain.OrgRoleMaintainer {
		t.Errorf("role = %q, want the adjusted role %q", m.Role, domain.OrgRoleMaintainer)
	}
}

// TestOrgStoreEndAffiliationIdempotent — minor fix: ending an already
// ended affiliation is a no-op; the historical end date is never
// re-stamped.
func TestOrgStoreEndAffiliationIdempotent(t *testing.T) {
	ctx := testCtx(t)
	fx := newOrgStoreFixture(t, ctx)
	store := fx.store

	if _, err := store.AddMembership(ctx, domain.OrganizationMembership{
		OrganizationID:   fx.org.ID,
		UserID:           fx.bob.ID,
		Role:             domain.OrgRoleViewer,
		AffiliationStart: todayUTC(),
	}); err != nil {
		t.Fatalf("add bob: %v", err)
	}
	first := todayUTC().Add(24 * time.Hour)
	if err := store.EndAffiliation(ctx, fx.org.ID, fx.bob.ID, first); err != nil {
		t.Fatalf("end bob: %v", err)
	}
	// A repeated leave (e.g. an owner removing an ex-member again) must
	// not overwrite the original departure date.
	if err := store.EndAffiliation(ctx, fx.org.ID, fx.bob.ID, todayUTC().Add(30*24*time.Hour)); err != nil {
		t.Fatalf("re-end bob: %v", err)
	}
	m, err := store.GetMembership(ctx, fx.org.ID, fx.bob.ID)
	if err != nil {
		t.Fatalf("reload membership: %v", err)
	}
	if m.AffiliationEnd == nil || !m.AffiliationEnd.Equal(first) {
		t.Errorf("affiliation_end = %v, want the original %v (never re-stamped)", m.AffiliationEnd, first)
	}
}

// TestOrgStoreAddMembershipRejectsDeactivatedOrg — minor fix: a
// membership insert re-checks deactivated_at inside the transaction while
// holding the organization row lock, so an invite racing a concurrent
// deactivation cannot land in an already-deactivated organization.
func TestOrgStoreAddMembershipRejectsDeactivatedOrg(t *testing.T) {
	ctx := testCtx(t)
	fx := newOrgStoreFixture(t, ctx)
	store := fx.store

	if err := store.DeactivateOrganization(ctx, fx.org.ID); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	_, err := store.AddMembership(ctx, domain.OrganizationMembership{
		OrganizationID:   fx.org.ID,
		UserID:           fx.bob.ID,
		Role:             domain.OrgRoleViewer,
		AffiliationStart: todayUTC(),
	})
	if !errors.Is(err, orgs.ErrOrgDeactivated) {
		t.Fatalf("AddMembership on deactivated org error = %v, want ErrOrgDeactivated", err)
	}
	// Nothing was inserted.
	if _, err := store.GetMembership(ctx, fx.org.ID, fx.bob.ID); !errors.Is(err, orgs.ErrMemberNotFound) {
		t.Errorf("membership after refused insert = %v, want ErrMemberNotFound", err)
	}
}
