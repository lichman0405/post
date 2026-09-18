package integration

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/gitprovider"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/testdb"
)

// Required test "reconciliation integration": the full T0309 chain — the
// periodic Git ↔ RSG drift check against real PostgreSQL, with the
// provider half scripted (CI-safe) and, for the live-boundary coverage,
// against a REAL Gitea instance (skipped loudly in CI, run at the G3 gate
// like the other gitea_* tests). The acceptance criteria are exercised
// end to end: drift constructed by the test is DETECTED (each class
// becomes its own high-severity finding), every NEW finding appends the
// system audit row (via='system', severity 'high' — the "high-severity
// alert/audit" half), and the repair proposals are recorded but NEVER
// applied: the checked surfaces (git_branch_refs heads, project_states,
// ingestion rows, provisions) stay exactly as the test left them.

const reconciliationTaskID = "T0309"

const (
	reconSHA1 = "1111111111111111111111111111111111111111"
	reconSHA2 = "2222222222222222222222222222222222222222"
	reconSHA3 = "3333333333333333333333333333333333333333"
)

// reconciliationFixture: one migrated test database, one user, one
// project, its genesis state (no provider commit — the state-hash check
// covers only git-pinned states) and the branches service. Drift is then
// constructed row by row: the canonical half through direct SQL (the
// fixture half of the drift), the provider half through the scripted
// port.
type reconciliationFixture struct {
	pool     *pgxpool.Pool
	branches *branches.Service
	alice    domain.User
	project  domain.Project
	genesis  domain.ProjectState
}

func newReconciliationFixture(t *testing.T, ctx context.Context) *reconciliationFixture {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), reconciliationTaskID)
	alice, err := persistence.NewCredentialStore(pool).CreateWithPassword(
		ctx, "reconciliation-alice@example.com", "hash", "reconciliation-alice", "Alice")
	if err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	project, _, err := persistence.NewProjectStore(pool).CreateProject(ctx, domain.Project{
		Slug:            "reconciliation-project",
		Name:            "Reconciliation Project",
		Purpose:         "T0309 integration",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, alice.ID)
	if err != nil {
		t.Fatalf("create fixture project: %v", err)
	}
	var genesisID string
	if err := pool.QueryRow(ctx, `INSERT INTO project_states
		(project_id, state_hash, manifest_version, git_commit_sha)
		VALUES ($1, 'genesis-recon', 'v1', NULL) RETURNING id`, project.ID).Scan(&genesisID); err != nil {
		t.Fatalf("create genesis state: %v", err)
	}
	return &reconciliationFixture{
		pool:     pool,
		branches: branches.NewService(persistence.NewBranchStore(pool)),
		alice:    alice,
		project:  project,
		genesis:  domain.ProjectState{ID: genesisID},
	}
}

// provision seeds the T0301 mapping row the provider checks join on.
func (f *reconciliationFixture) provision(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	if _, err := f.pool.Exec(ctx, `INSERT INTO git_repository_provisions
		(project_id, owner, name, gitea_repo_id, webhook_id, webhook_secret)
		VALUES ($1::uuid, 'recon-svc', $2, 1, 1, 's')`, f.project.ID, name); err != nil {
		t.Fatalf("seed provision row: %v", err)
	}
}

func (f *reconciliationFixture) createBranch(t *testing.T, ctx context.Context, name string) domain.Branch {
	t.Helper()
	branch, err := f.branches.Create(ctx, branches.CreateBranchParams{
		ProjectID:   f.project.ID,
		Name:        name,
		Visibility:  domain.BranchVisibilityPrivate,
		BaseStateID: f.genesis.ID,
		CreatedBy:   f.alice.ID,
	})
	if err != nil {
		t.Fatalf("create branch %s: %v", name, err)
	}
	return branch
}

// setMapping drives one mapping row through 00031's state machine into
// the given state with the given recorded head.
func (f *reconciliationFixture) setMapping(t *testing.T, ctx context.Context, branchID, state, headSHA string) {
	t.Helper()
	var err error
	switch state {
	case "synced":
		_, err = f.pool.Exec(ctx, `UPDATE git_branch_refs
			SET sync_state = 'synced', fork_sha = $2, head_sha = $2
			WHERE branch_id = $1`, branchID, headSHA)
	case "closed":
		if _, err = f.pool.Exec(ctx, `UPDATE git_branch_refs
			SET sync_state = 'closing', close_requested_at = now()
			WHERE branch_id = $1`, branchID); err == nil {
			_, err = f.pool.Exec(ctx, `UPDATE git_branch_refs
				SET sync_state = 'closed', head_sha = $2, closed_at = now()
				WHERE branch_id = $1`, branchID, headSHA)
		}
	}
	if err != nil {
		t.Fatalf("drive mapping %s to %s: %v", branchID, state, err)
	}
}

// gitState records one git-pinned project state with the given stored
// hash — correct (GitStateHash) or deliberately wrong (the drift).
func (f *reconciliationFixture) gitState(t *testing.T, ctx context.Context, sha, hash string) string {
	t.Helper()
	var id string
	if err := f.pool.QueryRow(ctx, `INSERT INTO project_states
		(project_id, state_hash, manifest_version, git_commit_sha)
		VALUES ($1, $2, 'v1', $3) RETURNING id`,
		f.project.ID, hash, sha).Scan(&id); err != nil {
		t.Fatalf("insert git state: %v", err)
	}
	return id
}

