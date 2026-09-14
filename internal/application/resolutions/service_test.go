package resolutions

import (
	"context"
	"errors"
	"testing"

	"github.com/lichman0405/post/internal/application/diffs"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/conflict"
	"github.com/lichman0405/post/internal/rsg/diff"
	"github.com/lichman0405/post/internal/rsg/manifest"
)

// fakeStore is the in-memory store double: plans and evidence both keyed
// by identity so the service's contract (upsert, per-side evidence) is
// exercised without PostgreSQL.
type fakeStore struct {
	plans    map[string][]domain.ConflictResolution
	evidence map[string][]EvidenceItem
	saveErr  error
}

func newFakeStore() *fakeStore {
	return &fakeStore{plans: map[string][]domain.ConflictResolution{}, evidence: map[string][]EvidenceItem{}}
}

func (f *fakeStore) planKey(p, b, s, t string) string { return p + "|" + b + "|" + s + "|" + t }

func (f *fakeStore) SavePlan(_ context.Context, in SavePlanParams) ([]domain.ConflictResolution, error) {
	if f.saveErr != nil {
		return nil, f.saveErr
	}
	key := f.planKey(in.ProjectID, in.BaseStateID, in.SourceStateID, in.TargetStateID)
	plan := f.plans[key]
	for _, d := range in.Decisions {
		replaced := false
		for i := range plan {
			if sameConflictIdentity(plan[i], d) {
				plan[i] = d
				replaced = true
			}
		}
		if !replaced {
			plan = append(plan, d)
		}
	}
	f.plans[key] = plan
	return append([]domain.ConflictResolution(nil), plan...), nil
}

func sameConflictIdentity(a, b domain.ConflictResolution) bool {
	return a.TargetKind == b.TargetKind && a.TargetID == b.TargetID && a.Code == b.Code &&
		joinStrings(a.Fields) == joinStrings(b.Fields) &&
		joinStrings(a.PayloadKeys) == joinStrings(b.PayloadKeys) &&
		ptrOrEmpty(a.OtherObjectID) == ptrOrEmpty(b.OtherObjectID)
}

func (f *fakeStore) ListPlan(_ context.Context, p, b, s, t string) ([]domain.ConflictResolution, error) {
	return append([]domain.ConflictResolution(nil), f.plans[f.planKey(p, b, s, t)]...), nil
}

func (f *fakeStore) ListEvidenceForObjectVersion(_ context.Context, v string) ([]EvidenceItem, error) {
	return append([]EvidenceItem(nil), f.evidence[v]...), nil
}

// fakeConflicts is the report double: a fixed report whose verdicts name
// the conflicts the tests decide about.
type fakeConflicts struct {
	report *conflict.Report
	err    error
	calls  int
}

func (f *fakeConflicts) Conflicts(context.Context, diffs.Params) (*conflict.Report, error) {
	f.calls++
	return f.report, f.err
}

// fakeProjects is the membership gate double.
type fakeProjects struct {
	role *domain.ProjectRole
	err  error
}

func (f *fakeProjects) GetMembership(context.Context, domain.User, string) (domain.ProjectMembership, error) {
	if f.err != nil {
		return domain.ProjectMembership{}, f.err
	}
	if f.role == nil {
		return domain.ProjectMembership{}, projects.ErrMemberNotFound
	}
	return domain.ProjectMembership{Role: *f.role}, nil
}

// reportWith builds a conflict report whose change list and verdicts agree
// on one object conflict (SCIENTIFIC_FIELD_DIVERGES over payload key
// "steps") — the shape the save-time identity check consumes. The diff is
// the real diff.Diff type: the service's evidence assembly walks it.
func reportWith(projectID, objectID, sourceVersionID, targetVersionID string) *conflict.Report {
	return &conflict.Report{
		FormatVersion: conflict.FormatV1,
		ProjectID:     projectID,
		Diff: &diff.Diff{
			FormatVersion: "v1",
			ProjectID:     projectID,
			ObjectChanges: []diff.ObjectChange{{
				ObjectID:      objectID,
				ObjectType:    "protocol",
				Kind:          diff.ChangeUpdated,
				SourceVersion: manifest.ObjectVersion{ID: sourceVersionID, ObjectID: objectID},
				TargetVersion: &manifest.ObjectVersion{ID: targetVersionID, ObjectID: objectID},
			}},
		},
		ObjectVerdicts: []conflict.ObjectVerdict{{
			ObjectID:      objectID,
			ObjectType:    "protocol",
			Kind:          diff.ChangeUpdated,
			AutoMergeable: false,
			Conflicts: []conflict.Conflict{{
				Category:    conflict.CategoryScientific,
				Code:        conflict.CodeScientificFieldDiverges,
				Fields:      []string{"payload"},
				PayloadKeys: []string{"steps"},
				Detail:      "the protocol steps diverged on the two branches",
			}},
		}},
		RelationVerdicts: []conflict.RelationVerdict{},
	}
}

