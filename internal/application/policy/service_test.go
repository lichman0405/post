package policy

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/application/orgs"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
)

// fakePolicyStore is the in-memory PolicyStore for service tests: it
// keeps versions per scope, records the againstOrg callback's input, and
// reproduces the store sentinels.
type fakePolicyStore struct {
	versions    []domain.PolicyVersion
	lastCheck   *domain.Policy // the org policy the lower-bound callback saw
	checkCalled int
	fail        error
}

func (f *fakePolicyStore) CreateOrgVersion(_ context.Context, v domain.PolicyVersion, _ domain.AuditEntry) (domain.PolicyVersion, error) {
	if f.fail != nil {
		return domain.PolicyVersion{}, f.fail
	}
	for _, existing := range f.versions {
		if existing.Scope == v.Scope && existing.Version == v.Version {
			return domain.PolicyVersion{}, ErrVersionTaken
		}
	}
	v.ID = "v-" + v.Scope.ProjectID + v.Scope.OrganizationID + "-" + v.Version
	v.CreatedAt = time.Now()
	f.versions = append(f.versions, v)
	return v, nil
}

func (f *fakePolicyStore) CreateProjectVersion(_ context.Context, v domain.PolicyVersion, _ *string, againstOrg func(*domain.Policy) error, _ domain.AuditEntry) (domain.PolicyVersion, error) {
	if f.fail != nil {
		return domain.PolicyVersion{}, f.fail
	}
	for _, existing := range f.versions {
		if existing.Scope == v.Scope && existing.Version == v.Version {
			return domain.PolicyVersion{}, ErrVersionTaken
		}
	}
	orgDoc, err := f.latestLocked(domain.PolicyScope{OrganizationID: "org"})
	if err != nil {
		return domain.PolicyVersion{}, err
	}
	if orgDoc != nil {
		doc := orgDoc.Policy
		f.lastCheck = &doc
	}
	f.checkCalled++
	if err := againstOrg(f.lastCheck); err != nil {
		return domain.PolicyVersion{}, err
	}
	v.ID = "v-project-" + v.Version
	v.CreatedAt = time.Now()
	f.versions = append(f.versions, v)
	return v, nil
}

func (f *fakePolicyStore) GetVersion(_ context.Context, id string) (domain.PolicyVersion, error) {
	if f.fail != nil {
		return domain.PolicyVersion{}, f.fail
	}
	for _, v := range f.versions {
		if v.ID == id {
			return v, nil
		}
	}
	return domain.PolicyVersion{}, ErrPolicyNotFound
}

func (f *fakePolicyStore) latestLocked(scope domain.PolicyScope) (*domain.PolicyVersion, error) {
	var newest *domain.PolicyVersion
	for i := range f.versions {
		v := &f.versions[i]
		if v.Scope != scope {
			continue
		}
		if newest == nil || v.CreatedAt.After(newest.CreatedAt) {
			newest = v
		}
	}
	return newest, nil
}

func (f *fakePolicyStore) Latest(_ context.Context, scope domain.PolicyScope) (domain.PolicyVersion, error) {
	if f.fail != nil {
		return domain.PolicyVersion{}, f.fail
	}
	v, err := f.latestLocked(scope)
	if err != nil {
		return domain.PolicyVersion{}, err
	}
	if v == nil {
		return domain.PolicyVersion{}, ErrPolicyNotFound
	}
	return *v, nil
}

func (f *fakePolicyStore) List(_ context.Context, scope domain.PolicyScope) ([]domain.PolicyVersion, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	var out []domain.PolicyVersion
	for _, v := range f.versions {
		if v.Scope == scope {
			out = append(out, v)
		}
	}
	return out, nil
}

// fakeOrgs is the in-memory OrgGate.
type fakeOrgs struct {
	org    domain.Organization
	member *domain.OrganizationMembership // nil = not a member
}

func (f *fakeOrgs) GetOrganization(_ context.Context, _ string) (domain.Organization, error) {
	if f.org.ID == "" {
		return domain.Organization{}, orgs.ErrOrgNotFound
	}
	return f.org, nil
}

