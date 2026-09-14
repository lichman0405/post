package gitprovider_test

import (
	"context"
	"errors"
	"testing"

	"github.com/lichman0405/post/internal/gitprovider"
)

// The T0309 reconciler's policy in isolation: which observations become
// which findings with which repair proposals, and the two failure shapes —
// a canonical-store failure aborts the pass, a provider failure only
// aborts the provider section (recorded, never fatal, never false
// findings). The store/persistence half is covered by the integration
// test (tests/integration/git_reconciliation_test.go); here the store is
// scripted and every finding the pass records is captured verbatim.

type recordedFinding struct {
	runID string
	f     gitprovider.NewFinding
}

type fakeReconcilerStore struct {
	states        []gitprovider.GitStateCheck
	violations    []gitprovider.MappingViolation
	mustExist     []gitprovider.CheckedRef
	mustBeGone    []gitprovider.CheckedRef
	repos         []gitprovider.ProvisionedRepo
	names         map[string]bool
	statesErr     error
	violationsErr error
	mustExistErr  error
	mustBeGoneErr error
	reposErr      error
	namesErr      error

	findings      []recordedFinding
	resolvedDrift []gitprovider.FindingKey
	resolvedKinds []gitprovider.FindingKind
	resolveResult int
	resolveErr    error
	finished      gitprovider.RunSummary
	finishedOpen  int // the open-finding count FinishRun reports
	finishErr     error
}

func (s *fakeReconcilerStore) BeginRun(context.Context) (string, error) { return "run-1", nil }
func (s *fakeReconcilerStore) FinishRun(_ context.Context, sum gitprovider.RunSummary) (int, error) {
	s.finished = sum
	return s.finishedOpen, s.finishErr
}
func (s *fakeReconcilerStore) MustExistRefs(context.Context) ([]gitprovider.CheckedRef, error) {
	return s.mustExist, s.mustExistErr
}
func (s *fakeReconcilerStore) MustBeGoneRefs(context.Context) ([]gitprovider.CheckedRef, error) {
	return s.mustBeGone, s.mustBeGoneErr
}
func (s *fakeReconcilerStore) GitStates(context.Context) ([]gitprovider.GitStateCheck, error) {
	return s.states, s.statesErr
}
func (s *fakeReconcilerStore) MappingViolations(context.Context) ([]gitprovider.MappingViolation, error) {
	return s.violations, s.violationsErr
}
func (s *fakeReconcilerStore) ProvisionedRepos(context.Context) ([]gitprovider.ProvisionedRepo, error) {
	return s.repos, s.reposErr
}
func (s *fakeReconcilerStore) BranchNames(_ context.Context, _ string) (map[string]bool, error) {
	return s.names, s.namesErr
}
func (s *fakeReconcilerStore) RecordFinding(_ context.Context, runID string, f gitprovider.NewFinding) (bool, error) {
	s.findings = append(s.findings, recordedFinding{runID: runID, f: f})
	return true, nil
}
func (s *fakeReconcilerStore) ResolveFindings(_ context.Context, drifted []gitprovider.FindingKey, kinds []gitprovider.FindingKind) (int, error) {
	s.resolvedDrift = append([]gitprovider.FindingKey{}, drifted...)
	s.resolvedKinds = append([]gitprovider.FindingKind{}, kinds...)
	return s.resolveResult, s.resolveErr
}

// scriptedPort answers per (owner, name, ref-name) so each checked ref can
// be scripted independently.
type scriptedPort struct {
	refs     map[string]gitprovider.BranchRef // key: owner/name/branch
	refsErr  map[string]error
	lists    map[string][]gitprovider.BranchRef // key: owner/name
	listsErr map[string]error
	repos    map[string]gitprovider.Repository // key: owner/name
	reposErr map[string]error
	calls    []string
}

func newScriptedPort() *scriptedPort {
	return &scriptedPort{
		refs: map[string]gitprovider.BranchRef{}, refsErr: map[string]error{},
		lists: map[string][]gitprovider.BranchRef{}, listsErr: map[string]error{},
		repos: map[string]gitprovider.Repository{}, reposErr: map[string]error{},
	}
}

func (p *scriptedPort) GetBranch(_ context.Context, repo gitprovider.Repository, name string) (gitprovider.BranchRef, error) {
	key := repo.Owner + "/" + repo.Name + "/" + name
	p.calls = append(p.calls, "get:"+key)
	if err := p.refsErr[key]; err != nil {
		return gitprovider.BranchRef{}, err
	}
	if ref, ok := p.refs[key]; ok {
		return ref, nil
	}
	return gitprovider.BranchRef{}, gitprovider.ErrNotFound
}

