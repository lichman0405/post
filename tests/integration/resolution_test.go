package integration

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/lichman0405/post/internal/application/diffs"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/resolutions"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/rsg/conflict"
)

// Package integration: the Scientific Conflict Resolution store contract
// (T0407) over real PostgreSQL — the same service composition
// cmd/api/main.go uses (diffs over the real graph, the pgx resolution
// store, the real membership matrix). The HTTP half of the journey lives
// in tests/e2e (conflict_e2e_test.go); this file pins the store and
// service invariants directly:
//
//   - every one of the five human decision kinds is storable, and saving
//     again overwrites the decision in place (one row, latest wins) with
//     one audit row per save naming the human;
//   - a decision naming a conflict the report does not contain is
//     refused and never touches the store;
//   - a triple that mixes another project's state is refused.

// resolutionFixture composes the resolution service over diffFixture's
// real stack.
type resolutionFixture struct {
	f   *diffFixture
	svc *resolutions.Service
}

func newResolutionFixture(t *testing.T, ctx context.Context) *resolutionFixture {
	t.Helper()
	f := newDiffFixture(t, ctx)
	projectsSvc := projects.NewService(
		persistence.NewProjectStore(f.pool),
		persistence.NewOrgStore(f.pool),
		authz.NewMatrixEngine(),
	)
	return &resolutionFixture{
		f: f,
		svc: resolutions.NewService(
			f.diffs,
			resolutions.NewPGStore(f.pool),
			projectsSvc,
			authz.NewMatrixEngine(),
		),
	}
}