// ---- Canonical-row probes (the shapes the assertions consume).

type reconFindingRow struct {
	ID      string
	Kind    string
	Subject string
	Status  string
	Repair  map[string]any
}

func (f *reconciliationFixture) findingRows(t *testing.T, ctx context.Context) []reconFindingRow {
	t.Helper()
	rows, err := f.pool.Query(ctx, `SELECT id::text, kind, subject_ref, status, repair_proposal
		FROM git_reconciliation_findings ORDER BY created_at, id`)
	if err != nil {
		t.Fatalf("probe findings: %v", err)
	}
	defer rows.Close()
	var out []reconFindingRow
	for rows.Next() {
		var r reconFindingRow
		var repair []byte
		if err := rows.Scan(&r.ID, &r.Kind, &r.Subject, &r.Status, &repair); err != nil {
			t.Fatalf("scan finding: %v", err)
		}
		if err := json.Unmarshal(repair, &r.Repair); err != nil {
			t.Fatalf("decode repair proposal: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("findings rows: %v", err)
	}
	return out
}

type reconRunRow struct {
	Finished    bool
	ProviderErr string
	Refs        int
	States      int
	Violations  int // mapping_violations: broken mapping invariants FOUND
	Repos       int // repositories_checked: provisioned repositories VERIFIED
	Opened      int
	Open        int
	Resolved    int
}

func (f *reconciliationFixture) runRows(t *testing.T, ctx context.Context) []reconRunRow {
	t.Helper()
	rows, err := f.pool.Query(ctx, `SELECT finished_at IS NOT NULL, COALESCE(provider_error, ''),
		refs_checked, states_checked, mapping_violations, repositories_checked, findings_opened, findings_open, findings_resolved
		FROM git_reconciliation_runs ORDER BY started_at, id`)
	if err != nil {
		t.Fatalf("probe runs: %v", err)
	}
	defer rows.Close()
	var out []reconRunRow
	for rows.Next() {
		var r reconRunRow
		if err := rows.Scan(&r.Finished, &r.ProviderErr, &r.Refs, &r.States, &r.Violations, &r.Repos,
			&r.Opened, &r.Open, &r.Resolved); err != nil {
			t.Fatalf("scan run: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("run rows: %v", err)
	}
	return out
}

// driftAudits returns the audit rows the findings appended: the
// "high-severity alert/audit" acceptance evidence — via='system' with no
// actor, the stable action name, severity 'high' in the metadata.
type driftAuditRow struct {
	Severity string
	Kind     string
	RunID    string
	Via      string
	NoActor  bool
}

func (f *reconciliationFixture) driftAudits(t *testing.T, ctx context.Context) []driftAuditRow {
	t.Helper()
	rows, err := f.pool.Query(ctx, `SELECT metadata->>'severity', metadata->>'kind', metadata->>'run_id',
		via, actor_id IS NULL
		FROM audit_log
		WHERE action = 'git.reconciliation.drift_detected'
		ORDER BY occurred_at, id`)
	if err != nil {
		t.Fatalf("probe drift audits: %v", err)
	}
	defer rows.Close()
	var out []driftAuditRow
	for rows.Next() {
		var a driftAuditRow
		if err := rows.Scan(&a.Severity, &a.Kind, &a.RunID, &a.Via, &a.NoActor); err != nil {
			t.Fatalf("scan drift audit: %v", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("drift audit rows: %v", err)
	}
	return out
}

func (f *reconciliationFixture) countRows(t *testing.T, ctx context.Context, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// ---- Scripted provider (the CI-safe provider half).

// reconPort scripts the provider half of the drift without Gitea: which
// repositories exist, which refs exist at which heads, which ref lists
// come back — plus per-key errors for the outage shapes.
type reconPort struct {
	repos    map[string]gitprovider.Repository // key: owner/name
	reposErr map[string]error
	refs     map[string]gitprovider.BranchRef // key: owner/name/branch
	refsErr  map[string]error
	lists    map[string][]gitprovider.BranchRef // key: owner/name
	listsErr map[string]error
}

func newReconPort() *reconPort {
	return &reconPort{
		repos:    map[string]gitprovider.Repository{},
		reposErr: map[string]error{},
		refs:     map[string]gitprovider.BranchRef{},
		refsErr:  map[string]error{},
		lists:    map[string][]gitprovider.BranchRef{},
		listsErr: map[string]error{},
	}
}

func (p *reconPort) GetRepository(_ context.Context, owner, name string) (gitprovider.Repository, error) {
	key := owner + "/" + name
	if err := p.reposErr[key]; err != nil {
		return gitprovider.Repository{}, err
	}
	if repo, ok := p.repos[key]; ok {
		return repo, nil
	}
	return gitprovider.Repository{}, gitprovider.ErrNotFound
}

func (p *reconPort) GetBranch(_ context.Context, repo gitprovider.Repository, name string) (gitprovider.BranchRef, error) {
	key := repo.Owner + "/" + repo.Name + "/" + name
	if err := p.refsErr[key]; err != nil {
		return gitprovider.BranchRef{}, err
	}
	if ref, ok := p.refs[key]; ok {
		return ref, nil
	}
	return gitprovider.BranchRef{}, gitprovider.ErrNotFound
}

func (p *reconPort) ListBranches(_ context.Context, repo gitprovider.Repository) ([]gitprovider.BranchRef, error) {
	key := repo.Owner + "/" + repo.Name
	if err := p.listsErr[key]; err != nil {
		return nil, err
	}
	return p.lists[key], nil
}

// TestReconciliationDetectsDriftAndAlerts is the core acceptance evidence:
// the test constructs every drift class, one pass detects each as its own
// high-severity finding with a repair proposal, every NEW finding appends
// the system audit row, and NOTHING is repaired — the checked surfaces are
// untouched. A second pass over the same drift adds no finding and no
// audit row (dedupe); healing the drift resolves the findings; a drift
// that recurs after resolution is a NEW finding (a resolved row never
// reopens).
func TestReconciliationDetectsDriftAndAlerts(t *testing.T) {
	ctx := testCtx(t)
	fx := newReconciliationFixture(t, ctx)
	fx.provision(t, ctx, "p-recon")

	// ---- Construct the drift, canonical half.
	// A git-pinned state whose stored hash disagrees with the derived one
	// (and a healthy sibling — two states checked, one finding).
	badState := fx.gitState(t, ctx, reconSHA1, "corrupted-hash")
	fx.gitState(t, ctx, reconSHA2, gitprovider.GitStateHash(reconSHA2))

	// Branch mappings: synced rows the provider contradicts (head moved /
	// ref gone), a closed row whose ref lingers, a pending row whose
	// derived ref was corrupted (guard bypassed — the drift the check
	// exists to catch), and a branch whose mapping row was deleted.
	topic := fx.createBranch(t, ctx, "topic")
	fx.setMapping(t, ctx, topic.ID, "synced", reconSHA1)
	lost := fx.createBranch(t, ctx, "lost")
	fx.setMapping(t, ctx, lost.ID, "synced", reconSHA1)
	closed := fx.createBranch(t, ctx, "closed")
	fx.setMapping(t, ctx, closed.ID, "closed", reconSHA1)
	mismatch := fx.createBranch(t, ctx, "mismatch")
	fx.bypassGuard(t, ctx, `UPDATE git_branch_refs SET git_ref = 'refs/heads/derailed' WHERE branch_id = $1`, mismatch.ID)
	unmapped := fx.createBranch(t, ctx, "unmapped")
	if _, err := fx.pool.Exec(ctx, `DELETE FROM git_branch_refs WHERE branch_id = $1`, unmapped.ID); err != nil {
		t.Fatalf("delete mapping row: %v", err)
	}

	// A second project whose provision row names a repository the provider
	// no longer has (repository_missing), and a third that claims
	// provisioned without any provision row (provision_mapping_missing).
	proj2, _, err := persistence.NewProjectStore(fx.pool).CreateProject(ctx, domain.Project{
		Slug: "reconciliation-gone", Name: "Gone Project", Purpose: "T0309 integration",
		Visibility: domain.VisibilityPrivate, ProvisionStatus: domain.ProvisionPending,
	}, fx.alice.ID)
	if err != nil {
		t.Fatalf("create gone project: %v", err)
	}
	if _, err := fx.pool.Exec(ctx, `INSERT INTO git_repository_provisions
		(project_id, owner, name, gitea_repo_id, webhook_id, webhook_secret)
		VALUES ($1::uuid, 'recon-svc', 'p-gone', 2, 2, 's')`, proj2.ID); err != nil {
		t.Fatalf("seed gone provision row: %v", err)
	}
	proj3, _, err := persistence.NewProjectStore(fx.pool).CreateProject(ctx, domain.Project{
		Slug: "reconciliation-unprovisioned", Name: "Unprovisioned Project", Purpose: "T0309 integration",
		Visibility: domain.VisibilityPrivate, ProvisionStatus: domain.ProvisionPending,
	}, fx.alice.ID)
	if err != nil {
		t.Fatalf("create unprovisioned project: %v", err)
	}
	// CreateProject always births 'pending'; the drift shape is the
	// project CLAIMING provisioned with no provision row — set directly
	// (00022's consistency check requires the external id alongside).
	if _, err := fx.pool.Exec(ctx, `UPDATE projects
		SET provision_status = 'provisioned', git_repository_external_id = 'gitea:999'
		WHERE id = $1`, proj3.ID); err != nil {
		t.Fatalf("mark unprovisioned project provisioned: %v", err)
	}

	// ---- Construct the drift, provider half.
	// p-recon: topic moved, closed lingers, lost is gone, sneaky is
	// unmapped, main is the protected ref (skipped, never unmapped).
	// p-gone: the repository no longer exists.
	port := newReconPort()
	port.repos["recon-svc/p-recon"] = gitprovider.Repository{Owner: "recon-svc", Name: "p-recon"}
	port.refs["recon-svc/p-recon/topic"] = gitprovider.BranchRef{Name: "topic", HeadSHA: reconSHA2}
	port.refs["recon-svc/p-recon/closed"] = gitprovider.BranchRef{Name: "closed", HeadSHA: reconSHA1}
	port.lists["recon-svc/p-recon"] = []gitprovider.BranchRef{
		{Name: "main", HeadSHA: reconSHA1},
		{Name: "topic", HeadSHA: reconSHA2},
		{Name: "closed", HeadSHA: reconSHA1},
		{Name: "sneaky", HeadSHA: reconSHA3},
	}

	// ---- Pass 1: every drift detected, nothing repaired.
	rec := gitprovider.NewReconciler(port, gitprovider.NewReconcilerStore(fx.pool))
	sum, err := rec.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if sum.FindingsOpened != 9 {
		t.Fatalf("findings opened = %d, want 9 (state hash, 3 mapping, repo, unmapped, missing, moved, dangling)", sum.FindingsOpened)
	}
	if sum.ProviderError != "" {
		t.Fatalf("unexpected provider error: %s", sum.ProviderError)
	}
	if sum.RefsChecked != 7 || sum.StatesChecked != 2 || sum.MappingViolations != 3 || sum.RepositoriesChecked != 1 {
		t.Errorf("scope counts = %d refs / %d states / %d violations / %d repos, want 7/2/3/1", sum.RefsChecked, sum.StatesChecked, sum.MappingViolations, sum.RepositoriesChecked)
	}

	findings := fx.findingRows(t, ctx)
	wantKinds := []string{
		"state_hash_mismatch", // the corrupted hash
		"mapping_violation",   // branch_unmapped
		"mapping_violation",   // derived_ref_mismatch
		"mapping_violation",   // provision_mapping_missing
		"repository_missing",
		"unmapped_ref",
		"ref_missing",
		"ref_head_moved",
		"dangling_ref",
	}
	if len(findings) != len(wantKinds) {
		t.Fatalf("findings = %d, want %d: %+v", len(findings), len(wantKinds), findings)
	}
	for i, kind := range wantKinds {
		if findings[i].Kind != kind {
			t.Errorf("finding %d kind = %s, want %s", i, findings[i].Kind, kind)
		}
		if findings[i].Status != "open" {
			t.Errorf("finding %d status = %s, want open", i, findings[i].Status)
		}
	}
	if findings[0].Subject != badState {
		t.Errorf("state-hash finding subject = %s, want the state %s", findings[0].Subject, badState)
	}
	if findings[0].Repair["action"] != "restore_derived_state_hash" {
		t.Errorf("state-hash repair = %v", findings[0].Repair)
	}
	if findings[5].Subject != "refs/heads/sneaky" {
		t.Errorf("unmapped subject = %s", findings[5].Subject)
	}
	if findings[7].Subject != "refs/heads/topic" {
		t.Errorf("moved subject = %s", findings[7].Subject)
	}
	if findings[7].Repair["action"] != "record_unrecorded_push" ||
		findings[7].Repair["before"] != reconSHA1 || findings[7].Repair["after"] != reconSHA2 {
		t.Errorf("moved repair = %v, want the unrecorded push proposal", findings[7].Repair)
	}
	if findings[6].Repair["action"] != "enqueue_branch_ref_sync" || findings[6].Repair["direction"] != "create" {
		t.Errorf("missing repair = %v", findings[6].Repair)
	}
	if findings[8].Repair["action"] != "enqueue_branch_ref_sync" || findings[8].Repair["direction"] != "close" {
		t.Errorf("dangling repair = %v", findings[8].Repair)
	}

	// High-severity alert/audit: one system audit row per NEW finding,
	// severity 'high', no actor, the stable action name (the probe itself
	// filters on it), run-correlated. Rows are compared as a multiset —
	// findings that land in the same instant share a created_at.
	audits := fx.driftAudits(t, ctx)
	if len(audits) != 9 {
		t.Fatalf("drift audit rows = %d, want 9", len(audits))
	}
	kindCounts := map[string]int{}
	for _, kind := range wantKinds {
		kindCounts[kind]++
	}
	for i, a := range audits {
		if a.Severity != "high" {
			t.Errorf("audit %d severity = %q, want high", i, a.Severity)
		}
		if a.Via != "system" || !a.NoActor {
			t.Errorf("audit %d via = %q noActor %v, want system with no actor", i, a.Via, a.NoActor)
		}
		if a.RunID != sum.RunID {
			t.Errorf("audit %d run = %s, want %s", i, a.RunID, sum.RunID)
		}
		kindCounts[a.Kind]--
	}
	for kind, left := range kindCounts {
		if left != 0 {
			t.Errorf("audit kind %s: %d rows, want the finding count", kind, kindCounts[kind]+1)
		}
	}

	// NO silent repair: the checked surfaces are exactly as the test left
	// them — the moved head is not recorded, the missing ref is not
	// re-created, the deleted mapping row is not restored, the corrupted
	// ref is not healed, the missing repo is not re-provisioned, the wrong
	// hash is not rewritten.
	var topicHead string
	if err := fx.pool.QueryRow(ctx, `SELECT COALESCE(head_sha, '') FROM git_branch_refs WHERE branch_id = $1`, topic.ID).Scan(&topicHead); err != nil {
		t.Fatalf("probe topic head: %v", err)
	}
	if topicHead != reconSHA1 {
		t.Errorf("topic head = %s, want %s (the reconciler must not record the unrecorded push)", topicHead, reconSHA1)
	}
	if fx.countRows(t, ctx, `SELECT count(*) FROM git_branch_refs WHERE branch_id = $1`, unmapped.ID) != 0 {
		t.Error("deleted mapping row was restored — the reconciler must not repair")
	}
	if fx.countRows(t, ctx, `SELECT count(*) FROM git_branch_refs WHERE git_ref = 'refs/heads/derailed'`) != 1 {
		t.Error("corrupted git_ref was healed — the reconciler must not repair")
	}
	if fx.countRows(t, ctx, `SELECT count(*) FROM project_states`) != 3 {
		t.Error("project_states rows changed — the reconciler must not write checked surfaces")
	}
	if fx.countRows(t, ctx, `SELECT count(*) FROM git_push_ingestions`) != 0 {
		t.Error("ingestion rows appeared — the reconciler must not record pushes")
	}
	if fx.countRows(t, ctx, `SELECT count(*) FROM git_repository_provisions`) != 2 {
		t.Error("provision rows changed — the reconciler must not re-provision")
	}

	runs := fx.runRows(t, ctx)
	if len(runs) != 1 || !runs[0].Finished {
		t.Fatalf("runs = %+v, want one finished run", runs)
	}
	if runs[0].Opened != 9 || runs[0].Open != 9 || runs[0].Resolved != 0 {
		t.Errorf("run outcome = %+v, want 9 opened / 9 open / 0 resolved", runs[0])
	}
	// The run row's scope columns are one column one meaning: the pass
	// found 3 broken mapping invariants (branch_unmapped,
	// derived_ref_mismatch, provision_mapping_missing) AND verified 1
	// provisioned repository's existence (p-gone is missing — a finding,
	// not a verification).
	if runs[0].Violations != 3 || runs[0].Repos != 1 {
		t.Errorf("run scope = %+v, want mapping_violations 3 / repositories_checked 1", runs[0])
	}

	// ---- Pass 2 (same drift): dedupe — the open findings are re-used, no
	// new row, no new alert.
	if _, err := rec.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile (pass 2): %v", err)
	}
	runs = fx.runRows(t, ctx)
	if len(runs) != 2 || runs[1].Opened != 0 || runs[1].Open != 9 || runs[1].Resolved != 0 {
		t.Errorf("pass-2 run = %+v, want 0 opened / 9 open / 0 resolved", runs[1])
	}
	if len(fx.driftAudits(t, ctx)) != 9 {
		t.Error("pass 2 appended audit rows for the already-open findings")
	}

	// ---- Heal every drift except the state hash (project_states is
	// append-only-guarded — healing it is deliberately out of reach here):
	// the provider and the mapping return to their goal states, and the
	// next pass resolves the eight findings it no longer observes.
	port.refs["recon-svc/p-recon/lost"] = gitprovider.BranchRef{Name: "lost", HeadSHA: reconSHA1}
	delete(port.refs, "recon-svc/p-recon/closed")
	port.lists["recon-svc/p-recon"] = []gitprovider.BranchRef{
		{Name: "main", HeadSHA: reconSHA1},
		{Name: "topic", HeadSHA: reconSHA2},
		{Name: "lost", HeadSHA: reconSHA1},
	}
	port.repos["recon-svc/p-gone"] = gitprovider.Repository{Owner: "recon-svc", Name: "p-gone"}
	if _, err := fx.pool.Exec(ctx, `UPDATE git_branch_refs SET head_sha = $2 WHERE branch_id = $1`, topic.ID, reconSHA2); err != nil {
		t.Fatalf("heal topic head: %v", err)
	}
	if _, err := fx.pool.Exec(ctx, `INSERT INTO git_branch_refs (branch_id, git_ref)
		VALUES ($1, 'refs/heads/unmapped')`, unmapped.ID); err != nil {
		t.Fatalf("heal deleted mapping row: %v", err)
	}
	fx.bypassGuard(t, ctx, `UPDATE git_branch_refs SET git_ref = 'refs/heads/mismatch' WHERE branch_id = $1`, mismatch.ID)
	// The unprovisioned project gets its provision row; the provider
	// answers for the new repository too (it has no branches, so only the
	// existence check runs).
	if _, err := fx.pool.Exec(ctx, `INSERT INTO git_repository_provisions
		(project_id, owner, name, gitea_repo_id, webhook_id, webhook_secret)
		VALUES ($1::uuid, 'recon-svc', 'p-healed', 3, 3, 's')`, proj3.ID); err != nil {
		t.Fatalf("heal unprovisioned project: %v", err)
	}
	port.repos["recon-svc/p-healed"] = gitprovider.Repository{Owner: "recon-svc", Name: "p-healed"}
	if _, err := rec.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile (healed pass): %v", err)
	}
	runs = fx.runRows(t, ctx)
	if len(runs) != 3 || runs[2].Resolved != 8 || runs[2].Open != 1 || runs[2].Opened != 0 {
		t.Errorf("healed-pass run = %+v, want 8 resolved / 1 open (the state hash) / 0 opened", runs[2])
	}
	// The two scope dimensions are independent: this pass found ZERO
	// mapping violations (all three healed) while it VERIFIED three
	// provisioned repositories' existence — the run row must say both.
	if runs[2].Violations != 0 || runs[2].Repos != 3 {
		t.Errorf("healed-pass scope = %+v, want mapping_violations 0 / repositories_checked 3", runs[2])
	}
	findings = fx.findingRows(t, ctx)
	openCount, resolvedCount := 0, 0
	for _, f := range findings {
		switch f.Status {
		case "open":
			openCount++
			if f.Kind != "state_hash_mismatch" {
				t.Errorf("still-open finding = %s %s, want only the state-hash one", f.Kind, f.Subject)
			}
		case "resolved":
			resolvedCount++
		}
	}
	if openCount != 1 || resolvedCount != 8 {
		t.Errorf("open %d / resolved %d, want 1 / 8", openCount, resolvedCount)
	}

	// ---- A drift that recurs after resolution is a NEW finding (the
	// resolved row never reopens — the guard): move topic's head again and
	// the pass opens a fresh row, leaving the resolved one resolved.
	port.refs["recon-svc/p-recon/topic"] = gitprovider.BranchRef{Name: "topic", HeadSHA: reconSHA3}
	if _, err := rec.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile (recurrence pass): %v", err)
	}
	runs = fx.runRows(t, ctx)
	if len(runs) != 4 || runs[3].Opened != 1 || runs[3].Resolved != 0 {
		t.Errorf("recurrence run = %+v, want 1 opened / 0 resolved", runs[3])
	}
	findings = fx.findingRows(t, ctx)
	var movedRows int
	for _, f := range findings {
		if f.Kind == "ref_head_moved" && f.Subject == "refs/heads/topic" {
			movedRows++
		}
	}
	if movedRows != 2 {
		t.Errorf("ref_head_moved rows for topic = %d, want 2 (one resolved, one new open)", movedRows)
	}
	if len(fx.driftAudits(t, ctx)) != 10 {
		t.Error("the recurring drift did not append its own audit row")
	}
}

// bypassGuard disables one guard trigger around the statement — the
// deliberate bypass that constructs the guard-breach drift shapes.
func (f *reconciliationFixture) bypassGuard(t *testing.T, ctx context.Context, stmt string, args ...any) {
	t.Helper()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin bypass tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `ALTER TABLE git_branch_refs DISABLE TRIGGER git_branch_ref_guard_trigger`); err != nil {
		t.Fatalf("disable guard: %v", err)
	}
	if _, err := tx.Exec(ctx, stmt, args...); err != nil {
		t.Fatalf("bypass stmt %q: %v", stmt, err)
	}
	if _, err := tx.Exec(ctx, `ALTER TABLE git_branch_refs ENABLE TRIGGER git_branch_ref_guard_trigger`); err != nil {
		t.Fatalf("re-enable guard: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit bypass tx: %v", err)
	}
}

// TestReconciliationProviderOutage: a provider failure aborts only the
// provider-side checks — the pass completes with provider_error recorded
// on the run, no false findings appear (a down provider is not "every ref
// missing"), an open provider finding is NOT resolved by a pass that
// could not re-verify it, and the next healthy pass resolves it.
func TestReconciliationProviderOutage(t *testing.T) {
	ctx := testCtx(t)
	fx := newReconciliationFixture(t, ctx)
	fx.provision(t, ctx, "p-recon")
	topic := fx.createBranch(t, ctx, "topic")
	fx.setMapping(t, ctx, topic.ID, "synced", reconSHA1)

	port := newReconPort()
	port.repos["recon-svc/p-recon"] = gitprovider.Repository{Owner: "recon-svc", Name: "p-recon"}
	port.refs["recon-svc/p-recon/topic"] = gitprovider.BranchRef{Name: "topic", HeadSHA: reconSHA2}
	port.lists["recon-svc/p-recon"] = []gitprovider.BranchRef{
		{Name: "main", HeadSHA: reconSHA1},
		{Name: "topic", HeadSHA: reconSHA2},
	}
	rec := gitprovider.NewReconciler(port, gitprovider.NewReconcilerStore(fx.pool))

	if _, err := rec.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if findings := fx.findingRows(t, ctx); len(findings) != 1 || findings[0].Kind != "ref_head_moved" {
		t.Fatalf("findings = %+v, want one ref_head_moved", findings)
	}

	// The provider goes down: the pass must still complete, record the
	// outage, and neither create nor resolve provider findings.
	port.reposErr["recon-svc/p-recon"] = gitprovider.ErrUnavailable
	sum, err := rec.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile (outage pass): %v", err)
	}
	if sum.ProviderError == "" {
		t.Fatal("provider_error not recorded")
	}
	runs := fx.runRows(t, ctx)
	if len(runs) != 2 || runs[1].ProviderErr == "" {
		t.Fatalf("runs = %+v, want the outage on the second run", runs)
	}
	if runs[1].Opened != 0 || runs[1].Resolved != 0 || runs[1].Open != 1 {
		t.Errorf("outage run = %+v, want the finding left untouched (0 opened / 0 resolved / 1 open)", runs[1])
	}
	if findings := fx.findingRows(t, ctx); len(findings) != 1 || findings[0].Status != "open" {
		t.Errorf("findings after outage = %+v, want the provider finding still open", findings)
	}

	// The provider recovers and the drift is healed: the next pass
	// resolves the finding.
	delete(port.reposErr, "recon-svc/p-recon")
	if _, err := fx.pool.Exec(ctx, `UPDATE git_branch_refs SET head_sha = $2 WHERE branch_id = $1`, topic.ID, reconSHA2); err != nil {
		t.Fatalf("heal topic head: %v", err)
	}
	if _, err := rec.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile (recovered pass): %v", err)
	}
	runs = fx.runRows(t, ctx)
	if len(runs) != 3 || runs[2].ProviderErr != "" || runs[2].Resolved != 1 || runs[2].Open != 0 {
		t.Errorf("recovered run = %+v, want no provider error, 1 resolved, 0 open", runs[2])
	}
}

// ---- Live-Gitea half (skipped loudly in CI — no Gitea service there; the
// real-instance coverage is the G3 gate's gitea-real-services override).

// pushBranchCommit advances one branch ref by a real commit whose parent
// is base, with a real git client — no webhook delivery, the push the
// platform never recorded.
func pushBranchCommit(t *testing.T, base, token, owner, name, branch, baseSHA string) string {
	t.Helper()
	dir := t.TempDir()
	remoteURL := base + "/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + ".git"
	env := append(os.Environ(), "HOME="+dir)
	env = append(env, gitAuthEnv("Authorization: token "+token)...)
	run := func(args ...string) string {
		t.Helper()
		full := append([]string{"-C", dir}, args...)
		cmd := exec.Command("git", full...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gitea integration: git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-b", "work")
	run("fetch", "--depth=1", remoteURL, baseSHA)
	run("reset", "--hard", "FETCH_HEAD")
	if err := os.WriteFile(filepath.Join(dir, "drift.txt"), []byte("an unrecorded push\n"), 0o644); err != nil {
		t.Fatalf("gitea integration: write push file: %v", err)
	}
	run("config", "user.email", "gitea-integration@example.com")
	run("config", "user.name", "Gitea Integration")
	run("add", "-A")
	run("commit", "-m", "T0309 drift: unrecorded push")
	sha := run("rev-parse", "HEAD")
	run("push", remoteURL, "HEAD:refs/heads/"+branch)
	if sha == "" {
		t.Fatal("gitea integration: rev-parse HEAD came back empty")
	}
	return sha
}

// TestGiteaReconciliationDetectsRealDrift is the acceptance evidence over
// the live boundary: real provider drift — a push whose webhook never
// arrived, a ref created out-of-band, a closed branch whose ref lingers —
// and the pass detects each with the repair proposal, records the alert,
// and repairs NOTHING: the recorded head stays, no ingestion row appears.
func TestGiteaReconciliationDetectsRealDrift(t *testing.T) {
	ctx := testCtx(t)
	fx := newBranchRefGiteaFixture(t, ctx)
	owner, name := fx.provision(t, ctx)
	mainSHA := fx.seedMain(t, owner, name)
	// seedMain's delivery-trigger push leaves a test-only ref behind; it
	// is fixture residue, not the drift under test — remove it.
	fx.deleteRef(t, owner, name, "gitea-delivery")

	// The fork state carries the T0305-shaped identity: state_hash =
	// GitStateHash(sha) — a correct row the state-hash check must pass.
	forkState := insertGitState(t, ctx, fx.pool, fx.project.ID, mainSHA, gitprovider.GitStateHash(mainSHA))
	topic := fx.createBranch(t, ctx, "feature-x", forkState)
	if err := fx.syncer().Sync(ctx, topic.ID); err != nil {
		t.Fatalf("gitea integration: Sync (create): %v", err)
	}
	_, _, _, headSHA, _ := fx.mappingRow(t, ctx, topic.ID)
	if headSHA != mainSHA {
		t.Fatalf("gitea integration: recorded head = %s, want %s", headSHA, mainSHA)
	}

	// Drift 1: a real push with no webhook delivery — the ref advances,
	// the recorded head does not.
	newSHA := pushBranchCommit(t, fx.base, fx.token, owner, name, "feature-x", mainSHA)

	// Drift 2: a ref created out-of-band that no branch row names.
	fx.createRefAt(t, owner, name, "sneaky", mainSHA)

	// Drift 3: a merged branch whose close never deleted the ref — the
	// row reaches closed (its recorded final head), the provider ref
	// lingers.
	merged := fx.createBranch(t, ctx, "feature-y", forkState)
	if err := fx.syncer().Sync(ctx, merged.ID); err != nil {
		t.Fatalf("gitea integration: Sync (feature-y create): %v", err)
	}
	if _, err := fx.branches.Merge(ctx, fx.project.ID, merged.ID); err != nil {
		t.Fatalf("gitea integration: Merge: %v", err)
	}
	if _, err := fx.pool.Exec(ctx, `UPDATE git_branch_refs
		SET sync_state = 'closed', head_sha = $2, closed_at = now()
		WHERE branch_id = $1`, merged.ID, mainSHA); err != nil {
		t.Fatalf("gitea integration: close mapping row without deleting the ref: %v", err)
	}

	// ---- The pass: three findings, no repairs.
	rec := gitprovider.NewReconciler(
		gitprovider.NewGiteaAdapter(fx.cfg), gitprovider.NewReconcilerStore(fx.pool))
	sum, err := rec.Reconcile(ctx)
	if err != nil {
		t.Fatalf("gitea integration: Reconcile: %v", err)
	}
	if sum.FindingsOpened != 3 {
		t.Fatalf("gitea integration: findings opened = %d, want 3 (moved, unmapped, dangling)", sum.FindingsOpened)
	}
	if sum.ProviderError != "" {
		t.Fatalf("gitea integration: unexpected provider error: %s", sum.ProviderError)
	}

	findings := findingRowsOn(t, ctx, fx.pool)
	if len(findings) != 3 {
		t.Fatalf("gitea integration: findings = %+v, want 3", findings)
	}
	bySubject := map[string]reconFindingRow{}
	for _, f := range findings {
		bySubject[f.Subject] = f
	}
	moved, ok := bySubject["refs/heads/feature-x"]
	if !ok || moved.Kind != "ref_head_moved" {
		t.Errorf("gitea integration: moved finding = %+v", moved)
	}
	if moved.Repair["action"] != "record_unrecorded_push" ||
		moved.Repair["before"] != mainSHA || moved.Repair["after"] != newSHA {
		t.Errorf("gitea integration: moved repair = %v, want the unrecorded push %s..%s", moved.Repair, mainSHA, newSHA)
	}
	if sneaky, ok := bySubject["refs/heads/sneaky"]; !ok || sneaky.Kind != "unmapped_ref" {
		t.Errorf("gitea integration: unmapped finding = %+v", sneaky)
	}
	if dangling, ok := bySubject["refs/heads/feature-y"]; !ok || dangling.Kind != "dangling_ref" {
		t.Errorf("gitea integration: dangling finding = %+v", dangling)
	}

	// NO silent repair: the recorded head is untouched and the unrecorded
	// push was not ingested.
	_, _, _, headSHA, _ = fx.mappingRow(t, ctx, topic.ID)
	if headSHA != mainSHA {
		t.Errorf("gitea integration: recorded head changed to %s — the reconciler must not record pushes", headSHA)
	}
	var ingestions int
	if err := fx.pool.QueryRow(ctx, `SELECT count(*) FROM git_push_ingestions`).Scan(&ingestions); err != nil {
		t.Fatalf("gitea integration: count ingestions: %v", err)
	}
	if ingestions != 0 {
		t.Errorf("gitea integration: %d ingestion rows appeared — the reconciler must not ingest", ingestions)
	}

	// High-severity alert: one system audit row per finding.
	audits := driftAuditsOn(t, ctx, fx.pool)
	if len(audits) != 3 {
		t.Fatalf("gitea integration: drift audit rows = %d, want 3", len(audits))
	}
	for i, a := range audits {
		if a.Severity != "high" || a.Via != "system" || !a.NoActor || a.RunID != sum.RunID {
			t.Errorf("gitea integration: audit %d = %+v, want high/system/no-actor on this run", i, a)
		}
	}

	// A second pass over the same drift adds nothing (dedupe).
	if _, err := rec.Reconcile(ctx); err != nil {
		t.Fatalf("gitea integration: Reconcile (pass 2): %v", err)
	}
	if runs := runRowsOn(t, ctx, fx.pool); len(runs) != 2 || runs[1].Opened != 0 || runs[1].Open != 3 {
		t.Errorf("gitea integration: pass-2 run = %+v, want 0 opened / 3 open", runs[1])
	}
	if len(driftAuditsOn(t, ctx, fx.pool)) != 3 {
		t.Error("gitea integration: pass 2 appended audit rows for the already-open findings")
	}
}

// insertGitState records one git-pinned project state with the exact
// stored hash (fixture setup — the T0305 ingestion fact).
func insertGitState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID, sha, hash string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx, `INSERT INTO project_states
		(project_id, state_hash, manifest_version, git_commit_sha)
		VALUES ($1, $2, 'v1', $3) RETURNING id`,
		projectID, hash, sha).Scan(&id); err != nil {
		t.Fatalf("gitea integration: insert fork state: %v", err)
	}
	return id
}

// findingRowsOn / runRowsOn / driftAuditsOn are the pool-level variants
// of the fixture probes for the Gitea fixture (which carries its own
// fixture type).
func findingRowsOn(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []reconFindingRow {
	t.Helper()
	return (&reconciliationFixture{pool: pool}).findingRows(t, ctx)
}

func runRowsOn(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []reconRunRow {
	t.Helper()
	return (&reconciliationFixture{pool: pool}).runRows(t, ctx)
}

func driftAuditsOn(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []driftAuditRow {
	t.Helper()
	return (&reconciliationFixture{pool: pool}).driftAudits(t, ctx)
}