func (p *scriptedPort) ListBranches(_ context.Context, repo gitprovider.Repository) ([]gitprovider.BranchRef, error) {
	key := repo.Owner + "/" + repo.Name
	p.calls = append(p.calls, "list:"+key)
	if err := p.listsErr[key]; err != nil {
		return nil, err
	}
	return p.lists[key], nil
}

func (p *scriptedPort) GetRepository(_ context.Context, owner, name string) (gitprovider.Repository, error) {
	key := owner + "/" + name
	p.calls = append(p.calls, "repo:"+key)
	if err := p.reposErr[key]; err != nil {
		return gitprovider.Repository{}, err
	}
	if repo, ok := p.repos[key]; ok {
		return repo, nil
	}
	return gitprovider.Repository{}, gitprovider.ErrNotFound
}

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	shaC = "cccccccccccccccccccccccccccccccccccccccc"
)

// TestReconcilerRefFindings: a synced ref whose provider head moved
// becomes ref_head_moved with the record-unrecorded-push proposal; a
// synced ref that is gone becomes ref_missing with the re-sync proposal; a
// closed ref that exists becomes dangling_ref; an unmapped provider ref
// becomes unmapped_ref; a provision whose repository is gone becomes
// repository_missing (and its refs are then skipped — the finding already
// covers them). No repair is ever applied — the store sees only finding
// records.
func TestReconcilerRefFindings(t *testing.T) {
	ctx := context.Background()
	store := &fakeReconcilerStore{
		repos: []gitprovider.ProvisionedRepo{
			{Owner: "o", Name: "ok1", ProjectID: "p1"},
			{Owner: "o", Name: "ok2", ProjectID: "p2"},
			{Owner: "o", Name: "gone", ProjectID: "p3"},
		},
		mustExist: []gitprovider.CheckedRef{
			{BranchID: "b1", ProjectID: "p1", GitRef: "refs/heads/topic", Name: "topic", Owner: "o", Repo: "ok1", HeadSHA: shaA},
			{BranchID: "b2", ProjectID: "p2", GitRef: "refs/heads/lost", Name: "lost", Owner: "o", Repo: "ok2", HeadSHA: shaA},
			// In the missing repository: skipped, already covered by the
			// repository_missing finding.
			{BranchID: "b4", ProjectID: "p3", GitRef: "refs/heads/skip", Name: "skip", Owner: "o", Repo: "gone", HeadSHA: shaA},
		},
		mustBeGone: []gitprovider.CheckedRef{
			{BranchID: "b3", ProjectID: "p1", GitRef: "refs/heads/closed", Name: "closed", Owner: "o", Repo: "ok1", HeadSHA: shaA},
		},
		names: map[string]bool{"topic": true, "closed": true, "lost": true},
	}
	port := newScriptedPort()
	port.repos["o/ok1"] = gitprovider.Repository{Owner: "o", Name: "ok1"}
	port.repos["o/ok2"] = gitprovider.Repository{Owner: "o", Name: "ok2"}
	port.refs["o/ok1/topic"] = gitprovider.BranchRef{Name: "topic", HeadSHA: shaB}   // moved
	port.refs["o/ok1/closed"] = gitprovider.BranchRef{Name: "closed", HeadSHA: shaA} // dangling
	port.lists["o/ok1"] = []gitprovider.BranchRef{
		{Name: "main", HeadSHA: shaA}, // protected main (T0302): never unmapped
		{Name: "topic", HeadSHA: shaB},
		{Name: "sneaky", HeadSHA: shaC}, // unmapped
	}

	rec := gitprovider.NewReconciler(port, store)
	sum, err := rec.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if sum.ProviderError != "" {
		t.Fatalf("unexpected provider error: %s", sum.ProviderError)
	}
	if sum.FindingsOpened != 5 {
		t.Fatalf("findings opened = %d, want 5 (repo-missing, unmapped, moved, missing, dangling)", sum.FindingsOpened)
	}
	if len(store.findings) != 5 {
		t.Fatalf("recorded findings = %d, want 5", len(store.findings))
	}

	// Provider section order: repository existence first, then unmapped
	// refs, then must-exist refs, then must-be-gone refs.
	repomiss := store.findings[0].f
	if repomiss.Kind != gitprovider.FindingRepositoryMissing || repomiss.SubjectRef != "o/gone" || repomiss.Repair["action"] != "re_provision" {
		t.Fatalf("finding 0 = %+v", repomiss)
	}

	unmapped := store.findings[1].f
	if unmapped.Kind != gitprovider.FindingUnmappedRef || unmapped.SubjectRef != "refs/heads/sneaky" {
		t.Fatalf("finding 1 = %+v", unmapped)
	}
	if unmapped.Repair["action"] != "adopt_or_delete" {
		t.Fatalf("finding 1 repair = %v", unmapped.Repair)
	}
	if _, ok := unmapped.Repair["options"].([]string); !ok {
		t.Fatalf("finding 1 repair options = %v", unmapped.Repair["options"])
	}

	moved := store.findings[2].f
	if moved.Kind != gitprovider.FindingRefHeadMoved || moved.SubjectRef != "refs/heads/topic" || moved.ProjectID != "p1" {
		t.Fatalf("finding 2 = %+v", moved)
	}
	if moved.Repair["action"] != "record_unrecorded_push" || moved.Repair["before"] != shaA || moved.Repair["after"] != shaB {
		t.Fatalf("finding 2 repair = %v", moved.Repair)
	}

	missing := store.findings[3].f
	if missing.Kind != gitprovider.FindingRefMissing || missing.SubjectRef != "refs/heads/lost" {
		t.Fatalf("finding 3 = %+v", missing)
	}
	if missing.Repair["action"] != "enqueue_branch_ref_sync" || missing.Repair["direction"] != "create" || missing.Repair["branch_id"] != "b2" {
		t.Fatalf("finding 3 repair = %v", missing.Repair)
	}

	dangling := store.findings[4].f
	if dangling.Kind != gitprovider.FindingDanglingRef || dangling.Repair["direction"] != "close" || dangling.Repair["branch_id"] != "b3" {
		t.Fatalf("finding 4 = %+v", dangling)
	}
}