// seedProtocolConflict builds the T0405 acceptance shape — both branches
// move the same protocol step to different temperatures — and returns the
// triple + object.
func (r *resolutionFixture) seedProtocolConflict(t *testing.T, ctx context.Context) (base, source, target, objectID string) {
	t.Helper()
	f := r.f
	pObject, _ := f.createObject(t, ctx, f.main, "protocol",
		`{"purpose":"synthesize","steps":[{"id":"s1","temperature":300}]}`)
	feature, err := f.svc.CreateBranch(ctx, f.alice, f.project.ID, rsg.CreateBranchInput{
		Name:       "feature",
		BaseRef:    *f.head(t, ctx, f.main),
		Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create feature branch: %v", err)
	}
	f.updateObject(t, ctx, feature.ID, pObject, 1,
		`{"purpose":"synthesize","steps":[{"id":"s1","temperature":350}]}`)
	f.updateObject(t, ctx, f.main, pObject, 2,
		`{"purpose":"synthesize","steps":[{"id":"s1","temperature":400}]}`)
	return *feature.BaseStateID, *f.head(t, ctx, feature.ID), *f.head(t, ctx, f.main), pObject
}

// seedProtocolConflictTwoKeys builds the same conflict shape with TWO
// diverged payload keys: both branches moved "steps" and "atmosphere" to
// different values, so the detector reports payload_keys
// ["atmosphere","steps"] (its canonical sorted order).
func (r *resolutionFixture) seedProtocolConflictTwoKeys(t *testing.T, ctx context.Context) (base, source, target, objectID string) {
	t.Helper()
	f := r.f
	pObject, _ := f.createObject(t, ctx, f.main, "protocol",
		`{"purpose":"synthesize","steps":[{"id":"s1","temperature":300}],"atmosphere":"air"}`)
	feature, err := f.svc.CreateBranch(ctx, f.alice, f.project.ID, rsg.CreateBranchInput{
		Name:       "feature",
		BaseRef:    *f.head(t, ctx, f.main),
		Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create feature branch: %v", err)
	}
	f.updateObject(t, ctx, feature.ID, pObject, 1,
		`{"purpose":"synthesize","steps":[{"id":"s1","temperature":350}],"atmosphere":"argon"}`)
	f.updateObject(t, ctx, f.main, pObject, 2,
		`{"purpose":"synthesize","steps":[{"id":"s1","temperature":400}],"atmosphere":"vacuum"}`)
	return *feature.BaseStateID, *f.head(t, ctx, feature.ID), *f.head(t, ctx, f.main), pObject
}

// decision renders one decision on the seeded conflict.
func resolutionDecision(objectID, kind string) resolutions.Decision {
	return resolutions.Decision{
		TargetKind:  domain.ConflictResolutionTargetObject,
		TargetID:    objectID,
		Code:        conflict.CodeScientificFieldDiverges,
		Fields:      []string{"payload"},
		PayloadKeys: []string{"steps"},
		Kind:        domain.ResolutionKind(kind),
		Note:        "integration decision",
	}
}

// TestResolutionSavesAllFiveKinds pins the five human decisions through
// the real store: each kind is accepted, a re-save overwrites the row in
// place (never appends), and every save appends its own audit row naming
// the human who decided.
func TestResolutionSavesAllFiveKinds(t *testing.T) {
	ctx := testCtx(t)
	r := newResolutionFixture(t, ctx)
	base, source, target, objectID := r.seedProtocolConflict(t, ctx)

	kinds := []string{
		"accept_source", "accept_target", "keep_both", "validation_branch", "unresolved",
	}
	for _, kind := range kinds {
		plan, err := r.svc.Save(ctx, r.f.alice, resolutions.SaveInput{
			ProjectID:     r.f.project.ID,
			BaseStateID:   base,
			SourceStateID: source,
			TargetStateID: target,
			Decisions:     []resolutions.Decision{resolutionDecision(objectID, kind)},
		})
		if err != nil {
			t.Fatalf("Save kind %s: %v", kind, err)
		}
		if len(plan) != 1 || plan[0].Kind != domain.ResolutionKind(kind) {
			t.Fatalf("plan after %s = %+v, want exactly that decision", kind, plan)
		}
		if plan[0].DecidedBy != r.f.alice.ID {
			t.Fatalf("decided_by = %s, want the human actor %s", plan[0].DecidedBy, r.f.alice.ID)
		}
	}

	// Five saves, one row: the overwrite replaces in place.
	var rows int
	if err := r.f.pool.QueryRow(ctx,
		"SELECT count(*) FROM conflict_resolutions WHERE project_id = $1", r.f.project.ID).Scan(&rows); err != nil {
		t.Fatalf("count resolution rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("resolution rows = %d, want 1 (in-place overwrite)", rows)
	}
	// One audit row per save, each naming the human.
	var audits int
	if err := r.f.pool.QueryRow(ctx,
		"SELECT count(*) FROM audit_log WHERE action = 'conflict.resolution_saved' AND project_id = $1 AND actor_id = $2",
		r.f.project.ID, r.f.alice.ID).Scan(&audits); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if audits != 5 {
		t.Fatalf("audit rows = %d, want 5 (one per save)", audits)
	}

	// The view carries the latest decision.
	view, err := r.svc.View(ctx, r.f.project.ID, base, source, target)
	if err != nil {
		t.Fatalf("View: %v", err)
	}
	if len(view.Resolutions) != 1 || view.Resolutions[0].Kind != domain.ResolutionUnresolved {
		t.Fatalf("view resolutions = %+v, want the latest decision (unresolved)", view.Resolutions)
	}
	if view.Report == nil || len(view.Evidence) != 1 || view.Evidence[0].ObjectID != objectID {
		t.Fatalf("view = report %v evidence %+v, want the conflicted object's context", view.Report, view.Evidence)
	}
}

// TestResolutionRefusesUnknownConflict pins the identity check over the
// real stack: a decision naming a conflict the report does not contain is
// refused (ErrConflictNotFound) and never touches the store.
func TestResolutionRefusesUnknownConflict(t *testing.T) {
	ctx := testCtx(t)
	r := newResolutionFixture(t, ctx)
	base, source, target, objectID := r.seedProtocolConflict(t, ctx)

	_, err := r.svc.Save(ctx, r.f.alice, resolutions.SaveInput{
		ProjectID:     r.f.project.ID,
		BaseStateID:   base,
		SourceStateID: source,
		TargetStateID: target,
		Decisions: []resolutions.Decision{{
			TargetKind:  domain.ConflictResolutionTargetObject,
			TargetID:    "00000000-0000-4000-8000-000000000999", // not in the report
			Code:        conflict.CodeScientificFieldDiverges,
			Fields:      []string{"payload"},
			PayloadKeys: []string{"steps"},
			Kind:        domain.ResolutionAcceptSource,
		}},
	})
	if !errors.Is(err, resolutions.ErrConflictNotFound) {
		t.Fatalf("err = %v, want ErrConflictNotFound", err)
	}
	var rows int
	if err := r.f.pool.QueryRow(ctx,
		"SELECT count(*) FROM conflict_resolutions WHERE project_id = $1", r.f.project.ID).Scan(&rows); err != nil {
		t.Fatalf("count resolution rows: %v", err)
	}
	if rows != 0 {
		t.Fatalf("resolution rows = %d, want 0 (refused save)", rows)
	}
	var audits int
	if err := r.f.pool.QueryRow(ctx,
		"SELECT count(*) FROM audit_log WHERE action = 'conflict.resolution_saved' AND project_id = $1",
		r.f.project.ID).Scan(&audits); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if audits != 0 {
		t.Fatalf("audit rows = %d, want 0 (refused save)", audits)
	}
	_ = objectID
}

// TestResolutionShuffledPayloadKeysSingleRow pins the one-conflict-one-row
// guarantee at the DB index: the same decision submitted twice with the
// payload_keys order shuffled must still be ONE row. The service
// normalizes the stored identity with the same canonical order the
// classifier key uses, so the order-sensitive jsonb unique index (the
// store's upsert is keyed on exactly those columns) sees the same
// conflict and overwrites in place instead of inserting a second row.
func TestResolutionShuffledPayloadKeysSingleRow(t *testing.T) {
	ctx := testCtx(t)
	r := newResolutionFixture(t, ctx)
	base, source, target, objectID := r.seedProtocolConflictTwoKeys(t, ctx)

	first := resolutionDecision(objectID, "accept_source")
	first.PayloadKeys = []string{"steps", "atmosphere"} // shuffled, non-canonical
	plan, err := r.svc.Save(ctx, r.f.alice, resolutions.SaveInput{
		ProjectID:     r.f.project.ID,
		BaseStateID:   base,
		SourceStateID: source,
		TargetStateID: target,
		Decisions:     []resolutions.Decision{first},
	})
	if err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if len(plan) != 1 || len(plan[0].PayloadKeys) != 2 ||
		plan[0].PayloadKeys[0] != "atmosphere" || plan[0].PayloadKeys[1] != "steps" {
		t.Fatalf("stored payload_keys = %v, want the canonical order [atmosphere steps]", plan[0].PayloadKeys)
	}

	second := resolutionDecision(objectID, "keep_both")
	second.PayloadKeys = []string{"atmosphere", "steps"} // the detector's order
	if _, err := r.svc.Save(ctx, r.f.alice, resolutions.SaveInput{
		ProjectID:     r.f.project.ID,
		BaseStateID:   base,
		SourceStateID: source,
		TargetStateID: target,
		Decisions:     []resolutions.Decision{second},
	}); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	var rows int
	if err := r.f.pool.QueryRow(ctx,
		"SELECT count(*) FROM conflict_resolutions WHERE project_id = $1", r.f.project.ID).Scan(&rows); err != nil {
		t.Fatalf("count resolution rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("resolution rows = %d, want 1 (same conflict, shuffled payload_keys order)", rows)
	}
	// The stored identity is canonical, and the second save overwrote in
	// place: the latest decision kind wins.
	var storedKind, storedKeys string
	if err := r.f.pool.QueryRow(ctx,
		"SELECT resolution, conflict_payload_keys FROM conflict_resolutions WHERE project_id = $1",
		r.f.project.ID).Scan(&storedKind, &storedKeys); err != nil {
		t.Fatalf("read stored row: %v", err)
	}
	if storedKind != "keep_both" {
		t.Fatalf("stored resolution = %s, want the latest decision (keep_both)", storedKind)
	}
	var keys []string
	if err := json.Unmarshal([]byte(storedKeys), &keys); err != nil {
		t.Fatalf("stored conflict_payload_keys %q is not a JSON array: %v", storedKeys, err)
	}
	if len(keys) != 2 || keys[0] != "atmosphere" || keys[1] != "steps" {
		t.Fatalf("stored conflict_payload_keys = %s, want the canonical order [atmosphere steps]", storedKeys)
	}
}

// TestResolutionRefusesCrossProjectState pins the boundary: a triple that
// names another project's state is refused, never stored.
func TestResolutionRefusesCrossProjectState(t *testing.T) {
	ctx := testCtx(t)
	r := newResolutionFixture(t, ctx)
	base, source, _, objectID := r.seedProtocolConflict(t, ctx)

	// A second project with its own state.
	alice2, err := persistence.NewCredentialStore(r.f.pool).CreateWithPassword(
		ctx, "res-bob@example.com", "hash", "res-bob", "Bob")
	if err != nil {
		t.Fatalf("seed bob: %v", err)
	}
	org2, _, err := persistence.NewOrgStore(r.f.pool).CreateOrganization(ctx, domain.Organization{
		Slug: "res-fixture-2", Name: "Res Fixture 2",
	}, alice2.ID, todayUTC())
	if err != nil {
		t.Fatalf("create org 2: %v", err)
	}
	project2, _, err := persistence.NewProjectStore(r.f.pool).CreateProject(ctx, domain.Project{
		OrganizationID:  &org2.ID,
		Slug:            "res-project-2",
		Name:            "Res Project 2",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, alice2.ID)
	if err != nil {
		t.Fatalf("create project 2: %v", err)
	}
	main2, err := r.f.svc.CreateBranch(ctx, alice2, project2.ID, rsg.CreateBranchInput{
		Name:       "main",
		BaseRef:    "",
		Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create branch 2: %v", err)
	}
	state2, err := r.f.statesSvc.GetBranchHead(ctx, main2.ID)
	if err != nil {
		t.Fatalf("GetBranchHead 2: %v", err)
	}

	_, err = r.svc.Save(ctx, r.f.alice, resolutions.SaveInput{
		ProjectID:     r.f.project.ID,
		BaseStateID:   base,
		SourceStateID: source,
		TargetStateID: state2.ID, // a state of project 2
		Decisions:     []resolutions.Decision{resolutionDecision(objectID, "accept_source")},
	})
	if err == nil || !errors.Is(err, diffs.ErrValidation) {
		t.Fatalf("err = %v, want diffs.ErrValidation", err)
	}
	var rows int
	if err := r.f.pool.QueryRow(ctx,
		"SELECT count(*) FROM conflict_resolutions WHERE project_id = $1", r.f.project.ID).Scan(&rows); err != nil {
		t.Fatalf("count resolution rows: %v", err)
	}
	if rows != 0 {
		t.Fatalf("resolution rows = %d, want 0 (refused save)", rows)
	}
}
