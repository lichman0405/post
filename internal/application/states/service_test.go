package states

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/application/validation"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// testGuard builds a guard wired to a probe that sees nothing — enough for
// the service-shape tests (the guard itself is covered in the validation
// package; the integration tests exercise the real probe).
func testGuard(t *testing.T) *validation.Guard {
	t.Helper()
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	return validation.NewGuard(rsgvalidation.NewValidator(reg), &emptyProbe{})
}

// emptyProbe is a TxProbe that reports an empty branch — every write the
// guard then sees is the one the commit facts describe.
type emptyProbe struct{}

func (*emptyProbe) ListStatesTx(context.Context, validation.TxQuerier, string) ([]domain.ProjectState, error) {
	return nil, nil
}
func (*emptyProbe) ListCommitsTx(context.Context, validation.TxQuerier, string) ([]domain.StateCommit, error) {
	return nil, nil
}
func (*emptyProbe) ListStateObjectVersionsTx(context.Context, validation.TxQuerier, string) ([]domain.ScientificObjectVersion, error) {
	return nil, nil
}
func (*emptyProbe) ListStateRelationVersionsTx(context.Context, validation.TxQuerier, string) ([]domain.RelationVersion, error) {
	return nil, nil
}

// fakeRepo implements Repository for service-level tests: it records the
// validated CommitStateParams the service derives, so the tests prove the
// service computes the content hash and maps outcomes — not the adapter.
type fakeRepo struct {
	commitCalled bool
	commitIn     CommitStateParams
	commitWrite  WriteFunc
	commitState  domain.ProjectState
	commitCommit domain.StateCommit
	commitErr    error

	initialIn  InitialStateParams
	initialErr error
	initialOut domain.ProjectState

	headState domain.ProjectState
	headErr   error
}

func (f *fakeRepo) CommitState(_ context.Context, in CommitStateParams, write WriteFunc) (domain.ProjectState, domain.StateCommit, error) {
	f.commitCalled = true
	f.commitIn = in
	f.commitWrite = write
	return f.commitState, f.commitCommit, f.commitErr
}

func (f *fakeRepo) CreateInitialState(_ context.Context, in InitialStateParams) (domain.ProjectState, error) {
	f.initialIn = in
	return f.initialOut, f.initialErr
}

func (f *fakeRepo) GetState(context.Context, string) (domain.ProjectState, error) {
	return domain.ProjectState{}, nil
}
func (f *fakeRepo) GetStateByHash(context.Context, string, string) (domain.ProjectState, error) {
	return domain.ProjectState{}, nil
}
func (f *fakeRepo) GetBranchHead(_ context.Context, _ string) (domain.ProjectState, error) {
	return f.headState, f.headErr
}
func (f *fakeRepo) ListStates(context.Context, string) ([]domain.ProjectState, error) {
	return nil, nil
}
func (f *fakeRepo) GetCommit(context.Context, string) (domain.StateCommit, error) {
	return domain.StateCommit{}, nil
}
func (f *fakeRepo) ListCommits(context.Context, string) ([]domain.StateCommit, error) {
	return nil, nil
}
func (f *fakeRepo) ListStateObjectVersions(context.Context, string) ([]domain.ScientificObjectVersion, error) {
	return nil, nil
}
func (f *fakeRepo) ListStateRelationVersions(context.Context, string) ([]domain.RelationVersion, error) {
	return nil, nil
}

func validCommitParams() CommitParams {
	return CommitParams{
		ProjectID: "11111111-1111-1111-1111-111111111111",
		BranchID:  "22222222-2222-2222-2222-222222222222",
		ActorID:   "33333333-3333-3333-3333-333333333333",
		Via:       domain.ViaAPI,
		Message:   "create the first hypothesis",
		Operations: []domain.StateOperation{
			{Kind: domain.OperationObjectVersionCreated, EntityID: "44444444-4444-4444-4444-444444444444", VersionNo: 1},
		},
		ManifestVersion: "v1",
		Gate:            rsgvalidation.GateDraft,
	}
}

