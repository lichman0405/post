package milestones

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/releases"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
)

// fakeProjectPort implements ProjectAuthzPort with canned rows, recording
// the membership lookups (the authz class input).
type fakeProjectPort struct {
	project    domain.Project
	membership domain.ProjectMembership
	memberErr  error
	projectErr error

	gotMembershipProjectID string
	gotMembershipUserID    string
	memberships            int
}

func (f *fakeProjectPort) GetProject(context.Context, string) (domain.Project, error) {
	return f.project, f.projectErr
}

func (f *fakeProjectPort) GetMembership(_ context.Context, projectID, userID string) (domain.ProjectMembership, error) {
	f.memberships++
	f.gotMembershipProjectID, f.gotMembershipUserID = projectID, userID
	if f.memberErr != nil {
		return domain.ProjectMembership{}, f.memberErr
	}
	return f.membership, nil
}

// fakeReleasePort implements ReleasePort: a canned release or an error
// (releases.ErrReleaseNotFound is the production sentinel).
type fakeReleasePort struct {
	release domain.Release
	err     error
	gotID   string
	calls   int
}

func (f *fakeReleasePort) GetRelease(_ context.Context, _, releaseID string) (domain.Release, error) {
	f.calls++
	f.gotID = releaseID
	return f.release, f.err
}

// fakeStorePort implements StorePort, recording every write/read with
// its audit row and idempotency key.
type fakeStorePort struct {
	milestone domain.Milestone
	err       error
	// lookup is the milestone LookupCreation replays (nil = no entry).
	lookup *domain.Milestone

	gotMilestone      domain.Milestone
	gotAudit          domain.AuditEntry
	gotIdempotencyKey *string
	creates           int
}

func (f *fakeStorePort) CreateMilestone(_ context.Context, m domain.Milestone, audit domain.AuditEntry, key *string) (domain.Milestone, error) {
	f.creates++
	f.gotMilestone, f.gotAudit, f.gotIdempotencyKey = m, audit, key
	return f.milestone, f.err
}

func (f *fakeStorePort) LookupCreation(context.Context, string, string) (*domain.Milestone, error) {
	return f.lookup, nil
}

func (f *fakeStorePort) ListMilestones(context.Context, string) ([]domain.Milestone, error) {
	return nil, nil
}

func (f *fakeStorePort) GetMilestone(context.Context, string, string) (domain.Milestone, error) {
	return domain.Milestone{}, ErrMilestoneNotFound
}

func fixtureProject() domain.Project {
	return domain.Project{ID: "proj-00000001", Name: "MOF Screening", ActivityStatus: "active"}
}

func fixtureActor() domain.User {
	return domain.User{ID: "user-00000001"}
}

// newTestCommand wires a command over fakes with a real matrix engine.
func newTestCommand(p *fakeProjectPort, r *fakeReleasePort, s *fakeStorePort) *Command {
	if p == nil {
		p = &fakeProjectPort{project: fixtureProject(), membership: domain.ProjectMembership{Role: domain.ProjectRoleOwner}}
	}
	if r == nil {
		r = &fakeReleasePort{}
	}
	if s == nil {
		s = &fakeStorePort{milestone: domain.Milestone{ID: "milestone-00000001", ProjectID: "proj-00000001"}}
	}
	return NewCommand(p, r, s, authz.NewMatrixEngine())
}

func baseParams() CreateMilestoneParams {
	return CreateMilestoneParams{
		ProjectID:  "proj-00000001",
		Kind:       domain.MilestonePaperSubmitted,
		Label:      "JACS 2026",
		OccurredAt: time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC),
	}
}