// TestReconcilerStateHashAndMapping: a git state whose stored hash
// disagrees with GitStateHash becomes state_hash_mismatch naming the
// derived value; each mapping violation class becomes mapping_violation
// with its own repair.
func TestReconcilerStateHashAndMapping(t *testing.T) {
	ctx := context.Background()
	store := &fakeReconcilerStore{
		states: []gitprovider.GitStateCheck{
			{StateID: "s1", ProjectID: "p1", CommitSHA: shaA, StoredHash: gitprovider.GitStateHash(shaB)}, // wrong
			{StateID: "s2", ProjectID: "p1", CommitSHA: shaA, StoredHash: gitprovider.GitStateHash(shaA)}, // right
		},
		violations: []gitprovider.MappingViolation{
			{Violation: gitprovider.MappingBranchUnmapped, ProjectID: "p1", BranchID: "b1", GitRef: "refs/heads/x"},
			{Violation: gitprovider.MappingProvisionMissing, ProjectID: "p2"},
		},
	}

	rec := gitprovider.NewReconciler(nil, store) // no provider: canonical only
	sum, err := rec.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if sum.FindingsOpened != 3 || sum.StatesChecked != 2 || sum.MappingViolations != 2 {
		t.Fatalf("summary = %+v, want 3 findings over 2 states + 2 mappings", sum)
	}

	hashFinding := store.findings[0].f
	if hashFinding.Kind != gitprovider.FindingStateHashMismatch || hashFinding.SubjectRef != "s1" {
		t.Fatalf("finding 0 = %+v", hashFinding)
	}
	if hashFinding.Detail["derived_hash"] != gitprovider.GitStateHash(shaA) {
		t.Fatalf("finding 0 detail = %v", hashFinding.Detail)
	}
	if hashFinding.Repair["action"] != "restore_derived_state_hash" || hashFinding.Repair["state_id"] != "s1" {
		t.Fatalf("finding 0 repair = %v", hashFinding.Repair)
	}

	unmappedBranch := store.findings[1].f
	if unmappedBranch.Kind != gitprovider.FindingMappingViolation ||
		unmappedBranch.Detail["violation"] != "branch_unmapped" ||
		unmappedBranch.Repair["action"] != "insert_mapping_row" {
		t.Fatalf("finding 1 = %+v", unmappedBranch)
	}

	provisionMissing := store.findings[2].f
	if provisionMissing.Kind != gitprovider.FindingMappingViolation ||
		provisionMissing.Detail["violation"] != "provision_mapping_missing" ||
		provisionMissing.Repair["action"] != "re_provision" {
		t.Fatalf("finding 2 = %+v", provisionMissing)
	}

	// Resolution must be scoped to the canonical kinds only (no provider).
	if len(store.resolvedKinds) != 2 || store.resolvedKinds[0] != gitprovider.FindingStateHashMismatch || store.resolvedKinds[1] != gitprovider.FindingMappingViolation {
		t.Fatalf("resolved kinds = %v, want the two canonical kinds", store.resolvedKinds)
	}
}