func TestCommitValidatesAndComputesHash(t *testing.T) {
	ctx := context.Background()
	repo := &fakeRepo{
		commitState:  domain.ProjectState{ID: "state-1"},
		commitCommit: domain.StateCommit{ID: "commit-1"},
	}
	svc := NewService(repo, testGuard(t))
	in := validCommitParams()
	wantHash, err := domain.ComputeStateHash(in.BaseStateID, in.Operations)
	if err != nil {
		t.Fatalf("ComputeStateHash: %v", err)
	}
	write := func(context.Context, Transaction, string) error { return nil }
	state, commit, err := svc.Commit(ctx, in, write)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if state.ID != "state-1" || commit.ID != "commit-1" {
		t.Fatalf("Commit returned %+v %+v, want the repo's values", state, commit)
	}
	if !repo.commitCalled {
		t.Fatal("repo.CommitState not called")
	}
	if repo.commitIn.StateHash != wantHash {
		t.Errorf("StateHash = %q, want %q (service-computed content address)", repo.commitIn.StateHash, wantHash)
	}
	if repo.commitWrite == nil {
		t.Error("write callback not passed through to the repo")
	}
	opsJSON, err := json.Marshal(repo.commitIn.Operations)
	if err != nil {
		t.Fatalf("marshal ops: %v", err)
	}
	if string(opsJSON) != `[{"kind":"object_version_created","entity_id":"44444444-4444-4444-4444-444444444444","version_no":1}]` {
		t.Errorf("operations marshaled to %s, want the canonical shape", opsJSON)
	}
}

func TestCommitRejectsInvalidInputs(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		mutate func(*CommitParams)
	}{
		{"nil write callback", func(*CommitParams) {}},
		{"empty project", func(p *CommitParams) { p.ProjectID = "" }},
		{"empty branch", func(p *CommitParams) { p.BranchID = "" }},
		{"empty actor", func(p *CommitParams) { p.ActorID = "" }},
		{"unknown via", func(p *CommitParams) { p.Via = "webhook" }},
		{"empty message", func(p *CommitParams) { p.Message = "   " }},
		{"no operations", func(p *CommitParams) { p.Operations = nil }},
		{"unknown operation kind", func(p *CommitParams) {
			p.Operations = []domain.StateOperation{{Kind: "object_created", EntityID: "x"}}
		}},
		{"empty operation entity", func(p *CommitParams) {
			p.Operations = []domain.StateOperation{{Kind: domain.OperationBlobAttached}}
		}},
		{"invalid operation detail", func(p *CommitParams) {
			p.Operations = []domain.StateOperation{{Kind: domain.OperationBlobAttached, EntityID: "x", Detail: json.RawMessage(`{bad`)}}
		}},
		{"empty manifest version", func(p *CommitParams) { p.ManifestVersion = "" }},
		{"empty gate", func(p *CommitParams) { p.Gate = "" }},
		{"unknown gate", func(p *CommitParams) { p.Gate = "gold" }},
		{"release gate is not a commit gate", func(p *CommitParams) { p.Gate = rsgvalidation.GateRelease }},
		{"asset gate is not a commit gate", func(p *CommitParams) { p.Gate = rsgvalidation.GateAsset }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeRepo{}
			svc := NewService(repo, testGuard(t))
			in := validCommitParams()
			tc.mutate(&in)
			var write WriteFunc = func(context.Context, Transaction, string) error { return nil }
			if tc.name == "nil write callback" {
				write = nil
			}
			if _, _, err := svc.Commit(ctx, in, write); !errors.Is(err, ErrValidation) {
				t.Fatalf("Commit error = %v, want ErrValidation", err)
			}
			if repo.commitCalled {
				t.Error("repo.CommitState called for invalid input")
			}
		})
	}
}