func TestCreateValidatesShape(t *testing.T) {
	s := &fakeStorePort{milestone: domain.Milestone{ID: "m1"}}
	cmd := newTestCommand(nil, nil, s)
	actor := fixtureActor()

	cases := []struct {
		name   string
		mutate func(*CreateMilestoneParams)
		want   string
	}{
		{"blank project", func(p *CreateMilestoneParams) { p.ProjectID = "" }, "project_id is required"},
		{"unknown kind", func(p *CreateMilestoneParams) { p.Kind = "completed" }, "unknown milestone kind"},
		{"zero occurred_at", func(p *CreateMilestoneParams) { p.OccurredAt = time.Time{} }, "occurred_at is required"},
		{"custom without label", func(p *CreateMilestoneParams) { p.Kind = domain.MilestoneCustom; p.Label = "  " }, "requires a label"},
		{"oversized label", func(p *CreateMilestoneParams) { p.Label = strings.Repeat("x", domain.MaxMilestoneLabelLen+1) }, "at most"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := baseParams()
			tc.mutate(&in)
			_, err := cmd.Create(context.Background(), actor, in)
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("Create error = %v, want ErrValidation", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Create error = %q, want it to mention %q", err, tc.want)
			}
			if s.creates != 0 {
				t.Fatalf("store writes = %d, want 0 (validation precedes persistence)", s.creates)
			}
		})
	}
}

func TestCreateTrimsLabel(t *testing.T) {
	s := &fakeStorePort{milestone: domain.Milestone{ID: "m1"}}
	cmd := newTestCommand(nil, nil, s)
	in := baseParams()
	in.Label = "  JACS 2026  "
	if _, err := cmd.Create(context.Background(), fixtureActor(), in); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.gotMilestone.Label != "JACS 2026" {
		t.Fatalf("stored label = %q, want the trimmed form", s.gotMilestone.Label)
	}
}

func TestCreateAuthorizesBeforeLookups(t *testing.T) {
	// A contributor is denied ActionCreateRelease (the reused governance
	// action, matrix semantics: maintainer/owner only) — and the denial
	// precedes the release-link lookup, so nothing beyond the membership
	// read ever runs.
	p := &fakeProjectPort{project: fixtureProject(), membership: domain.ProjectMembership{Role: domain.ProjectRoleContributor}}
	r := &fakeReleasePort{}
	s := &fakeStorePort{}
	cmd := newTestCommand(p, r, s)
	in := baseParams()
	releaseID := "release-00000001"
	in.ReleaseID = &releaseID

	_, err := cmd.Create(context.Background(), fixtureActor(), in)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("Create error = %v, want ErrForbidden", err)
	}
	if r.calls != 0 {
		t.Fatalf("release lookups = %d, want 0 (denial precedes every lookup)", r.calls)
	}
	if s.creates != 0 {
		t.Fatalf("store writes = %d, want 0", s.creates)
	}
}

func TestCreateOwnerAllowed(t *testing.T) {
	p := &fakeProjectPort{project: fixtureProject(), membership: domain.ProjectMembership{Role: domain.ProjectRoleOwner}}
	s := &fakeStorePort{milestone: domain.Milestone{ID: "m1"}}
	cmd := newTestCommand(p, nil, s)

	stored, err := cmd.Create(context.Background(), fixtureActor(), baseParams())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if stored.ID != "m1" {
		t.Fatalf("stored id = %q, want m1", stored.ID)
	}
	if s.creates != 1 {
		t.Fatalf("store writes = %d, want 1", s.creates)
	}
	if p.gotMembershipUserID != "user-00000001" {
		t.Fatalf("membership looked up for %q, want the actor", p.gotMembershipUserID)
	}
}

func TestCreateWritesAuditAndStoredShape(t *testing.T) {
	s := &fakeStorePort{milestone: domain.Milestone{ID: "m1"}}
	cmd := newTestCommand(nil, nil, s)
	in := baseParams()
	releaseID := "release-00000001"
	in.ReleaseID = &releaseID
	in.OccurredAt = time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	if _, err := cmd.Create(context.Background(), fixtureActor(), in); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got := s.gotMilestone
	if got.ProjectID != "proj-00000001" || got.Kind != domain.MilestonePaperSubmitted ||
		got.Label != "JACS 2026" || got.CreatedBy != "user-00000001" ||
		!got.OccurredAt.Equal(in.OccurredAt) {
		t.Fatalf("stored milestone = %+v, want the validated input shape", got)
	}
	if got.ReleaseID == nil || *got.ReleaseID != releaseID {
		t.Fatalf("stored release link = %v, want %q", got.ReleaseID, releaseID)
	}
	if s.gotAudit.Action != domain.ActionMilestoneCreated {
		t.Fatalf("audit action = %q, want milestone.created", s.gotAudit.Action)
	}
	if s.gotAudit.ProjectID != "proj-00000001" || s.gotAudit.ActorID != "user-00000001" {
		t.Fatalf("audit scope = %+v, want the project and the actor", s.gotAudit)
	}
}