func serviceForTest(t *testing.T, store *fakeStore, role *domain.ProjectRole, fc *fakeConflicts) *Service {
	t.Helper()
	return NewService(fc, store, &fakeProjects{role: role}, authz.NewMatrixEngine())
}

func TestSaveRecordsDecisionsAndReturnsPlan(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	role := domain.ProjectRoleOwner
	svc := serviceForTest(t, store, &role, &fakeConflicts{report: reportWith("p1", "o1", "v-src", "v-tgt")})
	actor := domain.User{ID: "u1"}

	plan, err := svc.Save(ctx, actor, SaveInput{
		ProjectID:     "p1",
		BaseStateID:   "s0",
		SourceStateID: "s1",
		TargetStateID: "s2",
		Decisions: []Decision{{
			TargetKind:  domain.ConflictResolutionTargetObject,
			TargetID:    "o1",
			Code:        conflict.CodeScientificFieldDiverges,
			Fields:      []string{"payload"},
			PayloadKeys: []string{"steps"},
			Kind:        domain.ResolutionKeepBoth,
			Note:        "keep both protocol variants",
		}},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if len(plan) != 1 {
		t.Fatalf("plan has %d rows, want 1", len(plan))
	}
	if plan[0].Kind != domain.ResolutionKeepBoth || plan[0].DecidedBy != "u1" || plan[0].Note != "keep both protocol variants" {
		t.Fatalf("saved row = %+v", plan[0])
	}

	// An overwrite replaces the decision in place and keeps one row.
	plan, err = svc.Save(ctx, actor, SaveInput{
		ProjectID:     "p1",
		BaseStateID:   "s0",
		SourceStateID: "s1",
		TargetStateID: "s2",
		Decisions: []Decision{{
			TargetKind:  domain.ConflictResolutionTargetObject,
			TargetID:    "o1",
			Code:        conflict.CodeScientificFieldDiverges,
			Fields:      []string{"payload"},
			PayloadKeys: []string{"steps"},
			Kind:        domain.ResolutionAcceptSource,
		}},
	})
	if err != nil {
		t.Fatalf("Save (overwrite): %v", err)
	}
	if len(plan) != 1 || plan[0].Kind != domain.ResolutionAcceptSource {
		t.Fatalf("plan after overwrite = %+v", plan)
	}
}

func TestSaveRefusesUnknownConflict(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	role := domain.ProjectRoleOwner
	svc := serviceForTest(t, store, &role, &fakeConflicts{report: reportWith("p1", "o1", "v-src", "v-tgt")})

	_, err := svc.Save(ctx, domain.User{ID: "u1"}, SaveInput{
		ProjectID:     "p1",
		BaseStateID:   "s0",
		SourceStateID: "s1",
		TargetStateID: "s2",
		Decisions: []Decision{{
			TargetKind: domain.ConflictResolutionTargetObject,
			TargetID:   "o-nope", // not in the report
			Code:       conflict.CodeScientificFieldDiverges,
			Fields:     []string{"payload"},
			Kind:       domain.ResolutionAcceptSource,
		}},
	})
	if !errors.Is(err, ErrConflictNotFound) {
		t.Fatalf("err = %v, want ErrConflictNotFound", err)
	}
	if len(store.plans) != 0 {
		t.Fatalf("refused save must not touch the store: %+v", store.plans)
	}

	// A wrong payload key for an otherwise real conflict is also refused.
	_, err = svc.Save(ctx, domain.User{ID: "u1"}, SaveInput{
		ProjectID:     "p1",
		BaseStateID:   "s0",
		SourceStateID: "s1",
		TargetStateID: "s2",
		Decisions: []Decision{{
			TargetKind:  domain.ConflictResolutionTargetObject,
			TargetID:    "o1",
			Code:        conflict.CodeScientificFieldDiverges,
			Fields:      []string{"payload"},
			PayloadKeys: []string{"temperature"}, // report says "steps"
			Kind:        domain.ResolutionAcceptSource,
		}},
	})
	if !errors.Is(err, ErrConflictNotFound) {
		t.Fatalf("err = %v, want ErrConflictNotFound (payload key mismatch)", err)
	}
}

func TestSaveValidation(t *testing.T) {
	ctx := context.Background()
	svc := serviceForTest(t, newFakeStore(), nil, &fakeConflicts{report: reportWith("p1", "o1", "v-src", "v-tgt")})
	actor := domain.User{ID: "u1"}
	base := SaveInput{
		ProjectID:     "p1",
		BaseStateID:   "s0",
		SourceStateID: "s1",
		TargetStateID: "s2",
		Decisions: []Decision{{
			TargetKind: domain.ConflictResolutionTargetObject,
			TargetID:   "o1",
			Code:       conflict.CodeScientificFieldDiverges,
			Fields:     []string{"payload"},
			Kind:       domain.ResolutionAcceptSource,
		}},
	}

	empty := base
	empty.Decisions = nil
	if _, err := svc.Save(ctx, actor, empty); !errors.Is(err, ErrValidation) {
		t.Fatalf("empty decisions err = %v, want ErrValidation", err)
	}

	badKind := base
	badKind.Decisions[0].Kind = "average"
	if _, err := svc.Save(ctx, actor, badKind); !errors.Is(err, ErrValidation) {
		t.Fatalf("unknown kind err = %v, want ErrValidation", err)
	}

	badTargetKind := base
	badTargetKind.Decisions[0].TargetKind = "version"
	if _, err := svc.Save(ctx, actor, badTargetKind); !errors.Is(err, ErrValidation) {
		t.Fatalf("unknown target kind err = %v, want ErrValidation", err)
	}

	noTarget := base
	noTarget.Decisions[0].TargetID = ""
	if _, err := svc.Save(ctx, actor, noTarget); !errors.Is(err, ErrValidation) {
		t.Fatalf("empty target err = %v, want ErrValidation", err)
	}

	noTriple := base
	noTriple.SourceStateID = ""
	if _, err := svc.Save(ctx, actor, noTriple); !errors.Is(err, ErrValidation) {
		t.Fatalf("empty source state err = %v, want ErrValidation", err)
	}
}

func TestSaveForbiddenForViewer(t *testing.T) {
	ctx := context.Background()
	role := domain.ProjectRoleViewer
	svc := serviceForTest(t, newFakeStore(), &role, &fakeConflicts{report: reportWith("p1", "o1", "v-src", "v-tgt")})
	_, err := svc.Save(ctx, domain.User{ID: "u1"}, SaveInput{
		ProjectID:     "p1",
		BaseStateID:   "s0",
		SourceStateID: "s1",
		TargetStateID: "s2",
		Decisions: []Decision{{
			TargetKind: domain.ConflictResolutionTargetObject,
			TargetID:   "o1",
			Code:       conflict.CodeScientificFieldDiverges,
			Fields:     []string{"payload"},
			Kind:       domain.ResolutionAcceptSource,
		}},
	})
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
}

// TestSaveForbiddenForNonMember pins the same shape for a caller with no
// membership at all: the matrix's authenticated_nonmember column refuses
// the write (OwnForkOnly is not a permit).
func TestSaveForbiddenForNonMember(t *testing.T) {
	ctx := context.Background()
	svc := serviceForTest(t, newFakeStore(), nil, &fakeConflicts{report: reportWith("p1", "o1", "v-src", "v-tgt")})
	_, err := svc.Save(ctx, domain.User{ID: "u1"}, SaveInput{
		ProjectID:     "p1",
		BaseStateID:   "s0",
		SourceStateID: "s1",
		TargetStateID: "s2",
		Decisions: []Decision{{
			TargetKind: domain.ConflictResolutionTargetObject,
			TargetID:   "o1",
			Code:       conflict.CodeScientificFieldDiverges,
			Fields:     []string{"payload"},
			Kind:       domain.ResolutionAcceptSource,
		}},
	})
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
}

func TestViewComposesReportEvidenceAndPlan(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	store.evidence["v-src"] = []EvidenceItem{{RelationType: "supports", EvidenceObjectID: "ev1", EvidenceTitle: "raw data"}}
	store.evidence["v-tgt"] = []EvidenceItem{{RelationType: "contradicts", EvidenceObjectID: "ev2", EvidenceTitle: "replication"}}
	role := domain.ProjectRoleOwner
	svc := serviceForTest(t, store, &role, &fakeConflicts{report: reportWith("p1", "o1", "v-src", "v-tgt")})

	// Seed one saved decision so the view carries it.
	if _, err := svc.Save(ctx, domain.User{ID: "u1"}, SaveInput{
		ProjectID:     "p1",
		BaseStateID:   "s0",
		SourceStateID: "s1",
		TargetStateID: "s2",
		Decisions: []Decision{{
			TargetKind:  domain.ConflictResolutionTargetObject,
			TargetID:    "o1",
			Code:        conflict.CodeScientificFieldDiverges,
			Fields:      []string{"payload"},
			PayloadKeys: []string{"steps"},
			Kind:        domain.ResolutionUnresolved,
		}},
	}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	view, err := svc.View(ctx, "p1", "s0", "s1", "s2")
	if err != nil {
		t.Fatalf("View: %v", err)
	}
	if view.Report == nil || view.Report.ProjectID != "p1" {
		t.Fatalf("view report = %+v", view.Report)
	}
	if len(view.Evidence) != 1 || view.Evidence[0].ObjectID != "o1" {
		t.Fatalf("view evidence = %+v", view.Evidence)
	}
	if len(view.Evidence[0].SourceEvidence) != 1 || view.Evidence[0].SourceEvidence[0].EvidenceTitle != "raw data" {
		t.Fatalf("source evidence = %+v", view.Evidence[0].SourceEvidence)
	}
	if len(view.Evidence[0].TargetEvidence) != 1 || view.Evidence[0].TargetEvidence[0].EvidenceTitle != "replication" {
		t.Fatalf("target evidence = %+v", view.Evidence[0].TargetEvidence)
	}
	if len(view.Resolutions) != 1 || view.Resolutions[0].Kind != domain.ResolutionUnresolved {
		t.Fatalf("view resolutions = %+v", view.Resolutions)
	}
}