func TestCommitUnwrapsCommitWriteError(t *testing.T) {
	ctx := context.Background()
	repo := &fakeRepo{commitErr: &CommitWriteError{Err: errors.New("the object version insert failed")}}
	svc := NewService(repo, testGuard(t))
	_, _, err := svc.Commit(ctx, validCommitParams(), func(context.Context, Transaction, string) error { return nil })
	if err == nil || err.Error() != "the object version insert failed" {
		t.Fatalf("Commit error = %v, want the callback's own error unwrapped", err)
	}
}

func TestCommitMapsStoreOutcomes(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		err    error
		wantIs error
	}{
		{"state conflict passes through", &StateConflictError{BranchID: "b"}, nil},
		{"state exists passes through", ErrStateExists, ErrStateExists},
		{"branch not found passes through", ErrBranchNotFound, ErrBranchNotFound},
		{"adapter failure becomes ErrStore", errors.New("connection refused"), ErrStore},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeRepo{commitErr: tc.err}
			svc := NewService(repo, testGuard(t))
			_, _, err := svc.Commit(ctx, validCommitParams(), func(context.Context, Transaction, string) error { return nil })
			if tc.wantIs == nil {
				var sc *StateConflictError
				if !errors.As(err, &sc) {
					t.Fatalf("error = %v, want *StateConflictError", err)
				}
				return
			}
			if !errors.Is(err, tc.wantIs) {
				t.Fatalf("error = %v, want %v", err, tc.wantIs)
			}
		})
	}
}

func TestCommitConflictCode(t *testing.T) {
	e := &StateConflictError{BranchID: "b", Expected: ptr("e"), Actual: ptr("a")}
	if e.Code() != CodeBranchStateConflict {
		t.Errorf("Code() = %q, want %q", e.Code(), CodeBranchStateConflict)
	}
	if e.Error() == "" {
		t.Error("Error() is empty")
	}
}

func TestCreateInitialStateComputesGenesisHash(t *testing.T) {
	ctx := context.Background()
	repo := &fakeRepo{initialOut: domain.ProjectState{ID: "genesis-1"}}
	svc := NewService(repo, testGuard(t))
	state, err := svc.CreateInitialState(ctx, CreateInitialStateParams{
		ProjectID:       "11111111-1111-1111-1111-111111111111",
		ManifestVersion: "v1",
	})
	if err != nil {
		t.Fatalf("CreateInitialState: %v", err)
	}
	if state.ID != "genesis-1" {
		t.Fatalf("returned %+v, want the repo's value", state)
	}
	want, err := domain.ComputeStateHash(nil, nil)
	if err != nil {
		t.Fatalf("ComputeStateHash: %v", err)
	}
	if repo.initialIn.StateHash != want {
		t.Errorf("StateHash = %q, want the genesis hash %q", repo.initialIn.StateHash, want)
	}
	if repo.initialIn.ManifestVersion != "v1" {
		t.Errorf("ManifestVersion = %q, want v1", repo.initialIn.ManifestVersion)
	}
}

func TestCreateInitialStateRejectsInvalid(t *testing.T) {
	ctx := context.Background()
	repo := &fakeRepo{}
	svc := NewService(repo, testGuard(t))
	if _, err := svc.CreateInitialState(ctx, CreateInitialStateParams{ManifestVersion: "v1"}); !errors.Is(err, ErrValidation) {
		t.Errorf("empty project error = %v, want ErrValidation", err)
	}
	if _, err := svc.CreateInitialState(ctx, CreateInitialStateParams{ProjectID: "p"}); !errors.Is(err, ErrValidation) {
		t.Errorf("empty manifest version error = %v, want ErrValidation", err)
	}
}

func TestGetBranchHeadMapsNotFound(t *testing.T) {
	ctx := context.Background()
	repo := &fakeRepo{headErr: ErrStateNotFound}
	svc := NewService(repo, testGuard(t))
	if _, err := svc.GetBranchHead(ctx, "b"); !errors.Is(err, ErrStateNotFound) {
		t.Fatalf("GetBranchHead error = %v, want ErrStateNotFound", err)
	}
}