func TestCreateReplaysIdempotencyKey(t *testing.T) {
	// A known key replays before any release-link resolution: the
	// released row comes back and the release port is never touched (a
	// replay is a read — docs/22).
	replayed := domain.Milestone{ID: "m-replay", Kind: domain.MilestonePaperSubmitted}
	s := &fakeStorePort{milestone: domain.Milestone{ID: "m-new"}}
	s.lookup = &replayed
	r := &fakeReleasePort{}
	cmd := newTestCommand(nil, r, s)
	in := baseParams()
	key := "key-0001"
	releaseID := "release-00000001"
	in.IdempotencyKey = &key
	in.ReleaseID = &releaseID

	got, err := cmd.Create(context.Background(), fixtureActor(), in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.ID != "m-replay" {
		t.Fatalf("replayed id = %q, want m-replay", got.ID)
	}
	if s.creates != 0 {
		t.Fatalf("store writes = %d, want 0 (a replay is a read)", s.creates)
	}
	if r.calls != 0 {
		t.Fatalf("release lookups = %d, want 0 (replay precedes link resolution)", r.calls)
	}
}

func TestCreateReleaseLinkMustResolveInProject(t *testing.T) {
	r := &fakeReleasePort{err: releases.ErrReleaseNotFound}
	s := &fakeStorePort{}
	cmd := newTestCommand(nil, r, s)
	in := baseParams()
	releaseID := "release-foreign"
	in.ReleaseID = &releaseID

	_, err := cmd.Create(context.Background(), fixtureActor(), in)
	if !errors.Is(err, ErrReleaseNotFound) {
		t.Fatalf("Create error = %v, want ErrReleaseNotFound", err)
	}
	if s.creates != 0 {
		t.Fatalf("store writes = %d, want 0 (the link must resolve first)", s.creates)
	}
}

func TestCreateReleaseLinkOptional(t *testing.T) {
	// No link, no release lookup — the association is optional, never
	// required.
	r := &fakeReleasePort{}
	s := &fakeStorePort{milestone: domain.Milestone{ID: "m1"}}
	cmd := newTestCommand(nil, r, s)

	if _, err := cmd.Create(context.Background(), fixtureActor(), baseParams()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if r.calls != 0 {
		t.Fatalf("release lookups = %d, want 0 (no link sent)", r.calls)
	}
	if s.gotMilestone.ReleaseID != nil {
		t.Fatalf("stored release link = %v, want nil", s.gotMilestone.ReleaseID)
	}
}

func TestCreateProjectNotFoundHidesExistence(t *testing.T) {
	// A project that does not exist (or is invisible) answers
	// ErrProjectNotFound from the membership resolution — never a role
	// decision, never "forbidden" (docs/45: an invisible project is not
	// a "not a member" case).
	p := &fakeProjectPort{memberErr: projects.ErrProjectNotFound}
	s := &fakeStorePort{}
	cmd := newTestCommand(p, nil, s)

	_, err := cmd.Create(context.Background(), fixtureActor(), baseParams())
	if !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("Create error = %v, want ErrProjectNotFound", err)
	}
}

func TestListAndGetValidateIDs(t *testing.T) {
	cmd := newTestCommand(nil, nil, nil)
	if _, err := cmd.List(context.Background(), ""); !errors.Is(err, ErrValidation) {
		t.Fatalf("List error = %v, want ErrValidation", err)
	}
	if _, err := cmd.Get(context.Background(), "p", ""); !errors.Is(err, ErrValidation) {
		t.Fatalf("Get error = %v, want ErrValidation", err)
	}
}

func TestGetMapsStoreNotFound(t *testing.T) {
	cmd := newTestCommand(nil, nil, &fakeStorePort{})
	_, err := cmd.Get(context.Background(), "proj-00000001", "milestone-none")
	if !errors.Is(err, ErrMilestoneNotFound) {
		t.Fatalf("Get error = %v, want ErrMilestoneNotFound", err)
	}
}
