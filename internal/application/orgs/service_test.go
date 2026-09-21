package orgs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/domain"
)

// fakeStore is an in-memory OrgStore for the service policy tests. It
// reproduces the adapter's invariants (last-owner guard, row retention on
// affiliation end, PK uniqueness) so the service rules can be tested
// without PostgreSQL. The real adapter is exercised by the integration
// suite against the real database.
type fakeStore struct {
	orgs     map[string]domain.Organization
	members  map[[2]string]domain.OrganizationMembership
	users    map[string]domain.User
	nextID   int
	failWith error // when set, every method returns it (wrapped)
	// attributions is organizations.attestation_attribution, which the
	// real adapter keeps on the organization row but domain.Organization
	// has no field for (T0812 does not widen the domain type), so the fake
	// keeps it beside the row exactly as the store does.
	attributions map[string]string
	// failMembershipWith fails only GetMembership — it reproduces a store
	// outage on the membership read while the organization read works,
	// the exact path where availability errors were masked as 403/404.
	failMembershipWith error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		orgs:         map[string]domain.Organization{},
		members:      map[[2]string]domain.OrganizationMembership{},
		users:        map[string]domain.User{},
		attributions: map[string]string{},
		nextID:       1,
	}
}

func (f *fakeStore) id() string {
	f.nextID++
	return time.Now().Add(time.Duration(f.nextID) * time.Minute).Format("00000000")
}

func (f *fakeStore) seedUser(id, handle string) {
	f.users[handle] = domain.User{ID: id, Handle: handle, DisplayName: handle}
}

func (f *fakeStore) CreateOrganization(_ context.Context, org domain.Organization, creatorUserID string, start time.Time) (domain.Organization, domain.OrganizationMembership, error) {
	if f.failWith != nil {
		return domain.Organization{}, domain.OrganizationMembership{}, f.failWith
	}
	for _, o := range f.orgs {
		if o.Slug == org.Slug {
			return domain.Organization{}, domain.OrganizationMembership{}, ErrSlugTaken
		}
	}
	org.ID = f.id()
	org.CreatedAt = time.Now()
	f.orgs[org.ID] = org
	m := domain.OrganizationMembership{
		OrganizationID:   org.ID,
		UserID:           creatorUserID,
		Role:             domain.OrgRoleOwner,
		AffiliationStart: start,
		Verified:         true,
	}
	f.members[[2]string{org.ID, creatorUserID}] = m
	return org, m, nil
}

func (f *fakeStore) GetOrganization(_ context.Context, orgID string) (domain.Organization, error) {
	if f.failWith != nil {
		return domain.Organization{}, f.failWith
	}
	o, ok := f.orgs[orgID]
	if !ok {
		return domain.Organization{}, ErrOrgNotFound
	}
	return o, nil
}

func (f *fakeStore) UpdateOrganization(_ context.Context, org domain.Organization, attribution *string) (domain.Organization, error) {
	if f.failWith != nil {
		return domain.Organization{}, f.failWith
	}
	existing, ok := f.orgs[org.ID]
	if !ok {
		return domain.Organization{}, ErrOrgNotFound
	}
	existing.Name = org.Name
	existing.Description = org.Description
	f.orgs[org.ID] = existing
	if attribution != nil {
		f.attributions[org.ID] = *attribution
	}
	return existing, nil
}

func (f *fakeStore) GetAttestationAttribution(_ context.Context, orgID string) (string, error) {
	if f.failWith != nil {
		return "", f.failWith
	}
	if _, ok := f.orgs[orgID]; !ok {
		return "", ErrOrgNotFound
	}
	// The column's DEFAULT, reproduced: an organization that has never set
	// it is anonymous.
	if v, ok := f.attributions[orgID]; ok {
		return v, nil
	}
	return string(AttestationAttributionAnonymous), nil
}

func (f *fakeStore) DeactivateOrganization(_ context.Context, orgID string) error {
	if f.failWith != nil {
		return f.failWith
	}
	o, ok := f.orgs[orgID]
	if !ok {
		return ErrOrgNotFound
	}
	now := time.Now()
	o.DeactivatedAt = &now
	f.orgs[orgID] = o
	return nil
}