func (f *fakeOrgs) GetMembership(_ context.Context, _, _ string) (domain.OrganizationMembership, error) {
	if f.member == nil {
		return domain.OrganizationMembership{}, orgs.ErrMemberNotFound
	}
	return *f.member, nil
}

// fakeProjects is the in-memory ProjectGate.
type fakeProjects struct {
	project domain.Project
	member  *domain.ProjectMembership // nil = not a member
}

func (f *fakeProjects) GetProject(_ context.Context, _ string) (domain.Project, error) {
	if f.project.ID == "" {
		return domain.Project{}, projects.ErrProjectNotFound
	}
	return f.project, nil
}

func (f *fakeProjects) GetMembership(_ context.Context, _, _ string) (domain.ProjectMembership, error) {
	if f.member == nil {
		return domain.ProjectMembership{}, projects.ErrMemberNotFound
	}
	return *f.member, nil
}

const (
	actorID = "actor-1"
	orgID   = "org"
	projID  = "proj"
)

func newTestService(t *testing.T) (*Service, *fakePolicyStore, *fakeOrgs, *fakeProjects) {
	t.Helper()
	store := &fakePolicyStore{}
	orgGate := &fakeOrgs{
		org:    domain.Organization{ID: orgID, Slug: "lab"},
		member: &domain.OrganizationMembership{OrganizationID: orgID, UserID: actorID, Role: domain.OrgRoleOwner, Verified: true, AffiliationStart: time.Now().AddDate(0, 0, -1)},
	}
	projectGate := &fakeProjects{
		project: domain.Project{ID: projID, OrganizationID: strPtr(orgID)},
		member:  &domain.ProjectMembership{ProjectID: projID, UserID: actorID, Role: domain.ProjectRoleOwner},
	}
	return NewService(store, orgGate, projectGate, nil), store, orgGate, projectGate
}

func strPtr(s string) *string { return &s }

func timePtr(t time.Time) *time.Time { return &t }

func actor() domain.User { return domain.User{ID: actorID} }

func doc(t *testing.T, s string) json.RawMessage {
	t.Helper()
	if !json.Valid([]byte(s)) {
		t.Fatalf("invalid test policy doc: %s", s)
	}
	return json.RawMessage(s)
}

func TestSetOrgPolicy(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newTestService(t)

	v, err := svc.SetOrgPolicy(ctx, actor(), orgID, "1", doc(t, `{"main_protected":true,"release_min_reviewers":2}`))
	if err != nil {
		t.Fatalf("SetOrgPolicy: %v", err)
	}
	if v.Scope.OrganizationID != orgID || v.Version != "1" || v.CreatedBy != actorID {
		t.Errorf("stored version mismatch: %+v", v)
	}
	rule, ok := v.Policy.Rule(domain.RuleMainProtected)
	if !ok || string(rule) != "true" {
		t.Errorf("policy rule lost: %s %v", rule, ok)
	}
	// A second version appends — both stay queryable.
	if _, err := svc.SetOrgPolicy(ctx, actor(), orgID, "2", doc(t, `{"release_min_reviewers":3}`)); err != nil {
		t.Fatalf("second SetOrgPolicy: %v", err)
	}
	versions, err := svc.ListVersions(ctx, actor(), domain.PolicyScope{OrganizationID: orgID})
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 2 {
		t.Errorf("ListVersions = %d versions, want 2 (old versions must stay queryable)", len(versions))
	}
	// Duplicate version string is refused.
	if _, err := svc.SetOrgPolicy(ctx, actor(), orgID, "1", doc(t, `{}`)); !errors.Is(err, ErrVersionTaken) {
		t.Errorf("duplicate version: got %v, want ErrVersionTaken", err)
	}
}