// TestReconcilerProviderOutage: a provider failure aborts only the
// provider section — the pass completes, provider_error is recorded on the
// run, no provider findings are created (a down provider is not "every ref
// missing"), and provider-kind findings are NOT resolved (they were not
// re-verified).
func TestReconcilerProviderOutage(t *testing.T) {
	ctx := context.Background()
	store := &fakeReconcilerStore{
		repos:     []gitprovider.ProvisionedRepo{{Owner: "o", Name: "r", ProjectID: "p1"}},
		mustExist: []gitprovider.CheckedRef{{BranchID: "b1", ProjectID: "p1", GitRef: "refs/heads/t", Name: "t", Owner: "o", Repo: "r", HeadSHA: shaA}},
		states:    []gitprovider.GitStateCheck{{StateID: "s1", ProjectID: "p1", CommitSHA: shaA, StoredHash: "nope"}},
	}
	port := newScriptedPort()
	port.reposErr["o/r"] = gitprovider.ErrUnavailable

	rec := gitprovider.NewReconciler(port, store)
	sum, err := rec.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v (a provider outage must not fail the pass)", err)
	}
	if sum.ProviderError == "" {
		t.Fatal("provider_error not recorded")
	}
	// The canonical state-hash finding still lands; no ref findings.
	if len(store.findings) != 1 || store.findings[0].f.Kind != gitprovider.FindingStateHashMismatch {
		t.Fatalf("findings = %+v, want only the canonical one", store.findings)
	}
	if len(store.resolvedKinds) != 2 {
		t.Fatalf("resolved kinds = %v, want canonical kinds only", store.resolvedKinds)
	}
}

// TestReconcilerCanonicalStoreFailure: a canonical-store read failure
// aborts the pass and returns the error — the run row stays unfinished
// (visible), the pass never silently completes.
func TestReconcilerCanonicalStoreFailure(t *testing.T) {
	ctx := context.Background()
	store := &fakeReconcilerStore{statesErr: errors.New("postgres down")}

	rec := gitprovider.NewReconciler(nil, store)
	if _, err := rec.Reconcile(ctx); err == nil {
		t.Fatal("Reconcile: want error for a canonical-store failure")
	}
	if len(store.findings) != 0 {
		t.Fatalf("findings recorded despite store failure: %+v", store.findings)
	}
}

// TestReconcilerDedupeIsStoreConcern: the reconciler forwards every
// observation; dedupe against open findings is the store's job (the
// integration test asserts the dedupe behavior on the real store).
func TestReconcilerDedupeIsStoreConcern(t *testing.T) {
	ctx := context.Background()
	store := &fakeReconcilerStore{
		states: []gitprovider.GitStateCheck{{StateID: "s1", ProjectID: "p1", CommitSHA: shaA, StoredHash: "nope"}},
	}
	rec := gitprovider.NewReconciler(nil, store)
	if _, err := rec.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(store.findings) != 1 {
		t.Fatalf("findings = %+v", store.findings)
	}
}

// TestReconcilerFindingsOpenPropagates: FinishRun owns the pass-end
// open-finding count (the store reads it with the run update, the summary
// is passed by value). Reconcile must carry the RETURNED count into the
// summary it hands the sweep — the sweep's 'findings_open' log line reads
// that field, and a 0 there while findings are open is a misleading
// operational signal.
func TestReconcilerFindingsOpenPropagates(t *testing.T) {
	ctx := context.Background()
	store := &fakeReconcilerStore{finishedOpen: 3}
	rec := gitprovider.NewReconciler(nil, store)
	sum, err := rec.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if sum.FindingsOpen != 3 {
		t.Fatalf("FindingsOpen = %d, want 3 — FinishRun's open-finding count must reach the caller's summary", sum.FindingsOpen)
	}
}