func (f *fakeStore) ListOrganizationsForUser(_ context.Context, userID string) ([]domain.Organization, error) {
	if f.failWith != nil {
		return nil, f.failWith
	}
	var out []domain.Organization
	for key, m := range f.members {
		if m.UserID == userID && m.AffiliationEnd == nil {
			out = append(out, f.orgs[key[0]])
		}
	}
	return out, nil
}

func (f *fakeStore) GetMembership(_ context.Context, orgID, userID string) (domain.OrganizationMembership, error) {
	if f.failWith != nil {
		return domain.OrganizationMembership{}, f.failWith
	}
	if f.failMembershipWith != nil {
		return domain.OrganizationMembership{}, f.failMembershipWith
	}
	m, ok := f.members[[2]string{orgID, userID}]
	if !ok {
		return domain.OrganizationMembership{}, ErrMemberNotFound
	}
	return m, nil
}

func (f *fakeStore) GetUserByHandle(_ context.Context, handle string) (domain.User, error) {
	if f.failWith != nil {
		return domain.User{}, f.failWith
	}
	u, ok := f.users[handle]
	if !ok {
		return domain.User{}, ErrUserNotFound
	}
	return u, nil
}

func (f *fakeStore) ListMembers(_ context.Context, orgID string) ([]domain.OrganizationMembership, error) {
	if f.failWith != nil {
		return nil, f.failWith
	}
	var out []domain.OrganizationMembership
	for key, m := range f.members {
		if key[0] == orgID {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *fakeStore) AddMembership(_ context.Context, m domain.OrganizationMembership) (domain.OrganizationMembership, error) {
	if f.failWith != nil {
		return domain.OrganizationMembership{}, f.failWith
	}
	if _, ok := f.orgs[m.OrganizationID]; !ok {
		return domain.OrganizationMembership{}, ErrOrgNotFound
	}
	found := false
	for _, u := range f.users {
		if u.ID == m.UserID {
			found = true
		}
	}
	if !found {
		return domain.OrganizationMembership{}, ErrUserNotFound
	}
	key := [2]string{m.OrganizationID, m.UserID}
	if _, exists := f.members[key]; exists {
		return domain.OrganizationMembership{}, ErrAlreadyMember
	}
	f.members[key] = m
	return m, nil
}

func (f *fakeStore) UpdateMembershipRoleAndDates(_ context.Context, orgID, userID string, role domain.OrgRole, start time.Time, verified bool) (domain.OrganizationMembership, error) {
	if f.failWith != nil {
		return domain.OrganizationMembership{}, f.failWith
	}
	key := [2]string{orgID, userID}
	current, ok := f.members[key]
	if !ok {
		return domain.OrganizationMembership{}, ErrMemberNotFound
	}
	if current.Role == domain.OrgRoleOwner && current.AffiliationEnd == nil && role != domain.OrgRoleOwner && f.activeOwners(orgID) <= 1 {
		return domain.OrganizationMembership{}, ErrLastOwner
	}
	current.Role = role
	current.AffiliationStart = start
	current.Verified = verified
	f.members[key] = current
	return current, nil
}

func (f *fakeStore) EndAffiliation(_ context.Context, orgID, userID string, end time.Time) error {
	if f.failWith != nil {
		return f.failWith
	}
	key := [2]string{orgID, userID}
	current, ok := f.members[key]
	if !ok {
		return ErrMemberNotFound
	}
	if current.Role == domain.OrgRoleOwner && current.AffiliationEnd == nil && f.activeOwners(orgID) <= 1 {
		return ErrLastOwner
	}
	current.AffiliationEnd = &end
	f.members[key] = current // row kept — 离职不删除历史
	return nil
}

func (f *fakeStore) activeOwners(orgID string) int {
	n := 0
	for key, m := range f.members {
		if key[0] == orgID && m.Role == domain.OrgRoleOwner && m.AffiliationEnd == nil {
			n++
		}
	}
	return n
}

func testUser(id, handle string) domain.User {
	return domain.User{ID: id, Handle: handle, DisplayName: handle}
}

// TestServiceCreateMakesOwner: creating an organization makes the creator
// its verified owner with today's affiliation start.
func TestServiceCreateMakesOwner(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)
	alice := testUser("alice-id", "alice")

	org, m, err := svc.Create(context.Background(), alice, "ACME", "Acme Research", "  labs  ")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if org.Slug != "acme" {
		t.Errorf("slug = %q, want normalized %q", org.Slug, "acme")
	}
	if m.Role != domain.OrgRoleOwner || !m.Verified || m.AffiliationEnd != nil {
		t.Errorf("creator membership = %+v, want verified owner with open affiliation", m)
	}
	if m.UserID != alice.ID {
		t.Errorf("creator membership user = %q, want %q", m.UserID, alice.ID)
	}
	if !m.AffiliationStart.Equal(today()) {
		t.Errorf("affiliation start = %v, want today %v", m.AffiliationStart, today())
	}
}

// TestServiceCreateRejectsBadInput: slug/name validation and slug
// conflicts.
func TestServiceCreateRejectsBadInput(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)
	alice := testUser("alice-id", "alice")

	if _, _, err := svc.Create(context.Background(), alice, "Bad_Slug", "x", ""); !errors.Is(err, ErrValidation) {
		t.Errorf("bad slug error = %v, want ErrValidation", err)
	}
	if _, _, err := svc.Create(context.Background(), alice, "ok-slug", "", ""); !errors.Is(err, ErrValidation) {
		t.Errorf("blank name error = %v, want ErrValidation", err)
	}
	if _, _, err := svc.Create(context.Background(), alice, "ok-slug", "First", ""); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, _, err := svc.Create(context.Background(), alice, "ok-slug", "Second", ""); !errors.Is(err, ErrSlugTaken) {
		t.Errorf("duplicate slug error = %v, want ErrSlugTaken", err)
	}
}