// A version string with surrounding whitespace is accepted by
// ValidPolicyVersion and STORED trimmed: " v1 " and "v1" are the same
// version, so the per-scope uniqueness guard sees one version, not two
// spellings of it.
func TestSetPolicyVersionStoresTrimmedForm(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newTestService(t)

	v, err := svc.SetOrgPolicy(ctx, actor(), orgID, " v1 ", doc(t, `{"main_protected":true}`))
	if err != nil {
		t.Fatalf("SetOrgPolicy: %v", err)
	}
	if v.Version != "v1" {
		t.Errorf("stored version = %q, want the trimmed %q", v.Version, "v1")
	}
	// The trimmed spelling IS the version now taken — not a second one.
	if _, err := svc.SetOrgPolicy(ctx, actor(), orgID, "v1", doc(t, `{}`)); !errors.Is(err, ErrVersionTaken) {
		t.Errorf("reusing the trimmed spelling: got %v, want ErrVersionTaken", err)
	}
}

func TestSetOrgPolicyAuthorization(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		member *domain.OrganizationMembership
		org    domain.Organization
		want   error
	}{
		{"not a member", nil, domain.Organization{ID: orgID}, ErrForbidden},
		{"viewer member", &domain.OrganizationMembership{OrganizationID: orgID, UserID: actorID, Role: domain.OrgRoleViewer, AffiliationStart: time.Now().AddDate(0, 0, -1)}, domain.Organization{ID: orgID}, ErrForbidden},
		{"deactivated org", &domain.OrganizationMembership{OrganizationID: orgID, UserID: actorID, Role: domain.OrgRoleOwner, AffiliationStart: time.Now().AddDate(0, 0, -1)}, domain.Organization{ID: orgID, DeactivatedAt: timePtr(time.Now().AddDate(0, 0, -1))}, ErrOrgDeactivated},
		{"unknown org", &domain.OrganizationMembership{OrganizationID: orgID, UserID: actorID, Role: domain.OrgRoleOwner}, domain.Organization{}, ErrOrgNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakePolicyStore{}
			gate := &fakeOrgs{org: tc.org, member: tc.member}
			svc := NewService(store, gate, &fakeProjects{}, nil)
			_, err := svc.SetOrgPolicy(ctx, actor(), orgID, "1", doc(t, `{}`))
			if !errors.Is(err, tc.want) {
				t.Errorf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestSetOrgPolicyValidation(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newTestService(t)
	cases := []struct {
		name    string
		version string
		doc     string
	}{
		{"bad version", "has space", `{}`},
		{"empty version", "", `{}`},
		{"not a policy object", "1", `[1]`},
		{"unknown-shaped rule value", "1", `{"main_protected":"yes"}`},
		{"null rule", "1", `{"main_protected":null}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.SetOrgPolicy(ctx, actor(), orgID, tc.version, doc(t, tc.doc))
			if !errors.Is(err, ErrValidation) {
				t.Errorf("got %v, want ErrValidation", err)
			}
		})
	}
}

func TestSetProjectPolicyLowerBound(t *testing.T) {
	ctx := context.Background()
	svc, store, _, _ := newTestService(t)

	if _, err := svc.SetOrgPolicy(ctx, actor(), orgID, "1", doc(t, `{"main_protected":true,"release_min_reviewers":2}`)); err != nil {
		t.Fatalf("SetOrgPolicy: %v", err)
	}
	// Stricter is accepted.
	v, err := svc.SetProjectPolicy(ctx, actor(), projID, "1", doc(t, `{"release_min_reviewers":3}`))
	if err != nil {
		t.Fatalf("stricter project policy: %v", err)
	}
	if v.Scope.ProjectID != projID {
		t.Errorf("scope = %+v, want project", v.Scope)
	}
	if store.lastCheck == nil {
		t.Fatal("store re-check never saw the org policy")
	}
	// Relaxing is refused, naming the rule.
	_, err = svc.SetProjectPolicy(ctx, actor(), projID, "2", doc(t, `{"main_protected":false}`))
	if !errors.Is(err, ErrProjectRelaxesOrg) {
		t.Fatalf("relaxing policy: got %v, want ErrProjectRelaxesOrg", err)
	}
	var mv *domain.MergeViolations
	if !errors.As(err, &mv) || len(mv.Violations) != 1 || mv.Violations[0].Key != domain.RuleMainProtected {
		t.Errorf("violation detail lost: %v", err)
	}
	// The relaxed version was never stored.
	versions, err := svc.ListVersions(ctx, actor(), domain.PolicyScope{ProjectID: projID})
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 1 {
		t.Errorf("stored versions = %d, want 1 (relaxing write must not persist)", len(versions))
	}
}

func TestSetProjectPolicyPersonalProject(t *testing.T) {
	ctx := context.Background()
	svc, _, _, projectGate := newTestService(t)
	projectGate.project = domain.Project{ID: projID} // personal: no organization

	// No lower bound: any well-formed policy is accepted.
	v, err := svc.SetProjectPolicy(ctx, actor(), projID, "1", doc(t, `{"main_protected":false}`))
	if err != nil {
		t.Fatalf("personal project policy: %v", err)
	}
	if v.Scope.ProjectID != projID {
		t.Errorf("scope = %+v", v.Scope)
	}
}

func TestSetProjectPolicyAuthorization(t *testing.T) {
	ctx := context.Background()
	store := &fakePolicyStore{}
	projectGate := &fakeProjects{
		project: domain.Project{ID: projID, OrganizationID: strPtr(orgID)},
		member:  &domain.ProjectMembership{ProjectID: projID, UserID: actorID, Role: domain.ProjectRoleMaintainer},
	}
	svc := NewService(store, &fakeOrgs{org: domain.Organization{ID: orgID}}, projectGate, nil)
	if _, err := svc.SetProjectPolicy(ctx, actor(), projID, "1", doc(t, `{}`)); !errors.Is(err, ErrForbidden) {
		t.Errorf("maintainer write: got %v, want ErrForbidden (writes are owner-only)", err)
	}
}

func TestEffectivePolicyAndEvaluate(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newTestService(t)

	if _, err := svc.SetOrgPolicy(ctx, actor(), orgID, "1", doc(t, `{"main_protected":true,"release_min_reviewers":2}`)); err != nil {
		t.Fatalf("SetOrgPolicy: %v", err)
	}
	if _, err := svc.SetProjectPolicy(ctx, actor(), projID, "1", doc(t, `{"release_min_reviewers":3,"raw_data_retention_days":90}`)); err != nil {
		t.Fatalf("SetProjectPolicy: %v", err)
	}
	eff, err := svc.EffectivePolicy(ctx, actor(), projID)
	if err != nil {
		t.Fatalf("EffectivePolicy: %v", err)
	}
	if eff.Org == nil || eff.Project == nil {
		t.Fatalf("EffectivePolicy missing sides: %+v", eff)
	}
	// The merge: stricter project rule wins, org-only rule carries.
	d, err := svc.Evaluate(ctx, eff.Effective, Query{Rule: domain.RuleReleaseMinReviewers})
	if err != nil || !d.Found || d.Int != 3 {
		t.Errorf("effective reviewers = %+v, %v; want 3", d, err)
	}
	d, err = svc.Evaluate(ctx, eff.Effective, Query{Rule: domain.RuleMainProtected})
	if err != nil || !d.Found || !d.Bool {
		t.Errorf("effective main_protected = %+v, %v; want true", d, err)
	}
	// Absent rule: Found=false, no error.
	d, err = svc.Evaluate(ctx, eff.Effective, Query{Rule: domain.RulePublicAssetIPReview})
	if err != nil || d.Found {
		t.Errorf("absent rule = %+v, %v; want Found=false", d, err)
	}
	// Unknown rule key: default deny.
	if _, err := svc.Evaluate(ctx, eff.Effective, Query{Rule: "no_such_rule"}); !errors.Is(err, domain.ErrUnknownRule) {
		t.Errorf("unknown rule: got %v, want ErrUnknownRule", err)
	}
	// EvaluateEffective goes through the same merge.
	d, err = svc.EvaluateEffective(ctx, actor(), projID, Query{Rule: domain.RuleRawDataRetentionDays})
	if err != nil || !d.Found || d.Int != 90 {
		t.Errorf("EvaluateEffective = %+v, %v; want 90", d, err)
	}
	// EvaluateVersion answers against the pinned version, not the newest.
	v1 := eff.Project
	if _, err := svc.SetProjectPolicy(ctx, actor(), projID, "2", doc(t, `{"raw_data_retention_days":180}`)); err != nil {
		t.Fatalf("second project policy: %v", err)
	}
	d, err = svc.EvaluateVersion(ctx, actor(), v1.ID, Query{Rule: domain.RuleRawDataRetentionDays})
	if err != nil || !d.Found || d.Int != 90 {
		t.Errorf("pinned EvaluateVersion = %+v, %v; want 90 (the pinned version's value)", d, err)
	}
}

func TestEffectivePolicyOrgWinsConflicts(t *testing.T) {
	ctx := context.Background()
	svc, store, _, _ := newTestService(t)

	if _, err := svc.SetOrgPolicy(ctx, actor(), orgID, "1", doc(t, `{"release_min_reviewers":2}`)); err != nil {
		t.Fatalf("SetOrgPolicy: %v", err)
	}
	if _, err := svc.SetProjectPolicy(ctx, actor(), projID, "1", doc(t, `{"release_min_reviewers":3}`)); err != nil {
		t.Fatalf("SetProjectPolicy: %v", err)
	}
	// The org tightens past the project's stored value: the write is
	// allowed (the project version was valid when written), and the
	// effective policy keeps the org bound — evaluation never fails.
	if _, err := svc.SetOrgPolicy(ctx, actor(), orgID, "2", doc(t, `{"release_min_reviewers":5}`)); err != nil {
		t.Fatalf("org tightening: %v", err)
	}
	_ = store
	eff, err := svc.EffectivePolicy(ctx, actor(), projID)
	if err != nil {
		t.Fatalf("EffectivePolicy after org tightening: %v", err)
	}
	d, err := svc.Evaluate(ctx, eff.Effective, Query{Rule: domain.RuleReleaseMinReviewers})
	if err != nil || !d.Found || d.Int != 5 {
		t.Errorf("effective reviewers = %+v, %v; want 5 (org lower bound wins)", d, err)
	}
}

func TestReadAuthorization(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		orgMem  *domain.OrganizationMembership
		projMem *domain.ProjectMembership
		want    error
	}{
		{"outsider everywhere", nil, nil, ErrProjectNotFound},
		{"org member only", &domain.OrganizationMembership{OrganizationID: orgID, UserID: actorID, Role: domain.OrgRoleViewer, AffiliationStart: time.Now().AddDate(0, 0, -1)}, nil, ErrProjectNotFound},
		{"project viewer", nil, &domain.ProjectMembership{ProjectID: projID, UserID: actorID, Role: domain.ProjectRoleViewer}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakePolicyStore{}
			store.versions = append(store.versions, domain.PolicyVersion{
				ID: "pv-1", Scope: domain.PolicyScope{ProjectID: projID}, Version: "1", Policy: domain.EmptyPolicy(),
			})
			svc := NewService(store,
				&fakeOrgs{org: domain.Organization{ID: orgID}, member: tc.orgMem},
				&fakeProjects{project: domain.Project{ID: projID, OrganizationID: strPtr(orgID)}, member: tc.projMem},
				nil)
			_, err := svc.GetProjectPolicy(ctx, actor(), projID)
			if !errors.Is(err, tc.want) {
				t.Errorf("GetProjectPolicy: got %v, want %v", err, tc.want)
			}
			if tc.want == nil {
				v, err := svc.GetVersion(ctx, actor(), "pv-1")
				if err != nil || v.ID != "pv-1" {
					t.Errorf("GetVersion = %+v, %v", v, err)
				}
			}
		})
	}
}

func TestStoreFailureFailsClosed(t *testing.T) {
	ctx := context.Background()
	svc, store, _, _ := newTestService(t)
	store.fail = errors.New("db down")
	_, err := svc.SetOrgPolicy(ctx, actor(), orgID, "1", doc(t, `{}`))
	if !errors.Is(err, ErrStore) {
		t.Errorf("store failure: got %v, want ErrStore", err)
	}
	_, err = svc.GetOrgPolicy(ctx, actor(), orgID)
	if !errors.Is(err, ErrStore) {
		t.Errorf("read store failure: got %v, want ErrStore", err)
	}
}