// probeWithUnknownSchema is a TxProbe that shows the guard one version row
// whose schema is not registered — a blocking failure at every gate.
type probeWithUnknownSchema struct{}

func (p *probeWithUnknownSchema) ListStatesTx(_ context.Context, _ validation.TxQuerier, branchID string) ([]domain.ProjectState, error) {
	return []domain.ProjectState{{ID: "state-1", BranchID: &branchID}}, nil
}
func (p *probeWithUnknownSchema) ListCommitsTx(context.Context, validation.TxQuerier, string) ([]domain.StateCommit, error) {
	return nil, nil
}
func (p *probeWithUnknownSchema) ListStateObjectVersionsTx(_ context.Context, _ validation.TxQuerier, _ string) ([]domain.ScientificObjectVersion, error) {
	return []domain.ScientificObjectVersion{{
		ID: "v-1", ObjectID: "44444444-4444-4444-4444-444444444444", VersionNo: 1,
		StateID: "state-1", SchemaID: "https://example.com/unknown.schema.json",
		SchemaVersion: "1", Title: "H", LifecycleState: domain.LifecycleActive,
		Payload: json.RawMessage(`{"statement":"probe"}`), CreatedBy: "33333333-3333-3333-3333-333333333333",
		CreatedAt: timeNow(), IntegrityHash: strings.Repeat("0", 64),
	}}, nil
}
func (p *probeWithUnknownSchema) ListStateRelationVersionsTx(context.Context, validation.TxQuerier, string) ([]domain.RelationVersion, error) {
	return nil, nil
}

func timeNow() (t time.Time) { return time.Now().UTC() }

// TestCommitRunsTheGuardInsideTheTransaction pins the "command 再次 server
// validate" contract at the service seam: the write the adapter receives is
// the caller's callback wrapped with the guard, and a blocked gate surfaces
// as *rsgvalidation.GateBlockedError carrying the full report.
func TestCommitRunsTheGuardInsideTheTransaction(t *testing.T) {
	ctx := context.Background()
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	repo := &fakeRepo{}
	svc := NewService(repo, validation.NewGuard(rsgvalidation.NewValidator(reg), &probeWithUnknownSchema{}))
	in := validCommitParams()
	if _, _, err := svc.Commit(ctx, in, func(context.Context, Transaction, string) error { return nil }); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if repo.commitWrite == nil {
		t.Fatal("the service did not pass a write callback to the repo")
	}

	// The guard runs after the write inside the transaction: the unknown
	// schema the probe shows blocks the draft gate.
	err = repo.commitWrite(ctx, nil, "state-1")
	var blocked *rsgvalidation.GateBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("wrapped write error = %v, want *GateBlockedError", err)
	}
	if blocked.Report.Gate != rsgvalidation.GateDraft {
		t.Errorf("report gate = %s, want draft (the commit's own gate)", blocked.Report.Gate)
	}
	if blocked.Code() != "SCHEMA_VALIDATION_FAILED" {
		t.Errorf("Code() = %s, want SCHEMA_VALIDATION_FAILED", blocked.Code())
	}
}

func TestCommitWriteShortCircuitsTheGuard(t *testing.T) {
	ctx := context.Background()
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	writeErr := errors.New("the object version insert failed")
	repo := &fakeRepo{}
	svc := NewService(repo, validation.NewGuard(rsgvalidation.NewValidator(reg), &probeWithUnknownSchema{}))
	if _, _, err := svc.Commit(ctx, validCommitParams(), func(context.Context, Transaction, string) error {
		return writeErr
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := repo.commitWrite(ctx, nil, "state-1"); !errors.Is(err, writeErr) {
		t.Fatalf("wrapped write error = %v, want the write's own error (the guard must not mask a failed write)", err)
	}
}

func ptr(s string) *string { return &s }