// TestServiceOwnerInvitesAndAdjusts: the owner can invite a member and
// adjust their role/verified/start; the acceptance "Owner 可邀请/调整
// role" at service level.
func TestServiceOwnerInvitesAndAdjusts(t *testing.T) {
	store := newFakeStore()
	store.seedUser("bob-id", "bob")
	store.seedUser("carol-id", "carol")
	svc := NewService(store)
	alice := testUser("alice-id", "alice")

	org, _, err := svc.Create(context.Background(), alice, "acme", "Acme", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	invited, err := svc.Invite(context.Background(), alice, org.ID, "bob", domain.OrgRoleContributor, nil, false)
	if err != nil {
		t.Fatalf("Invite: %v", err)
	}
	if invited.Role != domain.OrgRoleContributor || invited.Verified {
		t.Errorf("invite = %+v, want unverified contributor", invited)
	}
	if !invited.AffiliationStart.Equal(today()) {
		t.Errorf("invite start = %v, want today", invited.AffiliationStart)
	}

	verified := true
	role := domain.OrgRoleMaintainer
	adjusted, err := svc.UpdateMembership(context.Background(), alice, org.ID, "bob-id", &role, nil, &verified)
	if err != nil {
		t.Fatalf("UpdateMembership: %v", err)
	}
	if adjusted.Role != domain.OrgRoleMaintainer || !adjusted.Verified {
		t.Errorf("adjusted = %+v, want verified maintainer", adjusted)
	}

	// The owner may also appoint a second owner.
	coOwner := domain.OrgRoleOwner
	if _, err := svc.UpdateMembership(context.Background(), alice, org.ID, "bob-id", &coOwner, nil, nil); err != nil {
		t.Fatalf("promote to owner: %v", err)
	}
}

// TestServiceNonOwnerCannotPromoteSelf: a non-owner changing any
// membership — their own included — is refused (acceptance: 普通成员不能
// 提升自己). Even a maintainer cannot promote themselves to owner.
func TestServiceNonOwnerCannotPromoteSelf(t *testing.T) {
	store := newFakeStore()
	store.seedUser("bob-id", "bob")
	svc := NewService(store)
	alice := testUser("alice-id", "alice")
	bob := testUser("bob-id", "bob")

	org, _, err := svc.Create(context.Background(), alice, "acme", "Acme", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.Invite(context.Background(), alice, org.ID, "bob", domain.OrgRoleContributor, nil, false); err != nil {
		t.Fatalf("Invite: %v", err)
	}

	// Self-promotion.
	owner := domain.OrgRoleOwner
	if _, err := svc.UpdateMembership(context.Background(), bob, org.ID, "bob-id", &owner, nil, nil); !errors.Is(err, ErrForbidden) {
		t.Errorf("self-promotion error = %v, want ErrForbidden", err)
	}
	// Changing someone else's membership.
	viewer := domain.OrgRoleViewer
	if _, err := svc.UpdateMembership(context.Background(), bob, org.ID, "alice-id", &viewer, nil, nil); !errors.Is(err, ErrForbidden) {
		t.Errorf("non-owner adjusting owner error = %v, want ErrForbidden", err)
	}
	// Inviting someone.
	if _, err := svc.Invite(context.Background(), bob, org.ID, "carol", domain.OrgRoleViewer, nil, false); !errors.Is(err, ErrForbidden) {
		t.Errorf("non-owner invite error = %v, want ErrForbidden", err)
	}
	// Removing someone else.
	if err := svc.RemoveMember(context.Background(), bob, org.ID, "alice-id"); !errors.Is(err, ErrForbidden) {
		t.Errorf("non-owner remove error = %v, want ErrForbidden", err)
	}
	// Deactivating the organization.
	if err := svc.Deactivate(context.Background(), bob, org.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("non-owner deactivate error = %v, want ErrForbidden", err)
	}
	// Updating the organization profile.
	newName := "New"
	if _, err := svc.Update(context.Background(), bob, org.ID, &newName, nil, nil); !errors.Is(err, ErrForbidden) {
		t.Errorf("non-owner update error = %v, want ErrForbidden", err)
	}
}

// TestServiceLeaveKeepsHistory: a member leaving ends their affiliation
// but the row stays — GetMembership still returns it with the end date,
// and the member loses access afterwards (acceptance: 离职不删除历史).
func TestServiceLeaveKeepsHistory(t *testing.T) {
	store := newFakeStore()
	store.seedUser("bob-id", "bob")
	svc := NewService(store)
	alice := testUser("alice-id", "alice")
	bob := testUser("bob-id", "bob")

	org, _, err := svc.Create(context.Background(), alice, "acme", "Acme", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.Invite(context.Background(), alice, org.ID, "bob", domain.OrgRoleContributor, nil, false); err != nil {
		t.Fatalf("Invite: %v", err)
	}

	if err := svc.RemoveMember(context.Background(), bob, org.ID, "bob-id"); err != nil {
		t.Fatalf("self-leave: %v", err)
	}

	// The membership row still exists with the end date stamped.
	m, err := store.GetMembership(context.Background(), org.ID, "bob-id")
	if err != nil {
		t.Fatalf("membership row must survive the leave: %v", err)
	}
	if m.AffiliationEnd == nil {
		t.Error("affiliation_end must be set after leaving")
	}
	if !m.AffiliationEnd.Equal(today()) {
		t.Errorf("affiliation end = %v, want today", *m.AffiliationEnd)
	}
	// The ex-member no longer sees the organization.
	if _, err := svc.Get(context.Background(), bob, org.ID); !errors.Is(err, ErrOrgNotFound) {
		t.Errorf("ex-member Get error = %v, want ErrOrgNotFound", err)
	}
	// The owner still sees the historical membership in the list.
	members, err := svc.ListMembers(context.Background(), alice, org.ID)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("member list has %d entries, want 2 (row kept)", len(members))
	}
}

// TestServiceLastOwnerProtection: the last active owner can neither leave
// nor be demoted nor be removed (the organization always keeps an owner).
func TestServiceLastOwnerProtection(t *testing.T) {
	store := newFakeStore()
	store.seedUser("bob-id", "bob")
	svc := NewService(store)
	alice := testUser("alice-id", "alice")

	org, _, err := svc.Create(context.Background(), alice, "acme", "Acme", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := svc.RemoveMember(context.Background(), alice, org.ID, "alice-id"); !errors.Is(err, ErrLastOwner) {
		t.Errorf("last owner self-leave error = %v, want ErrLastOwner", err)
	}
	viewer := domain.OrgRoleViewer
	if _, err := svc.UpdateMembership(context.Background(), alice, org.ID, "alice-id", &viewer, nil, nil); !errors.Is(err, ErrLastOwner) {
		t.Errorf("last owner self-demotion error = %v, want ErrLastOwner", err)
	}

	// With a second owner in place, the first owner may leave.
	if _, err := svc.Invite(context.Background(), alice, org.ID, "bob", domain.OrgRoleOwner, nil, false); err != nil {
		t.Fatalf("Invite co-owner: %v", err)
	}
	if err := svc.RemoveMember(context.Background(), alice, org.ID, "alice-id"); err != nil {
		t.Fatalf("owner leave with co-owner: %v", err)
	}
	// The co-owner may not leave now — they are the last one.
	if err := svc.RemoveMember(context.Background(), testUser("bob-id", "bob"), org.ID, "bob-id"); !errors.Is(err, ErrLastOwner) {
		t.Errorf("new last owner leave error = %v, want ErrLastOwner", err)
	}
}

// TestServiceDeactivatedOrgRefusesWrites: after deactivation every
// governance write is refused, including by the owner.
func TestServiceDeactivatedOrgRefusesWrites(t *testing.T) {
	store := newFakeStore()
	store.seedUser("bob-id", "bob")
	svc := NewService(store)
	alice := testUser("alice-id", "alice")

	org, _, err := svc.Create(context.Background(), alice, "acme", "Acme", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.Deactivate(context.Background(), alice, org.ID); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}

	if _, err := svc.Invite(context.Background(), alice, org.ID, "bob", domain.OrgRoleViewer, nil, false); !errors.Is(err, ErrOrgDeactivated) {
		t.Errorf("invite after deactivation error = %v, want ErrOrgDeactivated", err)
	}
	if err := svc.RemoveMember(context.Background(), alice, org.ID, "alice-id"); !errors.Is(err, ErrOrgDeactivated) {
		t.Errorf("remove after deactivation error = %v, want ErrOrgDeactivated", err)
	}
	newName := "New"
	if _, err := svc.Update(context.Background(), alice, org.ID, &newName, nil, nil); !errors.Is(err, ErrOrgDeactivated) {
		t.Errorf("update after deactivation error = %v, want ErrOrgDeactivated", err)
	}
	// Reads still work for members.
	if _, err := svc.Get(context.Background(), alice, org.ID); err != nil {
		t.Errorf("member read after deactivation: %v", err)
	}
}

// TestServiceUnknownOrgReadsHidden: a non-member probing an existing
// organization gets ErrOrgNotFound (existence hiding on reads).
func TestServiceUnknownOrgReadsHidden(t *testing.T) {
	store := newFakeStore()
	store.seedUser("bob-id", "bob")
	svc := NewService(store)
	alice := testUser("alice-id", "alice")
	outsider := testUser("carol-id", "carol")

	org, _, err := svc.Create(context.Background(), alice, "acme", "Acme", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.Get(context.Background(), outsider, org.ID); !errors.Is(err, ErrOrgNotFound) {
		t.Errorf("outsider Get error = %v, want ErrOrgNotFound", err)
	}
	if _, err := svc.ListMembers(context.Background(), outsider, org.ID); !errors.Is(err, ErrOrgNotFound) {
		t.Errorf("outsider ListMembers error = %v, want ErrOrgNotFound", err)
	}
}

// TestServiceInviteUnknownUser: inviting a handle with no account answers
// ErrUserNotFound.
func TestServiceInviteUnknownUser(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)
	alice := testUser("alice-id", "alice")

	org, _, err := svc.Create(context.Background(), alice, "acme", "Acme", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.Invite(context.Background(), alice, org.ID, "nobody", domain.OrgRoleViewer, nil, false); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("unknown user invite error = %v, want ErrUserNotFound", err)
	}
}

// TestServiceUpdatePartialFields — M1 fix: Update takes pointers so a
// PATCH that names one field leaves the other untouched (nil = unchanged),
// and an explicit empty description clears it.
func TestServiceUpdatePartialFields(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)
	alice := testUser("alice-id", "alice")

	org, _, err := svc.Create(context.Background(), alice, "acme", "Acme Research", "lab")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Name only: the description must survive.
	newName := "Acme Research Renamed"
	updated, err := svc.Update(context.Background(), alice, org.ID, &newName, nil, nil)
	if err != nil {
		t.Fatalf("Update name: %v", err)
	}
	if updated.Name != newName || updated.Description != "lab" {
		t.Errorf("after name-only update = %q/%q, want %q/%q", updated.Name, updated.Description, newName, "lab")
	}
	// Description only: the name must survive.
	newDesc := "renewed lab"
	updated, err = svc.Update(context.Background(), alice, org.ID, nil, &newDesc, nil)
	if err != nil {
		t.Fatalf("Update description: %v", err)
	}
	if updated.Name != newName || updated.Description != newDesc {
		t.Errorf("after description-only update = %q/%q, want %q/%q", updated.Name, updated.Description, newName, newDesc)
	}
	// Neither field: refused.
	if _, err := svc.Update(context.Background(), alice, org.ID, nil, nil, nil); !errors.Is(err, ErrValidation) {
		t.Errorf("empty update error = %v, want ErrValidation", err)
	}
	// A blank name can never result (even via explicit empty).
	blank := ""
	if _, err := svc.Update(context.Background(), alice, org.ID, &blank, nil, nil); !errors.Is(err, ErrValidation) {
		t.Errorf("blank name error = %v, want ErrValidation", err)
	}
}

// TestServiceStoreFailureSurfacesErrStore — minor fix: a store outage on
// the membership read answers ErrStore (503), never a masked 403/404. A
// plain non-sentinel error stands for the driver failure.
func TestServiceStoreFailureSurfacesErrStore(t *testing.T) {
	store := newFakeStore()
	store.seedUser("bob-id", "bob")
	svc := NewService(store)
	alice := testUser("alice-id", "alice")

	org, _, err := svc.Create(context.Background(), alice, "acme", "Acme", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	store.failMembershipWith = errors.New("db down")

	if _, err := svc.Get(context.Background(), alice, org.ID); !errors.Is(err, ErrStore) {
		t.Errorf("Get during outage error = %v, want ErrStore", err)
	}
	verified := true
	if _, err := svc.UpdateMembership(context.Background(), alice, org.ID, "alice-id", nil, nil, &verified); !errors.Is(err, ErrStore) {
		t.Errorf("UpdateMembership during outage error = %v, want ErrStore", err)
	}
	if _, err := svc.Invite(context.Background(), alice, org.ID, "bob", domain.OrgRoleViewer, nil, false); !errors.Is(err, ErrStore) {
		t.Errorf("Invite during outage error = %v, want ErrStore", err)
	}
	if err := svc.RemoveMember(context.Background(), alice, org.ID, "bob-id"); !errors.Is(err, ErrStore) {
		t.Errorf("RemoveMember during outage error = %v, want ErrStore", err)
	}
	if err := svc.Deactivate(context.Background(), alice, org.ID); !errors.Is(err, ErrStore) {
		t.Errorf("Deactivate during outage error = %v, want ErrStore", err)
	}
	newName := "x"
	if _, err := svc.Update(context.Background(), alice, org.ID, &newName, nil, nil); !errors.Is(err, ErrStore) {
		t.Errorf("Update during outage error = %v, want ErrStore", err)
	}
}

// TestServiceAdjustEndedMembershipKeepsEndDate — B1 fix (contract pin):
// adjusting a membership whose affiliation already ended must leave the
// departure date untouched at the service level too; the real adapter's
// behavior is covered by TestOrgStoreUpdateMembershipPreservesAffiliationEnd
// in the integration suite.
func TestServiceAdjustEndedMembershipKeepsEndDate(t *testing.T) {
	store := newFakeStore()
	store.seedUser("bob-id", "bob")
	svc := NewService(store)
	alice := testUser("alice-id", "alice")

	org, _, err := svc.Create(context.Background(), alice, "acme", "Acme", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.Invite(context.Background(), alice, org.ID, "bob", domain.OrgRoleContributor, nil, false); err != nil {
		t.Fatalf("Invite: %v", err)
	}
	if err := svc.RemoveMember(context.Background(), alice, org.ID, "bob-id"); err != nil {
		t.Fatalf("remove bob: %v", err)
	}
	endBefore, err := store.GetMembership(context.Background(), org.ID, "bob-id")
	if err != nil || endBefore.AffiliationEnd == nil {
		t.Fatalf("end date before adjustment: %v (err %v)", endBefore.AffiliationEnd, err)
	}

	verified := true
	adjusted, err := svc.UpdateMembership(context.Background(), alice, org.ID, "bob-id", nil, nil, &verified)
	if err != nil {
		t.Fatalf("adjust ended membership: %v", err)
	}
	if adjusted.AffiliationEnd == nil || !adjusted.AffiliationEnd.Equal(*endBefore.AffiliationEnd) {
		t.Errorf("affiliation_end after adjustment = %v, want unchanged %v", adjusted.AffiliationEnd, *endBefore.AffiliationEnd)
	}
}
